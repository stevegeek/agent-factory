package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/floor": "/floor", "/t/T0001?kind=review": "/t/T0001?kind=review",
		"//evil.example": "/", "/\\evil.example": "/", "/\t/evil.example": "/", "/\r\n/evil.example": "/",
		"/%09/evil.example": "/%09/evil.example", "https://evil.example": "/", "/login?next=/": "/", "": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
	h := newHarness(t, false)
	c := h.login()
	if w := h.do("GET", "/login?next=%2F%09%2Fevil.example", nil, c); w.Header().Get("Location") != "/" {
		t.Errorf("a tab in next must not leave the site: %q", w.Header().Get("Location"))
	}
	w := h.do("POST", "/login", url.Values{"secret": {testSecret}, "next": {"/\t/evil.example"}})
	if w.Header().Get("Location") != "/" {
		t.Errorf("login with a tab in next: %q", w.Header().Get("Location"))
	}
}

func TestNoGlobalLockout(t *testing.T) {
	h := newHarness(t, false)
	post := func(remote, xff, secret string) int {
		r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"secret": {secret}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 40; i++ {
		post("192.0.2."+strconv.Itoa(i+1)+":5555", "", "wrong")
	}
	if code := post("198.51.100.1:5555", "", testSecret); code != http.StatusSeeOther {
		t.Errorf("wrong tries from elsewhere must not lock the maintainer out: %d", code)
	}
	// behind tailscale serve, the proxy's X-Forwarded-For is the address that counts
	for i := 0; i < failLimit; i++ {
		post("127.0.0.1:5555", "100.64.0.9", "wrong")
	}
	if code := post("127.0.0.1:5555", "100.64.0.9", testSecret); code != http.StatusTooManyRequests {
		t.Errorf("the guessing client waits: %d", code)
	}
	if code := post("127.0.0.1:5555", "100.64.0.10", testSecret); code != http.StatusSeeOther {
		t.Errorf("another tailnet client still gets in: %d", code)
	}
}

func TestWrongSecretIsSlow(t *testing.T) {
	a := NewAuth([]byte(testSecret))
	a.delay = 50 * time.Millisecond
	start := time.Now()
	if a.Login("192.0.2.1", "wrong") || time.Since(start) < a.delay {
		t.Errorf("a wrong secret should fail after the delay")
	}
	start = time.Now()
	if !a.Login("192.0.2.1", testSecret) || time.Since(start) >= a.delay {
		t.Errorf("the right secret should pass at once")
	}
}

func TestTailscaleOnlyNamedUsers(t *testing.T) {
	h := newHarness(t, true)
	for login, ok := range map[string]bool{"someone@example.com": true, "SOMEONE@example.com": true, "other@example.com": false, "": false} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("Tailscale-User-Login", login)
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if (w.Code == 200) != ok {
			t.Errorf("%q: %d", login, w.Code)
		}
	}
}

func TestHostAllowlist(t *testing.T) {
	h := newHarness(t, false)
	for host, ok := range map[string]bool{"127.0.0.1:8790": true, "[::1]:8790": true, "localhost:8790": true,
		"192.0.2.7": true, "example.com": true, "EXAMPLE.com.": true, "rebound.example:8790": false} {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if (w.Code != http.StatusMisdirectedRequest) != ok {
			t.Errorf("Host %q: %d", host, w.Code)
		}
	}
}

func TestRevokeSessions(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	a := NewAuth([]byte(testSecret))
	a.since = sinceFile(filepath.Join(t.TempDir(), "secret"))
	a.now = func() time.Time { return at }
	old := a.NewSession()
	if a.session(old) == "" {
		t.Fatal("a fresh session must pass")
	}
	if err := a.Revoke(); err != nil {
		t.Fatal(err)
	}
	if a.session(old) != "" {
		t.Errorf("a session from before the revocation must fail")
	}
	at = at.Add(2 * time.Second)
	if a.session(a.NewSession()) == "" {
		t.Errorf("a session after the revocation must pass")
	}
}

func TestSecretPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conf")
	os.Mkdir(dir, 0o700)
	p := filepath.Join(dir, "secret")
	os.WriteFile(p, []byte("a-long-enough-secret-value\n"), 0o644)
	os.Chmod(p, 0o644)
	if _, err := LoadSecret(p); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Errorf("a readable secret should be tightened to 0600: %v", info.Mode())
	}
	os.Chmod(dir, 0o777)
	defer os.Chmod(dir, 0o700)
	if _, err := LoadSecret(p); err == nil {
		t.Errorf("a secret in a world-writable directory must be refused")
	}
}

func TestAgentTypedFieldKeepsTicket(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "board", "tickets")
	os.MkdirAll(dir, 0o755)
	data, _ := os.ReadFile("testdata/board/tickets/T0001.json")
	s := strings.Replace(string(data), `"runtime_behaviour": "yes"`, `"runtime_behaviour": false`, -1)
	s = strings.Replace(s, `"title": "Fix the widget finder"`, `"title": 7`, 1)
	os.WriteFile(filepath.Join(dir, "T0001.json"), []byte(s), 0o644)
	ts := NewBoard(root).Load()
	if len(ts) != 1 || ts[0].ID != "T0001" || AskFor(ts[0]) == nil || ts[0].Gate == nil || ts[0].PR == nil {
		t.Fatalf("the ticket should still load: %+v", ts)
	}
	if ts[0].Gate.RuntimeBehavior != false || ts[0].Title != "" {
		t.Errorf("runtime_behaviour %v, title %q", ts[0].Gate.RuntimeBehavior, ts[0].Title)
	}
}

func TestBoardKeepsLastGoodRead(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "board", "tickets")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "T0001.json")
	data, _ := os.ReadFile("testdata/board/tickets/T0001.json")
	os.WriteFile(p, data, 0o644)
	b := NewBoard(root)
	if len(b.Load()) != 1 {
		t.Fatal("first read")
	}
	os.WriteFile(p, []byte(`{"id": "T0001", "trunc`), 0o644)
	if ts := b.Load(); len(ts) != 1 || ts[0].Title == "" {
		t.Errorf("a half-written file should keep the last good read: %+v", ts)
	}
}

func TestFloorFutureAndPastTense(t *testing.T) {
	ts := fixtures(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := BuildFloor(ts, time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC), now)
	if f.Title != "Week of 7 January" || f.Next != "" {
		t.Errorf("a future week: %q next %q", f.Title, f.Next)
	}
	f = BuildFloor(ts, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), now)
	waiting := 0
	for _, d := range f.Days {
		for _, e := range d.Events {
			for _, x := range append(append([]string{}, e.Earlier...), e.Detail) {
				if e.T.ID == "T0001" && strings.Contains(x, "waits for you") {
					t.Errorf("T0001 is past its start ask: %q", x)
				}
			}
			if e.T.ID == "T0002" && e.Label == "Accepted" {
				waiting++
				if !strings.Contains(e.Detail, "waits for you") {
					t.Errorf("T0002 still waits: %q", e.Detail)
				}
			}
		}
	}
	if waiting != 1 {
		t.Errorf("T0002's accepted line: %d", waiting)
	}
	t1 := find(ts, "T0001")
	for i, h := range t1.History {
		if strings.Contains(h.Detail, "triage accepted") {
			if _, _, d := floorOutcome(t1, i); d != "triage accepted it · you started it" {
				t.Errorf("past start ask: %q", d)
			}
		}
	}
}

func TestLabelsAndAnswers(t *testing.T) {
	ts := fixtures(t)
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/t/T0005", nil, c).Body.String()
	if !strings.Contains(body, `Proposals: <a class="mono" href="/t/T0006">T0006</a>`) {
		t.Errorf("a prospect links its proposals")
	}
	if l := ticketColumnLabel(&Ticket{Column: "rejected", Kind: "proposal"}); l != "Dropped" {
		t.Errorf("dropped proposal label %q", l)
	}
	if columnLabel("rejected") != "Closed" {
		t.Errorf("rejected reads %q", columnLabel("rejected"))
	}
	t2 := find(ts, "T0002")
	if as := Answers(t2, AskFor(t2)); as[1].Sub != "the issue stays open on GitHub" {
		t.Errorf("not now on an issue: %+v", as[1])
	}
	t1 := find(ts, "T0001")
	as := Answers(t1, AskFor(t1))
	if last := as[len(as)-1]; last.Label != "Close it on GitHub" || last.Href == "" {
		t.Errorf("review answers %+v", as)
	}
	body = h.do("GET", "/t/T0001", nil, c).Body.String()
	// answers first; the composer below says which answer uses the note, and the button is marked for the script
	if !strings.Contains(body, `value="send_back" data-needs-note`) || !strings.Contains(body, "“Send back with a note” sends this note.") {
		t.Errorf("send back should be tied to the note")
	}
	if !strings.Contains(body, `class="btn danger ext"`) {
		t.Errorf("closing the PR should read as destructive")
	}
}

// A real browser's own form post: under the old no-referrer policy it carried "Origin: null".
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
		if got := sameOrigin(r); got != c.want {
			t.Errorf("Sec-Fetch-Site %q, Origin %q: sameOrigin = %v, want %v", c.site, c.origin, got, c.want)
		}
	}
}
