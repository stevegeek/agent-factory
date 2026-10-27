package auth

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

const testSecret = "test-secret-0123456789"

type bufLog struct{ b *bytes.Buffer }

func (l bufLog) Printf(format string, args ...any) { fmt.Fprintf(l.b, format+"\n", args...) }

func logTo(b *bytes.Buffer) Logger { return bufLog{b} }

func newAuth(t *testing.T, opt Options) *Auth {
	t.Helper()
	if opt.FailDelay == 0 {
		opt.FailDelay = time.Nanosecond
	}
	a, err := New([]byte(testSecret), opt)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Foreman's TestSecretFileCreated.
func TestSecretFileCreated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "secret")
	s, err := LoadSecret(p, nil)
	if err != nil || len(s) != 64 {
		t.Fatalf("secret %q %v", s, err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	dir, _ := os.Stat(filepath.Dir(p))
	if dir.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", dir.Mode())
	}
	again, _ := LoadSecret(p, nil)
	if string(again) != string(s) {
		t.Errorf("the secret must persist")
	}
	short := filepath.Join(t.TempDir(), "short")
	os.WriteFile(short, []byte("tooshort\n"), 0o600)
	if _, err := LoadSecret(short, nil); err == nil || !strings.Contains(err.Error(), "shorter than 16") {
		t.Errorf("a short secret: %v", err)
	}
}

// Foreman's TestSecretPermissions.
func TestSecretPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conf")
	os.Mkdir(dir, 0o700)
	p := filepath.Join(dir, "secret")
	os.WriteFile(p, []byte("a-long-enough-secret-value\n"), 0o644)
	os.Chmod(p, 0o644)
	var logged bytes.Buffer
	if _, err := LoadSecret(p, logTo(&logged)); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Errorf("a readable secret should be tightened to 0600: %v", info.Mode())
	}
	if !strings.Contains(logged.String(), "Rotate it") {
		t.Errorf("tightening should say to rotate: %q", logged.String())
	}
	os.Chmod(dir, 0o777)
	defer os.Chmod(dir, 0o700)
	if _, err := LoadSecret(p, nil); err == nil {
		t.Errorf("a secret in a world-writable directory must be refused")
	}
}

// Foreman's TestRevokeSessions.
func TestRevokeSessions(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clk := testkit.NewStep(at, 0)
	a := newAuth(t, Options{SinceFile: SinceFile(filepath.Join(t.TempDir(), "secret")), Clock: clk})
	old := a.NewSession(Maintainer)
	if a.Session(old) == "" {
		t.Fatal("a fresh session must pass")
	}
	if err := a.Revoke(Maintainer); err != nil {
		t.Fatal(err)
	}
	if a.Session(old) != "" {
		t.Errorf("a session from before the revocation must fail")
	}
	clk.Advance(2 * time.Second)
	if a.Session(a.NewSession(Maintainer)) == "" {
		t.Errorf("a session after the revocation must pass")
	}
	// --revoke-sessions writes the same file
	b := newAuth(t, Options{SinceFile: SinceFile(filepath.Join(t.TempDir(), "secret")), Clock: testkit.Fixed{T: at}})
	s := b.NewSession(Maintainer)
	if err := RevokeSessions(strings.TrimSuffix(b.since, ".since"), at); err != nil {
		t.Fatal(err)
	}
	if b.Session(s) != "" {
		t.Errorf("RevokeSessions must end the sessions issued until now")
	}
}

func TestSessionsAreSignedAndExpire(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	a := newAuth(t, Options{Clock: testkit.Fixed{T: now}})
	v := a.NewSession(Maintainer)
	if id := a.Session(v); !strings.HasPrefix(id, "session:") || a.UserOf(id) != Maintainer {
		t.Fatalf("session %q", id)
	}
	forged := strings.Replace(v, v[:4], "9999", 1)
	if a.Session(forged) != "" {
		t.Errorf("a changed cookie must not pass")
	}
	if a.Session("garbage") != "" || a.Session("") != "" {
		t.Errorf("garbage passed")
	}
	other, _ := New([]byte("another-secret-0123456789"), Options{Clock: testkit.Fixed{T: now}})
	if a.Session(other.NewSession(Maintainer)) != "" {
		t.Errorf("another secret's session passed")
	}
	old := newAuth(t, Options{Clock: testkit.Fixed{T: now.Add(-31 * 24 * time.Hour)}})
	if a.Session(old.NewSession(Maintainer)) != "" {
		t.Errorf("an expired session must not pass")
	}
	if a.UserOf("tailscale:someone@example.com") != Maintainer {
		t.Errorf("a trusted Tailscale login is the maintainer")
	}
}

// Foreman's TestSafeNext, the function half.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/floor": "/floor", "/t/T0001?kind=review": "/t/T0001?kind=review",
		"//evil.example": "/", "/\\evil.example": "/", "/\t/evil.example": "/", "/\r\n/evil.example": "/",
		"/%09/evil.example": "/%09/evil.example", "https://evil.example": "/", "/login?next=/": "/", "": "/",
		"/\x7fx": "/"} {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// Foreman's TestSameOriginTrustsTheBrowsersFetchSite.
func TestSameOriginTrustsTheBrowsersFetchSite(t *testing.T) {
	for _, c := range []struct {
		site, origin string
		want         bool
	}{
		{"same-origin", "null", true},
		{"same-origin", "http://desk.example:8790", true},
		{"none", "", true},
		{"cross-site", "http://desk.example:8790", false},
		{"same-site", "http://desk.example:8790", false},
		{"", "http://desk.example:8790", true},
		{"", "http://evil.example", false},
		{"", "null", false},
		{"", "", true},
	} {
		r := httptest.NewRequest("POST", "http://desk.example:8790/login", nil)
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := SameOrigin(r); got != c.want {
			t.Errorf("Sec-Fetch-Site %q, Origin %q: SameOrigin = %v, want %v", c.site, c.origin, got, c.want)
		}
	}
}

// Foreman's TestHostAllowlist, the Host check alone (the web tests run it on
// every route).
func TestHostAllowlist(t *testing.T) {
	a := newAuth(t, Options{Hosts: []string{"Example.com."}})
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := a.CheckHost(ok)
	for host, want := range map[string]bool{"127.0.0.1:8790": true, "[::1]:8790": true, "localhost:8790": true,
		"192.0.2.7": true, "example.com": true, "EXAMPLE.com.": true, "rebound.example:8790": false, "": false} {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if (w.Code != http.StatusMisdirectedRequest) != want {
			t.Errorf("Host %q: %d", host, w.Code)
		}
	}
	if !a.KnownHost("example.com") || a.KnownHost("localhost") {
		t.Errorf("KnownHost is the --host list only")
	}
}

// Foreman's TestWrongSecretIsSlow.
func TestWrongSecretIsSlow(t *testing.T) {
	a := newAuth(t, Options{FailDelay: 50 * time.Millisecond})
	start := time.Now()
	if _, ok := a.LoginAs("192.0.2.1", "", "wrong"); ok || time.Since(start) < 50*time.Millisecond {
		t.Errorf("a wrong secret should fail after the delay")
	}
	start = time.Now()
	if u, ok := a.LoginAs("192.0.2.1", "", testSecret); !ok || u != Maintainer || time.Since(start) >= 50*time.Millisecond {
		t.Errorf("the right secret should pass at once")
	}
}

// Foreman's TestLoginRateLimit and TestNoGlobalLockout, on the throttle.
func TestThrottleBlocksAnAddressNeverEveryone(t *testing.T) {
	var logged bytes.Buffer
	a := newAuth(t, Options{Log: logTo(&logged)})
	for i := 0; i < FailLimit; i++ {
		if !a.Allowed("192.0.2.1") {
			t.Fatalf("blocked after %d", i)
		}
		a.LoginAs("192.0.2.1", "", "wrong")
	}
	if a.Allowed("192.0.2.1") {
		t.Errorf("after %d failures the address waits", FailLimit)
	}
	for i := 0; i < FailAlarm; i++ {
		a.LoginAs("198.51.100."+strconv.Itoa(i+1), "", "wrong")
	}
	if !a.Allowed("203.0.113.9") {
		t.Errorf("wrong tries from elsewhere must not lock anyone else out")
	}
	if !strings.Contains(logged.String(), "someone may be guessing") {
		t.Errorf("the alarm should be logged: %q", logged.String())
	}
}

// Bug 4: the failure map is bounded. Expired keys go on every failure, and
// past the cap a new key is not tracked (the global delay still bounds
// guessing).
func TestFailMapIsBounded(t *testing.T) {
	clk := testkit.NewStep(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), 0)
	a := newAuth(t, Options{Clock: clk})
	for i := 0; i < 50; i++ {
		a.Failed("192.0.2." + strconv.Itoa(i))
	}
	if n := a.failKeys(); n != 50 {
		t.Fatalf("%d keys", n)
	}
	clk.Advance(FailWindow + time.Second)
	a.Failed("198.51.100.1")
	if n := a.failKeys(); n != 1 {
		t.Errorf("expired keys should be pruned: %d left", n)
	}
	for i := 0; i < maxFailKeys+100; i++ {
		a.Failed("k" + strconv.Itoa(i))
	}
	if n := a.failKeys(); n > maxFailKeys {
		t.Errorf("the map grew past its cap: %d", n)
	}
	if a.Allowed("k"+strconv.Itoa(maxFailKeys+50)) != true {
		t.Errorf("an untracked key is not blocked")
	}
}

func TestClientIPBehindTailscaleServe(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Add("X-Forwarded-For", "203.0.113.1")
	r.Header.Add("X-Forwarded-For", "10.0.0.1, 100.64.0.9")
	if ip := ClientIP(r); ip != "100.64.0.9" {
		t.Errorf("loopback: %q", ip)
	}
	r.RemoteAddr = "192.0.2.4:5555"
	if ip := ClientIP(r); ip != "192.0.2.4" {
		t.Errorf("a remote peer's X-Forwarded-For counts for nothing: %q", ip)
	}
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if ip := ClientIP(r); ip != "127.0.0.1" {
		t.Errorf("garbage: %q", ip)
	}
}

// Foreman's TestTailscaleHeader and TestTailscaleOnlyNamedUsers, on
// Identity.
func TestTailscaleHeader(t *testing.T) {
	for _, trust := range []bool{false, true} {
		var users []string
		if trust {
			users = []string{"Someone@example.com"}
		}
		a := newAuth(t, Options{TailscaleUsers: users})
		for login, ok := range map[string]bool{"someone@example.com": true, "SOMEONE@example.com": true, "other@example.com": false, "": false} {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = "127.0.0.1:5555"
			r.Header.Set("Tailscale-User-Login", login)
			if got := a.Identity(r) != ""; got != (ok && trust) {
				t.Errorf("trust=%v %q: identity %v", trust, login, got)
			}
			r.RemoteAddr = "192.0.2.4:5555"
			if a.Identity(r) != "" {
				t.Errorf("trust=%v: the header from a non-loopback address must not count", trust)
			}
		}
	}
}

// Require: a login for GETs (redirect) and others (403), and on every
// non-GET the same-origin check and the CSRF token, with the body capped.
func TestRequireNeedsALoginAndTheCSRFToken(t *testing.T) {
	a := newAuth(t, Options{})
	ran := 0
	h := a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran++
		if IdentityOf(r) == "" {
			t.Errorf("the identity should ride in the context")
		}
	}))
	do := func(method, target string, form url.Values, cookie string, hdr map[string]string) int {
		r := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusSeeOther && w.Header().Get("Location") != "/login?next=%2Fx%3Fa%3D1" {
			t.Errorf("redirect to %q", w.Header().Get("Location"))
		}
		return w.Code
	}
	if c := do("GET", "/x?a=1", nil, "", nil); c != http.StatusSeeOther {
		t.Errorf("GET without login: %d", c)
	}
	if c := do("POST", "/x", url.Values{}, "", nil); c != http.StatusForbidden {
		t.Errorf("POST without login: %d", c)
	}
	s := a.NewSession(Maintainer)
	id := a.Session(s)
	csrf := a.CSRF(id)
	if len(csrf) != 32 {
		t.Errorf("csrf %q", csrf)
	}
	if c := do("GET", "/x", nil, s, nil); c != 200 {
		t.Errorf("GET with login: %d", c)
	}
	if c := do("POST", "/x", url.Values{"csrf": {"nope"}}, s, nil); c != http.StatusForbidden {
		t.Errorf("wrong CSRF: %d", c)
	}
	if c := do("POST", "/x", url.Values{"csrf": {csrf}}, s, map[string]string{"Sec-Fetch-Site": "cross-site"}); c != http.StatusForbidden {
		t.Errorf("cross-site: %d", c)
	}
	if c := do("POST", "/x", url.Values{"csrf": {csrf}, "big": {strings.Repeat("x", 70<<10)}}, s, nil); c != http.StatusForbidden {
		t.Errorf("a body over 64 KiB: %d", c)
	}
	before := ran
	if c := do("POST", "/x", url.Values{"csrf": {csrf}}, s, nil); c != 200 || ran != before+1 {
		t.Errorf("good POST: %d", c)
	}
}

func TestCookies(t *testing.T) {
	a := newAuth(t, Options{})
	r := httptest.NewRequest("POST", "/login", nil)
	w := httptest.NewRecorder()
	a.SetSession(w, r, Maintainer)
	c := w.Result().Cookies()[0]
	if c.Name != SessionCookie || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || c.MaxAge != 30*24*3600 {
		t.Errorf("cookie flags %+v", c)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	a.SetSession(w, r, Maintainer)
	if !w.Result().Cookies()[0].Secure {
		t.Errorf("over https the cookie is Secure")
	}
	w = httptest.NewRecorder()
	ClearSession(w, r)
	if c := w.Result().Cookies()[0]; c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("cleared %+v", c)
	}
	if a.MAC("flash", "x") == a.MAC("flash", "y") || a.MAC("flash", "x") != a.MAC("flash", "x") {
		t.Errorf("MAC")
	}
}
