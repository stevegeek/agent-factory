package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "foreman_session"
	sessionLife   = 30 * 24 * time.Hour
	failLimit     = 5  // per address per window
	failAlarm     = 30 // all addresses per window: logged, never a lockout (anyone could trip it)
	failWindow    = 15 * time.Minute
	failDelay     = time.Second // each wrong try holds the one check lock this long
)

// LoadSecret reads the shared login secret, creating one (mode 0600) when there is none.
func LoadSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err = newSecret(path); err != nil {
			return nil, err
		}
		log.Printf("wrote a new login secret to %s", path)
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if err := checkPerms(path); err != nil {
		return nil, err
	}
	s := []byte(strings.TrimSpace(string(data)))
	if len(s) < 16 {
		return nil, fmt.Errorf("%s: the secret is shorter than 16 characters", path)
	}
	return s, nil
}

// checkPerms: a secret others can read is tightened; one in a directory others can write is refused.
func checkPerms(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("%s is readable by others and could not be made 0600: %v", path, err)
		}
		log.Printf("%s was readable by others; it is now 0600. Rotate it if anyone else may have read it", path)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if dir.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by others; keep the secret in a directory only you can write", filepath.Dir(path))
	}
	return nil
}

func newSecret(path string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(hex.EncodeToString(raw) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// sinceFile holds a unix time; sessions issued before it are revoked.
func sinceFile(secretFile string) string { return secretFile + ".since" }

// RevokeSessions ends every session issued until now, on every device.
func RevokeSessions(secretFile string) error {
	return writeSince(sinceFile(secretFile), time.Now())
}

func writeSince(p string, now time.Time) error {
	return os.WriteFile(p, []byte(strconv.FormatInt(now.Unix()+1, 10)+"\n"), 0o600)
}

// Revoke: logging out anywhere logs out everywhere; sessions are stateless otherwise.
func (a *Auth) Revoke() error {
	if a.since == "" {
		return nil
	}
	return writeSince(a.since, a.now())
}

type Auth struct {
	secret, key []byte
	tsUsers     map[string]bool // logins trusted in Tailscale-User-Login from loopback; empty: header ignored
	hosts       map[string]bool // Host names served besides IP literals and localhost
	since       string          // revocation file; "" for none
	mu          sync.Mutex
	fails       map[string][]time.Time
	global      []time.Time
	check       sync.Mutex
	delay       time.Duration
	sinceMu     sync.Mutex
	sinceMod    time.Time
	sinceAt     int64
	now         func() time.Time
}

func NewAuth(secret []byte) *Auth {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("foreman session key v1"))
	return &Auth{secret: secret, key: m.Sum(nil), tsUsers: map[string]bool{}, hosts: map[string]bool{},
		fails: map[string][]time.Time{}, delay: failDelay, now: time.Now}
}

// TrustTailscale: accept these logins in Tailscale-User-Login from loopback (`tailscale serve`).
func (a *Auth) TrustTailscale(users ...string) {
	for _, u := range users {
		a.tsUsers[strings.ToLower(strings.TrimSpace(u))] = true
	}
}

// AllowHosts: Host names to answer to besides IP literals and localhost, e.g. the ts.net name.
func (a *Auth) AllowHosts(hosts ...string) {
	for _, h := range hosts {
		a.hosts[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))] = true
	}
}

func (a *Auth) mac(parts ...string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(strings.Join(parts, "\x00")))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Auth) CheckSecret(given string) bool {
	g, s := sha256.Sum256([]byte(strings.TrimSpace(given))), sha256.Sum256(a.secret)
	return subtle.ConstantTimeCompare(g[:], s[:]) == 1
}

func (a *Auth) NewSession() string {
	nonce := make([]byte, 12)
	rand.Read(nonce)
	payload := strconv.FormatInt(a.now().Add(sessionLife).Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	return payload + "." + a.mac("session", payload)
}

// revokedBefore: the unix time a session must be issued at or after; re-read when the file changes.
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

// identity of a valid session cookie value, or "".
func (a *Auth) session(v string) string {
	i := strings.LastIndexByte(v, '.')
	if i < 0 {
		return ""
	}
	payload, sig := v[:i], v[i+1:]
	if !hmac.Equal([]byte(sig), []byte(a.mac("session", payload))) {
		return ""
	}
	exp, err := strconv.ParseInt(strings.SplitN(payload, ".", 2)[0], 10, 64)
	if err != nil || a.now().Unix() > exp || exp-int64(sessionLife.Seconds()) < a.revokedBefore() {
		return ""
	}
	return "session:" + payload
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientIP: behind `tailscale serve` every request comes from loopback and the proxy's
// last X-Forwarded-For entry names the client. Only used to count failed logins.
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

// Identity: who is asking, or "" when nobody has logged in.
func (a *Auth) Identity(r *http.Request) string {
	if len(a.tsUsers) > 0 && isLoopback(remoteIP(r)) {
		// `tailscale serve` proxies from loopback and sets this; only the named logins count
		if u := strings.ToLower(r.Header.Get("Tailscale-User-Login")); a.tsUsers[u] {
			return "tailscale:" + u
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return a.session(c.Value)
	}
	return ""
}

func (a *Auth) CSRF(identity string) string { return a.mac("csrf", identity)[:32] }

func (a *Auth) checkCSRF(identity, token string) bool {
	return identity != "" && hmac.Equal([]byte(token), []byte(a.CSRF(identity)))
}

// Allowed: false while an address has failed too often lately.
func (a *Auth) Allowed(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := a.now().Add(-failWindow)
	a.fails[ip] = recent(a.fails[ip], cut)
	if len(a.fails[ip]) == 0 {
		delete(a.fails, ip)
	}
	return len(a.fails[ip]) < failLimit
}

func (a *Auth) Failed(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fails[ip] = append(a.fails[ip], a.now())
	a.global = append(recent(a.global, a.now().Add(-failWindow)), a.now())
	if len(a.global) == failAlarm {
		log.Printf("%d failed logins in %s across all addresses: someone may be guessing the secret", failAlarm, failWindow)
	}
}

// Login checks one secret at a time and holds the lock a while after a wrong one: guessing
// stays slow from any number of addresses, and nobody can lock the maintainer out.
func (a *Auth) Login(ip, given string) bool {
	a.check.Lock()
	defer a.check.Unlock()
	if a.CheckSecret(given) {
		return true
	}
	a.Failed(ip)
	time.Sleep(a.delay)
	return false
}

func recent(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cut) {
		i++
	}
	return ts[i:]
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *Auth) SetSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: a.NewSession(), Path: "/", MaxAge: int(sessionLife.Seconds()),
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
}

func ClearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
}

type ctxKey struct{}

func identityOf(r *http.Request) string {
	s, _ := r.Context().Value(ctxKey{}).(string)
	return s
}

// Require: a login for everything it wraps, and a CSRF token on every non-GET.
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
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if !sameOrigin(r) || !a.checkCSRF(id, r.PostFormValue("csrf")) {
				http.Error(w, "this form has expired; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// sameOrigin: a browser's Origin, when it sends one, must be this host.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// safeNext keeps a post-login redirect on this site. Browsers drop tabs and newlines in a URL
// and read "\" as "/", so "/\t/evil.example" would leave it.
func safeNext(s string) string {
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

// hostOK: DNS rebinding needs a name, so IP literals and localhost pass; other names only when listed.
func (a *Auth) hostOK(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	return net.ParseIP(host) != nil || host == "localhost" || a.hosts[host]
}

// CheckHost refuses a request for a Host name this server was not told to answer to.
func (a *Auth) CheckHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hostOK(r.Host) {
			log.Printf("refused Host %q; start foreman with --host for it if it is yours", r.Host)
			http.Error(w, "unknown host name; start foreman with --host for it", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
