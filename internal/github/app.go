package github

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
)

const (
	// tokenMargin: a cached installation token is used until this long
	// before it expires (installation_token, F:2377).
	tokenMargin = 5 * time.Minute
	// maxKeyBytes caps the key file read; an RSA PEM is a few KiB.
	maxKeyBytes = 64 << 10
)

// App is the GitHub App: it signs its JWT with the App's private key and
// mints installation tokens with it.
//
// Its Clock should be the real clock even when the factory's own clock is
// fixed (FACTORY_TEST_NOW): GitHub checks iat, exp and expires_at against its
// own time, which is why Python reads time.time() here and nowhere else.
type App struct {
	// ClientID is [github] app_client_id, the JWT's iss (str() of the
	// config value).
	ClientID string
	// InstallationID is [github] installation_id, as it appears in the
	// path.
	InstallationID string
	// KeyPath is the PEM file ([github] key, default
	// ~/.config/factory/github-app.pem), already expanded: this package
	// does not read HOME.
	KeyPath string
	Clock   clock.Clock
	// API is the client the token is minted through; its Auth is not used.
	API *Client

	mu      sync.Mutex // guards the cache; held while minting, so one mint at a time
	token   string
	expires time.Time
}

// Installation is the answer to minting an installation token.
type Installation struct {
	Token     string
	ExpiresAt time.Time
	// Permissions is the token's permissions object, in GitHub's order
	// (factory github prints it sorted).
	Permissions jsonx.Object
}

// JWT is github_app_jwt (F:2352): a 9-minute RS256 JWT with header
// {"alg": "RS256", "typ": "JWT"} and claims {"iat": now-60, "exp": now+540,
// "iss": ClientID}, both as Python's json.dumps writes them, base64url
// without padding. The signature is PKCS #1 v1.5 over SHA-256, which is what
// "openssl dgst -sha256 -sign" made. The key is read on every call.
func (a *App) JWT() (string, error) {
	key, err := readKey(a.KeyPath)
	if err != nil {
		return "", fmt.Errorf("signing the GitHub App JWT failed: %w", err)
	}
	if a.Clock == nil {
		return "", errors.New("signing the GitHub App JWT failed: no clock")
	}
	issued := a.Clock.Now().Unix()
	header := jsonx.ObjectOf(
		jsonx.Member{Key: "alg", Value: jsonx.String("RS256")},
		jsonx.Member{Key: "typ", Value: jsonx.String("JWT")})
	claims := jsonx.ObjectOf(
		jsonx.Member{Key: "iat", Value: jsonx.Int(issued - 60)},
		jsonx.Member{Key: "exp", Value: jsonx.Int(issued + 540)},
		jsonx.Member{Key: "iss", Value: jsonx.String(a.ClientID)})
	signing := b64url(jsonx.EncodePython(header, -1)) + "." + b64url(jsonx.EncodePython(claims, -1))
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the GitHub App JWT failed: %w", err)
	}
	return signing + "." + b64url(sig), nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// readKey reads an RSA private key from a PEM file: PKCS #1 ("RSA PRIVATE
// KEY", what openssl genrsa and GitHub write) or PKCS #8 ("PRIVATE KEY").
// Encrypted keys and other key types are refused.
func readKey(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("no key file configured")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > maxKeyBytes {
		return nil, fmt.Errorf("%s is too large to be a key", path)
	}
	for rest := data; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return nil, fmt.Errorf("%s holds no PEM private key", path)
		}
		if !strings.HasSuffix(b.Type, "PRIVATE KEY") {
			continue
		}
		if b.Type == "ENCRYPTED PRIVATE KEY" || strings.Contains(b.Headers["Proc-Type"], "ENCRYPTED") {
			return nil, fmt.Errorf("%s is encrypted; the App key must be unencrypted", path)
		}
		switch b.Type {
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return k, nil
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			rk, ok := k.(*rsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("%s is a %T, not an RSA key", path, k)
			}
			return rk, nil
		default:
			return nil, fmt.Errorf("%s holds a %q, not an RSA private key", path, b.Type)
		}
	}
}

// Mint POSTs /app/installations/<id>/access_tokens with the JWT and returns
// the new token, uncached: every repo of the installation, with all of the
// App's permissions. factory github uses it for the permissions, as
// cmd_github does (F:4720).
func (a *App) Mint(ctx context.Context) (*Installation, error) { return a.MintScoped(ctx, nil, nil) }

// bareRepo is a repo name with no owner: letters, digits, _ . and -, not
// starting with a dot or a dash.
func bareRepo(r string) bool {
	if r == "" || r[0] == '.' || r[0] == '-' {
		return false
	}
	for _, c := range r {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// MintScoped is Mint for a token limited to repos and perms (permission
// to "read" or "write"), such as the self-deploy's read of the config
// repo. Each repo is a bare name, which GitHub looks up in the
// installation's own account: an owner/repo is refused rather than cut to
// its name, so the caller checks the owner. With both nil it sends no
// body, as Mint always did. Nothing is cached.
func (a *App) MintScoped(ctx context.Context, repos []string, perms map[string]string) (*Installation, error) {
	body := jsonx.Null()
	if repos != nil || perms != nil {
		var names []jsonx.Value
		for _, r := range repos {
			if !bareRepo(r) {
				return nil, fmt.Errorf("github: %q is not a bare repo name of the installation's account", r)
			}
			names = append(names, jsonx.String(r))
		}
		var ps []jsonx.Member
		for _, k := range slices.Sorted(maps.Keys(perms)) {
			ps = append(ps, jsonx.Member{Key: k, Value: jsonx.String(perms[k])})
		}
		var ms []jsonx.Member
		if repos != nil {
			ms = append(ms, jsonx.Member{Key: "repositories", Value: jsonx.ArrayOf(names...)})
		}
		if perms != nil {
			ms = append(ms, jsonx.Member{Key: "permissions", Value: jsonx.ObjectOf(ps...)})
		}
		body = jsonx.ObjectOf(ms...)
	}
	if a.API == nil {
		return nil, errors.New("github: the App has no API client")
	}
	if a.InstallationID == "" {
		return nil, errors.New("github: no installation id")
	}
	jwt, err := a.JWT()
	if err != nil {
		return nil, err
	}
	resp, err := a.API.send(ctx, "POST", "/app/installations/"+a.InstallationID+"/access_tokens", "Bearer "+jwt, body)
	if err != nil {
		return nil, err
	}
	tok, ok := resp.Value.Get("token").Str()
	if !ok || tok == "" {
		return nil, errors.New("github: the installation token answer has no token")
	}
	at, ok := resp.Value.Get("expires_at").Str()
	if !ok {
		return nil, errors.New("github: the installation token answer has no expires_at")
	}
	expires, err := clock.Parse(at)
	if err != nil {
		return nil, fmt.Errorf("github: installation token expires_at: %w", err)
	}
	granted, _ := resp.Value.Get("permissions").Object()
	return &Installation{Token: tok, ExpiresAt: expires, Permissions: granted.Clone()}, nil
}

// Token is installation_token (F:2377): the installation token, minted once
// and reused until five minutes before it expires. The cache lives in the App,
// so per process, as in Python (§13.2 quirk 9). A failed mint is not cached.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && a.Clock != nil && a.Clock.Now().Before(a.expires.Add(-tokenMargin)) {
		return a.token, nil
	}
	inst, err := a.Mint(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expires = inst.Token, inst.ExpiresAt
	return a.token, nil
}

// Client is a copy of the App's API client that authenticates with the App's
// installation token.
func (a *App) Client() *Client {
	var c Client
	if a.API != nil {
		c = *a.API
	}
	c.Auth = a
	return &c
}
