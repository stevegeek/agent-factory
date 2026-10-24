// Package guard admits a request to Foreman's token APIs, /api/v1 and /mcp:
// the peer, the Origin, the bearer token, the caller's rate and the body's
// size and type, in that order and before any body is read. It also holds
// the slots both APIs share: a few verbs at a time, one per token, each on
// a context the client cannot cancel.
//
// It is Foreman's apiguard.go and the slot half of its APIRunner, moved
// in-process. It decides nothing about tickets: what a token may do is
// ticketapi's to decide on every call. It only reads the token store
// (tokens.Store), never writes it.
package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

const (
	// BodyLimit is the most a request body may hold.
	BodyLimit = 256 << 10
	// FailLimit bad tokens per client key in FailWindow lock the key out.
	FailLimit  = 10
	FailWindow = 15 * time.Minute
	// maxFailKeys caps the keys whose bad tokens are tracked (bug 4): past
	// it a new key is not tracked.
	maxFailKeys = 4096
)

// Rate is a token bucket: PerMin calls a minute, at most Burst at once.
type Rate struct{ PerMin, Burst float64 }

// DefaultAll is the rate of every call a token makes.
func DefaultAll() Rate { return Rate{PerMin: 120, Burst: 30} }

// DefaultWrites is the rate of a token's writes, drawn besides DefaultAll.
func DefaultWrites() Rate { return Rate{PerMin: 30, Burst: 10} }

// Logger is Foreman's journal.
type Logger interface {
	Printf(format string, args ...any)
}

// Options configures a Guard. Zero values are the defaults.
type Options struct {
	// Tokens is the dispatcher's token store, read only. nil holds no
	// tokens.
	Tokens *tokens.Store
	// Hosts are the names an Origin may carry: foreman's --host names.
	Hosts []string
	// Open is true when Foreman listens beyond loopback: tailnet peers may
	// then call directly.
	Open bool
	// All and Writes are the rates per token (DefaultAll, DefaultWrites).
	All, Writes Rate
	// Concurrency is how many verbs run at once across both APIs
	// (--api-concurrency, default 2); one token holds at most one.
	Concurrency int
	// SlotWait is how long a call waits for a slot (default 10 s);
	// Timeout is how long it may run (default 30 s).
	SlotWait, Timeout time.Duration
	Clock             clock.Clock // nil: the system clock
	Log               Logger      // nil: silent
}

// Guard admits calls to the token APIs. One Guard serves /api/v1 and /mcp,
// so a token's rate and the slots are shared.
type Guard struct {
	tokens            *tokens.Store
	hosts             map[string]bool
	open              bool
	all, writes       Rate
	clock             clock.Clock
	log               Logger
	slotWait, timeout time.Duration

	slots chan struct{}

	mu      sync.Mutex
	fails   map[string][]time.Time
	rates   map[string]*rate
	holding map[string]chan struct{} // one slot per token at a time
}

// New builds a Guard.
func New(o Options) *Guard {
	g := &Guard{tokens: o.Tokens, hosts: map[string]bool{}, open: o.Open, all: o.All, writes: o.Writes,
		clock: o.Clock, log: o.Log, slotWait: o.SlotWait, timeout: o.Timeout,
		fails: map[string][]time.Time{}, rates: map[string]*rate{}, holding: map[string]chan struct{}{}}
	for _, h := range o.Hosts {
		g.hosts[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))] = true
	}
	if g.all == (Rate{}) {
		g.all = DefaultAll()
	}
	if g.writes == (Rate{}) {
		g.writes = DefaultWrites()
	}
	if g.clock == nil {
		g.clock = clock.System{}
	}
	if g.slotWait <= 0 {
		g.slotWait = DefaultSlotWait
	}
	if g.timeout <= 0 {
		g.timeout = DefaultTimeout
	}
	n := o.Concurrency
	if n <= 0 {
		n = 2
	}
	g.slots = make(chan struct{}, n)
	return g
}

func (g *Guard) now() time.Time { return g.clock.Now() }

func (g *Guard) logf(format string, args ...any) {
	if g.log != nil {
		g.log.Printf(format, args...)
	}
}

// Admit answers the request itself and returns "" when it may not go on;
// otherwise the token's name. write says the call writes: it draws on the
// write bucket too. After Admit the body is capped at BodyLimit.
func (g *Guard) Admit(w http.ResponseWriter, r *http.Request, write bool) string {
	if !g.peerOK(r) {
		WriteError(w, http.StatusForbidden, "forbidden", "this API answers only on the tailnet", "")
		return ""
	}
	if !g.originOK(r.Header.Values("Origin")) {
		WriteError(w, http.StatusForbidden, "forbidden", "requests from web pages on other sites are refused", "")
		return ""
	}
	token, presented := bearer(r)
	key := failKey(clientIP(r), token)
	if !g.allowed(key) {
		w.Header().Set("Retry-After", strconv.Itoa(int(FailWindow.Seconds())))
		WriteError(w, http.StatusTooManyRequests, "too_many", "too many bad tokens from this address; wait a while", "")
		return ""
	}
	name := ""
	if presented {
		// a token past its expiry is stale, not a guess: it doesn't count
		// towards the lockout
		if name = g.verify(token); name == "" && !g.stale(token) {
			g.failed(key)
		}
	}
	if name == "" {
		Unauthorized(w)
		return ""
	}
	if wait := g.take(name, write); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		WriteError(w, http.StatusTooManyRequests, "too_many", "this token is calling too often; slow down", "")
		return ""
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		if t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || t != "application/json" {
			WriteError(w, http.StatusUnsupportedMediaType, "bad_request", "send the body as application/json", "")
			return ""
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, BodyLimit)
	return name
}

func (g *Guard) verify(token string) string {
	if g.tokens == nil {
		return ""
	}
	if name, ok := g.tokens.Verify(token); ok {
		return name
	}
	return ""
}

func (g *Guard) stale(token string) bool {
	return g.tokens != nil && g.tokens.Stale(token)
}

// failKey is whose bad tokens count together. Every caller on this host
// shares loopback, so there one stale credential would lock out every local
// agent: count them by the token presented instead. Tokens are random
// 256-bit values, so guessing them gains nothing from that.
func failKey(ip, token string) string {
	if !isLoopback(ip) {
		return ip
	}
	sum := sha256.Sum256([]byte(token))
	return "token " + hex.EncodeToString(sum[:8])
}

// Unauthorized is the 401 with its WWW-Authenticate challenge.
func Unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="factory"`)
	WriteError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is needed", "")
}

// tailnet is Tailscale's address ranges.
func tailnet() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func onTailnet(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range tailnet() {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientIP: behind `tailscale serve` every request comes from loopback and
// the proxy's last X-Forwarded-For entry names the client.
func clientIP(r *http.Request) string {
	ip := remoteIP(r)
	if xs := r.Header.Values("X-Forwarded-For"); isLoopback(ip) && len(xs) > 0 {
		parts := strings.Split(xs[len(xs)-1], ",")
		if f := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(f) != nil {
			return f
		}
	}
	return ip
}

func isLoopback(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && p.IsLoopback()
}

// peerOK: `tailscale serve` proxies from loopback; a direct caller must be
// on the tailnet, and only when Foreman listens beyond loopback. A request
// that came in through Funnel, or whose proxy names a client off the
// tailnet, came from the internet. Funnel should never be on: this is a
// second line.
func (g *Guard) peerOK(r *http.Request) bool {
	ip := remoteIP(r)
	if isLoopback(ip) {
		if r.Header.Get("Tailscale-Funnel-Request") != "" {
			return false
		}
		if xs := r.Header.Values("X-Forwarded-For"); len(xs) > 0 {
			c := clientIP(r)
			return isLoopback(c) || onTailnet(c)
		}
		return true
	}
	return g.open && onTailnet(ip)
}

// originOK: tools send no Origin; a browser's must be this site over https,
// which a rebound name never is.
func (g *Guard) originOK(origins []string) bool {
	if len(origins) == 0 {
		return true
	}
	if len(origins) > 1 {
		return false
	}
	u, err := url.Parse(origins[0])
	if err != nil || u.Scheme != "https" || u.Path != "" || u.User != nil {
		return false
	}
	return g.hosts[strings.ToLower(u.Hostname())]
}

// bearer is the token from the one Authorization header; presented is false
// when there is nothing to check.
func bearer(r *http.Request) (token string, presented bool) {
	hs := r.Header.Values("Authorization")
	if len(hs) != 1 {
		return "", len(hs) > 1
	}
	scheme, tok, ok := strings.Cut(strings.TrimSpace(hs[0]), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", true
	}
	return tok, true
}

func recent(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cut) {
		i++
	}
	return ts[i:]
}

func (g *Guard) allowed(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.fails[key]; ok {
		if f = recent(f, g.now().Add(-FailWindow)); len(f) == 0 {
			delete(g.fails, key)
		} else {
			g.fails[key] = f
		}
	}
	return len(g.fails[key]) < FailLimit
}

// failed counts a bad token from key. Every expired entry goes first, so
// the map holds only the window's keys, and past maxFailKeys a new key is
// not tracked (bug 4).
func (g *Guard) failed(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	cut := now.Add(-FailWindow)
	for k, f := range g.fails {
		if f = recent(f, cut); len(f) == 0 {
			delete(g.fails, k)
		} else {
			g.fails[k] = f
		}
	}
	if _, ok := g.fails[key]; ok || len(g.fails) < maxFailKeys {
		g.fails[key] = append(g.fails[key], now)
	}
}

func (g *Guard) failKeys() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.fails)
}

type bucket struct {
	level float64
	at    time.Time
}

type rate struct{ all, writes bucket }

// fill brings the bucket up to now and says how long until it holds one
// call.
func (b *bucket) fill(s Rate, now time.Time) time.Duration {
	if b.at.IsZero() {
		b.level = s.Burst
	} else {
		b.level = math.Min(s.Burst, b.level+now.Sub(b.at).Minutes()*s.PerMin)
	}
	b.at = now
	if b.level >= 1 {
		return 0
	}
	return time.Duration((1 - b.level) / s.PerMin * float64(time.Minute))
}

func (g *Guard) rate(name string) *rate {
	rt := g.rates[name]
	if rt == nil {
		rt = &rate{}
		g.rates[name] = rt
	}
	return rt
}

// TakeWrite draws on the write bucket alone, for a call admitted as a read
// whose body then shows it writes (MCP's tools/call). 0 when it may go,
// else how long to wait.
func (g *Guard) TakeWrite(name string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt := g.rate(name)
	if wait := rt.writes.fill(g.writes, g.now()); wait > 0 {
		return wait
	}
	rt.writes.level--
	return 0
}

// take: 0 when the call may go, else how long to wait. A refused call draws
// nothing.
func (g *Guard) take(name string, write bool) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt := g.rate(name)
	now := g.now()
	wait := rt.all.fill(g.all, now)
	if write {
		wait = max(wait, rt.writes.fill(g.writes, now))
	}
	if wait > 0 {
		return wait
	}
	rt.all.level--
	if write {
		rt.writes.level--
	}
	return 0
}
