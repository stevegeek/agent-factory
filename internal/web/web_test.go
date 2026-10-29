package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Foreman's TestAuth, through the whole handler.
func TestAuth(t *testing.T) {
	h := newHarness(t, false)
	w := h.do("GET", "/", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2F" {
		t.Fatalf("no cookie: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := h.do("GET", "/healthz", nil); w.Code != 200 || w.Body.String() != healthzHead {
		t.Errorf("healthz needs no login: %d %q", w.Code, w.Body.String())
	}
	if w := h.do("GET", "/static/foreman.css", nil); w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("static needs no login: %d", w.Code)
	}
	if w := h.do("POST", "/login", url.Values{"password": {"wrong"}}); w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 ||
		!strings.Contains(w.Body.String(), "That is not the secret.") {
		t.Errorf("bad secret: %d", w.Code)
	}
	c := h.login()
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || c.MaxAge != 30*24*3600 {
		t.Errorf("cookie flags %+v", c)
	}
	w = h.do("GET", "/", nil, c)
	if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Frame-Options") != "DENY" ||
		w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "same-origin" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("desk with cookie: %d %v", w.Code, w.Header())
	}
	forged := *c
	forged.Value = strings.Replace(c.Value, c.Value[:4], "9999", 1)
	if w := h.do("GET", "/", nil, &forged); w.Code != http.StatusSeeOther {
		t.Errorf("a changed cookie must not pass: %d", w.Code)
	}
	if w := h.do("POST", "/t/T0002/act", url.Values{"answer": {"start"}}, c); w.Code != http.StatusForbidden {
		t.Errorf("CSRF missing: %d", w.Code)
	}
	if w := h.do("POST", "/t/T0002/act", url.Values{"answer": {"start"}, "csrf": {"nope"}}, c); w.Code != http.StatusForbidden {
		t.Errorf("CSRF wrong: %d", w.Code)
	}
	if w := h.do("POST", "/t/T0002/act", url.Values{"answer": {"start"}}); w.Code != http.StatusForbidden {
		t.Errorf("no session: %d", w.Code)
	}
	if got := h.ops.takeAnswers(); len(got) != 0 {
		t.Errorf("nothing may run without a valid session and token: %v", got)
	}
	// over TLS the cookie is Secure
	w = h.doWith("POST", "/login", url.Values{"password": {testSecret}}, map[string]string{"X-Forwarded-Proto": "https"})
	if cs := w.Result().Cookies(); len(cs) == 0 || !cs[0].Secure {
		t.Errorf("cookie over https should be Secure")
	}
	// a cross-site login is refused
	if w := h.doWith("POST", "/login", url.Values{"password": {testSecret}}, map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Errorf("cross-site login: %d", w.Code)
	}
	// the login page sends a logged-in visitor on
	if w := h.do("GET", "/login?next=%2Ffloor", nil, c); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/floor" {
		t.Errorf("login page when logged in: %d %q", w.Code, w.Header().Get("Location"))
	}
	// logging out ends the person's sessions
	if w := h.do("POST", "/logout", url.Values{"csrf": {h.csrf(c)}}, c); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := h.do("GET", "/", nil, c); w.Code != http.StatusSeeOther {
		t.Errorf("a session from before the logout passed: %d", w.Code)
	}
}

// Foreman's TestLoginRateLimit.
func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t, false)
	for i := 0; i < auth.FailLimit; i++ {
		h.do("POST", "/login", url.Values{"password": {"wrong"}})
	}
	w := h.do("POST", "/login", url.Values{"password": {testSecret}})
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "Too many wrong tries") {
		t.Errorf("after %d failures even the right secret waits: %d", auth.FailLimit, w.Code)
	}
}

// Foreman's TestNoGlobalLockout.
func TestNoGlobalLockout(t *testing.T) {
	h := newHarness(t, false)
	post := func(remote, xff, secret string) int {
		r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"password": {secret}}.Encode()))
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
	for i := 0; i < auth.FailLimit; i++ {
		post("127.0.0.1:5555", "100.64.0.9", "wrong")
	}
	if code := post("127.0.0.1:5555", "100.64.0.9", testSecret); code != http.StatusTooManyRequests {
		t.Errorf("the guessing client waits: %d", code)
	}
	if code := post("127.0.0.1:5555", "100.64.0.10", testSecret); code != http.StatusSeeOther {
		t.Errorf("another tailnet client still gets in: %d", code)
	}
}

// Foreman's TestTailscaleHeader and TestTailscaleOnlyNamedUsers.
func TestTailscaleHeader(t *testing.T) {
	for _, trust := range []bool{false, true} {
		h := newHarness(t, trust)
		for login, ok := range map[string]bool{"someone@example.com": true, "SOMEONE@example.com": true, "other@example.com": false, "": false} {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = "127.0.0.1:5555"
			r.Header.Set("Tailscale-User-Login", login)
			w := httptest.NewRecorder()
			h.h.ServeHTTP(w, r)
			if (w.Code == 200) != (trust && ok) {
				t.Errorf("trust=%v %q: %d", trust, login, w.Code)
			}
			r.RemoteAddr = "192.0.2.4:5555"
			w = httptest.NewRecorder()
			h.h.ServeHTTP(w, r)
			if w.Code == 200 {
				t.Errorf("trust=%v: the header from a non-loopback address must not count", trust)
			}
		}
	}
}

// Foreman's TestSafeNext, the HTTP half.
func TestSafeNextOnLogin(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	if w := h.do("GET", "/login?next=%2F%09%2Fevil.example", nil, c); w.Header().Get("Location") != "/" {
		t.Errorf("a tab in next must not leave the site: %q", w.Header().Get("Location"))
	}
	w := h.do("POST", "/login", url.Values{"password": {testSecret}, "next": {"/\t/evil.example"}})
	if w.Header().Get("Location") != "/" {
		t.Errorf("login with a tab in next: %q", w.Header().Get("Location"))
	}
	if page := h.do("GET", "/login?next=//evil.example", nil).Body.String(); !strings.Contains(page, `name="next" value="/"`) {
		t.Errorf("the login form carries an unsafe next")
	}
}

// Foreman's TestHostAllowlist, on every kind of route.
func TestHostAllowlist(t *testing.T) {
	h := newHarness(t, false)
	for host, ok := range map[string]bool{"127.0.0.1:8790": true, "[::1]:8790": true, "localhost:8790": true,
		"192.0.2.7": true, "example.com": true, "EXAMPLE.com.": true, "rebound.example:8790": false} {
		for _, path := range []string{"/healthz", "/api/v1/me", "/mcp", "/", "/static/foreman.css"} {
			r := httptest.NewRequest("GET", path, nil)
			r.Host = host
			w := httptest.NewRecorder()
			h.h.ServeHTTP(w, r)
			if (w.Code != http.StatusMisdirectedRequest) != ok {
				t.Errorf("Host %q %s: %d", host, path, w.Code)
			}
		}
	}
}

// The token APIs are mounted as given, never behind the login, and the
// login session never opens them (they check their own bearer tokens).
func TestTheAPIsAreMountedWithoutTheLogin(t *testing.T) {
	h := newHarness(t, false)
	if w := h.do("GET", "/api/v1/me", nil); w.Code != 200 || w.Body.String() != "api" || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("api: %d %q", w.Code, w.Body.String())
	}
	if w := h.do("POST", "/mcp", nil); w.Code != 200 || w.Body.String() != "mcp" {
		t.Errorf("mcp: %d %q", w.Code, w.Body.String())
	}
}

// Foreman's TestActions: what reaches the answer service. What each answer
// does to the board is ops's (ops/answer_test.go); here web must pass the
// answer, the note as typed, the picks and the ask the page showed, and
// report the outcome.
func TestActions(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	csrf := h.csrf(c)
	ts := h.fixtures()
	cases := []struct {
		id, answer, note string
		picks            []string
		want             []int
	}{
		{"T0001", "send_back", "split it", nil, nil},
		{"T0002", "start", "  mind the cache  ", nil, nil},
		{"T0002", "not_now", "", nil, nil},
		{"T0003", "reply", "is it real?", nil, nil},
		{"T0004", "approve", "", []string{"1", "3"}, []int{1, 3}},
		{"T0004", "redirect", "-v smaller", nil, nil},
		{"T0006", "file", "", nil, nil},
		{"T0007", "retry_note", "rebase first", nil, nil},
		{"T0012", "retry", "", nil, nil},
		{"T0008", "done", "", nil, nil},
		{"T0009", "answer", "-yes, keep it", nil, nil},
		{"T0009", "note", "plain note", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.id+"/"+tc.answer, func(t *testing.T) {
			form := url.Values{"csrf": {csrf}, "answer": {tc.answer}, "note": {tc.note}, "pick": tc.picks}
			want := AnswerRequest{ID: ticket.ID(tc.id), Value: tc.answer, Note: tc.note, Picks: tc.want}
			if a := present.AskFor(find(ts, tc.id)); a != nil {
				form.Set("ask", a.Kind)
				form.Set("since", a.Since)
				want.Kind, want.Since = a.Kind, a.Since
			}
			w := h.do("POST", "/t/"+tc.id+"/act", form, c)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if got := h.ops.takeAnswers(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Errorf("request\n got %+v\nwant %+v", got, want)
			}
			if f := flashOf(t, h, w); f == nil || f.Kind != "ok" || !strings.HasPrefix(f.Text, tc.id+": ") {
				t.Errorf("flash %+v", f)
			}
		})
	}

	// the flash names the answer and adds what the service said
	a := present.AskFor(find(ts, "T0002"))
	h.ops.out = "Moved to   Ready."
	w := h.do("POST", "/t/T0002/act", url.Values{"csrf": {csrf}, "answer": {"start"}, "ask": {a.Kind}, "since": {a.Since}}, c)
	if f := flashOf(t, h, w); f == nil || f.Text != "T0002: Start. Moved to Ready." {
		t.Errorf("success flash %+v", f)
	}
	h.ops.out = ""
	h.ops.takeAnswers()

	// a stale form (the ask moved on) reaches nothing
	form := url.Values{"csrf": {csrf}, "answer": {"start"}, "ask": {"start"}, "since": {"2020-01-01T00:00:00+00:00"}}
	w = h.do("POST", "/t/T0002/act", form, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || !strings.Contains(f.Text, "changed since you opened it") || len(h.ops.takeAnswers()) != 0 {
		t.Errorf("stale ask: %+v", f)
	}
	// a note needs no ask check
	w = h.do("POST", "/t/T0002/act", url.Values{"csrf": {csrf}, "answer": {"note"}, "note": {"x"}}, c)
	if got := h.ops.takeAnswers(); len(got) != 1 || got[0].Kind != a.Kind {
		t.Errorf("a note without the ask fields: %+v", got)
	}
	// a bad pick reaches nothing
	w = h.do("POST", "/t/T0004/act", url.Values{"csrf": {csrf}, "answer": {"approve"}, "pick": {"x"}, "ask": {"design"},
		"since": {present.AskFor(find(ts, "T0004")).Since}}, c)
	if f := flashOf(t, h, w); f == nil || f.Text != "bad pick" || len(h.ops.takeAnswers()) != 0 {
		t.Errorf("bad pick: %+v", f)
	}
	// the service's refusal reaches the flash, and the page stays on its ticket
	h.ops.fail = errors.New("boom: that went wrong")
	w = h.do("POST", "/t/T0002/act", url.Values{"csrf": {csrf}, "answer": {"start"}, "ask": {a.Kind}, "since": {a.Since}}, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || f.Text != "T0002: Boom: that went wrong." {
		t.Errorf("failure flash %+v", f)
	}
	if loc := w.Header().Get("Location"); loc != "/t/T0002" {
		t.Errorf("a failed answer stays on its ticket: %q", loc)
	}
	// the service's own stale refusal reads like web's
	h.ops.fail = fmt.Errorf("refused: %w", ErrStale)
	w = h.do("POST", "/t/T0002/act", url.Values{"csrf": {csrf}, "answer": {"start"}, "ask": {a.Kind}, "since": {a.Since}}, c)
	if f := flashOf(t, h, w); f == nil || f.Text != "T0002 changed since you opened it. Look again before answering." {
		t.Errorf("stale from the service: %+v", f)
	}
	h.ops.fail = nil
	// a filter is kept on the way back, and an unknown one dropped
	w = h.do("POST", "/t/T0002/act", url.Values{"csrf": {csrf}, "answer": {"start"}, "ask": {"start"}, "since": {"x"}, "filter": {"start"}}, c)
	if loc := w.Header().Get("Location"); loc != "/t/T0002?kind=start" {
		t.Errorf("filter kept: %q", loc)
	}
	w = h.do("POST", "/t/T0002/act", url.Values{"csrf": {csrf}, "answer": {"start"}, "ask": {"start"}, "since": {"x"}, "filter": {"bogus"}}, c)
	if loc := w.Header().Get("Location"); loc != "/t/T0002" {
		t.Errorf("unknown filter: %q", loc)
	}
}

// an answer from Foreman names its user: the main secret's names nobody
// (the service records the first of [board] people), a user their own
// name; the rest of the request is the same.
func TestAnAnswerNamesWhoGaveIt(t *testing.T) {
	h := newHarness(t, false)
	ts := h.fixtures()
	a := present.AskFor(find(ts, "T0002"))
	var got []AnswerRequest
	answer := func(c *http.Cookie) {
		w := h.do("POST", "/t/T0002/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"start"}, "note": {"go"}, "ask": {a.Kind}, "since": {a.Since}}, c)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%d", w.Code)
		}
		got = append(got, h.ops.takeAnswers()...)
	}
	answer(h.login())
	answer(h.person("robin", "widgets"))
	if len(got) != 2 || got[0].By != "" || got[1].By != "robin" {
		t.Fatalf("who answered: %+v", got)
	}
	got[1].By = ""
	if !reflect.DeepEqual(got[0], got[1]) {
		t.Errorf("the main secret's and robin's answers differ beyond who: %+v", got)
	}
}

// Foreman's TestIDValidation.
func TestIDValidation(t *testing.T) {
	for id, ok := range map[string]bool{"T0001": true, "T123456": true, "T001": false, "T1234567": false,
		"t0001": false, "T0001.json": false, "../T0001": false, "T00a1": false, "": false, "T０００１": false} {
		if validID(id) != ok {
			t.Errorf("validID(%q) = %v", id, !ok)
		}
	}
	h := newHarness(t, false)
	c := h.login()
	for _, p := range []string{"/t/T001", "/t/t0001", "/t/T0001x", "/t/..%2fconfig.toml", "/t/T9999", "/b/nowhere", "/t/T9999/talk", "/t/T01/talk"} {
		if w := h.do("GET", p, nil, c); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := h.do("POST", "/t/T01/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"start"}}, c); w.Code != http.StatusNotFound {
		t.Errorf("bad id on act: %d", w.Code)
	}
	if w := h.do("POST", "/t/T9999/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"start"}}, c); w.Code != http.StatusNotFound {
		t.Errorf("missing ticket on act: %d", w.Code)
	}
	if got := h.ops.takeAnswers(); len(got) != 0 {
		t.Errorf("something ran for a bad id: %v", got)
	}
}

// Foreman's TestPagesRender.
func TestPagesRender(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	for _, p := range []string{"/", "/?kind=unstick", "/?kind=bogus", "/floor", "/floor?week=2026-10-05", "/boards", "/b/widgets", "/b/factory", "/settings", "/system", "/logs", "/floor/metrics", "/new", "/live"} {
		if w := h.do("GET", p, nil, c); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	for _, tk := range h.fixtures() {
		for _, p := range []string{"/t/" + string(tk.ID), "/t/" + string(tk.ID) + "/talk"} {
			w := h.do("GET", p, nil, c)
			if w.Code != 200 {
				t.Errorf("%s: %d", p, w.Code)
			}
			if strings.Contains(w.Body.String(), "javascript:") {
				t.Errorf("%s: a javascript: link was rendered", p)
			}
		}
	}
	body := h.do("GET", "/t/T0001", nil, c).Body.String()
	for _, want := range []string{"Review and merge PR #23?", `href="https://github.com/example/widgets/pull/23"`, "How it got here", `name="csrf"`} {
		if !strings.Contains(body, want) {
			t.Errorf("T0001 page lacks %q", want)
		}
	}
	if w := h.do("GET", "/floor?week=yesterday", nil, c); w.Code != http.StatusBadRequest {
		t.Errorf("bad week: %d", w.Code)
	}
	// htmx requests learn the URL to push
	if w := h.doWith("GET", "/floor", nil, map[string]string{"HX-Request": "true"}, c); w.Header().Get("HX-Push-Url") != "/floor" {
		t.Errorf("HX-Push-Url %q", w.Header().Get("HX-Push-Url"))
	}
	// a closed ticket is on the plain page, under Boards
	if body := h.do("GET", "/t/T0010", nil, c).Body.String(); !strings.Contains(body, `class="detail solo"`) || !strings.Contains(body, `aria-current="page">Boards`) {
		t.Errorf("a closed ticket's page")
	}
}

// Foreman's TestLivePollAnswersOnlyWhenTheBoardChanged.
func TestLivePollAnswersOnlyWhenTheBoardChanged(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	page := h.do("GET", "/t/T0001", nil, c).Body.String()
	v := version(h.fixtures())
	if !strings.Contains(page, `data-v="`+v+`"`) || !strings.Contains(page, "foreman.js") {
		t.Fatalf("page lacks the live version or script")
	}
	poll := func(c *http.Cookie, version string) *httptest.ResponseRecorder {
		return h.doWith("GET", "/t/T0001", nil, map[string]string{liveHeader: version}, c)
	}
	if w := poll(c, v); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Errorf("unchanged board: %d %q", w.Code, w.Body.String())
	}
	if w := poll(c, "stale"); w.Code != 200 || !strings.Contains(w.Body.String(), `id="tk-head"`) {
		t.Errorf("changed board: %d", w.Code)
	}
	h.change("T0001", func(tk *ticket.Ticket) { tk.Notes = append(tk.Notes, ticket.Note{Text: "x"}) })
	if w := poll(c, v); w.Code != 200 {
		t.Errorf("a new note should change the version: %d", w.Code)
	}
}

// A poll from a person gets its 204 while nothing changed, and a
// change anywhere is news: everyone sees every board.
func TestLivePollIsPerViewer(t *testing.T) {
	h := newHarness(t, false)
	c := h.person("robin", "factory")
	page := h.do("GET", "/", nil, c).Body.String()
	i := strings.Index(page, `data-v="`)
	if i < 0 {
		t.Fatal("no version on robin's page")
	}
	v := page[i+8 : i+8+16]
	if w := h.doWith("GET", "/", nil, map[string]string{liveHeader: v}, c); w.Code != http.StatusNoContent {
		t.Errorf("robin's unchanged desk: %d", w.Code)
	}
	h.change("T0001", func(tk *ticket.Ticket) { tk.Notes = append(tk.Notes, ticket.Note{Text: "x"}) })
	if w := h.doWith("GET", "/", nil, map[string]string{liveHeader: v}, c); w.Code != http.StatusOK {
		t.Errorf("a change to another project is no news to robin: %d", w.Code)
	}
}

// The flash survives a poll: only the next real page takes it.
func TestAPollDoesNotTakeTheFlash(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	w := h.do("POST", "/t/T0009/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"note"}, "note": {"x"}}, c)
	var flash *http.Cookie
	for _, k := range w.Result().Cookies() {
		if k.Name == flashCookie {
			flash = k
		}
	}
	if flash == nil {
		t.Fatal("no flash")
	}
	poll := h.doWith("GET", "/floor", nil, map[string]string{liveHeader: "stale"}, c, flash)
	if strings.Contains(poll.Body.String(), `class="toast`) || len(poll.Result().Cookies()) != 0 {
		t.Errorf("a poll took the flash")
	}
	page := h.do("GET", "/floor", nil, c, flash)
	if !strings.Contains(page.Body.String(), `<div class="toast ok" role="status">T0009: Note saved`) {
		t.Errorf("the page lacks the flash")
	}
	// a forged flash is ignored
	bad := *flash
	bad.Value = strings.Replace(flash.Value, ".", "x.", 1)
	if strings.Contains(h.do("GET", "/floor", nil, c, &bad).Body.String(), `class="toast`) {
		t.Errorf("a forged flash was shown")
	}
}

// Foreman's TestNoteButtonAndDraftReasons, the page half.
func TestNoteButtonAndDraftReasons(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/t/T0001", nil, c).Body.String()
	for _, want := range []string{`value="note" data-needs-note data-busy="Saving…"><span>Save note</span>`,
		`value="ask" data-needs-note data-busy="Asking…"><span>Ask the agent</span>`,
		"Ask the agent: one reads the change and answers on your desk in a few minutes; nothing changes.",
		"Save note: kept for the next agent that works on it; nothing happens now.",
		"approve it there and the factory merges it", "a worker reworks it; the PR is a draft meanwhile",
		"Why this PR is a draft", "<li>1 file outside the paths that may merge automatically (docs, tests, config)</li>",
		"<li>40 changed lines; the limit for automatic approval is 10</li>", `<b>Waiting for you</b>`, `data-draft="T0001"`} {
		if !strings.Contains(body, want) {
			t.Errorf("T0001 page lacks %q", want)
		}
	}
}

func TestQuestionAnswerSitsUnderItsField(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/t/T0009", nil, c).Body.String()
	field := strings.Index(body, `<textarea id="note-T0009"`)
	send := strings.Index(body, `<span>Send the answer</span>`)
	more := strings.Index(body, `<summary>Not an answer? Ask an agent, or save a note</summary>`)
	ask := strings.Index(body, `<span>Ask the agent</span>`)
	note := strings.Index(body, `<span>Save note</span>`)
	if field < 0 || send < 0 || more < 0 || ask < 0 || note < 0 {
		t.Fatalf("T0009 page lacks the answer form: field %d, send %d, more %d, ask %d, note %d", field, send, more, ask, note)
	}
	if !(field < send && send < more && more < ask && more < note) {
		t.Errorf("want the field, then Send the answer, then the other actions folded away: %d %d %d %d %d", field, send, more, ask, note)
	}
	if strings.Count(body, "Send the answer") != 1 || strings.Contains(body, `<div class="answers">`) {
		t.Errorf("the answer is offered outside its field")
	}
}

// Foreman's TestLabelsAndAnswers, the page half.
func TestLabelsAndAnswersOnThePage(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/t/T0005", nil, c).Body.String()
	if !strings.Contains(body, `Proposals: <a class="mono" href="/t/T0006">T0006</a>`) {
		t.Errorf("a prospect links its proposals")
	}
	body = h.do("GET", "/t/T0001", nil, c).Body.String()
	if !strings.Contains(body, `value="send_back" data-needs-note`) || !strings.Contains(body, "“Send back with a note” sends this note.") {
		t.Errorf("send back should be tied to the note")
	}
	if !strings.Contains(body, `class="btn danger ext"`) {
		t.Errorf("closing the PR should read as destructive")
	}
}

// An answer link from the service is only ever http(s).
func TestAnswerLinksAreSafe(t *testing.T) {
	h := newHarness(t, false)
	h.srv.svc.Answers = &linkOps{}
	v := h.srv.ticketView(find(h.fixtures(), "T0001"), h.fixtures(), "")
	if len(v.Answers) != 1 || v.Answers[0].Href != "" {
		t.Errorf("an unsafe link survived: %+v", v.Answers)
	}
}

type linkOps struct{ fakeOps }

func (*linkOps) Answers(t *ticket.Ticket, a *ticket.Ask) []Answer {
	return []Answer{{Label: "x", Href: "javascript:alert(1)"}}
}

// Foreman's TestDeskRowsSayWhatItIsAndWhatItWants, the page half.
func TestDeskRowsSayWhatItIsAndWhatItWants(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/", nil, c).Body.String()
	ts := h.fixtures()
	it := present.Desk(ts, present.Viewer{By: "maintainer", Maintainer: true})[0]
	for _, want := range []string{`<span class="proj">` + it.T.Project + `</span>`, `<span class="qdo">` + present.Action(it.T, it.Ask) + `</span>`,
		`aria-current="true"`, `id="tk-head"`} {
		if !strings.Contains(body, want) {
			t.Errorf("desk lacks %q", want)
		}
	}
}

// An answer on the desk goes back to the desk (the maintainer's choice,
// 2026-10-20), not on to the next ask; a kind filter is kept.
func TestAnAnswerOnTheDeskGoesBackToTheDesk(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	first := string(present.Desk(h.fixtures(), present.Viewer{By: "maintainer", Maintainer: true})[0].T.ID)
	w := h.do("POST", "/t/"+first+"/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"note"}, "note": {"hmm"}}, c)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if to := w.Header().Get("Location"); to != "/" {
		t.Errorf("after a note the desk should come back, went to %q", to)
	}
	if got := h.ops.takeAnswers(); len(got) != 1 || got[0].Value != "note" {
		t.Errorf("ran %v", got)
	}
	// from a filtered desk, back to that filter
	kind := present.Desk(h.fixtures(), present.Viewer{By: "maintainer", Maintainer: true})[0].Ask.Kind
	w = h.do("POST", "/t/"+first+"/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"note"}, "note": {"hmm"}, "filter": {kind}}, c)
	if to := w.Header().Get("Location"); to != "/?kind="+kind {
		t.Errorf("after a note on a filtered desk, went to %q", to)
	}
	h.ops.takeAnswers()
	// a ticket with no ask comes back to itself
	w = h.do("POST", "/t/T0010/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"note"}, "note": {"hmm"}}, c)
	if to := w.Header().Get("Location"); to != "/t/T0010" {
		t.Errorf("a note on a ticket with no ask went to %q", to)
	}
}

// Foreman's TestAnAnsweredQuestionBringsItsTicketBackWithTheAnswer, the
// ticket view half.
func TestTheLatestAnswerLeadsThePage(t *testing.T) {
	h := newHarness(t, false)
	ts := h.fixtures()
	it := present.Desk(ts, present.Viewer{By: "maintainer", Maintainer: true})[0]
	p := ticket.Clone(it.T)
	since := present.ParseTime(it.Ask.Since)
	at := func(d time.Duration) string { return since.Add(d).UTC().Format(time.RFC3339) }
	child, _, err := ticket.Decode([]byte(`{"id": "T0999", "kind": "task", "parent": "` + string(p.ID) + `", "column": "done", "project": "` + p.Project +
		`", "result": "Because **lint** says so.\n\nMore detail here."}`))
	if err != nil {
		t.Fatal(err)
	}
	p.History = append(p.History, ticket.Entry{At: at(time.Hour), Event: "asked", Detail: "T0999: why the nil check?"},
		ticket.Entry{At: at(2 * time.Hour), Event: "answered", Detail: "T0999: Because lint says so."})
	all := []*ticket.Ticket{p, child}
	v := h.srv.ticketView(p, all, "")
	if v.Answer == nil || v.Answer.ID != "T0999" || v.Answer.Question != "why the nil check?" || !strings.Contains(v.Answer.Lead, "lint") ||
		v.Answer.Full != "Because **lint** says so.\n\nMore detail here." {
		t.Errorf("answer: %+v", v.Answer)
	}
	// a question from before the current ask is old news, kept in the story only
	p.History = []ticket.Entry{{At: since.Add(-time.Hour).UTC().Format(time.RFC3339), Event: "asked", Detail: "T0999: old"}}
	if latestAnswer(p, it.Ask, present.ByID(all)) != nil {
		t.Errorf("an answer about an earlier decision should not lead the page")
	}
}

// Foreman's TestFileANewTicketFromForeman.
func TestFileANewTicketFromForeman(t *testing.T) {
	h := newHarness(t, false)
	h.srv.projects = []string{"widgets", "gadgets"}
	c := h.login()
	csrf := h.csrf(c)
	page := h.do("GET", "/new", nil, c).Body.String()
	for _, want := range []string{`<option value="widgets" selected>`, `<option value="gadgets"`, `value="factory"`, `name="kind" value="intent"`,
		`data-title="Cut a release"`, `value="manage"`} {
		if !strings.Contains(page, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	if strings.Contains(page, "prospect") {
		t.Errorf("a prospecting recipe is offered on ＋")
	}
	post := func(project, kind, title, body string) *httptest.ResponseRecorder {
		return h.do("POST", "/new", url.Values{"csrf": {csrf}, "project": {project}, "kind": {kind}, "title": {title}, "body": {body}}, c)
	}
	for _, k := range []NewTicket{
		{"widgets", "change", "Release 1.2.0", "Bump **version**", ""},
		{"gadgets", "intent", "-weird title", "", ""},
		{"factory", "task", "What is new?", "-dash", ""},
		{"factory", "manage", "A morning check", "", ""},
	} {
		w := post(k.Project, k.Kind, "  "+k.Title+" ", k.Body+"\n")
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/t/T0099" {
			t.Errorf("%s: %d %s", k.Title, w.Code, w.Header().Get("Location"))
		}
		if got := h.ops.takeFiled(); len(got) != 1 || got[0] != k {
			t.Errorf("%s: filed %+v", k.Title, got)
		}
		if f := flashOf(t, h, w); f == nil || !strings.HasPrefix(f.Text, "T0099: filed. ") {
			t.Errorf("flash %+v", f)
		}
	}
	for _, bad := range [][4]string{{"nowhere", "change", "x", ""}, {"factory", "change", "x", ""}, {"widgets", "change", "", ""},
		{"widgets", "change", "two\nlines", ""}, {"widgets", "delete", "x", ""}, {"widgets", "manage", "x", ""},
		{"widgets", "change", strings.Repeat("x", 201), ""}, {"widgets", "change", "x", strings.Repeat("y", 8001)}, {"widgets", "prospect", "x", ""}} {
		if w := post(bad[0], bad[1], bad[2], bad[3]); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `class="error"`) {
			t.Errorf("%v: %d", bad, w.Code)
		}
		if got := h.ops.takeFiled(); len(got) != 0 {
			t.Errorf("%v filed %v", bad, got)
		}
	}
	// the service's refusal comes back on the form
	h.ops.fail = errors.New("no such project")
	if w := post("widgets", "change", "x", ""); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "No such project.") {
		t.Errorf("refused: %d", w.Code)
	}
}

// Another person sees and does everything; only their desk is theirs.
func TestAnotherPersonSeesEverything(t *testing.T) {
	h := newHarness(t, false)
	c := h.person("robin", "factory")
	desk := h.do("GET", "/", nil, c).Body.String()
	if !strings.Contains(desk, `<span class="menu-who">robin</span>`) || !strings.Contains(desk, `href="/system"`) {
		t.Errorf("robin's menu lacks her name or System status")
	}
	for _, path := range []string{"/t/T0001", "/t/T0001/talk", "/b/widgets", "/live", "/logs"} {
		if w := h.do("GET", path, nil, c); w.Code != http.StatusOK {
			t.Errorf("%s: %d, want 200", path, w.Code)
		}
	}
	if live := h.do("GET", "/live", nil, c).Body.String(); !strings.Contains(live, `href="#T0011"`) {
		t.Errorf("robin's Live lacks the screens")
	}
	if w := h.do("POST", "/t/T0001/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"note"}, "note": {"hi"}}, c); w.Code != http.StatusSeeOther {
		t.Errorf("a note on another project's ticket: %d", w.Code)
	}
	if got := h.ops.takeAnswers(); len(got) != 1 || got[0].By != "robin" {
		t.Errorf("robin's note: %+v", got)
	}
	newPage := h.do("GET", "/new", nil, c).Body.String()
	if !strings.Contains(newPage, `value="manage"`) || !strings.Contains(newPage, ">widgets<") {
		t.Errorf("the new-ticket page does not offer robin the manager or another project")
	}
	w := h.do("POST", "/new", url.Values{"csrf": {h.csrf(c)}, "project": {"factory"}, "kind": {"manage"}, "title": {"x"}}, c)
	if w.Code != http.StatusSeeOther || len(h.ops.takeFiled()) != 1 {
		t.Errorf("robin asked the manager: %d", w.Code)
	}
	// removing her ends her session
	if err := h.users.Remove("robin"); err != nil {
		t.Fatal(err)
	}
	if w := h.do("GET", "/", nil, c); w.Code != http.StatusSeeOther {
		t.Errorf("a removed user is still logged in: %d", w.Code)
	}
}

// Foreman's TestABoardTicketPageHasNoAnswerForms: a closed board ticket has nothing to answer.
func TestABoardTicketPageHasNoAnswerForms(t *testing.T) {
	h := newHarness(t, false, legacyContract)
	c := h.login()
	w := h.do("GET", "/t/T0004", nil, c)
	page := w.Body.String()
	if w.Code != 200 || !strings.Contains(page, "Date picker") || !strings.Contains(page, "Done") {
		t.Fatalf("board ticket page: %d", w.Code)
	}
	if strings.Contains(page, `name="note"`) || strings.Contains(page, `name="answer"`) {
		t.Errorf("a board ticket page offers answer forms")
	}
	if talk := h.do("GET", "/t/T0004/talk", nil, c).Body.String(); strings.Contains(talk, `id="reply"`) {
		t.Errorf("a board ticket's conversation offers a reply")
	}
	if w := h.do("GET", "/b/sandbox", nil, c); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<h3>Done <span>1") {
		t.Errorf("/b/sandbox: %d", w.Code)
	}
	if strings.Contains(h.do("GET", "/new", nil, c).Body.String(), "sandbox") {
		t.Errorf("＋ offers a board project")
	}
	if !strings.Contains(h.do("GET", "/boards", nil, c).Body.String(), `href="/b/sandbox"`) {
		t.Errorf("a board project is missing from Boards")
	}
	if w := h.do("POST", "/t/T0004/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"note"}, "note": {"x"}}, c); w.Code != http.StatusNotFound {
		t.Errorf("an answer to a board ticket: %d", w.Code)
	}
	if len(h.ops.takeAnswers()) != 0 {
		t.Errorf("a board ticket's answer reached the service")
	}
}

// Foreman's TestConversationShowsTheThreadAndAReplyStaysInIt.
func TestConversationShowsTheThreadAndAReplyStaysInIt(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/t/T0009/talk", nil, c).Body.String()
	for _, want := range []string{`class="thread"`, `value="talk"`, `name="view" value="talk"`, "Your answer", `aria-current="page">Conversation`} {
		if !strings.Contains(body, want) {
			t.Errorf("T0009 conversation lacks %q", want)
		}
	}
	if !strings.Contains(h.do("GET", "/t/T0009", nil, c).Body.String(), `href="/t/T0009/talk"`) {
		t.Errorf("the ticket page should link its conversation")
	}
	w := h.do("POST", "/t/T0009/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"talk"}, "view": {"talk"}, "note": {"Yes, keep the alias"}}, c)
	if w.Code != 303 || w.Header().Get("Location") != "/t/T0009/talk" {
		t.Fatalf("reply: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := h.ops.takeAnswers(); len(got) != 1 || got[0].Value != "talk" || got[0].Note != "Yes, keep the alias" {
		t.Errorf("reply: %+v", got)
	}
	if f := flashOf(t, h, w); f == nil || f.Text != "T0009: Answer sent; the agent carries on with it now." {
		t.Errorf("reply flash %+v", f)
	}
}

// The conversation reads archived reports beside the run's log, and only
// from the factory's own run names.
func TestConversationReadsArchivedReports(t *testing.T) {
	h := newHarness(t, false)
	h.env.WriteFile(t, "logs/T0001/t0001-w1/report.json", `{"status": "done", "summary": "From the archive."}`)
	if got := string(h.srv.archive("T0001", "t0001-w1")); !strings.Contains(got, "From the archive.") {
		t.Errorf("archive: %q", got)
	}
	for _, bad := range [][2]string{{"T0001", "../x"}, {"../T", "t0001-w1"}, {"T0001", "t0001-w1/../../x"}} {
		if h.srv.archive(ticket.ID(bad[0]), bad[1]) != nil {
			t.Errorf("archive(%q, %q) read something", bad[0], bad[1])
		}
	}
}

// Foreman's TestManagerDraftsAreShownAndAnswered, the page half.
func TestManagerDraftsAreShown(t *testing.T) {
	h := newHarness(t, false)
	h.env.WriteFile(t, "board/tickets/T0050.json", `{"id": "T0050", "project": "factory", "kind": "manage", "title": "A morning check",
		"column": "needs_human", "history": [], "updated": "2026-10-16T10:00:00+00:00",
		"ask": {"kind": "draft", "question": "Apply the manager's 1 config change?", "why": "", "since": "2026-10-16T10:00:00+00:00"},
		"drafts": [{"title": "Daily check", "why": "Files a check every morning.", "patch": "--- a/config/config.toml\n+++ b/config/config.toml\n"}]}`)
	c := h.login()
	body := h.do("GET", "/t/T0050", nil, c).Body.String()
	for _, want := range []string{"Draft 1: Daily check", "Copy the diff", `id="patch-T0050-0"`, "I&#39;ve applied it"} {
		if !strings.Contains(body, want) {
			t.Errorf("the draft page lacks %q", want)
		}
	}
}

// A change to the factory's walls carries a badge on the desk row and in
// its detail, which lists the files.
func TestDeskShowsWallsBadge(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	id := string(present.Desk(h.fixtures(), present.Viewer{By: "maintainer", Maintainer: true})[0].T.ID)
	if body := h.do("GET", "/", nil, c).Body.String(); strings.Contains(body, "Touches the walls") {
		t.Fatal("a badge with no walls")
	}
	h.change(id, func(tk *ticket.Ticket) {
		g := tk.Gate.Or(ticket.Gate{Blockers: []string{}})
		g.Walls = []string{"internal/gate/gate.go", "deploy.sh"}
		tk.Gate = ticket.Some(g)
	})
	for _, page := range []string{"/", "/t/" + id} {
		body := h.do("GET", page, nil, c).Body.String()
		for _, want := range []string{`<span class="badge small walls"`, "Touches the walls",
			`<li><code>internal/gate/gate.go</code></li>`, `<li><code>deploy.sh</code></li>`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", page, want)
			}
		}
	}
}

// A manager ticket's items each show their tier, their state, the diff,
// the walls and the checker's places, and the PR they landed as; the
// by-hand line only on a draft still the maintainer's to apply by hand.
func TestTicketPageShowsTierAndState(t *testing.T) {
	h := newHarness(t, false)
	h.env.WriteFile(t, "board/tickets/T0050.json", `{"id": "T0050", "project": "factory", "kind": "manage", "title": "Tidy",
		"column": "needs_human", "history": [], "updated": "2026-10-16T10:00:00+00:00",
		"ask": {"kind": "draft", "question": "Apply the manager's 1 change?", "why": "", "since": "2026-10-16T10:00:00+00:00"},
		"drafts": [
			{"title": "Lower the poll interval", "why": "Less noise.", "patch": "+every_days = 14\n", "tier": "auto", "state": "applied",
			 "places": ["schedules[bug-hunt].every_days"], "pr": 7, "pr_url": "https://github.com/example/config/pull/7"},
			{"title": "Gate limit", "why": "w", "patch": "+max_changed_lines = 900\n", "tier": "tap", "state": "waiting",
			 "places": ["gate.max_changed_lines"], "walls": ["gate.max_changed_lines"]}],
		"actions": [{"kind": "rebuild", "why": "Stale images.", "tier": "tap", "state": "waiting"}]}`)
	c := h.login()
	body := h.do("GET", "/t/T0050", nil, c).Body.String()
	for _, want := range []string{"Draft 1: Lower the poll interval", "applies by itself", "Applied", `href="https://github.com/example/config/pull/7"`,
		"Draft 2: Gate limit", "needs your tap", "Waits for your tap", "<code>gate.max_changed_lines</code>", `id="patch-T0050-1"`,
		"Action 3: rebuild", "Stale images."} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, not := range []string{"Apply it in the config repo on your machine"} {
		if strings.Contains(body, not) {
			t.Errorf("the page has %q", not)
		}
	}
}

// The ticket page's buttons through the real answers (ops.Answers): Apply
// it only while something waits, beside I've applied it for a by-hand
// draft, and neither while the factory is applying.
func TestTicketPageOffersTheOperatorAnswers(t *testing.T) {
	h := newHarness(t, false)
	h.srv.svc.Answers = Ops{}
	page := func(id, drafts, actions string) string {
		h.env.WriteFile(t, "board/tickets/"+id+".json", `{"id": "`+id+`", "project": "factory", "kind": "manage", "title": "Tidy",
			"column": "needs_human", "history": [], "updated": "2026-10-16T10:00:00+00:00",
			"ask": {"kind": "draft", "question": "Apply the manager's 1 change?", "why": "", "since": "2026-10-16T10:00:00+00:00"},
			"drafts": [`+drafts+`], "actions": [`+actions+`]}`)
		return h.do("GET", "/t/"+id, nil, h.login()).Body.String()
	}
	button := func(v string) string { return `name="answer" value="` + v + `"` }
	cases := []struct {
		id, drafts, actions string
		want, not           []string
	}{
		{"T0051", `{"title": "Gate", "why": "w", "patch": "+x\n", "tier": "tap", "state": "waiting"}`, ``,
			[]string{button("apply"), button("send_back"), button("drop")}, []string{button("applied")}},
		{"T0052", `{"title": "Old", "why": "w", "patch": "+x\n"}`, `{"kind": "rebuild", "why": "w", "tier": "tap", "state": "waiting"}`,
			[]string{button("applied"), button("apply"), button("drop")}, nil},
		{"T0053", `{"title": "Old", "why": "w", "patch": "+x\n"}`, ``,
			[]string{button("applied")}, []string{button("apply")}},
		{"T0054", `{"title": "Old", "why": "w", "patch": "+x\n"}`, `{"kind": "rebuild", "why": "w", "tier": "tap", "state": "requested"}`,
			[]string{button("send_back"), "Applying"}, []string{button("apply"), button("applied")}},
	}
	for _, c := range cases {
		body := page(c.id, c.drafts, c.actions)
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s lacks %q", c.id, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(body, n) {
				t.Errorf("%s has %q", c.id, n)
			}
		}
	}
}
