package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/tokens"
	"github.com/stevegeek/agent-factory/internal/web"
)

// These are the web flows rerun against the real services: package ops
// (with ops.Intents), ticketapi, config.Source, guard, api and mcp, wired as
// the foreman binary wires them, on a temp board. Only the subprocesses
// (scion, ssh) are faked, and wakes go to FACTORY_WAKE_LOG.

var now = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

const secret = "test-secret-0123456789"

const deskConfig = `[board]
people = ["sam", "alex"]

[github]
installation_id = 7

[projects.widgets]
path = "%WORK%"
base = "main"
github = "example/widgets"

[hosts.helper]
ssh = "factory@helper.example"
key = "~/.ssh/helper-key"

[projects.notes]
mode = "board"
`

// fakeExec answers the read-only subprocesses Foreman runs: ssh … tail
// (and scion look, which it must no longer run: the Live test fails on
// it). Anything else fails, so a write verb reaching exec shows.
type fakeExec struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeExec) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c.Argv)
	f.mu.Unlock()
	res := execx.Result{Argv: c.Argv}
	switch {
	case len(c.Argv) > 1 && filepath.Base(c.Argv[0]) == "scion" && slices.Contains(c.Argv, "look"):
		res.Stdout = []byte("$ make test\nok\n")
	case len(c.Argv) > 0 && c.Argv[0] == "ssh" && slices.Contains(c.Argv, "tail"):
		res.Stdout = []byte("remote: running the suite\n")
	default:
		res.Code = 1
		res.Stderr = []byte("unexpected command")
	}
	return res, nil
}

func (f *fakeExec) Spawn(c execx.Cmd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{"spawn"}, c.Argv...))
	return nil
}

func (f *fakeExec) argvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// journal is Foreman's journal, recorded.
type journal struct {
	mu    sync.Mutex
	lines []string
}

func (j *journal) Printf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

type world struct {
	t     *testing.T
	env   *testkit.Env
	vars  map[string]string
	paths paths.Paths
	board *store.Board
	exec  *fakeExec
	auth  *auth.Auth
	users *auth.Users
	h     http.Handler
	wakes string
	log   *journal
}

func newWorld(t *testing.T) *world { t.Helper(); return newWorldWakes(t, true) }

// newWorldWakes is newWorld; with wakeLog false the wakes are real ones,
// not the FACTORY_WAKE_LOG seam.
func newWorldWakes(t *testing.T, wakeLog bool) *world {
	t.Helper()
	env := testkit.Root(t)
	work := filepath.Join(env.Root, "src", "widgets")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	env.WriteConfig(t, strings.ReplaceAll(deskConfig, "%WORK%", work))
	vars := map[string]string{}
	for k, v := range env.Vars {
		vars[k] = v
	}
	wakes := filepath.Join(env.Root, "wakes.log")
	if wakeLog {
		vars["FACTORY_WAKE_LOG"] = wakes
	}
	p := paths.FromEnv(vars)
	w := &world{t: t, env: env, vars: vars, paths: p, exec: &fakeExec{}, wakes: wakes, log: &journal{}}
	w.board = &store.Board{Reader: &boardread.Reader{Paths: p}, Paths: p, Clock: testkit.Fixed{T: now}}
	d := deps{env: vars, exec: w.exec, spawn: w.exec}
	clk := testkit.Fixed{T: now}
	svc, err := d.wire(context.Background(), web.Wiring{Paths: p, Hosts: []string{"example.com"}, Tokens: p.Tokens, API: true,
		APIConcurrency: 2, Clock: clk, Log: w.log})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Answers == nil || svc.Filer == nil || svc.Looker == nil || svc.Config == nil || svc.API == nil || svc.MCP == nil {
		t.Fatalf("services not wired: %+v", svc)
	}
	secretFile := filepath.Join(env.Home, ".config", "factory", "foreman-secret")
	w.users = auth.NewUsers(auth.UsersFile(secretFile), clk)
	w.auth, err = auth.New([]byte(secret), auth.Options{Hosts: []string{"example.com"}, Users: w.users, Clock: clk,
		FailDelay: time.Nanosecond, SinceFile: auth.SinceFile(secretFile)})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := web.New(web.Options{Paths: p, Auth: w.auth, Board: boardread.NewSnapshot(&boardread.Reader{Paths: p}),
		Services: svc, Projects: []string{"widgets", "gadgets"}, Clock: clk, Log: w.log})
	if err != nil {
		t.Fatal(err)
	}
	w.h = srv.Handler()
	return w
}

func (w *world) do(method, target string, form url.Values, c *http.Cookie) *httptest.ResponseRecorder {
	w.t.Helper()
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c != nil {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

func (w *world) loginWith(s string) *http.Cookie {
	w.t.Helper()
	rec := w.do("POST", "/login", url.Values{"secret": {s}, "next": {"/"}}, nil)
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	w.t.Fatalf("login: %d", rec.Code)
	return nil
}

func (w *world) csrf(c *http.Cookie) string { return w.auth.CSRF(w.auth.Session(c.Value)) }

// file files a ticket as new_ticket does, then lets change edit it.
func (w *world) file(in ticket.NewIn, change func(t *ticket.Ticket)) ticket.ID {
	w.t.Helper()
	if in.Project == "" {
		in.Project = "widgets"
	}
	if in.Door == "" {
		in.Door = "human"
	}
	tk, err := w.board.FileTicket(context.Background(), in)
	if err != nil {
		w.t.Fatal(err)
	}
	if change != nil {
		change(tk)
		if err := w.board.Save(tk); err != nil {
			w.t.Fatal(err)
		}
	}
	return tk.ID
}

func (w *world) load(id ticket.ID) *ticket.Ticket {
	w.t.Helper()
	tk, err := w.board.Reader.Load(id)
	if err != nil {
		w.t.Fatal(err)
	}
	return tk
}

// raw is the ticket file as JSON, for keys the typed view does not name.
func (w *world) raw(id ticket.ID) map[string]any {
	w.t.Helper()
	data, err := os.ReadFile(w.paths.TicketFile(string(id)))
	if err != nil {
		w.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		w.t.Fatal(err)
	}
	return m
}

// lastBy is the "by" of the ticket's last history entry that has one.
func (w *world) lastBy(id ticket.ID) string {
	h, _ := w.raw(id)["history"].([]any)
	for i := len(h) - 1; i >= 0; i-- {
		if e, ok := h[i].(map[string]any); ok {
			if by, ok := e["by"].(string); ok {
				return by
			}
		}
	}
	return ""
}

func (w *world) wakeCount() int {
	data, err := os.ReadFile(w.wakes)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return strings.Count(string(data), " wake\n")
}

// answer posts one answer to the ask the page shows now.
func (w *world) answer(c *http.Cookie, id ticket.ID, value, note string) *httptest.ResponseRecorder {
	w.t.Helper()
	form := url.Values{"csrf": {w.csrf(c)}, "answer": {value}, "note": {note}}
	if a := present.AskFor(w.load(id)); a != nil {
		form.Set("ask", a.Kind)
		form.Set("since", a.Since)
	}
	return w.do("POST", "/t/"+string(id)+"/act", form, c)
}

// flash is the message an answer's redirect carries (its signature is
// web's to check).
func flash(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "foreman_flash" {
			v, _, _ := strings.Cut(c.Value, ".")
			raw, _ := base64.RawURLEncoding.DecodeString(v)
			return string(raw)
		}
	}
	return ""
}

func moved(col string, reason model.Reason, detail string) func(*ticket.Ticket) {
	return func(t *ticket.Ticket) { ticket.Move(t, col, reason, detail, "", "", now) }
}

// Answers run in-process, under the ticket's lock, with one save: "Start"
// moves a triaged ticket to ready, a note joins the ticket for the next
// agent, and the page goes on down the desk.
func TestAnswersChangeTheBoardInProcess(t *testing.T) {
	w := newWorld(t)
	start := w.file(ticket.NewIn{Title: "Fix the parser", Body: "b", Column: "triage"},
		moved("needs_human", model.ReasonTriageAccepted, "triage accepted; move to ready to start work"))
	vague := w.file(ticket.NewIn{Title: "Vague", Body: "b", Column: "triage"},
		moved("needs_human", model.ReasonTriageUnsure, "triage unsure: no failing case"))
	c := w.loginWith(secret)

	rec := w.answer(c, start, "start", "  mind the cache  ")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if got := w.load(start); got.Column != string(model.ColReady) {
		t.Errorf("start moved the ticket to %s", got.Column)
	}
	notes, _ := w.raw(start)["notes"].([]any)
	if len(notes) != 1 || notes[0].(map[string]any)["text"] != "mind the cache" {
		t.Errorf("the note typed with the answer: %v", notes)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("after an answer Foreman goes back to the desk: %q", loc)
	}

	// a plain note leaves the decision with the person
	if rec := w.answer(c, vague, "note", "is it real?"); rec.Code != http.StatusSeeOther {
		t.Fatalf("note: %d", rec.Code)
	}
	if got := w.load(vague); got.Column != string(model.ColNeedsHuman) || len(got.Notes) != 1 {
		t.Errorf("note: column %s, notes %d", got.Column, len(got.Notes))
	}
	// an answer the ask does not take is refused and changes nothing
	before, _ := os.ReadFile(w.paths.TicketFile(string(vague)))
	rec = w.answer(c, vague, "merge", "")
	after, _ := os.ReadFile(w.paths.TicketFile(string(vague)))
	if rec.Code != http.StatusSeeOther || string(before) != string(after) {
		t.Errorf("a wrong answer changed the ticket (%d)", rec.Code)
	}
}

// An answer to an ask that has moved on since the page was rendered is
// refused under the lock, and nothing is written.
func TestAStaleAnswerIsRefused(t *testing.T) {
	w := newWorld(t)
	id := w.file(ticket.NewIn{Title: "Fix the parser", Body: "b", Column: "triage"},
		moved("needs_human", model.ReasonTriageAccepted, "triage accepted"))
	c := w.loginWith(secret)
	a := present.AskFor(w.load(id))
	if a == nil {
		t.Fatal("no ask")
	}
	before, _ := os.ReadFile(w.paths.TicketFile(string(id)))
	rec := w.do("POST", "/t/"+string(id)+"/act", url.Values{"csrf": {w.csrf(c)}, "answer": {"start"}, "ask": {a.Kind},
		"since": {"2026-01-01T00:00:00+00:00"}}, c)
	after, _ := os.ReadFile(w.paths.TicketFile(string(id)))
	if rec.Code != http.StatusSeeOther || string(before) != string(after) {
		t.Errorf("a stale answer: %d, changed %v", rec.Code, string(before) != string(after))
	}
	if f := flash(rec); !strings.Contains(f, "changed since you opened it") {
		t.Errorf("flash %q", f)
	}
}

// Foreman's wake writes foreman-state/wake, which factory-wake.path
// watches; it never runs systemd-run (the bus would let it run anything
// outside its sandbox) and never spawns a tick inside the sandbox.
func TestForemanWakesThroughTheTriggerFile(t *testing.T) {
	w := newWorldWakes(t, false)
	review := w.file(ticket.NewIn{Title: "Tidy README", Body: "b", Column: "ready_for_pr"}, func(tk *ticket.Ticket) {
		tk.Branch = ticket.Some("bot/tidy-readme")
		tk.PR = ticket.Some(ticket.PR{Number: 23, URL: "https://github.com/example/widgets/pull/23"})
		ticket.Move(tk, "pr_open", model.ReasonPROpened, "PR https://github.com/example/widgets/pull/23", "", "", now)
	})
	c := w.loginWith(secret)
	if rec := w.answer(c, review, "send_back", "split it"); rec.Code != http.StatusSeeOther {
		t.Fatalf("send back: %d", rec.Code)
	}
	if b, err := os.ReadFile(w.paths.WakeTrigger()); err != nil || !strings.HasSuffix(string(b), " wake\n") {
		t.Errorf("trigger %q, %v", b, err)
	}
	if calls := w.exec.argvs(); len(calls) != 0 {
		t.Errorf("the wake ran %v", calls)
	}
}

// Foreman never holds GitHub credentials: "Send back" from pr_open records
// hold_requested and "Open the PR" records open_requested, each a stamp on
// the ticket's pr, and the save wakes the tick, whose GitHub pass drains
// them first. Nothing ran a subprocess.
func TestGitHubAnswersRecordIntentsAndWakeTheTick(t *testing.T) {
	w := newWorld(t)
	review := w.file(ticket.NewIn{Title: "Tidy README", Body: "b", Column: "ready_for_pr"}, func(tk *ticket.Ticket) {
		tk.Branch = ticket.Some("bot/tidy-readme")
		tk.Gate = ticket.Some(ticket.Gate{ChecksPass: true, Blockers: []string{"40 changed lines (limit 10)"}})
		tk.PR = ticket.Some(ticket.PR{Number: 23, URL: "https://github.com/example/widgets/pull/23"})
		ticket.Move(tk, "pr_open", model.ReasonPROpened, "PR https://github.com/example/widgets/pull/23", "", "", now)
	})
	open := w.file(ticket.NewIn{Title: "Add a flag", Body: "b", Column: "gate"}, func(tk *ticket.Ticket) {
		tk.Branch = ticket.Some("bot/add-a-flag")
		tk.Gate = ticket.Some(ticket.Gate{ChecksPass: true})
		ticket.Move(tk, "ready_for_pr", model.ReasonGatePassed, "gate passed", "", "", now)
	})
	c := w.loginWith(secret)

	if rec := w.answer(c, review, "send_back", "split it"); rec.Code != http.StatusSeeOther {
		t.Fatalf("send back: %d", rec.Code)
	}
	got := w.load(review)
	pr, _ := got.PR.Get()
	if got.Column != string(model.ColReady) || pr.HoldRequested.Or("") != "2026-10-18T10:00:00+00:00" {
		t.Errorf("send back: column %s, pr %+v", got.Column, pr)
	}
	if n := w.wakeCount(); n != 1 {
		t.Errorf("send back woke the tick %d times", n)
	}

	if rec := w.answer(c, open, "open_pr", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("open: %d", rec.Code)
	}
	got = w.load(open)
	pr, _ = got.PR.Get()
	if got.Column != string(model.ColReadyForPR) || pr.OpenRequested.Or("") != "2026-10-18T10:00:00+00:00" {
		t.Errorf("open: column %s, pr %+v", got.Column, pr)
	}
	rawPR, _ := w.raw(open)["pr"].(map[string]any)
	if rawPR["open_requested"] != "2026-10-18T10:00:00+00:00" {
		t.Errorf("the intent as the tick reads it: %v", rawPR)
	}
	if n := w.wakeCount(); n != 2 {
		t.Errorf("woke the tick %d times in all", n)
	}
	if calls := w.exec.argvs(); len(calls) != 0 {
		t.Errorf("an answer ran %v", calls)
	}
}

// An answer from Foreman records who gave it: the maintainer as the
// first of [board] people, a second user by their own name; the journal
// says who clicked too.
func TestEveryAnswerIsByWhoGaveIt(t *testing.T) {
	w := newWorld(t)
	mine := w.file(ticket.NewIn{Title: "One", Body: "b", Column: "triage"}, moved("needs_human", model.ReasonTriageAccepted, "accepted"))
	theirs := w.file(ticket.NewIn{Title: "Two", Body: "b", Column: "triage"}, moved("needs_human", model.ReasonTriageAccepted, "accepted"))
	robin, err := w.users.Add("robin", []string{"widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := w.answer(w.loginWith(secret), mine, "start", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("maintainer: %d", rec.Code)
	}
	if rec := w.answer(w.loginWith(robin), theirs, "start", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("robin: %d", rec.Code)
	}
	if a, b := w.lastBy(mine), w.lastBy(theirs); a != "sam" || b != "robin" {
		t.Errorf("by: maintainer's answer %q, robin's %q; want sam and robin", a, b)
	}
	if data, _ := os.ReadFile(w.paths.TicketFile(string(mine))); strings.Contains(string(data), "robin") {
		t.Errorf("the maintainer's ticket names robin")
	}
	w.log.mu.Lock()
	logged := strings.Join(w.log.lines, "\n")
	w.log.mu.Unlock()
	if !strings.Contains(logged, string(theirs)+" start by robin") {
		t.Errorf("the journal does not say who clicked:\n%s", logged)
	}
}

// What Foreman does is in its own log, foreman-state/foreman.log, which
// the Live view shows with factory.log: logs/ is read-only to it, so it
// never writes there.
func TestForemansActionsAreInItsOwnLog(t *testing.T) {
	w := newWorld(t)
	id := w.file(ticket.NewIn{Title: "One", Body: "b", Column: "triage"}, moved("needs_human", model.ReasonTriageAccepted, "accepted"))
	c := w.loginWith(secret)
	if rec := w.answer(c, id, "start", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d", rec.Code)
	}
	data, err := os.ReadFile(w.paths.ForemanLog())
	if err != nil || !strings.Contains(string(data), string(id)) {
		t.Fatalf("foreman.log %q %v", data, err)
	}
	if _, err := os.Lstat(w.paths.FactoryLog); !os.IsNotExist(err) {
		t.Errorf("Foreman wrote logs/factory.log: %v", err)
	}
	if rec := w.do("GET", "/live/log", nil, c); !strings.Contains(rec.Body.String(), string(id)) {
		t.Errorf("the Live log lacks Foreman's line: %q", rec.Body.String())
	}
}

// The config is read again when it changes: a project added to
// config.toml takes a ticket on the next request, and a new maintainer is
// who the next ticket is by, with no restart.
func TestAConfigChangeIsSeenByTheNextRequest(t *testing.T) {
	w := newWorld(t)
	c := w.loginWith(secret)
	post := func() *httptest.ResponseRecorder {
		return w.do("POST", "/new", url.Values{"csrf": {w.csrf(c)}, "project": {"gadgets"}, "kind": {"change"},
			"title": {"Make it blue"}, "body": {"b"}}, c)
	}
	if rec := post(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Unknown project gadgets") {
		t.Fatalf("before the change: %d %s", rec.Code, rec.Body.String())
	}
	cfg, err := os.ReadFile(w.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(cfg), `people = ["sam", "alex"]`, `people = ["alex", "sam"]`, 1) +
		"\n[projects.gadgets]\npath = \"/nonexistent/gadgets\"\nbase = \"main\"\n"
	tmp := w.paths.Config + ".new"
	if err := os.WriteFile(tmp, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, w.paths.Config); err != nil {
		t.Fatal(err)
	}
	rec := post()
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/t/T") {
		t.Fatalf("after the change: %d %q %s", rec.Code, loc, rec.Body.String())
	}
	id := ticket.ID(strings.TrimPrefix(loc, "/t/"))
	got := w.load(id)
	if got.Project != "gadgets" || got.Title != "Make it blue" || got.Column != string(model.ColReady) {
		t.Errorf("filed: %s %q in %s", got.Project, got.Title, got.Column)
	}
	if by := w.lastBy(id); by != "alex" {
		t.Errorf("filed by %q, want the new maintainer alex", by)
	}
}

// Filing from ＋ is ops.Add in-process: each kind lands where the CLI's
// add puts it, by the maintainer through the human door.
func TestFileANewTicketInProcess(t *testing.T) {
	w := newWorld(t)
	c := w.loginWith(secret)
	for _, k := range []struct{ project, kind, title, col, want string }{
		{"widgets", "change", "Release 1.2.0", "ready", ""},
		{"widgets", "intent", "-weird title", "ready", "intent"},
		{"factory", "task", "What is new?", "ready", "task"},
		{"factory", "manage", "A morning check", "ready", "manage"},
	} {
		rec := w.do("POST", "/new", url.Values{"csrf": {w.csrf(c)}, "project": {k.project}, "kind": {k.kind},
			"title": {"  " + k.title + " "}, "body": {"-dash\n"}}, c)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/t/T") {
			t.Errorf("%s: %d %q", k.title, rec.Code, loc)
			continue
		}
		got := w.load(ticket.ID(strings.TrimPrefix(loc, "/t/")))
		if got.Project != k.project || got.Title != k.title || got.Column != k.col || got.Kind.Or("") != k.want ||
			got.Door != "human" || got.Body != "-dash" {
			t.Errorf("%s: %s %q %s kind %q door %s body %q", k.title, got.Project, got.Title, got.Column, got.Kind.Or(""), got.Door, got.Body)
		}
	}
}

// The Live tab reads a local agent's screen from the relay's file
// (screens/<agent>.txt), or its agent.log while the relay starts, and
// never runs scion; a helper job's through ssh … tail, read-only, with the
// helper's key as config names it (~ expanded). A local poll marks the
// watch for the relay.
func TestLiveScreensAreReadThroughTheReadOnlyAdapters(t *testing.T) {
	w := newWorld(t)
	local := w.file(ticket.NewIn{Title: "Running", Body: "b", Column: "working"}, func(tk *ticket.Ticket) {
		tk.Agent = ticket.Some(ticket.Agent{Name: "t0001-w1", Stage: "work", Started: "2026-10-18T09:00:00+00:00", Host: "local"})
	})
	remote := w.file(ticket.NewIn{Title: "Away", Body: "b", Column: "working"}, func(tk *ticket.Ticket) {
		tk.Agent = ticket.Some(ticket.Agent{Name: "t0002-w1", Stage: "work", Started: "2026-10-18T09:30:00+00:00", Host: "helper"})
	})
	starting := w.file(ticket.NewIn{Title: "Starting", Body: "b", Column: "working"}, func(tk *ticket.Ticket) {
		tk.Agent = ticket.Some(ticket.Agent{Name: "t0003-w1", Stage: "work", Started: "2026-10-18T09:00:00+00:00", Host: "local"})
	})
	if err := os.MkdirAll(w.paths.ForemanState(), 0o700); err != nil {
		t.Fatal(err)
	}
	c := w.loginWith(secret)

	// the relay's screen, with its age
	screen := w.env.WriteFile(t, "screens/t0001-w1.txt", "$ make test\nok\n")
	os.Chtimes(screen, now.Add(-4*time.Second), now.Add(-4*time.Second))
	rec := w.do("GET", "/live/"+string(local)+"/screen", nil, c)
	if body := rec.Body.String(); rec.Code != 200 || body != "t0001-w1 · work · started 2026-10-18T09:00:00+00:00 · screen 4 s old\n$ make test\nok" {
		t.Errorf("relayed screen: %d %q", rec.Code, body)
	}
	if _, err := os.Lstat(w.paths.Watching()); err != nil {
		t.Errorf("the poll did not mark the watch: %v", err)
	}
	// none yet: the agent's log, while the relay starts
	log := filepath.Join(w.env.Home, ".scion", "project-configs", "p", ".scion", "agents", "t0003-w1", "home", "agent.log")
	os.MkdirAll(filepath.Dir(log), 0o755)
	os.WriteFile(log, []byte("booting\n"), 0o644)
	rec = w.do("GET", "/live/"+string(starting)+"/screen", nil, c)
	if body := rec.Body.String(); rec.Code != 200 || body != "t0003-w1 · work · started 2026-10-18T09:00:00+00:00 · screen starting…\nbooting" {
		t.Errorf("local screen before the relay: %d %q", rec.Code, body)
	}

	rec = w.do("GET", "/live/"+string(remote)+"/screen", nil, c)
	if body := rec.Body.String(); rec.Code != 200 || body != "t0002-w1 · work @helper · started 2026-10-18T09:30:00+00:00\nremote: running the suite" {
		t.Errorf("helper screen: %d %q", rec.Code, body)
	}
	var sawSSH bool
	for _, argv := range w.exec.argvs() {
		switch {
		case argv[0] == "ssh":
			sawSSH = slices.Contains(argv, filepath.Join(w.env.Home, ".ssh", "helper-key")) &&
				slices.Contains(argv, "factory@helper.example") && slices.Contains(argv, "tail")
		default:
			t.Errorf("Foreman ran %v", argv)
		}
	}
	if !sawSSH {
		t.Errorf("ssh tail %v: %v", sawSSH, w.exec.argvs())
	}
}

// The token APIs are the real ones, in-process: a token from the store
// gets its own name back from /api/v1/me and MCP's serverInfo, behind the
// Host check and the security headers like every route.
func TestTheTokenAPIsServeTheRealVerbs(t *testing.T) {
	w := newWorld(t)
	st := &tokens.Store{Path: w.paths.Tokens, Clock: testkit.Fixed{T: now}}
	added, err := st.Add(context.Background(), tokens.AddIn{Name: "example-agent", Projects: []string{"notes"}, Can: "read,talk",
		People: []string{"sam", "alex"}, Known: []string{"widgets", "notes"}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, target, host, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host = host
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("Authorization", "Bearer "+added.Token)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, r)
		return rec
	}
	rec := call("GET", "/api/v1/me", "example.com", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"example-agent"`) || rec.Header().Get("Content-Security-Policy") == "" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("me: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	rec = call("POST", "/mcp", "127.0.0.1:8790", initReq)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"serverInfo"`) || !strings.Contains(rec.Body.String(), `"version":"dev"`) {
		t.Errorf("mcp initialize: %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/v1/me", "/mcp", "/healthz", "/"} {
		if rec := call("GET", path, "rebound.example:8790", ""); rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host rebound.example %s: %d", path, rec.Code)
		}
	}
	// the login session never opens the token APIs
	c := w.loginWith(secret)
	r := httptest.NewRequest("GET", "/api/v1/me", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.AddCookie(c)
	rec = httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("api with only a session: %d", rec.Code)
	}
}

// An agent's question on a board ticket, asked through the API, is an ask
// on the desk, and the answer from the web goes through the ticket API's
// answer verb as the maintainer: the agent reads it, and the ticket leaves
// Waiting.
func TestABoardQuestionIsAnsweredFromTheDesk(t *testing.T) {
	w := newWorld(t)
	st := &tokens.Store{Path: w.paths.Tokens, Clock: testkit.Fixed{T: now}}
	added, err := st.Add(context.Background(), tokens.AddIn{Name: "example-agent", Projects: []string{"notes"}, Can: "read,file,talk,work",
		People: []string{"sam", "alex"}, Known: []string{"widgets", "notes"}})
	if err != nil {
		t.Fatal(err)
	}
	api := func(target, body string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("POST", target, strings.NewReader(body))
		r.Host, r.RemoteAddr = "example.com", "127.0.0.1:5555"
		r.Header.Set("Authorization", "Bearer "+added.Token)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, r)
		var out map[string]any
		if rec.Code/100 != 2 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		return out
	}
	tk, _ := api("/api/v1/tickets", `{"project": "notes", "title": "Date picker"}`)["ticket"].(map[string]any)
	id, _ := tk["id"].(string)
	api("/api/v1/tickets/"+id+"/claim", `{}`)
	api("/api/v1/tickets/"+id+"/ask", `{"question": "Which locale?", "options": ["en-GB", "en-US"]}`)
	if got := w.load(ticket.ID(id)); got.Column != "waiting" {
		t.Fatalf("after the question %s is in %s", id, got.Column)
	}

	c := w.loginWith(secret)
	if desk := w.do("GET", "/", nil, c).Body.String(); !strings.Contains(desk, `href="/t/`+id+`"`) || !strings.Contains(desk, "Answer example-agent") {
		t.Fatalf("the question is not on the desk")
	}
	if b := w.do("GET", "/b/notes", nil, c).Body.String(); !strings.Contains(b, ">board<") || !strings.Contains(b, id) {
		t.Errorf("the board view lacks the ticket")
	}
	rec := w.answer(c, ticket.ID(id), "option-1", "and 24-hour times")
	if rec.Code != http.StatusSeeOther || !strings.Contains(flash(rec), "Answer sent to example-agent") {
		t.Fatalf("answer: %d %q", rec.Code, flash(rec))
	}
	got := w.load(ticket.ID(id))
	if len(got.Questions) != 1 || got.Questions[0].Answer.Or("") != "en-GB\n\nand 24-hour times" || got.Questions[0].AnsweredBy.Or("") != "sam" {
		t.Errorf("the question: %+v", got.Questions)
	}
	if got.Column != "doing" || present.AskFor(got) != nil || w.lastBy(ticket.ID(id)) != "sam" {
		t.Errorf("after the answer: %s, by %s", got.Column, w.lastBy(ticket.ID(id)))
	}
	// the conversation shows it
	if md := w.do("GET", "/t/"+id+"/talk", nil, c).Body.String(); !strings.Contains(md, "and 24-hour times") {
		t.Errorf("the conversation lacks the answer")
	}
	// answered twice is the ticket API's refusal, and the desk says so
	rec = w.do("POST", "/t/"+id+"/act", url.Values{"csrf": {w.csrf(c)}, "answer": {"option-2"}, "ask": {"question"},
		"since": {got.Questions[0].At}}, c)
	if f := flash(rec); !strings.Contains(f, "changed since you opened it") {
		t.Errorf("a second answer: %q", f)
	}
}

// With --api=false there are no token APIs, and the rest still works.
func TestWithoutTheAPI(t *testing.T) {
	env := testkit.Root(t)
	env.WriteConfig(t, "[board]\npeople = [\"sam\"]\n")
	p := paths.FromEnv(env.Vars)
	d := deps{env: env.Vars, exec: &fakeExec{}, spawn: &fakeExec{}}
	svc, err := d.wire(context.Background(), web.Wiring{Paths: p, Tokens: p.Tokens, Clock: testkit.Fixed{T: now}, Log: &journal{}})
	if err != nil {
		t.Fatal(err)
	}
	if svc.API != nil || svc.MCP != nil || svc.Answers == nil || svc.Filer == nil || svc.Looker == nil || svc.Config == nil || svc.Tickets == nil {
		t.Errorf("services: %+v", svc)
	}
}

// A token store others can read is named in the journal at start: no
// token works until it is fixed (the store refuses it on every read).
func TestALooseTokenStoreIsLoggedAtStart(t *testing.T) {
	env := testkit.Root(t)
	env.WriteConfig(t, "[board]\npeople = [\"sam\"]\n")
	p := paths.FromEnv(env.Vars)
	if err := os.WriteFile(p.Tokens, []byte(`{"version": 1, "tokens": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	log := &journal{}
	d := deps{env: env.Vars, exec: &fakeExec{}, spawn: &fakeExec{}}
	if _, err := d.wire(context.Background(), web.Wiring{Paths: p, Tokens: p.Tokens, API: true, Clock: testkit.Fixed{T: now}, Log: log}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(log.lines, "\n"), "readable by others") {
		t.Errorf("journal: %v", log.lines)
	}
}
