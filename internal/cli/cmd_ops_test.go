package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// fakeGitHub answers the API in-process: each request is recorded as
// "METHOD /path?query", and answer gives the status and JSON body. The App
// token is minted by default.
type fakeGitHub struct {
	mu     sync.Mutex
	calls  []string
	bodies []string
	answer func(method, path, body string) (int, string)
}

func (g *fakeGitHub) Do(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	path := r.URL.RequestURI()
	g.mu.Lock()
	g.calls = append(g.calls, r.Method+" "+path)
	g.bodies = append(g.bodies, body)
	g.mu.Unlock()
	code, out := 404, `{"message":"Not Found"}`
	if strings.HasSuffix(path, "/access_tokens") && r.Method == "POST" {
		code, out = 201, `{"token":"ghs_test","expires_at":"2099-01-01T00:00:00Z","permissions":{"pull_requests":"write","contents":"write","metadata":"read"}}`
	} else if g.answer != nil {
		code, out = g.answer(r.Method, path, body)
	}
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(out)),
		Header: http.Header{"Content-Type": {"application/json"}}, Request: r}, nil
}

func (g *fakeGitHub) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// fakeExec answers each command with what answer returns (exit 0 and no
// output when it returns nothing) and records the argv.
type fakeExec struct {
	mu     sync.Mutex
	argv   [][]string
	cmds   []execx.Cmd // each recorded run whole, its Env and Stdin included
	answer func(argv []string) (code int, out string)
}

func (f *fakeExec) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	if slices.Equal(c.Argv, []string{"git", "--version"}) {
		return execx.Result{Argv: c.Argv, Stdout: []byte("git version 2.50.1\n")}, nil // the git version check, not recorded
	}
	f.mu.Lock()
	f.argv = append(f.argv, c.Argv)
	f.cmds = append(f.cmds, c)
	f.mu.Unlock()
	r := execx.Result{Argv: c.Argv}
	if f.answer != nil {
		code, out := f.answer(c.Argv)
		r.Code, r.Stdout = code, []byte(out)
	}
	return r, nil
}

func (f *fakeExec) ran(word string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.argv {
		if slices.Contains(a, word) {
			return true
		}
	}
	return false
}

// opsRun runs argv through the router with the maintainer's verbs wired by
// o (the GitHub transport) and ex (every subprocess).
func opsRun(t *testing.T, f *fixture, o opsCLI, ex execx.Execer, argv ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	if ex == nil {
		ex = &fakeExec{}
	}
	e := &Env{Stdout: &out, Stderr: &errb, Environ: f.env.Vars, Exec: ex}
	e.init()
	cmds := commands()
	runners := o.runners()
	for i := range cmds {
		if r, ok := runners[cmds[i].Name]; ok {
			cmds[i].Run = r
		}
	}
	code := dispatch(t.Context(), e, cmds, argv)
	return code, out.String(), errb.String()
}

// wakeLog points wake at a file and returns it.
func wakeLog(f *fixture) string {
	p := f.env.Path("wake.log")
	f.env.Vars["FACTORY_WAKE_LOG"] = p
	return p
}

func wakes(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), " wake\n")
}

// appKey writes a fresh App key and returns its path.
func appKey(t *testing.T, f *fixture) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := f.env.Path("app.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// githubRoot is a root with GitHub out on and one project with a repository.
func githubRoot(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	key := appKey(t, f)
	f.env.WriteConfig(t, `
[board]
people = ["alice"]

[github]
installation_id = 1
app_client_id = "Iv1test"
app_slug = "example-factory"
key = "`+key+`"

[projects.gem]
path = "/nonexistent/gem"
github = "example/gem"
`)
	f.env.Vars["FACTORY_GITHUB_API"] = "http://github.test"
	return f
}

// file files a ticket on f's board and lets change set it up.
func file(t *testing.T, f *fixture, project, column string, change func(*ticket.Ticket)) *ticket.Ticket {
	t.Helper()
	e := &Env{Environ: f.env.Vars}
	e.init()
	tk, err := e.Board().FileTicket(t.Context(), ticket.NewIn{Project: project, Title: "Fix it", Body: "b", Door: "human", Column: column})
	if err != nil {
		t.Fatal(err)
	}
	if change != nil {
		change(tk)
	}
	if err := e.Board().Save(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func load(t *testing.T, f *fixture, id ticket.ID) *ticket.Ticket {
	t.Helper()
	e := &Env{Environ: f.env.Vars}
	e.init()
	tk, err := e.Board().Reader.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func events(tk *ticket.Ticket) []string {
	var out []string
	for _, h := range tk.History {
		out = append(out, h.Event+": "+h.Detail)
	}
	return out
}

// port: TF GithubOutTest.test_sending_a_pr_back_turns_it_back_to_draft
func TestMoveOutOfPROpenDraftsThePRBeforeItReturns(t *testing.T) {
	f := githubRoot(t)
	tk := file(t, f, "gem", "pr_open", func(tk *ticket.Ticket) {
		tk.PR = ticket.Some(ticket.PR{Number: 5, Draft: ticket.Some(false)})
	})
	gh := &fakeGitHub{answer: func(method, path, body string) (int, string) {
		switch {
		case method == "GET" && path == "/repos/example/gem/pulls/5":
			return 200, `{"number":5,"state":"open","draft":false,"node_id":"PR_5","html_url":"https://github.com/example/gem/pull/5","head":{"ref":"bot/fix-it","sha":"abc"}}`
		case method == "POST" && path == "/graphql":
			return 200, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":true}}}}`
		}
		return 404, `{"message":"Not Found"}`
	}}
	code, out, errs := opsRun(t, f, opsCLI{HTTP: gh}, nil, "move", string(tk.ID), "ready", "--note", "split it")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	calls := gh.seen()
	if !slices.Contains(calls, "POST /graphql") || !strings.Contains(strings.Join(gh.bodies, ""), "convertPullRequestToDraft") {
		t.Fatalf("the PR was not turned back to draft: %q", calls)
	}
	got := load(t, f, tk.ID)
	if got.Column != "ready" || !got.PR.V.Draft.Or(false) {
		t.Fatalf("column %s, pr %+v", got.Column, got.PR.V)
	}
	if ev := events(got); !slices.Contains(ev, "pr: PR #5 back to draft while it is reworked") {
		t.Fatalf("events %q", ev)
	}
}

func TestMoveWarnsOnStderrWhenThePRCannotBeDrafted(t *testing.T) {
	f := githubRoot(t)
	tk := file(t, f, "gem", "pr_open", func(tk *ticket.Ticket) { tk.PR = ticket.Some(ticket.PR{Number: 5}) })
	gh := &fakeGitHub{answer: func(string, string, string) (int, string) { return 502, `{"message":"Bad Gateway"}` }}
	code, out, errs := opsRun(t, f, opsCLI{HTTP: gh}, nil, "move", string(tk.ID), "ready")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if !strings.Contains(errs, "warning: PR #5 is still open for merging; it could not be turned back to draft") {
		t.Fatalf("stderr %q", errs)
	}
	got := load(t, f, tk.ID)
	if got.Column != "ready" || !strings.Contains(strings.Join(events(got), "\n"), "error: github: could not turn PR #5 back to draft") {
		t.Fatalf("column %s, events %q", got.Column, events(got))
	}
}

// Moving to done or rejected leaves the PR as it is: no GitHub call.
func TestMoveToDoneLeavesThePRAlone(t *testing.T) {
	f := githubRoot(t)
	tk := file(t, f, "gem", "pr_open", func(tk *ticket.Ticket) { tk.PR = ticket.Some(ticket.PR{Number: 5}) })
	gh := &fakeGitHub{}
	if code, _, errs := opsRun(t, f, opsCLI{HTTP: gh}, nil, "move", string(tk.ID), "done"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(gh.seen()) != 0 {
		t.Fatalf("GitHub was called: %q", gh.seen())
	}
}

func TestPRPushesOpensAndPrintsTheURL(t *testing.T) {
	f := githubRoot(t)
	tk := file(t, f, "gem", "ready_for_pr", func(tk *ticket.Ticket) {
		tk.Branch = ticket.Some("bot/fix-it")
		tk.Gate = ticket.Some(ticket.Gate{ChecksPass: true, Head: "abc123", Base: "main", Files: []string{"a.rb"}, Blockers: []string{}})
	})
	ex := &fakeExec{answer: func(argv []string) (int, string) {
		if slices.Contains(argv, "rev-parse") && slices.Contains(argv, "refs/heads/bot/fix-it") {
			return 0, "abc123\n"
		}
		return 0, ""
	}}
	gh := &fakeGitHub{answer: func(method, path, body string) (int, string) {
		if method == "POST" && path == "/repos/example/gem/pulls" {
			return 201, `{"number":9,"state":"open","draft":true,"node_id":"PR_9","html_url":"https://github.com/example/gem/pull/9","head":{"ref":"bot/fix-it","sha":"abc123"}}`
		}
		return 404, `{"message":"Not Found"}`
	}}
	code, out, errs := opsRun(t, f, opsCLI{HTTP: gh}, ex, "pr", string(tk.ID))
	if code != 0 || out != "https://github.com/example/gem/pull/9\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if !ex.ran("push") {
		t.Fatalf("the branch was not pushed: %q", ex.argv)
	}
	for _, a := range ex.argv {
		if strings.Contains(strings.Join(a, " "), "ghs_test") {
			t.Fatalf("the token is in an argv: %q", a)
		}
	}
	got := load(t, f, tk.ID)
	if got.Column != "pr_open" || got.PR.V.Number != 9 || got.PR.V.URL != "https://github.com/example/gem/pull/9" {
		t.Fatalf("column %s, pr %+v", got.Column, got.PR.V)
	}
}

// A refusal of the PR flow is a refusal of the command: exit 1, the
// message alone on stderr, the ticket unchanged.
func TestPRRefusesATicketNotReadyForOne(t *testing.T) {
	f := githubRoot(t)
	tk := file(t, f, "gem", "ready", nil)
	before, _ := os.ReadFile(f.env.Path("board", "tickets", string(tk.ID)+".json"))
	code, out, errs := opsRun(t, f, opsCLI{HTTP: &fakeGitHub{}}, nil, "pr", string(tk.ID))
	if code != 1 || out != "" || strings.TrimSpace(errs) != string(tk.ID)+" is not ready for a PR" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	after, _ := os.ReadFile(f.env.Path("board", "tickets", string(tk.ID)+".json"))
	if !bytes.Equal(before, after) {
		t.Fatal("the ticket was saved")
	}
}

func TestMergeRecordsTheRequestAndWakesTheTick(t *testing.T) {
	f := githubRoot(t)
	log := wakeLog(f)
	tk := file(t, f, "gem", "pr_open", func(tk *ticket.Ticket) {
		tk.PR = ticket.Some(ticket.PR{Number: 5})
		tk.Gate = ticket.Some(ticket.Gate{AutoEligible: true})
	})
	gh := &fakeGitHub{}
	code, out, errs := opsRun(t, f, opsCLI{HTTP: gh}, nil, "merge", string(tk.ID))
	if code != 0 || out != "PR #5 merges on the next tick (a minute or two; longer if main moved)\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if len(gh.seen()) != 0 {
		t.Fatalf("merge called GitHub itself: %q", gh.seen())
	}
	if n := wakes(t, log); n != 1 {
		t.Fatalf("%d wakes", n)
	}
	if !load(t, f, tk.ID).PR.V.MergeRequested.Present() {
		t.Fatal("no merge_requested")
	}
}

func TestAddPrintsTheNewIDAndForemansFormWorks(t *testing.T) {
	f := goldenRoot(t)
	code, out, errs := f.run("", "add", "gem", "A new change", "--body", "Do it for {github}.")
	if code != 0 || out != "T0020\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	got := load(t, f, "T0020")
	if got.Column != "ready" || got.Body != "Do it for example/gem." || got.Door != "human" {
		t.Fatalf("%+v", got)
	}
	// Foreman's new-ticket form: options, then "--", then a title starting with "-"
	code, out, errs = f.run("", "add", "--body=-x", "--task", "--", "factory", "-why?")
	if code != 0 || out != "T0021\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if got := load(t, f, "T0021"); got.Title != "-why?" || got.KindName() != "task" || got.Body != "-x" {
		t.Fatalf("%+v", got)
	}
	// refusals: exit 1, the message only
	for argv, want := range map[string]string{
		"add nowhere Title":          "unknown project nowhere; known: gem, paused (factory only with --task or --manage)",
		"add notes Title":            "notes is a board project: use `factory ticket create`",
		"add gem":                    "a title is needed (or --recipe)",
		"add gem T --task --intent":  "--task, --intent and --manage are different kinds; pick one",
		"add gem T --host elsewhere": "unknown host elsewhere; known: auto, local, helper",
	} {
		code, out, errs := f.run("", strings.Fields(argv)...)
		if code != 1 || out != "" || strings.TrimSpace(errs) != want {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", argv, code, out, errs)
		}
	}
	code, _, errs = f.run("", "add", "gem", "T", "--door", "nowhere")
	if code != 2 || !strings.Contains(errs, "invalid choice: 'nowhere'") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestAddWithARecipeAndATimeout(t *testing.T) {
	f := goldenRoot(t)
	code, out, errs := f.run("", "add", "gem", "--recipe", "docs", "--timeout", "15", "--host", "helper")
	if code != 0 || out != "T0020\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	got := load(t, f, "T0020")
	if got.Title != "Tidy the docs" || got.Body != "Make the README match example/gem." ||
		got.TimeoutMinutes.Or(0) != 15 || got.Host.Or("") != "helper" {
		t.Fatalf("%+v", got)
	}
}

// port: TT ModeTest.test_a_note_on_a_board_ticket_joins_its_conversation
func TestNoteOnABoardTicketJoinsItsConversation(t *testing.T) {
	f := goldenRoot(t)
	if code, out, errs := f.run("", "note", "T0012", "looked", "at", "it"); code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	got := load(t, f, "T0012")
	if len(got.Notes) != 0 {
		t.Fatalf("notes %+v", got.Notes)
	}
	raw, _ := os.ReadFile(f.env.Path("board", "tickets", "T0012.json"))
	var v struct {
		Comments []struct{ By, Text string }
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Comments) != 1 || v.Comments[0].By != "alice" || v.Comments[0].Text != "looked at it" {
		t.Fatalf("comments %+v (%v)", v.Comments, err)
	}
}

func TestNoteOnAFactoryTicketIsForTheNextAgent(t *testing.T) {
	f := goldenRoot(t)
	if code, _, errs := f.run("", "note", "T0001", "--", "-v", "keep", "it"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	got := load(t, f, "T0001")
	if len(got.Notes) != 1 || got.Notes[0].Text != "-v keep it" {
		t.Fatalf("notes %+v", got.Notes)
	}
}

// port: TF TickRaceTest.test_a_cli_change_gives_up_while_the_tick_holds_the_ticket
func TestRefusalsAreTheMessageAndExitOne(t *testing.T) {
	f := goldenRoot(t)
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"move", "T0001", "nowhere"}, "column must be one of: "},
		{[]string{"move", "T0008", "ready"}, "T0008 has a running agent (t0008-w1); wait for it first"},
		{[]string{"move", "T0012", "ready"}, "T0012 is on a board project; use `factory ticket …`"},
		{[]string{"note", "T9999", "x"}, "no ticket T9999"},
		{[]string{"note", "../T0001", "x"}, "no ticket ../T0001"},
		{[]string{"approve", "T0001"}, "T0001 is not an intent or proposal waiting for review"},
		{[]string{"approve", "T0003", "--only", "3"}, "pick numbers 1..2"},
		{[]string{"approve", "T0003", "--only", "x"}, "invalid literal for int() with base 10: 'x'"},
		{[]string{"redirect", "T0001", "--note", "x"}, "T0001 is not an intent waiting for review"},
		{[]string{"host", "T0001", "elsewhere"}, "unknown host elsewhere; known: auto, local, helper"},
		{[]string{"merge", "T0001"}, "T0001 has no open PR"},
		{[]string{"reply", "T0001", " "}, "reply what?"},
		{[]string{"ask", "T0001", " "}, "ask what?"},
		{[]string{"look", "T0001"}, "T0001 has no agent running"},
		{[]string{"audit", "T9999"}, "no ticket T9999"},
	}
	for _, c := range cases {
		code, out, errs := f.run("", c.argv...)
		if code != 1 || out != "" || !strings.HasPrefix(strings.TrimSpace(errs), c.want) || strings.HasPrefix(errs, "factory:") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", c.argv, code, out, errs)
		}
	}
	// a ticket the tick holds: the command gives up and says it is busy
	e := &Env{Environ: f.env.Vars}
	e.init()
	if err := os.MkdirAll(e.Paths().Locks, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := fsx.TryLock(e.Paths().TicketLock("T0001"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the wait ends at once
	var errb bytes.Buffer
	e2 := &Env{Stderr: &errb, Environ: f.env.Vars}
	e2.init()
	cmds := commands()
	code := dispatch(ctx, e2, cmds, []string{"note", "T0001", "x"})
	if code != 1 || len(load(t, f, "T0001").Notes) != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

func TestApproveRedirectAndHost(t *testing.T) {
	f := goldenRoot(t)
	code, out, errs := f.run("", "approve", "T0003", "--only", "2,2")
	if code != 0 || out != "T0020\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if got := load(t, f, "T0003"); got.Column != "done" {
		t.Fatalf("intent in %s", got.Column)
	}
	if got := load(t, f, "T0020"); got.Title != "Second part" || got.Parent.Or("") != "T0003" {
		t.Fatalf("%+v", got)
	}
	code, out, errs = f.run("", "approve", "T0005")
	if code != 0 || out != "T0021\n" {
		t.Fatalf("proposal: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if code, out, errs := f.run("", "host", "T0001", "helper"); code != 0 || out != "" {
		t.Fatalf("host: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if got := load(t, f, "T0001"); got.Host.Or("") != "helper" {
		t.Fatalf("host %+v", got.Host)
	}
	if code, _, _ := f.run("", "host", "T0001", "auto"); code != 0 || load(t, f, "T0001").Host.Present() {
		t.Fatal("auto did not unpin")
	}
}

func TestRedirectSendsTheIdeaBack(t *testing.T) {
	f := goldenRoot(t)
	if code, out, errs := f.run("", "redirect", "T0003", "--note=-smaller please"); code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	got := load(t, f, "T0003")
	if got.Column != "ready" || len(got.Feedback) != 1 || got.Feedback[0].Note != "-smaller please" {
		t.Fatalf("%s %+v", got.Column, got.Feedback)
	}
	if code, _, errs := f.run("", "redirect", "T0003"); code != 2 || !strings.Contains(errs, "--note") {
		t.Fatalf("--note is required: %d %q", code, errs)
	}
}

// port: TF AgentQuestionTest.test_an_agent_asks_the_reply_carries_the_stage_on
func TestReplyAndAskPrintTheirLineAndWake(t *testing.T) {
	f := goldenRoot(t)
	log := wakeLog(f)
	code, out, errs := f.run("", "ask", "T0001", "--", "-does", "it", "pass?")
	if code != 0 || out != "T0020\n" {
		t.Fatalf("ask: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	child := load(t, f, "T0020")
	if child.KindName() != "task" || child.Parent.Or("") != "T0001" || !strings.Contains(child.Body, "-does it pass?") {
		t.Fatalf("%+v", child)
	}
	// a reply to a ticket with no open question becomes an Ask: its id is the line
	code, out, errs = f.run("", "reply", "T0001", "why", "not?")
	if code != 0 || out != "T0021\n" {
		t.Fatalf("reply: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if n := wakes(t, log); n != 1 {
		t.Fatalf("%d wakes", n)
	}
	// a reply to an open question answers it
	tk := file(t, f, "gem", "needs_human", func(tk *ticket.Ticket) {
		tk.Question = ticket.Some(ticket.Question{Text: "Which API?", Stage: "work", At: "2026-10-18T09:00:00+00:00"})
	})
	code, out, errs = f.run("", "reply", string(tk.ID), "the", "old", "one")
	if code != 0 || out != string(tk.ID)+": answered; a new work run carries on\n" {
		t.Fatalf("reply: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if got := load(t, f, tk.ID); got.Column != "ready" || got.Question.V.Answered == "" {
		t.Fatalf("%s %+v", got.Column, got.Question)
	}
	if n := wakes(t, log); n != 2 {
		t.Fatalf("%d wakes", n)
	}
}

func TestWakeCommandWakesTheTick(t *testing.T) {
	f := goldenRoot(t)
	log := wakeLog(f)
	if code, out, errs := f.run("", "wake"); code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if n := wakes(t, log); n != 1 {
		t.Fatalf("%d wakes", n)
	}
}

// port: TF MigrateTest.test_migrate_backfills_asks_votes_and_dropped_proposals_once
func TestMigratePrintsOneLinePerChange(t *testing.T) {
	f := goldenRoot(t)
	// a ticket waiting for a person with no stored ask, as before asks
	tk := file(t, f, "gem", "needs_human", nil)
	raw, err := os.ReadFile(f.env.Path("board", "tickets", string(tk.ID)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "ask")
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(f.env.Path("board", "tickets", string(tk.ID)+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := f.run("", "migrate")
	if code != 0 || !strings.Contains(out, string(tk.ID)+"  ask ") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if !load(t, f, tk.ID).Ask.Present() {
		t.Fatal("no ask")
	}
	// safe to run again
	if code, out, _ := f.run("", "migrate"); code != 0 || strings.Contains(out, string(tk.ID)) {
		t.Fatalf("again: %d %q", code, out)
	}
}

func TestAnswerCarriesOutWhatTheDeskWould(t *testing.T) {
	f := goldenRoot(t)
	// T0003 is an idea waiting for review: approving with no --only files all
	code, out, errs := f.run("", "answer", "T0003", "approve")
	if code != 0 || out != "T0020 T0021\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	// T0002 is a task with a result waiting for a person
	code, out, errs = f.run("", "answer", "T0002", "note", "--note=-thanks")
	if code != 0 || out != "" {
		t.Fatalf("note: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if got := load(t, f, "T0002"); len(got.Notes) != 1 || got.Notes[0].Text != "-thanks" {
		t.Fatalf("notes %+v", got.Notes)
	}
	for argv, want := range map[string]string{
		"answer T0001 start":                          "this ticket is not waiting for an answer",
		"answer T0002 note":                           "that answer needs a note",
		"answer T0005 file --kind proposal --since x": "the ticket changed since the page was opened",
		"answer T0005 sing":                           `"sing" is not an answer to this ask`,
	} {
		code, out, errs := f.run("", strings.Fields(argv)...)
		if code != 1 || out != "" || !strings.Contains(errs, want) || strings.HasPrefix(errs, "factory:") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", argv, code, out, errs)
		}
	}
}

// The verbs never reach GitHub or git when they need neither: add, note,
// host and the rest run with no App key at all.
func TestVerbsWithoutGitHubNeedNoKey(t *testing.T) {
	f := goldenRoot(t)
	ex := &fakeExec{}
	for _, argv := range [][]string{{"add", "gem", "T"}, {"note", "T0001", "x"}, {"host", "T0001", "local"}, {"move", "T0001", "triage"}} {
		if code, _, errs := opsRun(t, f, opsCLI{HTTP: &fakeGitHub{}}, ex, argv...); code != 0 {
			t.Fatalf("%q: %d %s", argv, code, errs)
		}
	}
	if len(ex.argv) != 0 {
		t.Fatalf("ran %q", ex.argv)
	}
	_ = filepath.Join
}
