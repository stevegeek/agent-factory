// Package webpush sends one Web Push message to one device: VAPID (RFC
// 8292) signs the request, and the payload is encrypted for the device
// (RFC 8291, aes128gcm), so the push service sees nothing. It holds no
// board and no store: Foreman's push package builds on it, and so does
// factory-remote's watchdog, which pushes with a copy of Foreman's key and
// device list.
//
// The key is only ever read here (LoadVAPID): making one is push.New's
// job, so nothing outside Foreman can mint a key every device would reject.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
)

// Maintainer is the user name a subscription of the maintainer's may carry
// (auth.Maintainer); an empty user is the maintainer too.
const Maintainer = "maintainer"

// SendTimeout bounds one push request.
const SendTimeout = 20 * time.Second

func b64() *base64.Encoding { return base64.RawURLEncoding }

// pushHosts are the push services a subscription may point at. Anything
// else is refused, so a subscription can never make the sender send
// requests somewhere else (an SSRF guard).
func pushHosts() []string {
	return []string{".push.apple.com", "fcm.googleapis.com", ".push.services.mozilla.com", ".notify.windows.com"}
}

// HostOK reports an endpoint on a known push service: https, no userinfo,
// port 443 or none.
func HostOK(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, p := range pushHosts() {
		if h == p || (strings.HasPrefix(p, ".") && strings.HasSuffix(h, p)) {
			return true
		}
	}
	return false
}

// HostOf is an endpoint's host, for log lines (never the whole capability
// URL).
func HostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "?"
	}
	return u.Host
}

// Subscription is one device's subscription, as the browser gives it.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Added string `json:"added,omitempty"`
	User  string `json:"user,omitempty"` // who subscribed it; empty: the maintainer
}

// Valid checks a subscription: its endpoint passes allow (nil: HostOK),
// and its keys have the sizes RFC 8291 needs.
func Valid(s Subscription, allow func(string) bool) error {
	if allow == nil {
		allow = HostOK
	}
	if !allow(s.Endpoint) {
		return errors.New("not a known push service")
	}
	if k, err := b64().DecodeString(s.Keys.P256dh); err != nil || len(k) != 65 {
		return errors.New("bad p256dh key")
	}
	if a, err := b64().DecodeString(s.Keys.Auth); err != nil || len(a) != 16 {
		return errors.New("bad auth secret")
	}
	return nil
}

// ForMaintainer reports a subscription of the maintainer's.
func ForMaintainer(s Subscription) bool { return s.User == "" || s.User == Maintainer }

// LoadVAPID reads the VAPID key at path. It never makes one: a missing
// file is an error (one that errors.Is fs.ErrNotExist).
func LoadVAPID(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, fmt.Errorf("%s: not PEM", path)
	}
	k, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// ReadSubscriptions reads a device list (foreman-push.json, a JSON array);
// a missing file is none.
func ReadSubscriptions(path string) ([]Subscription, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	}
	var subs []Subscription
	if err := json.Unmarshal(data, &subs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return subs, nil
}

// PublicKey is the applicationServerKey a browser subscribes with, for key.
func PublicKey(key *ecdsa.PrivateKey) string {
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		return ""
	}
	return b64().EncodeToString(pub.Bytes())
}

// Sender sends pushes signed with Key. Subject is the VAPID contact: an
// https:// origin or a mailto:.
type Sender struct {
	Key     *ecdsa.PrivateKey
	Subject string
	HTTP    *http.Client // nil: a client with SendTimeout that follows no redirect
	Clock   clock.Clock  // nil: the system clock
	// AllowHost decides which endpoints may be sent to; nil is HostOK.
	// Tests point it at their own server.
	AllowHost func(string) bool
}

func (s Sender) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: SendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s Sender) now() time.Time {
	if s.Clock == nil {
		return clock.System{}.Now()
	}
	return s.Clock.Now()
}

// Send pushes payload to one device and returns the push service's status
// (201 when it took it; 404 and 410 mean the device is gone).
func (s Sender) Send(ctx context.Context, sub Subscription, payload []byte) (status int, err error) {
	if err := Valid(sub, s.AllowHost); err != nil {
		return 0, err
	}
	body, err := encrypt(sub, payload)
	if err != nil {
		return 0, err
	}
	jwt, err := s.vapid(sub.Endpoint)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+PublicKey(s.Key))
	resp, err := s.client().Do(req)
	if err != nil {
		// a *url.Error quotes the whole endpoint, the device's token with
		// it: the log gets the host and the cause alone
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("send to %s: %w", HostOf(sub.Endpoint), err)
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// vapid is an ES256 JWT for the push service's origin, valid 12 hours.
func (s Sender) vapid(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	head := b64().EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": s.now().Add(12 * time.Hour).Unix(), "sub": s.Subject})
	if err != nil {
		return "", err
	}
	signing := head + "." + b64().EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, ss, err := ecdsa.Sign(rand.Reader, s.Key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return signing + "." + b64().EncodeToString(sig), nil
}

// encrypt is RFC 8291 for one record. An ephemeral key agrees a secret with
// the device's key, mixed with the device's auth secret; the header carries
// the salt and the ephemeral public key.
func encrypt(s Subscription, plaintext []byte) ([]byte, error) {
	uaBytes, err := b64().DecodeString(s.Keys.P256dh)
	if err != nil {
		return nil, err
	}
	authSecret, err := b64().DecodeString(s.Keys.Auth)
	if err != nil {
		return nil, err
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaBytes)
	if err != nil {
		return nil, err
	}
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := asPriv.PublicKey().Bytes()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	cek, nonce, err := deriveKeys(shared, authSecret, salt, uaBytes, asPub)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, append(append([]byte{}, plaintext...), 0x02), nil) // 0x02: the last record
	head := make([]byte, 0, 21+len(asPub))
	head = append(head, salt...)
	head = binary.BigEndian.AppendUint32(head, 4096)
	head = append(head, byte(len(asPub)))
	head = append(head, asPub...)
	return append(head, sealed...), nil
}

func deriveKeys(shared, authSecret, salt, uaPub, asPub []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPub)+string(asPub), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}
