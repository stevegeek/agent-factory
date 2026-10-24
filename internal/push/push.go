// Package push is Foreman's Web Push: it watches the board and tells
// people's devices when something needs them or settles without them.
// Each message goes out through internal/webpush (VAPID-signed, encrypted
// for the device, so the push service sees nothing); this package keeps
// the devices, the news and who may hear what.
//
// Its files live in one directory (beside the login secret): the VAPID key
// foreman-vapid.pem, the subscriptions foreman-push.json and what the
// watcher last saw, foreman-seen.json. All are 0600.
package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/webpush"
)

// Maintainer is the user name a subscription of the maintainer's may carry
// (auth.Maintainer); an empty user is the maintainer too.
const Maintainer = webpush.Maintainer

const maxDevices = 20

// Subscription is one device's subscription, as the browser gives it.
type Subscription = webpush.Subscription

func isMaintainer(user string) bool { return webpush.ForMaintainer(Subscription{User: user}) }

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
		p.client = &http.Client{Timeout: webpush.SendTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if p.subs, err = webpush.ReadSubscriptions(p.subsFile()); err != nil {
		return nil, fmt.Errorf("push: %w", err)
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

// loadVAPID is webpush.LoadVAPID, and when there is no key yet, a new one
// written once (O_EXCL) at 0600. Only Foreman makes the key.
func loadVAPID(path string) (*ecdsa.PrivateKey, error) {
	k, err := webpush.LoadVAPID(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return k, err
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
func (p *Pusher) PublicKey() string { return webpush.PublicKey(p.key) }

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
	if err := webpush.Valid(s, nil); err != nil {
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
	// devices do not hear it
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
func HostOf(endpoint string) string { return webpush.HostOf(endpoint) }

func (p *Pusher) send(ctx context.Context, s Subscription, payload []byte) (int, error) {
	return webpush.Sender{Key: p.key, Subject: p.subject, HTTP: p.client, Clock: p.clock}.Send(ctx, s, payload)
}
