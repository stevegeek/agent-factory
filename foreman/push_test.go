package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// a device: its key pair and auth secret, as a browser would make them
func device(t *testing.T) (Subscription, *ecdh.PrivateKey, []byte) {
	t.Helper()
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	var s Subscription
	s.Endpoint = "https://web.push.apple.com/QGd1Zm9vYmFy"
	s.Keys.P256dh = b64.EncodeToString(priv.PublicKey().Bytes())
	s.Keys.Auth = b64.EncodeToString(auth)
	return s, priv, auth
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
	p, err := NewPusher(t.TempDir(), "https://foreman.example")
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := p.vapid("https://web.push.apple.com/abc")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	claims, _ := b64.DecodeString(parts[1])
	var c map[string]any
	json.Unmarshal(claims, &c)
	if c["aud"] != "https://web.push.apple.com" || c["sub"] != "https://foreman.example" {
		t.Errorf("claims %v", c)
	}
	sig, _ := b64.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&p.key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Errorf("signature does not verify")
	}
	again, _ := NewPusher(p.file[:strings.LastIndex(p.file, "/")], "x")
	if again.PublicKey() != p.PublicKey() {
		t.Errorf("the key must persist")
	}
}

func TestOnlyPushServicesAreAccepted(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://web.push.apple.com/abc":            true,
		"https://fcm.googleapis.com/fcm/send/x":     true,
		"https://updates.push.services.mozilla.com": true,
		"http://web.push.apple.com/abc":             false,
		"https://evil.example/web.push.apple.com":   false,
		"https://push.apple.com.evil.example/x":     false,
		"https://web.push.apple.com:8443/x":         false,
		"https://user@web.push.apple.com/x":         false,
		"https://127.0.0.1/x":                       false,
	} {
		if pushHostOK(u) != ok {
			t.Errorf("%s: want %v", u, ok)
		}
	}
	p, _ := NewPusher(t.TempDir(), "x")
	s, _, _ := device(t)
	s.Endpoint = "https://169.254.169.254/latest"
	if p.Add(s) == nil {
		t.Errorf("a non-push endpoint was stored")
	}
}

type fakePush struct {
	code int
	got  []*http.Request
}

func (f *fakePush) RoundTrip(r *http.Request) (*http.Response, error) {
	f.got = append(f.got, r)
	return &http.Response{StatusCode: f.code, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestSendSignsEncryptsAndForgetsGoneDevices(t *testing.T) {
	p, _ := NewPusher(t.TempDir(), "https://foreman.example")
	s, _, _ := device(t)
	if err := p.Add(s); err != nil {
		t.Fatal(err)
	}
	fake := &fakePush{code: 201}
	p.client.Transport = fake
	if n := p.Send(Message{Title: "hi", URL: "/"}); n != 1 {
		t.Fatalf("sent %d", n)
	}
	r := fake.got[0]
	if r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") ||
		!strings.Contains(r.Header.Get("Authorization"), ", k="+p.PublicKey()) || r.Header.Get("TTL") == "" {
		t.Errorf("headers %v", r.Header)
	}
	fake.code = 410
	p.Send(Message{Title: "again"})
	if p.Count() != 0 {
		t.Errorf("a gone device should be forgotten")
	}
}

func TestNewsIsWhatChangedWithoutYou(t *testing.T) {
	ts := fixtures(t)
	prev := map[string]snap{}
	for _, tk := range ts {
		prev[tk.ID] = snapOf(tk)
	}
	if n := News(prev, ts); len(n) != 0 {
		t.Fatalf("nothing changed, yet %v", n)
	}
	// a ticket arrives on the desk through the pipeline
	start := *find(ts, "T0002")
	delete(prev, "T0002")
	out := News(prev, []*Ticket{&start})
	if len(out) != 1 || out[0].URL != "/t/T0002" || !strings.HasPrefix(out[0].Title, "Start: ") {
		t.Errorf("new ask: %+v", out)
	}
	// the same, but you moved it there yourself
	mine := start
	mine.History = append(append([]Hist{}, start.History...), Hist{Event: "move", Detail: "ready -> needs_human: by human"})
	if out := News(prev, []*Ticket{&mine}); len(out) != 0 {
		t.Errorf("your own move is not news: %+v", out)
	}
	// a PR ticket closes on GitHub
	prev = map[string]snap{"T0001": snapOf(find(ts, "T0001"))}
	merged := *find(ts, "T0001")
	merged.Column = "done"
	merged.History = append(append([]Hist{}, merged.History...), Hist{Event: "move", Detail: "pr_open -> done: PR merged by stevegeek"})
	if out := News(prev, []*Ticket{&merged}); len(out) != 1 || out[0].Title != "T0001 done: PR #23" {
		t.Errorf("merge: %+v", out)
	}
	// the answer to a question comes back: it points at the ticket asked about
	asked := *find(ts, "T0001")
	asked.History = append(append([]Hist{}, asked.History...), Hist{Event: "asked", Detail: "T0099: Did Steep pass?"})
	parent := &asked
	q := &Ticket{ID: "T0099", Kind: "task", Parent: "T0001", Column: "working"}
	prev = map[string]snap{"T0099": snapOf(q), "T0001": snapOf(parent)}
	a := *q
	a.Column, a.Result = "needs_human", "Yes: Steep passed."
	out = News(prev, []*Ticket{parent, &a})
	if len(out) != 1 || out[0].URL != "/t/T0001" || out[0].Body != "Yes: Steep passed." {
		t.Errorf("answer: %+v", out)
	}
}

func TestAppFilesArePublicAndSettingsRender(t *testing.T) {
	h := newHarness(t, false)
	for _, p := range []string{"/manifest.webmanifest", "/sw.js", "/static/icons/apple-touch-icon.png"} {
		if w := h.do("GET", p, nil); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	var m map[string]any
	if json.Unmarshal(h.do("GET", "/manifest.webmanifest", nil).Body.Bytes(), &m) != nil || m["display"] != "standalone" {
		t.Errorf("manifest %v", m)
	}
	c := h.login()
	page := h.do("GET", "/settings", nil, c).Body.String()
	if !strings.Contains(page, "Add to Home Screen") || !strings.Contains(page, `rel="manifest"`) {
		t.Errorf("settings page")
	}
	if w := h.do("POST", "/push/subscribe", url.Values{"subscription": {"{}"}}, c); w.Code != http.StatusForbidden {
		t.Errorf("subscribe without the CSRF token: %d", w.Code)
	}
}

func TestWatchRemembersAcrossRestartsAndSkipsFailedReads(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewPusher(dir, "https://foreman.example")
	stop := make(chan struct{})
	close(stop)
	p.Watch(NewBoard("testdata"), time.Hour, stop) // one pass: the baseline
	data, err := os.ReadFile(p.seen)
	if err != nil || !strings.Contains(string(data), `"T0001"`) {
		t.Fatalf("seen file %q %v", data, err)
	}
	info, _ := os.Stat(p.seen)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("seen file mode %v", info.Mode())
	}
	// an unreadable board leaves what was seen alone
	p.Watch(NewBoard(filepath.Join(dir, "nowhere")), time.Hour, stop)
	again, _ := os.ReadFile(p.seen)
	if string(again) != string(data) {
		t.Errorf("a failed read replaced what was seen")
	}
	// a key that exists but cannot be read must not be replaced
	os.Chmod(filepath.Join(dir, "foreman-vapid.pem"), 0)
	if os.Getuid() != 0 {
		if _, err := NewPusher(dir, "x"); err == nil {
			t.Errorf("an unreadable key should stop the start")
		}
	}
}
