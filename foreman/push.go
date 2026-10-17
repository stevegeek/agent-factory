package main

// Web Push: Foreman watches the board and tells the maintainer's devices when something needs
// them or settles without them. Standard library only: VAPID (RFC 8292) signs each request, and
// the payload is encrypted for the device (RFC 8291, aes128gcm), so the push service sees nothing.

import (
	"bytes"
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
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var b64 = base64.RawURLEncoding

// pushHosts: the push services a subscription may point at. Anything else is refused, so a
// subscription can never make Foreman send requests somewhere else.
var pushHosts = []string{".push.apple.com", "fcm.googleapis.com", ".push.services.mozilla.com", ".notify.windows.com"}

func pushHostOK(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, p := range pushHosts {
		if h == p || (strings.HasPrefix(p, ".") && strings.HasSuffix(h, p)) {
			return true
		}
	}
	return false
}

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
	if !pushHostOK(s.Endpoint) {
		return errors.New("not a known push service")
	}
	if k, err := b64.DecodeString(s.Keys.P256dh); err != nil || len(k) != 65 {
		return errors.New("bad p256dh key")
	}
	if a, err := b64.DecodeString(s.Keys.Auth); err != nil || len(a) != 16 {
		return errors.New("bad auth secret")
	}
	return nil
}

// Pusher holds the VAPID key and the subscriptions (one per device), both in files beside the secret.
type Pusher struct {
	key     *ecdsa.PrivateKey
	subject string // VAPID contact: https://<foreman host> or a mailto:
	file    string
	seen    string // what the watcher last saw, so news during a restart is not lost
	mu      sync.Mutex
	subs    []Subscription
	client  *http.Client
	users   *Users
}

func NewPusher(dir, subject string) (*Pusher, error) {
	key, err := loadVAPID(filepath.Join(dir, "foreman-vapid.pem"))
	if err != nil {
		return nil, err
	}
	p := &Pusher{key: key, subject: subject, file: filepath.Join(dir, "foreman-push.json"), seen: filepath.Join(dir, "foreman-seen.json"),
		client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	// a list that cannot be read must stop the start, not be saved over as empty
	switch data, err := os.ReadFile(p.file); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &p.subs); err != nil {
			return nil, fmt.Errorf("%s: %v", p.file, err)
		}
	}
	return p, nil
}

// loadVAPID reads the key, or makes one only when there is none: every device's subscription is
// bound to it, so a key that cannot be read must stop the start rather than be replaced.
func loadVAPID(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		blk, _ := pem.Decode(data)
		if blk == nil {
			return nil, fmt.Errorf("%s: not PEM", path)
		}
		return x509.ParseECPrivateKey(blk.Bytes)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalECPrivateKey(key)
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

// PublicKey: the applicationServerKey a browser subscribes with.
func (p *Pusher) PublicKey() string {
	pub, _ := p.key.PublicKey.ECDH()
	return b64.EncodeToString(pub.Bytes())
}

func (p *Pusher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subs)
}

func (p *Pusher) Add(s Subscription) error {
	if err := s.valid(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.subs {
		if p.subs[i].Endpoint == s.Endpoint {
			p.subs[i] = s
			return p.save()
		}
	}
	if len(p.subs) >= 20 {
		return errors.New("too many devices; turn notifications off on one first")
	}
	s.Added = time.Now().UTC().Format(time.RFC3339)
	p.subs = append(p.subs, s)
	return p.save()
}

func (p *Pusher) Remove(endpoint string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.drop(endpoint)
}

func (p *Pusher) drop(endpoint string) error {
	out := p.subs[:0]
	for _, s := range p.subs {
		if s.Endpoint != endpoint {
			out = append(out, s)
		}
	}
	p.subs = out
	return p.save()
}

func (p *Pusher) save() error {
	data, _ := json.MarshalIndent(p.subs, "", "  ")
	return writePrivate(p.file, data)
}

// writePrivate replaces a file atomically, readable only by its owner (a stale temp file's mode is not kept).
func writePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Message: what a notification says. URL is a path in Foreman; Badge is the desk count.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag,omitempty"`
	Badge int    `json:"badge"`
	// the ticket's project: a device of someone other than the maintainer only hears of their projects
	Project string `json:"-"`
	// forOthers: already in the maintainer's batch summary; only other people's devices get it
	forOthers bool
}

// Send delivers to every device; a device the push service says is gone (404/410) is forgotten.
func (p *Pusher) Send(m Message) (sent int) {
	payload, _ := json.Marshal(m)
	p.mu.Lock()
	subs := append([]Subscription(nil), p.subs...)
	p.mu.Unlock()
	for _, s := range subs {
		if !p.allowed(s, m) {
			continue
		}
		payload := payload
		if s.User != "" && s.User != maintainer {
			mm := m
			mm.Badge = 0 // the maintainer's desk count is not theirs
			payload, _ = json.Marshal(mm)
		}
		code, err := p.send(s, payload)
		switch {
		case err != nil:
			log.Printf("push: %v", err)
		case code == http.StatusNotFound || code == http.StatusGone:
			log.Printf("push: a device unsubscribed (%d); forgetting it", code)
			p.Remove(s.Endpoint)
		case code >= 300:
			log.Printf("push: %s answered %d", hostOf(s.Endpoint), code)
		default:
			sent++
		}
	}
	return sent
}

// allowed: whether this device hears this news. The maintainer's devices hear everything except items
// already in their summary; anyone else's only news of a ticket in their own projects.
func (p *Pusher) allowed(s Subscription, m Message) bool {
	if s.User == "" || s.User == maintainer {
		return !m.forOthers
	}
	u := p.users.Get(s.User)
	return u != nil && m.Project != "" && u.Sees(m.Project)
}

func hostOf(endpoint string) string {
	u, _ := url.Parse(endpoint)
	if u == nil {
		return "?"
	}
	return u.Host
}

func (p *Pusher) send(s Subscription, payload []byte) (int, error) {
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
	req, _ := http.NewRequest("POST", s.Endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+p.PublicKey())
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// vapid: an ES256 JWT for the push service's origin, valid 12 hours.
func (p *Pusher) vapid(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	head := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": time.Now().Add(12 * time.Hour).Unix(), "sub": p.subject})
	signing := head + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, p.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// encrypt: RFC 8291 for one record. An ephemeral key agrees a secret with the device's key,
// mixed with the device's auth secret; the header carries the salt and the ephemeral public key.
func encrypt(s Subscription, plaintext []byte) ([]byte, error) {
	uaBytes, _ := b64.DecodeString(s.Keys.P256dh)
	authSecret, _ := b64.DecodeString(s.Keys.Auth)
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
	rand.Read(salt)
	cek, nonce, err := deriveKeys(shared, authSecret, salt, uaBytes, asPub)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
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

// ---------------------------------------------------------------- what is worth a notification

// snap: the parts of a ticket whose change might be news.
type snap struct {
	Column string `json:"c"`
	Ask    string `json:"a,omitempty"`
	Result string `json:"r,omitempty"`
}

func snapOf(t *Ticket) snap {
	s := snap{Column: t.Column, Result: t.Result}
	if a := AskFor(t); a != nil {
		s.Ask = a.Kind + " " + a.Since
	}
	return s
}

// askedBy: the ticket whose Ask filed this task, or nil (a task with a parent is not always a question).
func askedBy(t *Ticket, all map[string]*Ticket) *Ticket {
	p := all[t.Parent]
	if p == nil {
		return nil
	}
	for _, h := range p.History {
		if h.Event != "asked" {
			continue
		}
		if id, _ := askOf(h); id == t.ID {
			return p
		}
	}
	return nil
}

// byYou: the ticket's last move was the maintainer's own (a Foreman answer or the CLI).
func byYou(t *Ticket) bool {
	for i := len(t.History) - 1; i >= 0; i-- {
		if h := t.History[i]; h.Event == "move" {
			// a merge you confirmed in Foreman is yours too
			return moveOf(h).Human || strings.HasSuffix(h.Detail, "on your go-ahead")
		}
	}
	return false
}

// News compares the board with what was seen before: a new ask on the desk, the answer to a
// question you asked, and a PR ticket that closed without you (merged or closed on GitHub).
// What you did yourself is not news.
func News(prev map[string]snap, ts []*Ticket) []Message {
	all := byID(ts)
	desk := Waiting(Desk(ts))
	var out []Message
	for _, t := range ts {
		if t.board() {
			continue
		}
		was, seen := prev[t.ID]
		now := snapOf(t)
		if seen && was == now {
			continue
		}
		switch p := askedBy(t, all); {
		case t.Kind == "task" && p != nil && t.Result != "" && (!seen || was.Result != t.Result):
			out = append(out, Message{Title: "Answer to your question on " + p.ID, Body: clip(t.Result, 220), URL: "/t/" + p.ID, Tag: t.ID, Project: p.Project})
		case now.Ask != "" && (!seen || was.Ask != now.Ask) && !byYou(t):
			a := AskFor(t)
			m := Message{Title: kindLabel(a.Kind) + ": " + clip(Headline(t, a), 80), Body: clip(t.Title, 160),
				URL: "/t/" + t.ID, Tag: t.ID, Project: t.Project}
			if a.Kind == "question" {
				// an agent's question opens the conversation, reply box ready
				m.Title, m.Body, m.URL = t.ID+" asks: "+clip(a.Question, 90), clip(t.Title, 160), "/t/"+t.ID+"/talk#reply"
			}
			out = append(out, m)
		case seen && t.closed() && was.Column != t.Column && t.PR != nil && !byYou(t):
			verb := "closed"
			if t.Column == "done" {
				verb = "done"
			}
			out = append(out, Message{Title: fmt.Sprintf("%s %s: PR #%d", t.ID, verb, t.PR.Number), Body: clip(t.Title, 160),
				URL: "/t/" + t.ID, Tag: t.ID, Project: t.Project})
		}
	}
	for i := range out {
		out[i].Badge = desk
	}
	if len(out) > 3 {
		// the maintainer gets one summary; everyone else still hears of their own projects one by one
		batch := []Message{{Title: fmt.Sprintf("%d updates from the factory", len(out)), Body: out[0].Title + "; " + out[1].Title + "; …",
			URL: "/", Tag: "batch", Badge: desk}}
		for _, m := range out {
			m.forOthers = true
			batch = append(batch, m)
		}
		return batch
	}
	return out
}

// Watch reads the board every interval and pushes what is news. What it saw is kept on disk, so a
// change made while Foreman was down is still announced; with nothing kept, the first read is the baseline.
func (p *Pusher) Watch(b *Board, every time.Duration, stop <-chan struct{}) {
	var prev map[string]snap
	var last []byte
	if data, err := os.ReadFile(p.seen); err == nil && json.Unmarshal(data, &prev) == nil {
		last = data
	} else {
		prev = nil
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		// a board that reads as empty is a failed read, not every ticket gone: keep what was seen
		if ts := b.Load(); len(ts) > 0 {
			if prev != nil && p.Count() > 0 {
				for _, m := range News(prev, ts) {
					p.Send(m)
				}
			}
			prev = make(map[string]snap, len(ts))
			for _, t := range ts {
				prev[t.ID] = snapOf(t)
			}
			if data, _ := json.Marshal(prev); !bytes.Equal(data, last) {
				if err := writePrivate(p.seen, data); err != nil {
					log.Printf("push: %v", err)
				} else {
					last = data
				}
			}
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}
