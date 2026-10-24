package webpush

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
)

var testNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// a device: its key pair and auth secret, as a browser would make them
func device(t *testing.T) (Subscription, *ecdh.PrivateKey, []byte) {
	t.Helper()
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	var s Subscription
	s.Endpoint = "https://web.push.apple.com/QGd1Zm9vYmFy"
	s.Keys.P256dh = b64().EncodeToString(priv.PublicKey().Bytes())
	s.Keys.Auth = b64().EncodeToString(auth)
	return s, priv, auth
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func writeKey(t *testing.T, path string, k *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptionDecryptsOnTheDevice(t *testing.T) {
	s, priv, auth := device(t)
	msg := []byte(`{"title":"Review: Review and merge PR #23?"}`)
	body, err := encrypt(s, msg)
	if err != nil {
		t.Fatal(err)
	}
	// the receiving side of RFC 8291
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	asPub := body[21 : 21+idlen]
	if rs != 4096 || idlen != 65 {
		t.Fatalf("header rs=%d idlen=%d", rs, idlen)
	}
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := priv.ECDH(pub)
	cek, nonce, err := deriveKeys(shared, auth, salt, priv.PublicKey().Bytes(), asPub)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatalf("the device cannot decrypt: %v", err)
	}
	if string(plain[:len(plain)-1]) != string(msg) || plain[len(plain)-1] != 0x02 {
		t.Errorf("plaintext %q", plain)
	}
}

func TestVAPIDSignsForThePushServiceOrigin(t *testing.T) {
	s := Sender{Key: newKey(t), Subject: "https://foreman.example", Clock: clock.Fixed{T: testNow}}
	jwt, err := s.vapid("https://web.push.apple.com/abc")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	claims, _ := b64().DecodeString(parts[1])
	var c map[string]any
	json.Unmarshal(claims, &c)
	if c["aud"] != "https://web.push.apple.com" || c["sub"] != "https://foreman.example" {
		t.Errorf("claims %v", c)
	}
	if exp, _ := c["exp"].(float64); int64(exp) != testNow.Add(12*time.Hour).Unix() {
		t.Errorf("exp %v", c["exp"])
	}
	sig, _ := b64().DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&s.Key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Errorf("signature does not verify")
	}
}

func TestOnlyPushServicesAreAccepted(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://web.push.apple.com/abc":            true,
		"https://fcm.googleapis.com/fcm/send/x":     true,
		"https://updates.push.services.mozilla.com": true,
		"https://wns2-par02p.notify.windows.com/w":  true,
		"http://web.push.apple.com/abc":             false,
		"https://evil.example/web.push.apple.com":   false,
		"https://push.apple.com.evil.example/x":     false,
		"https://web.push.apple.com:8443/x":         false,
		"https://web.push.apple.com:443/x":          true,
		"https://user@web.push.apple.com/x":         false,
		"https://127.0.0.1/x":                       false,
		"https://fcm.googleapis.com.evil/x":         false,
		"::not a url":                               false,
	} {
		if HostOK(u) != ok {
			t.Errorf("%s: want %v", u, ok)
		}
	}
	s, _, _ := device(t)
	if err := Valid(s, nil); err != nil {
		t.Errorf("a good subscription: %v", err)
	}
	s.Endpoint = "https://169.254.169.254/latest"
	if Valid(s, nil) == nil {
		t.Errorf("a non-push endpoint passed")
	}
	if Valid(s, func(string) bool { return true }) != nil {
		t.Errorf("AllowHost did not decide the host")
	}
	s, _, _ = device(t)
	s.Keys.Auth = "short"
	if Valid(s, nil) == nil {
		t.Errorf("a bad auth secret passed")
	}
	s, _, _ = device(t)
	s.Keys.P256dh = b64().EncodeToString([]byte("x"))
	if Valid(s, nil) == nil {
		t.Errorf("a bad key passed")
	}
}

type fakePush struct {
	code int
	got  []*http.Request
	body [][]byte
}

func (f *fakePush) RoundTrip(r *http.Request) (*http.Response, error) {
	f.got = append(f.got, r)
	b, _ := io.ReadAll(r.Body)
	f.body = append(f.body, b)
	return &http.Response{StatusCode: f.code, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestSendSignsAndEncrypts(t *testing.T) {
	fake := &fakePush{code: 201}
	snd := Sender{Key: newKey(t), Subject: "https://foreman.example", Clock: clock.Fixed{T: testNow}, HTTP: &http.Client{Transport: fake}}
	sub, priv, auth := device(t)
	code, err := snd.Send(context.Background(), sub, []byte(`{"title":"hi"}`))
	if err != nil || code != 201 {
		t.Fatalf("send: %d %v", code, err)
	}
	r := fake.got[0]
	if r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") ||
		!strings.Contains(r.Header.Get("Authorization"), ", k="+PublicKey(snd.Key)) || r.Header.Get("TTL") != "86400" ||
		r.Header.Get("Urgency") != "normal" || r.Method != "POST" || r.URL.String() != sub.Endpoint {
		t.Errorf("request %s %s %v", r.Method, r.URL, r.Header)
	}
	body := fake.body[0]
	salt, idlen := body[:16], int(body[20])
	asPub := body[21 : 21+idlen]
	pub, _ := ecdh.P256().NewPublicKey(asPub)
	shared, _ := priv.ECDH(pub)
	cek, nonce, _ := deriveKeys(shared, auth, salt, priv.PublicKey().Bytes(), asPub)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	if plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil); err != nil || string(plain[:len(plain)-1]) != `{"title":"hi"}` {
		t.Errorf("payload %q %v", plain, err)
	}
	// a device off the push services is never sent to, unless AllowHost says so
	sub.Endpoint = "https://push.example/x"
	if _, err := snd.Send(context.Background(), sub, nil); err == nil || len(fake.got) != 1 {
		t.Errorf("sent to an unknown host: %v", err)
	}
	snd.AllowHost = func(string) bool { return true }
	if code, err := snd.Send(context.Background(), sub, nil); err != nil || code != 201 || len(fake.got) != 2 {
		t.Errorf("AllowHost: %d %v", code, err)
	}
}

type failingPush struct{}

func (failingPush) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connection refused")
}

// A failed send names the push service's host, never the endpoint's path:
// that is the device's token, and the error goes to a log.
func TestSendErrorHidesTheEndpoint(t *testing.T) {
	snd := Sender{Key: newKey(t), Subject: "https://foreman.example", Clock: clock.Fixed{T: testNow}, HTTP: &http.Client{Transport: failingPush{}}}
	sub, _, _ := device(t)
	_, err := snd.Send(context.Background(), sub, []byte(`{}`))
	if err == nil || strings.Contains(err.Error(), "QGd1Zm9vYmFy") || !strings.Contains(err.Error(), "web.push.apple.com") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %v", err)
	}
}

// The key is only read: a missing one is an error and no file appears, so
// nothing can mint a key whose pushes every device rejects.
func TestLoadVAPIDMissingIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreman-vapid.pem")
	if _, err := LoadVAPID(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing key: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LoadVAPID made a key file: %v", err)
	}
	k := newKey(t)
	writeKey(t, path, k)
	got, err := LoadVAPID(path)
	if err != nil || !got.Equal(k) {
		t.Errorf("load: %v", err)
	}
	os.WriteFile(path, []byte("not pem"), 0o600)
	if _, err := LoadVAPID(path); err == nil {
		t.Errorf("a corrupt key loaded")
	}
}

func TestReadSubscriptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foreman-push.json")
	if subs, err := ReadSubscriptions(path); err != nil || len(subs) != 0 {
		t.Errorf("a missing list: %v %v", subs, err)
	}
	os.WriteFile(path, []byte(`[{"endpoint": "https://web.push.apple.com/a", "keys": {"p256dh": "k", "auth": "a"}},
		{"endpoint": "https://web.push.apple.com/b", "user": "robin"}]`), 0o600)
	subs, err := ReadSubscriptions(path)
	if err != nil || len(subs) != 2 || subs[0].Keys.P256dh != "k" || subs[1].User != "robin" {
		t.Errorf("read %+v %v", subs, err)
	}
	if !ForMaintainer(subs[0]) || ForMaintainer(subs[1]) || !ForMaintainer(Subscription{User: Maintainer}) {
		t.Errorf("ForMaintainer")
	}
	os.WriteFile(path, []byte("{broken"), 0o600)
	if _, err := ReadSubscriptions(path); err == nil {
		t.Errorf("a broken list read")
	}
}
