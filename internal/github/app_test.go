package github

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
)

func decodeSegment(t *testing.T, s string) []byte {
	t.Helper()
	if strings.ContainsAny(s, "=+/") {
		t.Fatalf("segment %q is not unpadded base64url", s)
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// verifyJWT checks jwt's RS256 signature with pub and returns its parts.
func verifyJWT(t *testing.T, jwt string, pub *rsa.PublicKey) (header, claims []byte) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q has %d parts", jwt, len(parts))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], decodeSegment(t, parts[2])); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	return decodeSegment(t, parts[0]), decodeSegment(t, parts[1])
}

// port: TF GithubOutTest.test_app_jwt_is_rs256_signed_by_the_key
func TestAppJWTIsRS256SignedByTheKey(t *testing.T) {
	f := newFake(t)
	app := newApp(t, f, clock.Fixed{T: testNow.Add(700 * time.Millisecond)})
	jwt, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	header, claims := verifyJWT(t, jwt, &testKey().PublicKey)
	// Python's json.dumps, byte for byte: default separators, this key order.
	if got, want := string(header), `{"alg": "RS256", "typ": "JWT"}`; got != want {
		t.Errorf("header = %s, want %s", got, want)
	}
	iat := testNow.Unix() // int(time.time()) truncates the fraction
	want := `{"iat": ` + itoa(iat-60) + `, "exp": ` + itoa(iat+540) + `, "iss": "Iv23test"}`
	if string(claims) != want {
		t.Errorf("claims = %s, want %s", claims, want)
	}
	var c struct{ Iss string }
	if err := json.Unmarshal(claims, &c); err != nil || c.Iss != "Iv23test" {
		t.Errorf("iss = %q (%v)", c.Iss, err)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// The same check with openssl, as the Python test does, when openssl is here.
func TestAppJWTVerifiesWithOpenSSL(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("no openssl on PATH")
	}
	f := newFake(t)
	app := newApp(t, f, clock.Fixed{T: testNow})
	jwt, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	dir := t.TempDir()
	pubDER, err := x509.MarshalPKIXPublicKey(&testKey().PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(dir, "app.pub")
	sig := filepath.Join(dir, "app.sig")
	mustWrite(t, pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	mustWrite(t, sig, decodeSegment(t, parts[2]))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, openssl, "dgst", "-sha256", "-verify", pub, "-signature", sig)
	cmd.Stdin = strings.NewReader(parts[0] + "." + parts[1])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("openssl refuses the signature: %v\n%s", err, out)
	}
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAppJWTReadsPKCS1AndPKCS8(t *testing.T) {
	for _, typ := range []string{"RSA PRIVATE KEY", "PRIVATE KEY"} {
		t.Run(typ, func(t *testing.T) {
			app := &App{ClientID: "1", KeyPath: writeKey(t, testKey(), typ), Clock: clock.Fixed{T: testNow}}
			jwt, err := app.JWT()
			if err != nil {
				t.Fatal(err)
			}
			_, claims := verifyJWT(t, jwt, &testKey().PublicKey)
			if !strings.HasSuffix(string(claims), `"iss": "1"}`) {
				t.Errorf("claims = %s", claims)
			}
		})
	}
}

func TestAppJWTRefusesWhatIsNotAnRSAKey(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	mustWrite(t, garbage, []byte("not a key\n"))
	public := filepath.Join(dir, "public.pem")
	pubDER, _ := x509.MarshalPKIXPublicKey(&testKey().PublicKey)
	mustWrite(t, public, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	encrypted := filepath.Join(dir, "encrypted.pem")
	mustWrite(t, encrypted, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"},
		Bytes:   []byte{1, 2, 3}}))
	big := filepath.Join(dir, "big.pem")
	mustWrite(t, big, make([]byte, maxKeyBytes+1))
	cases := map[string]string{
		"missing":   filepath.Join(dir, "nope.pem"),
		"garbage":   garbage,
		"ec":        writeKey(t, ec, "PRIVATE KEY"),
		"public":    public,
		"encrypted": encrypted,
		"too big":   big,
		"no path":   "",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			app := &App{ClientID: "1", KeyPath: path, Clock: clock.Fixed{T: testNow}}
			jwt, err := app.JWT()
			if err == nil {
				t.Fatalf("JWT() = %q, want an error", jwt)
			}
			if !strings.HasPrefix(err.Error(), "signing the GitHub App JWT failed: ") {
				t.Errorf("error %q does not say what failed as Python does", err)
			}
		})
	}
}

func TestAppJWTEscapesTheClientIDAsPython(t *testing.T) {
	app := &App{ClientID: "Ivé\"x", KeyPath: writeKey(t, testKey(), "RSA PRIVATE KEY"), Clock: clock.Fixed{T: testNow}}
	jwt, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	_, claims := verifyJWT(t, jwt, &testKey().PublicKey)
	if !strings.HasSuffix(string(claims), esc(`"iss": "Iv~u00e9\"x"}`)) {
		t.Errorf("claims = %s, want ensure_ascii escapes", claims)
	}
}

const tokenReply = `{"token": "ghs_first", "expires_at": "2026-10-18T13:00:00Z", "permissions": {"pull_requests": "write", "contents": "write", "metadata": "read"}}`

func TestTokenMintsWithTheJWTAndCachesItUntilFiveMinutesBeforeExpiry(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/app/installations/42/access_tokens", 201, tokenReply)
	f.on("POST", "/app/installations/42/access_tokens", 201,
		`{"token": "ghs_second", "expires_at": "2026-10-18T14:00:00Z"}`)
	clk := clock.NewStep(testNow, 0)
	app := newApp(t, f, clk)
	ctx := context.Background()

	tok, err := app.Token(ctx)
	if err != nil || tok != "ghs_first" {
		t.Fatalf("Token() = %q, %v", tok, err)
	}
	r := f.last()
	if r.Method != "POST" || r.URI != "/app/installations/42/access_tokens" || r.Body != "" {
		t.Errorf("mint request = %s %s body %q", r.Method, r.URI, r.Body)
	}
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want the JWT as a bearer", r.Header.Get("Authorization"))
	}
	verifyJWT(t, auth, &testKey().PublicKey)
	checkAPIHeaders(t, r)

	// Reused while more than five minutes are left.
	clk.Advance(54*time.Minute + 59*time.Second)
	if tok, _ := app.Token(ctx); tok != "ghs_first" {
		t.Errorf("at 12:54:59 Token() = %q, want the cached one", tok)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d mint requests, want 1", n)
	}
	// expires - 300 > now is false at exactly five minutes before: a new one.
	clk.Advance(time.Second)
	if tok, _ := app.Token(ctx); tok != "ghs_second" {
		t.Errorf("at 12:55:00 Token() = %q, want a fresh one", tok)
	}
	if n := len(f.requests()); n != 2 {
		t.Errorf("%d mint requests, want 2", n)
	}
}

func TestTokenFailureIsNotCachedAndSaysWhy(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/app/installations/42/access_tokens", 401, `{"message": "A JSON web token could not be decoded"}`)
	f.on("POST", "/app/installations/42/access_tokens", 201, tokenReply)
	app := newApp(t, f, clock.Fixed{T: testNow})
	_, err := app.Token(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 401 || he.Message != "A JSON web token could not be decoded" {
		t.Fatalf("err = %v, want the 401 with GitHub's message", err)
	}
	if tok, err := app.Token(context.Background()); err != nil || tok != "ghs_first" {
		t.Errorf("after a failure Token() = %q, %v", tok, err)
	}
}

func TestTokenRefusesAnAnswerWithoutTokenOrExpiry(t *testing.T) {
	for name, body := range map[string]string{
		"no token":     `{"expires_at": "2026-10-18T13:00:00Z"}`,
		"no expiry":    `{"token": "ghs_x"}`,
		"bad expiry":   `{"token": "ghs_x", "expires_at": "soon"}`,
		"not a string": `{"token": 7, "expires_at": "2026-10-18T13:00:00Z"}`,
		"a list":       `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.on("POST", "/app/installations/42/access_tokens", 201, body)
			app := newApp(t, f, clock.Fixed{T: testNow})
			if tok, err := app.Token(context.Background()); err == nil {
				t.Errorf("Token() = %q, want an error", tok)
			}
		})
	}
}

func TestMintReturnsThePermissionsInOrderAndDoesNotTouchTheCache(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/app/installations/42/access_tokens", 201, tokenReply)
	app := newApp(t, f, clock.Fixed{T: testNow})
	inst, err := app.Mint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inst.Token != "ghs_first" || !inst.ExpiresAt.Equal(time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("Mint() = %+v", inst)
	}
	if got := strings.Join(inst.Permissions.Keys(), ","); got != "pull_requests,contents,metadata" {
		t.Errorf("permission keys = %s", got)
	}
	app.Token(context.Background())
	if n := len(f.requests()); n != 2 {
		t.Errorf("%d requests, want Mint and Token to mint once each", n)
	}
}

func TestConcurrentTokenCallsMintOnce(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/app/installations/42/access_tokens", 201, tokenReply)
	app := newApp(t, f, clock.Fixed{T: testNow})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if tok, err := app.Token(context.Background()); err != nil || tok != "ghs_first" {
				t.Errorf("Token() = %q, %v", tok, err)
			}
		})
	}
	wg.Wait()
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d mint requests, want 1", n)
	}
}

func TestAppClientUsesTheInstallationToken(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/app/installations/42/access_tokens", 201, tokenReply)
	f.on("GET", "/repos/o/r/pulls/3", 200, `{"number": 3}`)
	app := newApp(t, f, clock.Fixed{T: testNow})
	c := app.Client()
	if _, err := c.PR(context.Background(), "o/r", 3); err != nil {
		t.Fatal(err)
	}
	if got := f.last().Header.Get("Authorization"); got != "Bearer ghs_first" {
		t.Errorf("Authorization = %q", got)
	}
	if app.API.Auth != nil {
		t.Error("Client() changed the App's own API client")
	}
}

// MintScoped asks for a token limited to the named repos (bare names, of
// the installation's account) and permissions; Mint asks for none. An
// owner/repo is refused before any request: GitHub would take the name in
// the installation's account, whatever the owner said.
func TestMintScopedSendsTheBody(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/app/installations/42/access_tokens", 201, tokenReply)
	f.on("POST", "/app/installations/42/access_tokens", 201, tokenReply)
	app := newApp(t, f, clock.Fixed{T: testNow})
	for _, bad := range []string{"other-owner/config-repo", "", "a b"} {
		if _, err := app.MintScoped(context.Background(), []string{bad}, map[string]string{"contents": "read"}); err == nil {
			t.Errorf("MintScoped(%q) minted", bad)
		}
	}
	inst, err := app.MintScoped(context.Background(), []string{"config-repo"}, map[string]string{"metadata": "read", "contents": "read"})
	if err != nil || inst.Token != "ghs_first" {
		t.Fatalf("MintScoped() = %+v, %v", inst, err)
	}
	r := f.last()
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatalf("body %q: %v", r.Body, err)
	}
	if strings.Join(body.Repositories, ",") != "config-repo" || len(body.Permissions) != 2 || body.Permissions["contents"] != "read" || body.Permissions["metadata"] != "read" {
		t.Errorf("body = %q", r.Body)
	}
	if !strings.Contains(r.Body, `"contents": "read", "metadata": "read"`) {
		t.Errorf("permissions not in key order: %q", r.Body)
	}
	if _, err := app.Mint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.last(); r.Body != "" {
		t.Errorf("Mint body = %q, want none", r.Body)
	}
}
