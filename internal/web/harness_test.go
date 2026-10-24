package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The Foreman tests ran with the desk's clock in UTC.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

const (
	testSecret     = "test-secret-0123456789"
	legacyBoard    = "../ticket/testdata/legacy/board"
	legacyContract = "../ticket/testdata/legacy/contract"
)

var testNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// fakeOps stands in for package ops behind the interfaces web declares:
// Answers offers what Foreman's actions.go offered (so the pages read as
// they did), and Answer, File and Look record what reached them.
type fakeOps struct {
	mu      sync.Mutex
	answers []AnswerRequest
	filed   []NewTicket
	looks   []ticket.ID
	out     string
	fail    error
	fileID  ticket.ID
	lookOut string
	lookGo  chan struct{} // when set, Look waits for it
}

func (f *fakeOps) Answer(ctx context.Context, req AnswerRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.answers = append(f.answers, req)
	return f.out, nil
}

func (f *fakeOps) File(ctx context.Context, n NewTicket) (ticket.ID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.filed = append(f.filed, n)
	return f.fileID, nil
}

func (f *fakeOps) Look(ctx context.Context, id ticket.ID, lines int) (string, error) {
	if f.lookGo != nil {
		<-f.lookGo
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if lines != 150 {
		return "", fmt.Errorf("look with %d lines", lines)
	}
	f.looks = append(f.looks, id)
	return f.lookOut, nil
}

func (f *fakeOps) takeAnswers() []AnswerRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.answers
	f.answers = nil
	return out
}

func (f *fakeOps) takeFiled() []NewTicket {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.filed
	f.filed = nil
	return out
}

func (f *fakeOps) takeLooks() []ticket.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.looks
	f.looks = nil
	return out
}

func orSub(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// Answers is Foreman's actions.go Answers, as test data.
func (f *fakeOps) Answers(t *ticket.Ticket, a *ticket.Ask) []Answer {
	pr, hasPR := t.PR.Get()
	if a == nil {
		if t.Column == "ready_for_pr" {
			if hasPR && pr.Number > 0 {
				return []Answer{{Value: "open_pr", Label: fmt.Sprintf("Update PR #%d", pr.Number), Sub: "push the reworked branch; the same PR updates", Primary: true}}
			}
			return []Answer{{Value: "open_pr", Label: "Open the PR", Sub: "push the branch and open it on GitHub", Primary: true}}
		}
		return nil
	}
	issueStays := ""
	if s, ok := t.Source.Get(); ok && s.Issue != 0 {
		issueStays = "the issue stays open on GitHub"
	}
	switch a.Kind {
	case "review":
		var out []Answer
		u := present.SafeURL(pr.URL)
		if u != "" {
			if present.Mergeable(t) {
				out = append(out, Answer{Value: "merge", Label: "Merge it", Sub: fmt.Sprintf("the factory merges PR #%d and closes the ticket", pr.Number), Primary: true},
					Answer{Label: "Read it on GitHub", Sub: "the diff and the checks, before you decide", Href: u})
			} else {
				out = append(out, Answer{Label: fmt.Sprintf("Open PR #%d on GitHub", pr.Number),
					Sub: "approve it there and the factory merges it; or request changes for a rework", Primary: true, Href: u})
			}
		}
		out = append(out, Answer{Value: "send_back", Label: "Send back with a note", Sub: "a worker reworks it; the PR is a draft meanwhile", NeedsNote: true, NoteFor: "to send it back"})
		if u != "" {
			out = append(out, Answer{Label: "Close it on GitHub", Sub: "the factory sees it closed and drops the ticket", Href: u, Danger: true})
		}
		return out
	case "open":
		open := Answer{Value: "open_pr", Label: "Open the PR", Sub: "push the branch and open it on GitHub", Primary: true}
		if hasPR && pr.Number > 0 {
			open.Label, open.Sub = fmt.Sprintf("Update PR #%d", pr.Number), "push the reworked branch; the same PR updates"
		}
		return []Answer{open, {Value: "send_back", Label: "Send back with a note", Sub: "a worker changes it first", NeedsNote: true, NoteFor: "to send it back"}}
	case "start":
		return []Answer{{Value: "start", Label: "Start", Sub: "a worker picks it up on the next tick", Primary: true},
			{Value: "not_now", Label: "Not now", Sub: orSub(issueStays, "the ticket closes"), Danger: true}}
	case "decide":
		return []Answer{{Value: "start", Label: "Start anyway", Sub: "a worker picks it up on the next tick", Primary: true},
			{Value: "reply", Label: "Ask triage again", Sub: "triage reruns with your note", NeedsNote: true, NoteFor: "to ask triage again"},
			{Value: "drop", Label: "Drop", Sub: orSub(issueStays, "the ticket closes"), Danger: true}}
	case "design":
		return []Answer{{Value: "approve", Label: "Approve the ticked ones", Sub: "each goes to Ready on its own", Primary: true},
			{Value: "redirect", Label: "Redirect with a note", Sub: "the designer tries again", NeedsNote: true, NoteFor: "to redirect"}}
	case "findings":
		return []Answer{{Value: "file", Label: "File selected", Sub: "each ticked finding becomes a ticket", Primary: true},
			{Value: "file_none", Label: "File none", Sub: "the hunt closes; nothing is filed", Danger: true}}
	case "proposal":
		return []Answer{{Value: "file", Label: "File it", Sub: "becomes a change ticket in Ready", Primary: true},
			{Value: "drop", Label: "Drop", Sub: "the proposal closes; nothing is filed", Danger: true}}
	case "unstick":
		var out []Answer
		if rc := a.RetryColumn.Or(""); rc != "" {
			out = append(out, Answer{Value: "retry", Label: "Retry", Sub: "back to " + model.ColumnLabel(rc), Primary: true},
				Answer{Value: "retry_note", Label: "Retry with a note", NeedsNote: true, NoteFor: "to retry with it"})
		}
		return append(out, Answer{Value: "drop", Label: "Drop", Sub: "the ticket closes", Danger: true})
	case "notice":
		return []Answer{{Value: "done", Label: "Done", Sub: "close it; nothing else happens", Primary: true}}
	case "question":
		return []Answer{{Value: "answer", Label: "Send the answer", Primary: true, NeedsNote: true}}
	case "draft":
		return []Answer{{Value: "applied", Label: "I've applied it", Sub: "you deployed it from your machine; the ticket closes", Primary: true},
			{Value: "send_back", Label: "Send back with a note", Sub: "the manager redrafts it", NeedsNote: true, NoteFor: "to send it back"},
			{Value: "drop", Label: "Drop", Sub: "nothing changes; the ticket closes", Danger: true}}
	}
	return nil
}

// fakeConfig is a config.Source stand-in.
type fakeConfig struct{ cfg *config.Config }

func (f fakeConfig) Current() (*config.Config, error) {
	if f.cfg == nil {
		return nil, errors.New("no config")
	}
	return f.cfg, nil
}

type harness struct {
	t     *testing.T
	env   *testkit.Env
	paths paths.Paths
	srv   *Server
	h     http.Handler
	ops   *fakeOps
	users *auth.Users
}

// copyBoard copies every ticket file of dir into the root's board.
func copyBoard(t *testing.T, p paths.Paths, dir string) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "T*.json"))
	if err != nil || len(names) == 0 {
		t.Fatalf("fixtures in %s: %v", dir, err)
	}
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p.Tickets, filepath.Base(n)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const testConfig = `
[[recipes]]
name = "release"
title = "Cut a release"
body = "Bump the version."
kind = "change"
projects = ["widgets"]
`

// newHarness is Foreman's: the old-board fixtures on a temp root, a fixed
// clock at 2026-10-09 12:00 UTC, Host example.com allowed, fake services.
func newHarness(t *testing.T, trustTS bool, boards ...string) *harness {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	if len(boards) == 0 {
		boards = []string{legacyBoard}
	}
	for _, b := range boards {
		copyBoard(t, p, b)
	}
	users := auth.NewUsers(filepath.Join(env.Home, ".config", "factory", "foreman-users.json"), testkit.Fixed{T: testNow})
	opt := auth.Options{Hosts: []string{"example.com"}, Users: users, Clock: testkit.Fixed{T: testNow}, FailDelay: time.Nanosecond,
		SinceFile: auth.SinceFile(filepath.Join(env.Home, ".config", "factory", "foreman-secret"))}
	if trustTS {
		opt.TailscaleUsers = []string{"Someone@example.com"}
	}
	a, err := auth.New([]byte(testSecret), opt)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakeOps{fileID: "T0099", lookOut: "the agent's screen"}
	stub := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, name) })
	}
	srv, err := New(Options{Paths: p, Auth: a, Board: boardread.NewSnapshot(&boardread.Reader{Paths: p}),
		Services: Services{Answers: ops, Filer: ops, Looker: ops, Config: fakeConfig{cfg}, API: stub("api"), MCP: stub("mcp")},
		Clock:    testkit.Fixed{T: testNow}, Sealed: []string{filepath.Join(env.Home, ".config", "factory", "github-app.pem")}})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, env: env, paths: p, srv: srv, h: srv.Handler(), ops: ops, users: users}
}

func (h *harness) do(method, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return h.doWith(method, target, form, nil, cookies...)
}

func (h *harness) doWith(method, target string, form url.Values, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) loginWith(secret, next string) *http.Cookie {
	h.t.Helper()
	w := h.do("POST", "/login", url.Values{"secret": {secret}, "next": {next}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != next {
		h.t.Fatalf("login: %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	h.t.Fatal("no session cookie")
	return nil
}

func (h *harness) login() *http.Cookie { return h.loginWith(testSecret, "/floor") }

func (h *harness) csrf(c *http.Cookie) string { return h.srv.auth.CSRF(h.srv.auth.Session(c.Value)) }

func (h *harness) fixtures() []*ticket.Ticket {
	h.t.Helper()
	v, err := h.srv.board.Load()
	if err != nil {
		h.t.Fatal(err)
	}
	return v.Tickets
}

// change rewrites one ticket file on the harness's board.
func (h *harness) change(id string, fn func(*ticket.Ticket)) {
	h.t.Helper()
	data, err := os.ReadFile(h.paths.TicketFile(id))
	if err != nil {
		h.t.Fatal(err)
	}
	tk, _, err := ticket.Decode(data)
	if err != nil {
		h.t.Fatal(err)
	}
	fn(tk)
	if err := os.WriteFile(h.paths.TicketFile(id), ticket.Encode(tk), 0o644); err != nil {
		h.t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(h.paths.TicketFile(id), future, future)
}

func flashOf(t *testing.T, h *harness, w *httptest.ResponseRecorder) *Flash {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == flashCookie {
			r := httptest.NewRequest("GET", "/", nil)
			r.AddCookie(c)
			return h.srv.takeFlash(httptest.NewRecorder(), r)
		}
	}
	return nil
}

func find(ts []*ticket.Ticket, id string) *ticket.Ticket { return findTicket(ts, id) }
