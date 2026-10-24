package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
)

// Maintainer is the identity of the main secret and of a trusted Tailscale
// login: they see everything.
const Maintainer = "maintainer"

// SessionCookie is the login cookie's name.
const SessionCookie = "foreman_session"

const (
	sessionLife = 30 * 24 * time.Hour
	// FailLimit is the wrong tries one address may make per FailWindow.
	FailLimit = 5
	// FailAlarm is the wrong tries across all addresses per window that are
	// logged as someone guessing: never a lockout (anyone could trip it).
	FailAlarm = 30
	// FailWindow is how long a wrong try counts.
	FailWindow = 15 * time.Minute
	// DefaultFailDelay is how long each wrong try holds the one check lock.
	DefaultFailDelay = time.Second
	// maxFailKeys caps the addresses whose failures are tracked (bug 4).
	// With the check lock held FailDelay per wrong try, a window holds at
	// most 900 of them, so the cap is reached only with a tiny delay.
	maxFailKeys = 4096
	maxBody     = 64 << 10
)

// Options configures an Auth.
type Options struct {
	SinceFile      string        // the revocation file (SinceFile(secret)); "" for none
	Users          *Users        // other people's logins; nil: the maintainer only
	Hosts          []string      // Host names to answer to besides IP literals and localhost
	TailscaleUsers []string      // logins trusted in Tailscale-User-Login from loopback; none: the header is ignored
	FailDelay      time.Duration // 0: DefaultFailDelay
	Clock          clock.Clock   // nil: the system clock
	Log            Logger
}

// Auth checks who is asking. It is safe for concurrent use.
type Auth struct {
	secret, key []byte
	tsUsers     map[string]bool
	hosts       map[string]bool
	since       string
	users       *Users
	delay       time.Duration
	clock       clock.Clock
	log         Logger

	mu     sync.Mutex // fails and global
	fails  map[string][]time.Time
	global []time.Time
	check  sync.Mutex // one secret check at a time

	sinceMu  sync.Mutex
	sinceMod time.Time
	sinceAt  int64
}

// New is an Auth for the secret.
func New(secret []byte, opt Options) (*Auth, error) {
	if len(secret) == 0 {
		return nil, errors.New("auth: no secret")
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("foreman session key v1"))
	a := &Auth{secret: secret, key: m.Sum(nil), tsUsers: map[string]bool{}, hosts: map[string]bool{},
		since: opt.SinceFile, users: opt.Users, delay: opt.FailDelay, clock: opt.Clock, log: opt.Log,
		fails: map[string][]time.Time{}}
	if a.delay <= 0 {
		a.delay = DefaultFailDelay
	}
	if a.clock == nil {
		a.clock = clock.System{}
	}
	for _, u := range opt.TailscaleUsers {
		a.tsUsers[strings.ToLower(strings.TrimSpace(u))] = true
	}
	for _, h := range opt.Hosts {
		a.hosts[normHost(h)] = true
	}
	return a, nil
}

func normHost(h string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")) }

func (a *Auth) now() time.Time { return a.clock.Now() }

// Users is the users store this Auth checks sessions against (nil: none).
func (a *Auth) Users() *Users { return a.users }

// MAC is base64url HMAC-SHA256, under the session key, over the parts
// joined by NUL: what signs sessions, CSRF tokens and flash messages.
func (a *Auth) MAC(parts ...string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(strings.Join(parts, "\x00")))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Auth) checkSecret(given string) bool {
	g, s := sha256.Sum256([]byte(strings.TrimSpace(given))), sha256.Sum256(a.secret)
	return subtle.ConstantTimeCompare(g[:], s[:]) == 1
}

// NewSession is a signed cookie value for user: "<expiry>.<nonce>[.u-<user>].<mac>".
// Another user's name rides in it; the maintainer's does not.
func (a *Auth) NewSession(user string) string {
	nonce := make([]byte, 12)
	rand.Read(nonce)
	payload := strconv.FormatInt(a.now().Add(sessionLife).Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	if user != Maintainer {
		payload += ".u-" + user
	}
	return payload + "." + a.MAC("session", payload)
}

// UserOf is whose identity this is: Maintainer or another user's name.
func (a *Auth) UserOf(identity string) string {
	payload, ok := strings.CutPrefix(identity, "session:")
	if !ok {
		return Maintainer // a trusted Tailscale login
	}
	parts := strings.Split(payload, ".")
	if len(parts) == 3 && strings.HasPrefix(parts[2], "u-") {
		return parts[2][2:]
	}
	return Maintainer
}

// revokedBefore is the unix time a session must be issued at or after,
// re-read when the revocation file changes.
func (a *Auth) revokedBefore() int64 {
	if a.since == "" {
		return 0
	}
	a.sinceMu.Lock()
	defer a.sinceMu.Unlock()
	info, err := os.Stat(a.since)
	if err != nil {
		a.sinceAt, a.sinceMod = 0, time.Time{}
		return 0
	}
	if !info.ModTime().Equal(a.sinceMod) {
		data, _ := os.ReadFile(a.since)
		n, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		a.sinceAt, a.sinceMod = n, info.ModTime()
	}
	return a.sinceAt
}

// Session is the identity of a valid session cookie value, or "": the MAC
// is checked in constant time, the session must not have expired, must be
// issued no earlier than the revocation time, and its user must still exist.
func (a *Auth) Session(v string) string {
	i := strings.LastIndexByte(v, '.')
	if i < 0 {
		return ""
	}
	payload, sig := v[:i], v[i+1:]
	if !hmac.Equal([]byte(sig), []byte(a.MAC("session", payload))) {
		return ""
	}
	exp, err := strconv.ParseInt(strings.SplitN(payload, ".", 2)[0], 10, 64)
	if err != nil || a.now().Unix() > exp || exp-int64(sessionLife.Seconds()) < a.revokedBefore() {
		return ""
	}
	if u := a.UserOf("session:" + payload); u != Maintainer && (a.users == nil || a.users.Get(u) == nil) {
		return "" // a user who has been removed is logged out
	}
	return "session:" + payload
}

// Revoke ends every session: logging out anywhere logs out everywhere.
func (a *Auth) Revoke() error {
	if a.since == "" {
		return nil
	}
	return writeSince(a.since, a.now())
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ClientIP is the address failed logins are counted against: the peer, but
// behind `tailscale serve` (every request from loopback) the last entry of
// the last X-Forwarded-For header.
func ClientIP(r *http.Request) string {
	ip := remoteIP(r)
	if xs := r.Header.Values("X-Forwarded-For"); IsLoopback(ip) && len(xs) > 0 {
		parts := strings.Split(xs[len(xs)-1], ",")
		if f := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(f) != nil {
			return f
		}
	}
	return ip
}

// IsLoopback reports a loopback IP literal.
func IsLoopback(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && p.IsLoopback()
}

// Identity is who is asking, or "" when nobody has logged in. A trusted
// Tailscale login counts only from a loopback peer and only when listed.
func (a *Auth) Identity(r *http.Request) string {
	if len(a.tsUsers) > 0 && IsLoopback(remoteIP(r)) {
		if u := strings.ToLower(r.Header.Get("Tailscale-User-Login")); a.tsUsers[u] {
			return "tailscale:" + u
		}
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		return a.Session(c.Value)
	}
	return ""
}

// CSRF is the form token of an identity: per session, stateless.
func (a *Auth) CSRF(identity string) string { return a.MAC("csrf", identity)[:32] }

func (a *Auth) checkCSRF(identity, token string) bool {
	return identity != "" && hmac.Equal([]byte(token), []byte(a.CSRF(identity)))
}

// Allowed is false while an address has failed too often lately.
func (a *Auth) Allowed(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := a.now().Add(-FailWindow)
	if f, ok := a.fails[ip]; ok {
		if f = recent(f, cut); len(f) == 0 {
			delete(a.fails, ip)
		} else {
			a.fails[ip] = f
		}
	}
	return len(a.fails[ip]) < FailLimit
}

// Failed counts a wrong try from ip. Every expired entry goes first, so the
// map holds only the window's addresses, and past maxFailKeys a new address
// is not tracked (bug 4): the alarm still counts it, and the check lock
// still slows it.
func (a *Auth) Failed(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	cut := now.Add(-FailWindow)
	for k, f := range a.fails {
		if f = recent(f, cut); len(f) == 0 {
			delete(a.fails, k)
		} else {
			a.fails[k] = f
		}
	}
	if _, ok := a.fails[ip]; ok || len(a.fails) < maxFailKeys {
		a.fails[ip] = append(a.fails[ip], now)
	}
	a.global = append(recent(a.global, cut), now)
	if len(a.global) == FailAlarm {
		logf(a.log, "%d failed logins in %s across all addresses: someone may be guessing the secret", FailAlarm, FailWindow)
	}
}

func (a *Auth) failKeys() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.fails)
}

// LoginAs is whose secret this is: the maintainer's or another user's. One
// secret is checked at a time, and a wrong one holds the lock for the fail
// delay: guessing stays slow from any number of addresses, and nobody can
// lock the maintainer out.
func (a *Auth) LoginAs(ip, given string) (string, bool) {
	a.check.Lock()
	defer a.check.Unlock()
	if a.checkSecret(given) {
		return Maintainer, true
	}
	if a.users != nil {
		if u := a.users.Match(given); u != "" {
			return u, true
		}
	}
	a.Failed(ip)
	time.Sleep(a.delay)
	return "", false
}

func recent(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cut) {
		i++
	}
	return ts[i:]
}

// Secure reports a request that came over TLS, directly or through a proxy
// that says so.
func Secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SetSession sets a new session cookie for user.
func (a *Auth) SetSession(w http.ResponseWriter, r *http.Request, user string) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: a.NewSession(user), Path: "/", MaxAge: int(sessionLife.Seconds()),
		HttpOnly: true, Secure: Secure(r), SameSite: http.SameSiteStrictMode})
}

// ClearSession removes the session cookie.
func ClearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: Secure(r), SameSite: http.SameSiteStrictMode})
}

type ctxKey struct{}

// IdentityOf is the identity Require put on the request, or "".
func IdentityOf(r *http.Request) string {
	s, _ := r.Context().Value(ctxKey{}).(string)
	return s
}

// Require wants a login for everything it wraps (GET and HEAD go to the
// login page, anything else is refused), and on every non-GET a body of at
// most 64 KiB, the browser's word that it came from this site, and the
// CSRF token.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := a.Identity(r)
		if id == "" {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "log in first", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			if !SameOrigin(r) || !a.checkCSRF(id, r.PostFormValue("csrf")) {
				http.Error(w, "this form has expired; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// SameOrigin is a browser's word that the request came from this site.
// Sec-Fetch-Site says so directly; failing that, the Origin, when sent,
// must be this host ("null" never is).
func SameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "same-site", "cross-site":
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// SafeNext keeps a post-login redirect on this site. Browsers drop tabs and
// newlines in a URL and read "\" as "/", so "/\t/evil.example" would leave it.
func SafeNext(s string) string {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/login") {
		return "/"
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f || s[i] == '\\' {
			return "/"
		}
	}
	if u, err := url.Parse(s); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return s
}

// KnownHost reports a name given with --host.
func (a *Auth) KnownHost(name string) bool { return a.hosts[normHost(name)] }

// hostOK: DNS rebinding needs a name, so IP literals and localhost pass;
// other names only when listed. The port and a trailing dot are ignored.
func (a *Auth) hostOK(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	return net.ParseIP(host) != nil || host == "localhost" || a.hosts[host]
}

// CheckHost refuses a request for a Host name this server was not told to
// answer to (421), on every route.
func (a *Auth) CheckHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hostOK(r.Host) {
			logf(a.log, "refused Host %q; start foreman with --host for it if it is yours", r.Host)
			http.Error(w, "unknown host name; start foreman with --host for it", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
