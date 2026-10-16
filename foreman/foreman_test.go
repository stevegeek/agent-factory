package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	time.Local = time.UTC
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func fixtures(t *testing.T) []*Ticket {
	t.Helper()
	ts := NewBoard("testdata").Load()
	if len(ts) != 12 {
		t.Fatalf("loaded %d fixture tickets", len(ts))
	}
	return ts
}

func find(ts []*Ticket, id string) *Ticket { return byID(ts)[id] }

// ---------------------------------------------------------------- desk

func TestDeskOrderAndFallbackAsks(t *testing.T) {
	ts := fixtures(t)
	var got []string
	for _, it := range Desk(ts) {
		got = append(got, it.T.ID+":"+it.Ask.Kind)
	}
	want := []string{"T0009:question", "T0012:unstick", "T0007:unstick", "T0001:review", "T0002:start",
		"T0004:design", "T0006:proposal", "T0003:decide", "T0008:notice"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desk order\n got %v\nwant %v", got, want)
	}

	cases := []struct {
		id, kind, question, why, retry string
	}{
		{"T0001", "review", "Review and merge PR #23?", "Changed the widget finder to use the primary key.", ""},
		{"T0002", "start", "Start work on issue #18, “Cache the widget coder”?", "It is slow; caching helps.", ""},
		{"T0003", "decide", "Triage could not decide on issue #15, “Check thread safety”. Start it, or drop it?", "No failing case was reported.", ""},
		{"T0004", "design", "Approve the design for “Make widgets thread safe” and its 3 tickets?", "Freeze state after construction.", ""},
		{"T0006", "proposal", "File this as a change: Proposal 1?", "why 1", ""},
		{"T0007", "unstick", "Tests or lint failed at the gate. Run the gate again?", "", "gate"},
		{"T0008", "notice", "Read the answer to “What is the latest widget release?” and close it?", "1.2.3, released last week.", ""},
		{"T0009", "question", "Keep the old option name as an alias?", "Removing it breaks callers.", ""},
		{"T0012", "unstick", "An agent ran out of time. Try again?", "", "ready"},
	}
	for _, c := range cases {
		a := AskFor(find(ts, c.id))
		if a == nil {
			t.Fatalf("%s: no ask", c.id)
		}
		if a.Kind != c.kind || a.Question != c.question || a.RetryColumn != c.retry {
			t.Errorf("%s: got %q %q retry %q", c.id, a.Kind, a.Question, a.RetryColumn)
		}
		if c.why != "" && a.Why != c.why {
			t.Errorf("%s: why %q", c.id, a.Why)
		}
	}
	if !strings.Contains(AskFor(find(ts, "T0007")).Why, "Gate: tests or lint failed") {
		t.Errorf("unstick why should carry the move detail: %q", AskFor(find(ts, "T0007")).Why)
	}
	if AskFor(find(ts, "T0007")).Since != "2026-10-08T09:35:00+00:00" {
		t.Errorf("since is the arrival move")
	}
	for _, id := range []string{"T0005", "T0010", "T0011"} {
		if AskFor(find(ts, id)) != nil {
			t.Errorf("%s should have no ask", id)
		}
	}
}

// ---------------------------------------------------------------- stories

func whos(s Story) []string {
	var out []string
	for _, st := range s.Steps {
		out = append(out, st.Who)
	}
	return out
}

func TestStoryCollapsesRunsAndNamesPeople(t *testing.T) {
	ts := fixtures(t)
	s := BuildStory(find(ts, "T0001"), byID(ts))
	want := []string{"GitHub sweep", "Triage", "Dispatcher", "You", "Worker", "Reviewer", "Security", "Gate", "Dispatcher", "Waiting for you"}
	if !reflect.DeepEqual(whos(s), want) {
		t.Fatalf("steps\n got %v\nwant %v", whos(s), want)
	}
	st := s.Steps
	if st[0].When != "5 Oct 09:00" || st[0].What != "Filed issue #17 for triage." {
		t.Errorf("first step %+v", st[0])
	}
	if st[3].What != "Moved it to Ready." || st[3].When != "6 Oct 10:00" {
		t.Errorf("a by-human move is the person's: %+v", st[3])
	}
	if st[4].When != "10:01–10:11" || st[4].What != "Changed the widget finder to use the primary key." {
		t.Errorf("start and finished should be one worker step: %+v", st[4])
	}
	if st[6].What != "Tier 2 (input reaches a sink): 2 questions, none above low." {
		t.Errorf("security step %q", st[6].What)
	}
	if st[7].What != "Gate: needs a maintainer: 40 changed lines (limit 10)." {
		t.Errorf("a rule replaces the dispatcher's own words: %q", st[7].What)
	}
	if st[8].What != "Opened draft PR #23 and asked for review." || st[9].When != "now" || st[9].Tone != "now" {
		t.Errorf("tail %+v %+v", st[8], st[9])
	}
}

func TestStoryPanelVotesFromProspect(t *testing.T) {
	ts := fixtures(t)
	s := BuildStory(find(ts, "T0006"), byID(ts))
	want := []string{"Clock", "Prospector", "Panel", "Dispatcher", "Waiting for you"}
	if !reflect.DeepEqual(whos(s), want) {
		t.Fatalf("steps %v", whos(s))
	}
	panel := s.Steps[2]
	if panel.What != "Scored 4 · 5 · 4." || len(panel.Quotes) != 3 || panel.Quotes[1].Text != "clear win" || panel.When != "20:10–20:24" {
		t.Errorf("panel step %+v", panel)
	}
	if s.Steps[3].What != "Kept: 3 of 3 voters scored 4 or more (2 needed); mean 4.3 (3.5 needed)." {
		t.Errorf("keep rule %q", s.Steps[3].What)
	}
	if !strings.Contains(s.Caveat, "All 3 voters ran on helper") {
		t.Errorf("caveat %q", s.Caveat)
	}
	p := BuildStory(find(ts, "T0005"), byID(ts))
	if !reflect.DeepEqual(whos(p), []string{"Clock", "Prospector", "Panel", "Dispatcher"}) {
		t.Fatalf("prospect steps %v", whos(p))
	}
	if p.Steps[2].What != "3 voters scored each proposal 1–5, blind to each other." {
		t.Errorf("votes should collapse into one panel step: %q", p.Steps[2].What)
	}
}

func TestDroppedProposalsFromOlderBoards(t *testing.T) {
	ts := fixtures(t)
	d := droppedProposals(ts)
	if len(d) != 1 || d[0].Title != "Proposal 2" || d[0].Column != "rejected" || AskFor(d[0]) != nil {
		t.Fatalf("dropped %+v", d)
	}
	if got := d[0].History[0].Rule; got != "dropped: no voter scored 4 or more; mean 1.7 (3.5 needed)" {
		t.Errorf("drop rule %q", got)
	}
	s := BuildStory(d[0], byID(ts))
	last := s.Steps[len(s.Steps)-1]
	if last.Tone != "flag" || !strings.Contains(last.What, "never reached your desk") {
		t.Errorf("dropped story ends %+v", last)
	}
	// once the dispatcher files it as a ticket, it is not made up again
	filed := &Ticket{ID: "T0099", Kind: "proposal", Column: "rejected", Parent: "T0005", Proposal: &Proposal{Title: "Proposal 2"}}
	if len(droppedProposals(append(ts, filed))) != 0 {
		t.Errorf("a filed dropped proposal should not be invented again")
	}
}

// ---------------------------------------------------------------- floor

func TestFloorGroupsByWeekAndDay(t *testing.T) {
	ts := fixtures(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := BuildFloor(ts, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), now)
	if f.Title != "Week of 5 October" || f.Prev != "2026-09-28" || f.Next != "2026-10-12" {
		t.Errorf("week %q prev %q next %q", f.Title, f.Prev, f.Next)
	}
	var days []string
	for _, d := range f.Days {
		days = append(days, d.Name)
	}
	want := []string{"Friday 9 October", "Thursday 8 October", "Wednesday 7 October", "Tuesday 6 October", "Monday 5 October"}
	if !reflect.DeepEqual(days, want) {
		t.Fatalf("days\n got %v\nwant %v", days, want)
	}
	lines := map[string]FloorEvent{}
	for _, d := range f.Days {
		for i, e := range d.Events {
			lines[d.Name+" "+e.Line] = e
			if i > 0 && e.At.After(d.Events[i-1].At) {
				t.Errorf("%s: events should run newest first", d.Name)
			}
		}
	}
	if e := lines["Wednesday 7 October Proposal 2"]; e.Class != "dropped" || len(e.Story.Steps) == 0 {
		t.Errorf("dropped proposal line %+v", e)
	}
	if e := lines["Wednesday 7 October T0005 Look for improvements"]; e.Label != "1 kept" {
		t.Errorf("panel line %+v", e)
	}
	if e := lines["Tuesday 6 October T0001 Fix the widget finder"]; e.Label != "PR opened" || !reflect.DeepEqual(e.Earlier, []string{"started", "gate passed"}) {
		t.Errorf("one line per ticket per day, latest first: %+v", e)
	}
	if e := lines["Monday 5 October T0010 Ignore generated files"]; e.Class != "merged" {
		t.Errorf("merge line %+v", e)
	}
	if !strings.Contains(f.Summary, "1 PR opened, 1 merged") || !strings.Contains(f.Summary, "2 proposals, 1 dropped by the panel") {
		t.Errorf("summary %q", f.Summary)
	}
	if len(f.Running) != 1 || f.Running[0].T.ID != "T0011" || f.Running[0].Role != "Worker" {
		t.Errorf("running %+v", f.Running)
	}
	prev := BuildFloor(ts, time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), now)
	if len(prev.Days) != 1 || prev.Days[0].Events[0].Label != "PR opened" {
		t.Errorf("week before: %+v", prev.Days)
	}
	cur := BuildFloor(ts, now, now)
	if cur.Title != "This week" || cur.Next != "" || len(cur.Days) != 0 {
		t.Errorf("this week %+v", cur)
	}
}

// ---------------------------------------------------------------- HTTP: auth, CSRF, actions

const testSecret = "test-secret-0123456789"

type harness struct {
	t   *testing.T
	srv *Server
	h   http.Handler
	log string
}

func newHarness(t *testing.T, trustTS bool) *harness {
	dir := t.TempDir()
	script := filepath.Join(dir, "factory")
	logf := filepath.Join(dir, "argv.log")
	// one line per call: FACTORY_ROOT, then each argument, separated by 0x1f
	os.WriteFile(script, []byte("#!/bin/sh\n"+
		"if [ -n \"$FAKE_FAIL\" ]; then echo 'boom: that went wrong' >&2; exit 1; fi\n"+
		"{ printf '%s' \"$FACTORY_ROOT\"; for a in \"$@\"; do printf '\\037%s' \"$a\"; done; printf '\\n'; } >> \""+logf+"\"\n"), 0o755)
	a := NewAuth([]byte(testSecret))
	a.delay = 0
	a.AllowHosts("example.com") // httptest's default Host
	if trustTS {
		a.TrustTailscale("Someone@example.com")
	}
	s := NewServer(NewBoard("testdata"), a, &Runner{Factory: script, Root: "testdata"})
	s.now = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
	return &harness{t: t, srv: s, h: s.Handler(), log: logf}
}

func (h *harness) do(method, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) login() *http.Cookie {
	w := h.do("POST", "/login", url.Values{"secret": {testSecret}, "next": {"/floor"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/floor" {
		h.t.Fatalf("login: %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	h.t.Fatal("no session cookie")
	return nil
}

func (h *harness) argv() [][]string {
	data, _ := os.ReadFile(h.log)
	os.Remove(h.log)
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x1f")
		if parts[0] != "testdata" {
			h.t.Errorf("FACTORY_ROOT %q", parts[0])
		}
		out = append(out, parts[1:])
	}
	return out
}

func TestAuth(t *testing.T) {
	h := newHarness(t, false)
	w := h.do("GET", "/", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2F" {
		t.Fatalf("no cookie: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := h.do("GET", "/healthz", nil); w.Code != 200 {
		t.Errorf("healthz needs no login: %d", w.Code)
	}
	if w := h.do("GET", "/static/foreman.css", nil); w.Code != 200 {
		t.Errorf("static needs no login: %d", w.Code)
	}
	if w := h.do("POST", "/login", url.Values{"secret": {"wrong"}}); w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Errorf("bad secret: %d", w.Code)
	}
	c := h.login()
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || c.MaxAge != 30*24*3600 {
		t.Errorf("cookie flags %+v", c)
	}
	if w := h.do("GET", "/", nil, c); w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("desk with cookie: %d", w.Code)
	}
	forged := *c
	forged.Value = strings.Replace(c.Value, c.Value[:4], "9999", 1)
	if w := h.do("GET", "/", nil, &forged); w.Code != http.StatusSeeOther {
		t.Errorf("a changed cookie must not pass: %d", w.Code)
	}
	expired := NewAuth([]byte(testSecret))
	expired.now = func() time.Time { return time.Now().Add(-31 * 24 * time.Hour) }
	if w := h.do("GET", "/", nil, &http.Cookie{Name: sessionCookie, Value: expired.NewSession()}); w.Code != http.StatusSeeOther {
		t.Errorf("an expired session must not pass: %d", w.Code)
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
	if len(h.argv()) != 0 {
		t.Errorf("nothing may run without a valid session and token")
	}
	// over TLS the cookie is Secure
	r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"secret": {testSecret}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Forwarded-Proto", "https")
	rw := httptest.NewRecorder()
	h.h.ServeHTTP(rw, r)
	if cs := rw.Result().Cookies(); len(cs) == 0 || !cs[0].Secure {
		t.Errorf("cookie over https should be Secure")
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t, false)
	for i := 0; i < failLimit; i++ {
		h.do("POST", "/login", url.Values{"secret": {"wrong"}})
	}
	if w := h.do("POST", "/login", url.Values{"secret": {testSecret}}); w.Code != http.StatusTooManyRequests {
		t.Errorf("after %d failures even the right secret waits: %d", failLimit, w.Code)
	}
}

func TestTailscaleHeader(t *testing.T) {
	for _, trust := range []bool{false, true} {
		h := newHarness(t, trust)
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("Tailscale-User-Login", "someone@example.com")
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if (w.Code == 200) != trust {
			t.Errorf("trust=%v: %d", trust, w.Code)
		}
		r.RemoteAddr = "192.0.2.4:5555"
		w = httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if w.Code == 200 {
			t.Errorf("trust=%v: the header from a non-loopback address must not count", trust)
		}
	}
}

func TestActions(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	csrf := h.srv.auth.CSRF(h.srv.auth.session(c.Value))
	ts := fixtures(t)
	cases := []struct {
		id, answer, note string
		picks            []string
		want             [][]string
		flash            string
	}{
		{"T0001", "send_back", "split it", nil, [][]string{{"note", "T0001", "split it"}, {"move", "T0001", "ready"}}, "ok"},
		{"T0001", "send_back", "", nil, nil, "error"},
		{"T0002", "start", "", nil, [][]string{{"move", "T0002", "ready"}}, "ok"},
		{"T0002", "start", "  mind the cache  ", nil, [][]string{{"note", "T0002", "mind the cache"}, {"move", "T0002", "ready"}}, "ok"},
		{"T0002", "not_now", "", nil, [][]string{{"move", "T0002", "rejected"}}, "ok"},
		{"T0003", "start", "", nil, [][]string{{"move", "T0003", "ready"}}, "ok"},
		{"T0003", "drop", "", nil, [][]string{{"move", "T0003", "rejected"}}, "ok"},
		{"T0003", "reply", "is it real?", nil, [][]string{{"note", "T0003", "is it real?"}, {"move", "T0003", "triage"}}, "ok"},
		{"T0003", "reply", "", nil, nil, "error"},
		{"T0004", "approve", "", []string{"1", "3"}, [][]string{{"approve", "T0004", "--only", "1,3"}}, "ok"},
		{"T0004", "approve", "", []string{"1", "2", "3"}, [][]string{{"approve", "T0004"}}, "ok"},
		{"T0004", "approve", "", nil, nil, "error"},
		{"T0004", "approve", "", []string{"4"}, nil, "error"},
		{"T0004", "redirect", "smaller please", nil, [][]string{{"redirect", "T0004", "--note", "smaller please"}}, "ok"},
		{"T0004", "redirect", "-v smaller", nil, [][]string{{"redirect", "T0004", "--note=-v smaller"}}, "ok"},
		{"T0004", "redirect", "", nil, nil, "error"},
		{"T0006", "file", "", nil, [][]string{{"approve", "T0006"}}, "ok"},
		{"T0006", "drop", "", nil, [][]string{{"move", "T0006", "rejected"}}, "ok"},
		{"T0007", "retry", "", nil, [][]string{{"move", "T0007", "gate"}}, "ok"},
		{"T0007", "retry_note", "rebase first", nil, [][]string{{"note", "T0007", "rebase first"}, {"move", "T0007", "gate"}}, "ok"},
		{"T0007", "drop", "", nil, [][]string{{"move", "T0007", "rejected"}}, "ok"},
		{"T0012", "retry", "", nil, [][]string{{"move", "T0012", "ready"}}, "ok"},
		{"T0008", "done", "", nil, [][]string{{"move", "T0008", "done"}}, "ok"},
		{"T0009", "answer", "-yes, keep it", nil, [][]string{{"note", "T0009", "--", "-yes, keep it"}}, "ok"},
		{"T0009", "note", "plain note", nil, [][]string{{"note", "T0009", "plain note"}}, "ok"},
		{"T0011", "start", "", nil, nil, "error"},
		{"T0010", "note", "too late", nil, nil, "error"},
		{"T0002", "approve", "", nil, nil, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.id+"/"+tc.answer, func(t *testing.T) {
			form := url.Values{"csrf": {csrf}, "answer": {tc.answer}, "note": {tc.note}, "pick": tc.picks}
			if a := AskFor(find(ts, tc.id)); a != nil {
				form.Set("ask", a.Kind)
				form.Set("since", a.Since)
			}
			w := h.do("POST", "/t/"+tc.id+"/act", form, c)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if got := h.argv(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("argv\n got %q\nwant %q", got, tc.want)
			}
			if f := flashOf(t, h, w); f == nil || f.Kind != tc.flash {
				t.Errorf("flash %+v, want %s", f, tc.flash)
			}
		})
	}

	// a stale form (the ask moved on) runs nothing
	form := url.Values{"csrf": {csrf}, "answer": {"start"}, "ask": {"start"}, "since": {"2020-01-01T00:00:00+00:00"}}
	w := h.do("POST", "/t/T0002/act", form, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || len(h.argv()) != 0 {
		t.Errorf("stale ask: %+v", f)
	}
	// the factory's error reaches the flash
	t.Setenv("FAKE_FAIL", "1")
	a := AskFor(find(ts, "T0002"))
	form = url.Values{"csrf": {csrf}, "answer": {"start"}, "ask": {a.Kind}, "since": {a.Since}}
	w = h.do("POST", "/t/T0002/act", form, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || !strings.Contains(f.Text, "boom: that went wrong") {
		t.Errorf("failure flash %+v", f)
	}
	if loc := w.Header().Get("Location"); loc != "/t/T0002" {
		t.Errorf("a failed answer stays on its ticket: %q", loc)
	}
}

func flashOf(t *testing.T, h *harness, w *httptest.ResponseRecorder) *Flash {
	for _, c := range w.Result().Cookies() {
		if c.Name == flashCookie {
			r := httptest.NewRequest("GET", "/", nil)
			r.AddCookie(c)
			return takeFlash(httptest.NewRecorder(), r, h.srv.auth)
		}
	}
	return nil
}

func TestIDValidation(t *testing.T) {
	for id, ok := range map[string]bool{"T0001": true, "T123456": true, "T001": false, "T1234567": false,
		"t0001": false, "T0001.json": false, "../T0001": false, "T00a1": false, "": false} {
		if validID(id) != ok {
			t.Errorf("validID(%q) = %v", id, !ok)
		}
	}
	h := newHarness(t, false)
	c := h.login()
	for _, p := range []string{"/t/T001", "/t/t0001", "/t/T0001x", "/t/..%2fconfig.toml", "/t/T9999", "/b/nowhere"} {
		if w := h.do("GET", p, nil, c); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	csrf := h.srv.auth.CSRF(h.srv.auth.session(c.Value))
	if w := h.do("POST", "/t/T01/act", url.Values{"csrf": {csrf}, "answer": {"start"}}, c); w.Code != http.StatusNotFound {
		t.Errorf("bad id on act: %d", w.Code)
	}
	if len(h.argv()) != 0 {
		t.Errorf("nothing runs for a bad id")
	}
}

func TestPagesRender(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	for _, p := range []string{"/", "/?kind=unstick", "/floor", "/floor?week=2026-10-05", "/boards", "/b/widgets", "/b/factory"} {
		if w := h.do("GET", p, nil, c); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	for _, tk := range fixtures(t) {
		w := h.do("GET", "/t/"+tk.ID, nil, c)
		if w.Code != 200 {
			t.Errorf("%s: %d", tk.ID, w.Code)
		}
		if strings.Contains(w.Body.String(), "javascript:") {
			t.Errorf("%s: a javascript: link was rendered", tk.ID)
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
}

func TestSecretFileCreated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "secret")
	s, err := LoadSecret(p)
	if err != nil || len(s) != 64 {
		t.Fatalf("secret %q %v", s, err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	again, _ := LoadSecret(p)
	if string(again) != string(s) {
		t.Errorf("the secret must persist")
	}
}

func TestLivePollAnswersOnlyWhenTheBoardChanged(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	page := h.do("GET", "/t/T0001", nil, c).Body.String()
	v := boardVersion(NewBoard("testdata").Load())
	if !strings.Contains(page, `data-v="`+v+`"`) || !strings.Contains(page, "foreman.js") {
		t.Fatalf("page lacks the live version or script")
	}
	poll := func(version string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/t/T0001", nil)
		r.Header.Set(liveHeader, version)
		r.AddCookie(c)
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		return w
	}
	if w := poll(v); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Errorf("unchanged board: %d %q", w.Code, w.Body.String())
	}
	if w := poll("stale"); w.Code != 200 || !strings.Contains(w.Body.String(), `id="tk-head"`) {
		t.Errorf("changed board: %d", w.Code)
	}
	ts := NewBoard("testdata").Load()
	before := boardVersion(ts)
	find(ts, "T0001").Notes = append(find(ts, "T0001").Notes, Note{Text: "x"})
	if boardVersion(ts) == before {
		t.Errorf("a new note should change the version")
	}
}

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
	if strings.Contains(body, "Only add the note") {
		t.Errorf("the old note link is still there")
	}
	tk := *find(fixtures(t), "T0001")
	tk.Column, tk.Ask = "ready_for_pr", nil
	if a := Answers(&tk, nil); len(a) != 1 || a[0].Label != "Update PR #23" {
		t.Errorf("a reworked ticket should update its PR: %+v", a)
	}
}

func TestAskRunsTheCLIAndTheAnswerJoinsTheStory(t *testing.T) {
	tk := find(fixtures(t), "T0001")
	cmds, err := Plan(tk, AskFor(tk), "ask", "-did steep pass?", nil)
	if err != nil || len(cmds) != 1 || strings.Join(cmds[0], " ") != "ask T0001 -- -did steep pass?" {
		t.Fatalf("ask plan %v %v", cmds, err)
	}
	if _, err := Plan(tk, AskFor(tk), "ask", "", nil); err == nil {
		t.Errorf("an empty question must be refused")
	}
	parent := *tk
	parent.History = append(append([]Hist{}, tk.History...), Hist{At: "2026-10-14T21:00:00+00:00", Event: "asked", Detail: "T0099: Did steep pass?"})
	child := &Ticket{ID: "T0099", Kind: "task", Parent: "T0001", Column: "needs_human", Updated: "2026-10-14T21:05:00+00:00", Result: "Yes: both Steep runs passed."}
	all := map[string]*Ticket{"T0001": &parent, "T0099": child}
	var got []string
	for _, st := range BuildStory(&parent, all).Steps {
		got = append(got, st.Who+": "+st.What)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "You: Asked: Did steep pass?\nAnswer: Yes: both Steep runs passed.") {
		t.Errorf("question and answer should sit together in the story:\n%s", joined)
	}
	child.Result, child.Agent = "", &Agent{Name: "t0099-k1"}
	if !strings.Contains(fmtSteps(BuildStory(&parent, all)), "Answer: An agent is working on it (T0099).") {
		t.Errorf("a running answer should say so")
	}
}

func fmtSteps(s Story) string {
	var out []string
	for _, st := range s.Steps {
		out = append(out, st.Who+": "+st.What)
	}
	return strings.Join(out, "\n")
}

func TestMergeOnlyForAnEligiblePROnYourWord(t *testing.T) {
	tk := *find(fixtures(t), "T0001")
	g, pr := *tk.Gate, *tk.PR
	g.AutoEligible, pr.Draft = true, false
	tk.Gate, tk.PR = &g, &pr
	a := AskFor(&tk)
	if as := Answers(&tk, a); as[0].Value != "merge" || !as[0].Primary || as[1].Href == "" {
		t.Fatalf("an eligible PR should offer Merge it first: %+v", as)
	}
	if h := Headline(&tk, a); h != "Merge PR #23?" {
		t.Errorf("headline %q", h)
	}
	if cmds, err := Plan(&tk, a, "merge", "", nil); err != nil || strings.Join(cmds[0], " ") != "merge T0001" {
		t.Errorf("merge plan %v %v", cmds, err)
	}
	draft := *find(fixtures(t), "T0001")
	if _, err := Plan(&draft, AskFor(&draft), "merge", "", nil); err == nil {
		t.Errorf("a draft PR must not be merged by the factory")
	}
}

func TestReadyForPRIsOnTheDesk(t *testing.T) {
	tk := *find(fixtures(t), "T0001")
	tk.Column, tk.Ask = "ready_for_pr", nil
	a := AskFor(&tk)
	if a == nil || a.Kind != "open" {
		t.Fatalf("a ticket waiting to go out must ask: %+v", a)
	}
	as := Answers(&tk, a)
	if as[0].Value != "open_pr" || as[0].Label != "Update PR #23" || as[1].Value != "send_back" {
		t.Errorf("answers %+v", as)
	}
	if cmds, err := Plan(&tk, a, "open_pr", "", nil); err != nil || strings.Join(cmds[0], " ") != "pr T0001" {
		t.Errorf("open plan %v %v", cmds, err)
	}
	if cmds, err := Plan(&tk, a, "send_back", "tighten it", nil); err != nil || strings.Join(cmds[len(cmds)-1], " ") != "move T0001 ready" {
		t.Errorf("send back plan %v %v", cmds, err)
	}
}

func TestFileANewTicketFromForeman(t *testing.T) {
	h := newHarness(t, false)
	h.srv.projects = []string{"widgets", "gadgets"}
	c := h.login()
	csrf := h.srv.auth.CSRF(h.srv.auth.session(c.Value))
	page := h.do("GET", "/new", nil, c).Body.String()
	for _, want := range []string{`<option value="widgets" selected>`, `<option value="gadgets"`, `value="factory"`, `name="kind" value="intent"`} {
		if !strings.Contains(page, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	post := func(project, kind, title, body string) *httptest.ResponseRecorder {
		return h.do("POST", "/new", url.Values{"csrf": {csrf}, "project": {project}, "kind": {kind}, "title": {title}, "body": {body}}, c)
	}
	cases := []struct {
		project, kind, title, body string
		want                       []string
	}{
		{"widgets", "change", "Release 1.2.0", "Bump **version**", []string{"add", "--body=Bump **version**", "--", "widgets", "Release 1.2.0"}},
		{"gadgets", "intent", "-weird title", "", []string{"add", "--body=", "--intent", "--", "gadgets", "-weird title"}},
		{"factory", "task", "What is new?", "-dash", []string{"add", "--body=-dash", "--task", "--", "factory", "What is new?"}},
	}
	for _, k := range cases {
		if w := post(k.project, k.kind, k.title, k.body); w.Code != http.StatusSeeOther {
			t.Errorf("%s: %d %s", k.title, w.Code, w.Body.String())
		}
		if got := h.argv(); len(got) != 1 || !reflect.DeepEqual(got[0], k.want) {
			t.Errorf("%s: ran %v", k.title, got)
		}
	}
	for _, bad := range [][4]string{{"nowhere", "change", "x", ""}, {"factory", "change", "x", ""}, {"widgets", "change", "", ""},
		{"widgets", "change", "two\nlines", ""}, {"widgets", "delete", "x", ""}} {
		if w := post(bad[0], bad[1], bad[2], bad[3]); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d", bad, w.Code)
		}
		if got := h.argv(); len(got) != 0 && got[0] != nil {
			t.Errorf("%v ran %v", bad, got)
		}
	}
}

func TestARespondedAskWaitsLowerDownUntilTheFactoryMoves(t *testing.T) {
	ts := fixtures(t)
	all := Desk(ts)
	if len(all) < 2 {
		t.Fatalf("fixtures need two asks, have %d", len(all))
	}
	first := all[0].T
	since := parseTime(all[0].Ask.Since)
	at := func(d time.Duration) string { return since.Add(d).UTC().Format(time.RFC3339) }
	now := since.Add(2 * time.Hour)

	first.History = append(first.History, Hist{At: at(time.Hour), Event: "note", Detail: "not sure about this"})
	items := Desk(ts)
	if last := items[len(items)-1]; last.T != first || last.Responded != "note" {
		t.Errorf("a noted ask should sit last: %s %q", last.T.ID, last.Responded)
	}
	if Waiting(items) != len(items)-1 {
		t.Errorf("waiting %d of %d", Waiting(items), len(items))
	}
	v := deskView(items, "", "", now)
	if len(v.Done) != 1 || v.Done[0].ID != first.ID || v.Done[0].Responded != "You left a note 1 h ago · still yours to decide" {
		t.Errorf("done: %+v", v.Done)
	}
	for _, q := range v.Items {
		if q.ID == first.ID {
			t.Errorf("%s is under Needs you and You've responded", q.ID)
		}
	}

	first.History = append(first.History, Hist{At: at(90 * time.Minute), Event: "asked", Detail: "T0099: why?"})
	if it := Desk(ts)[len(items)-1]; it.Responded != "asked" {
		t.Errorf("a question should keep it down: %q", it.Responded)
	}
	// anything new from the factory brings it back up
	first.History = append(first.History, Hist{At: at(100 * time.Minute), Event: "review requested", Detail: "stevegeek"})
	if Desk(ts)[0].T != first || Waiting(Desk(ts)) != len(items) {
		t.Errorf("the factory moved on, so it needs you again")
	}
	// a note from before the ask arrived is old news
	first.History = append(first.History, Hist{At: since.Add(-time.Hour).UTC().Format(time.RFC3339), Event: "note", Detail: "old"})
	if r, _ := responded(first, all[0].Ask); r != "" {
		t.Errorf("an older note responds to nothing: %q", r)
	}
}

func TestDeskRowsSayWhatItIsAndWhatItWants(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/", nil, c).Body.String()
	ts := fixtures(t)
	it := Desk(ts)[0]
	for _, want := range []string{`<span class="proj">` + it.T.Project + `</span>`, `<span class="qdo">` + Action(it.T, it.Ask) + `</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("desk lacks %q", want)
		}
	}
	pr := &Ticket{Column: "pr_open", PR: &PR{Number: 26}}
	for _, k := range []struct {
		t    *Ticket
		kind string
		want string
	}{
		{pr, "review", "Review PR #26"},
		{&Ticket{Column: "ready_for_pr"}, "open", "Open a PR?"},
		{&Ticket{Proposed: make([]Proposed, 3)}, "design", "Approve design · 3 tickets"},
		{&Ticket{Source: &Source{Issue: 11}}, "decide", "Start or drop? · issue #11"},
		{&Ticket{Kind: "task"}, "notice", "Read the answer"},
	} {
		if got := Action(k.t, &Ask{Kind: k.kind}); got != k.want {
			t.Errorf("%s: %q, want %q", k.kind, got, k.want)
		}
	}
}

func TestANoteOnTheDeskMovesOnToTheNextAsk(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	csrf := h.srv.auth.CSRF(h.srv.auth.session(c.Value))
	first := Desk(fixtures(t))[0].T.ID
	w := h.do("POST", "/t/"+first+"/act", url.Values{"csrf": {csrf}, "answer": {"note"}, "note": {"hmm"}}, c)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if to := w.Header().Get("Location"); to == "/t/"+first || !strings.HasPrefix(to, "/t/") {
		t.Errorf("after a note the desk should move on, went to %q", to)
	}
	if got := h.argv(); len(got) != 1 || got[0][0] != "note" {
		t.Errorf("ran %v", got)
	}
}
