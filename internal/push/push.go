// Package push is Foreman's Web Push (ARCHITECTURE.md §2.3, inventory-foreman
// §10): it watches the board and tells people's devices when something needs
// them or settles without them. Standard library only: VAPID (RFC 8292)
// signs each request, and the payload is encrypted for the device (RFC 8291,
// aes128gcm), so the push service sees nothing.
//
// Its files live in one directory (beside the login secret): the VAPID key
// foreman-vapid.pem, the subscriptions foreman-push.json and what the
// watcher last saw, foreman-seen.json. All are 0600.
package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
)

// Maintainer is the user name a subscription of the maintainer's may carry
// (auth.Maintainer); an empty user is the maintainer too.
const Maintainer = "maintainer"

const (
	maxDevices  = 20
	sendTimeout = 20 * time.Second
)

func b64() *base64.Encoding { return base64.RawURLEncoding }

// pushHosts are the push services a subscription may point at. Anything
// else is refused, so a subscription can never make Foreman send requests
// somewhere else (an SSRF guard).
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

func (s *Subscription) valid() error {
	if !HostOK(s.Endpoint) {
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

func isMaintainer(user string) bool { return user == "" || user == Maintainer }

// Audience says whether a person other than the maintainer still exists and
// may see a project (auth.Users).
type Audience interface {
	Sees(user, project string) bool
}

// Logger is where push reports devices that subscribed, went or failed.
type Logger interface {
	Printf(format string, args ...any)
}

// Options configures a Pusher.
type Options struct {
	Audience Audience     // nil: only the maintainer's devices hear anything
	Clock    clock.Clock  // nil: the system clock
	Log      Logger       // nil: silent
	HTTP     *http.Client // nil: a client with a 20 s timeout that follows no redirect
	// Maintainer is the maintainer's name on the board (the first of
	// [board] people), which their own answers record as by; nil: the
	// default person.
	Maintainer func() string
}

// Pusher holds the VAPID key and the subscriptions (one per device). It is
// safe for concurrent use.
type Pusher struct {
	key        *ecdsa.PrivateKey
	subject    string // VAPID contact: https://<foreman host> or a mailto:
	dir        string
	audience   Audience
	maintainer func() string
	clock      clock.Clock
	log        Logger
	client     *http.Client

	mu   sync.Mutex
	subs []Subscription
}

// New loads (or, when there is none, makes) the VAPID key and loads the
// subscriptions in dir. A key or a list that cannot be read stops the start:
// every device's subscription is bound to the key, and a list must not be
// saved over as empty.
func New(dir, subject string, opt Options) (*Pusher, error) {
	key, err := loadVAPID(filepath.Join(dir, "foreman-vapid.pem"))
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	p := &Pusher{key: key, subject: subject, dir: dir, audience: opt.Audience, maintainer: opt.Maintainer, clock: opt.Clock, log: opt.Log, client: opt.HTTP}
	if p.clock == nil {
		p.clock = clock.System{}
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: sendTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	switch data, err := os.ReadFile(p.subsFile()); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("push: %w", err)
	default:
		if err := json.Unmarshal(data, &p.subs); err != nil {
			return nil, fmt.Errorf("push: %s: %w", p.subsFile(), err)
		}
	}
	return p, nil
}

func (p *Pusher) subsFile() string { return filepath.Join(p.dir, "foreman-push.json") }
func (p *Pusher) seenFile() string { return filepath.Join(p.dir, "foreman-seen.json") }

func (p *Pusher) logf(format string, args ...any) {
	if p.log != nil {
		p.log.Printf(format, args...)
	}
}

func loadVAPID(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
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
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		f.Close()
		return nil, err
	}
	return key, f.Close()
}

// PublicKey is the applicationServerKey a browser subscribes with.
func (p *Pusher) PublicKey() string {
	pub, err := p.key.PublicKey.ECDH()
	if err != nil {
		return ""
	}
	return b64().EncodeToString(pub.Bytes())
}

// Count is the number of devices of everyone.
func (p *Pusher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subs)
}

// CountFor is the number of one person's devices ("" or Maintainer: the
// maintainer's). Each person sees only their own count (bug 7).
func (p *Pusher) CountFor(user string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.subs {
		if sameUser(s.User, user) {
			n++
		}
	}
	return n
}

func sameUser(a, b string) bool {
	if isMaintainer(a) || isMaintainer(b) {
		return isMaintainer(a) && isMaintainer(b)
	}
	return a == b
}

// Add stores a device's subscription. Re-subscribing an endpoint replaces
// it; at most 20 devices in all.
func (p *Pusher) Add(s Subscription) error {
	if err := s.valid(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s.Added = p.clock.Now().UTC().Format(time.RFC3339)
	for i := range p.subs {
		if p.subs[i].Endpoint == s.Endpoint {
			p.subs[i] = s
			return p.save()
		}
	}
	if len(p.subs) >= maxDevices {
		return errors.New("too many devices; turn notifications off on one first")
	}
	p.subs = append(p.subs, s)
	return p.save()
}

// RemoveFor forgets one of that person's devices; another person's device
// with that endpoint stays.
func (p *Pusher) RemoveFor(user, endpoint string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.drop(func(s Subscription) bool { return s.Endpoint == endpoint && sameUser(s.User, user) })
}

func (p *Pusher) forget(endpoint string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.drop(func(s Subscription) bool { return s.Endpoint == endpoint })
}

func (p *Pusher) drop(gone func(Subscription) bool) error {
	out := p.subs[:0]
	changed := false
	for _, s := range p.subs {
		if gone(s) {
			changed = true
			continue
		}
		out = append(out, s)
	}
	p.subs = out
	if !changed {
		return nil
	}
	return p.save()
}

func (p *Pusher) save() error {
	data, err := json.MarshalIndent(p.subs, "", "  ")
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(p.subsFile(), data, 0o600); err != nil {
		return fmt.Errorf("push: save subscriptions: %w", err)
	}
	return nil
}

// Message is what a notification says. URL is a path in Foreman; Badge is
// the desk count.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag,omitempty"`
	Badge int    `json:"badge"`
	// Project is the ticket's project: a device of someone other than the
	// maintainer hears only of their projects.
	Project string `json:"-"`
	// forOthers: already in the maintainer's batch summary; only other
	// people's devices get it
	forOthers bool
	// by: the person whose own doing it is, by their name on the board; their
	// devices do not hear it (D19)
	by string
}

// Send delivers m to every device that may hear it and returns how many
// took it. A device the push service says is gone (404, 410) is forgotten.
func (p *Pusher) Send(ctx context.Context, m Message) int {
	return p.deliver(ctx, m, func(s Subscription) bool { return p.allowed(s, m) })
}

// SendTo delivers m to one person's own devices only ("" or Maintainer: the
// maintainer's): the settings page's test message.
func (p *Pusher) SendTo(ctx context.Context, user string, m Message) int {
	return p.deliver(ctx, m, func(s Subscription) bool { return sameUser(s.User, user) })
}

func (p *Pusher) deliver(ctx context.Context, m Message, to func(Subscription) bool) (sent int) {
	p.mu.Lock()
	subs := append([]Subscription(nil), p.subs...)
	p.mu.Unlock()
	for _, s := range subs {
		if !to(s) {
			continue
		}
		mm := m
		if !isMaintainer(s.User) {
			mm.Badge = 0 // the maintainer's desk count is not theirs
		}
		payload, err := json.Marshal(mm)
		if err != nil {
			continue
		}
		code, err := p.send(ctx, s, payload)
		switch {
		case err != nil:
			p.logf("push: %v", err)
		case code == http.StatusNotFound || code == http.StatusGone:
			p.logf("push: a device unsubscribed (%d); forgetting it", code)
			if err := p.forget(s.Endpoint); err != nil {
				p.logf("%v", err)
			}
		case code >= 300:
			p.logf("push: %s answered %d", HostOf(s.Endpoint), code)
		default:
			sent++
		}
	}
	return sent
}

// allowed: whether this device hears this news. The maintainer's devices
// hear everything except items already in their summary; anyone else's
// only news of a ticket in their own projects, while they still exist.
func (p *Pusher) allowed(s Subscription, m Message) bool {
	if isMaintainer(s.User) {
		return !m.forOthers && m.by != p.maintainerName()
	}
	return p.audience != nil && m.Project != "" && m.by != s.User && p.audience.Sees(s.User, m.Project)
}

// maintainerName is the maintainer's name on the board (Options.Maintainer).
func (p *Pusher) maintainerName() string {
	if p.maintainer == nil {
		return model.DefaultPeople()[0]
	}
	return p.maintainer()
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

func (p *Pusher) send(ctx context.Context, s Subscription, payload []byte) (int, error) {
	if err := s.valid(); err != nil {
		return 0, err
	}
	body, err := encrypt(s, payload)
	if err != nil {
		return 0, err
	}
	jwt, err := p.vapid(s.Endpoint)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+p.PublicKey())
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send to %s: %w", HostOf(s.Endpoint), err)
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// vapid is an ES256 JWT for the push service's origin, valid 12 hours.
func (p *Pusher) vapid(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	head := b64().EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": p.clock.Now().Add(12 * time.Hour).Unix(), "sub": p.subject})
	if err != nil {
		return "", err
	}
	signing := head + "." + b64().EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, p.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
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
