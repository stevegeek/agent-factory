package main

import (
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiBody       = 256 << 10
	apiFailLimit  = 10 // bad tokens per client address per window
	apiFailWindow = 15 * time.Minute
)

var tailnetPrefixes = []*net.IPNet{mustCIDR("100.64.0.0/10"), mustCIDR("fd7a:115c:a1e0::/48")}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func onTailnet(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range tailnetPrefixes {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// Guard admits a request to the token APIs (/api/v1 and /mcp): the peer, the Origin, the bearer
// token, the caller's rate and the body's size and type, in that order and before any body is read.
type Guard struct {
	tokens *APITokens
	hosts  map[string]bool // names an Origin may carry: the --host names
	open   bool            // listening beyond loopback: tailnet peers may call directly
	now    func() time.Time
	all    bucketSpec // every call
	writes bucketSpec // and a write besides

	mu    sync.Mutex
	fails map[string][]time.Time
	rates map[string]*apiRate
}

func NewGuard(tokens *APITokens, hosts map[string]bool, open bool) *Guard {
	return &Guard{tokens: tokens, hosts: hosts, open: open, now: time.Now, all: apiAll, writes: apiWrites,
		fails: map[string][]time.Time{}, rates: map[string]*apiRate{}}
}

// Admit answers the request itself and returns "" when it may not go on; otherwise the token's name.
func (g *Guard) Admit(w http.ResponseWriter, r *http.Request, write bool) string {
	if !g.peerOK(r) {
		apiError(w, http.StatusForbidden, "forbidden", "this API answers only on the tailnet", "")
		return ""
	}
	if !g.originOK(r.Header.Values("Origin")) {
		apiError(w, http.StatusForbidden, "forbidden", "requests from web pages on other sites are refused", "")
		return ""
	}
	ip := clientIP(r)
	if !g.allowed(ip) {
		w.Header().Set("Retry-After", strconv.Itoa(int(apiFailWindow.Seconds())))
		apiError(w, http.StatusTooManyRequests, "too_many", "too many bad tokens from this address; wait a while", "")
		return ""
	}
	token, presented := bearer(r)
	name := ""
	if presented {
		if name = g.tokens.Name(token); name == "" {
			g.failed(ip)
		}
	}
	if name == "" {
		unauthorized(w)
		return ""
	}
	if wait := g.take(name, write); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		apiError(w, http.StatusTooManyRequests, "too_many", "this token is calling too often; slow down", "")
		return ""
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		if t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || t != "application/json" {
			apiError(w, http.StatusUnsupportedMediaType, "bad_request", "send the body as application/json", "")
			return ""
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, apiBody)
	return name
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="factory"`)
	apiError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is needed", "")
}

// peerOK: `tailscale serve` proxies from loopback; a direct caller must be on the tailnet, and only when
// Foreman listens beyond loopback. A request that came in through Funnel, or whose proxy names a client
// off the tailnet, came from the internet. Funnel should never be on: this is a second line.
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

// originOK: tools send no Origin; a browser's must be this site over https, which a rebound name never is.
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

// bearer: the token from the one Authorization header; presented is false when there is nothing to check.
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

func (g *Guard) allowed(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fails[ip] = recent(g.fails[ip], g.now().Add(-apiFailWindow))
	if len(g.fails[ip]) == 0 {
		delete(g.fails, ip)
	}
	return len(g.fails[ip]) < apiFailLimit
}

func (g *Guard) failed(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fails[ip] = append(g.fails[ip], g.now())
}

// Rate per token: every call draws on one bucket, a write on a second, smaller one too.
var (
	apiAll    = bucketSpec{perMin: 120, burst: 30}
	apiWrites = bucketSpec{perMin: 30, burst: 10}
)

type bucketSpec struct{ perMin, burst float64 }

type bucket struct {
	level float64
	at    time.Time
}

type apiRate struct{ all, writes bucket }

// fill: the bucket as it is now, and how long until it holds one call.
func (b *bucket) fill(s bucketSpec, now time.Time) time.Duration {
	if b.at.IsZero() {
		b.level = s.burst
	} else {
		b.level = math.Min(s.burst, b.level+now.Sub(b.at).Minutes()*s.perMin)
	}
	b.at = now
	if b.level >= 1 {
		return 0
	}
	return time.Duration((1 - b.level) / s.perMin * float64(time.Minute))
}

func (g *Guard) rate(name string) *apiRate {
	rt := g.rates[name]
	if rt == nil {
		rt = &apiRate{}
		g.rates[name] = rt
	}
	return rt
}

// takeWrite: the write bucket alone, for a call admitted as a read whose body then shows it writes (MCP's
// tools/call). 0 when it may go, else how long to wait.
func (g *Guard) takeWrite(name string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt := g.rate(name)
	if wait := rt.writes.fill(g.writes, g.now()); wait > 0 {
		return wait
	}
	rt.writes.level--
	return 0
}

// take: 0 when the call may go, else how long to wait. A refused call draws nothing.
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
