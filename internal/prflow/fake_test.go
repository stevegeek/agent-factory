package prflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The fakes the PR flow's tests share. The tests may import only prflow's
// row of the import table (plus testkit), so the board, git and the merge
// check are small in-memory fakes here, and GitHub is the real
// github.Client over a fake transport: every request the flow makes goes
// through the client's real paths, headers and body encoding.

// request is one call GitHub received.
type request struct {
	Method, Path string
	Body         jsonx.Value
	Auth         string
}

func (r request) String() string { return r.Method + " " + r.Path }

// reply is an answer: a JSON value (any encoding/json value), an HTTP
// refusal, or a failure to answer at all.
type reply struct {
	Code    int    // 0 is 200
	Message string // the refusal's "message"
	Value   any
	NetErr  error // the request gets no answer
}

func ok(v any) reply { return reply{Value: v} }

func refuse(code int, msg string) reply { return reply{Code: code, Message: msg} }

// route answers requests whose method matches and whose path starts with
// Prefix, as the Python tests' routed() fake does: the first match wins.
type route struct {
	Method, Prefix string
	Reply          func(body jsonx.Value) reply
}

func at(method, prefix string, r reply) route {
	return route{Method: method, Prefix: prefix, Reply: func(jsonx.Value) reply { return r }}
}

// fakeGitHub is an http transport for github.Client.
type fakeGitHub struct {
	mu       sync.Mutex
	requests []request
	routes   []route
	// state, when set, answers instead of routes (the port of goldens.py's
	// FakeGitHub).
	state *stateGitHub
}

func (f *fakeGitHub) Do(req *http.Request) (*http.Response, error) {
	var body jsonx.Value
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		if len(raw) > 0 {
			v, err := jsonx.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("fake github: bad body: %w", err)
			}
			body = v
		}
	}
	path := req.URL.Path
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	f.mu.Lock()
	f.requests = append(f.requests, request{Method: req.Method, Path: path, Body: body, Auth: req.Header.Get("Authorization")})
	var r reply
	if f.state != nil {
		r = f.state.answer(req.Method, path, body)
	} else {
		r = ok(map[string]any{})
		for _, rt := range f.routes {
			if rt.Method == req.Method && strings.HasPrefix(path, rt.Prefix) {
				r = rt.Reply(body)
				break
			}
		}
	}
	f.mu.Unlock()
	if r.NetErr != nil {
		return nil, r.NetErr
	}
	code := r.Code
	if code == 0 {
		code = 200
	}
	var raw []byte
	if code >= 300 {
		raw, _ = json.Marshal(map[string]any{"message": r.Message})
	} else if r.Value != nil {
		raw, _ = json.Marshal(r.Value)
	}
	return &http.Response{
		StatusCode: code, Status: strconv.Itoa(code) + " " + http.StatusText(code),
		Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw)), Request: req,
	}, nil
}

func (f *fakeGitHub) calls() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

func (f *fakeGitHub) did(method, path string) bool {
	for _, r := range f.calls() {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}

func (f *fakeGitHub) any(method string) bool {
	for _, r := range f.calls() {
		if r.Method == method {
			return true
		}
	}
	return false
}

func (f *fakeGitHub) body(t *testing.T, method, path string) jsonx.Value {
	t.Helper()
	for _, r := range f.calls() {
		if r.Method == method && r.Path == path {
			return r.Body
		}
	}
	t.Fatalf("no %s %s in %v", method, path, f.calls())
	return jsonx.Null()
}

// stateGitHub is goldens.py's FakeGitHub: one repository's PRs, reviews and
// comments, answering as GitHub would.
type stateGitHub struct {
	prs           map[int]map[string]any
	reviews       []map[string]any
	comments      []map[string]any
	heads         map[string]string
	refuseMerges  int
	next          int
	unscriptedErr []string
}

func newStateGitHub(heads map[string]string) *stateGitHub {
	return &stateGitHub{prs: map[int]map[string]any{}, heads: heads, next: 12, reviews: []map[string]any{}, comments: []map[string]any{}}
}

func (s *stateGitHub) answer(method, path string, body jsonx.Value) reply {
	rest := path
	if strings.HasPrefix(path, "/repos/") {
		parts := strings.SplitN(path, "/", 5)
		if len(parts) == 5 {
			rest = parts[4]
		}
	}
	b := toAny(body)
	if method == "POST" && rest == "pulls" {
		bm := b.(map[string]any)
		n := s.next
		s.next++
		head := bm["head"].(string)
		s.prs[n] = map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/example/gem/pull/%d", n),
			"draft": bm["draft"], "state": "open", "node_id": fmt.Sprintf("PR_%d", n), "title": bm["title"],
			"body": bm["body"], "head": map[string]any{"ref": head, "sha": s.heads[head]},
			"mergeable_state": "clean", "merged": false}
		return ok(deepCopy(s.prs[n]))
	}
	if method == "POST" && path == "/graphql" {
		bm := b.(map[string]any)
		id := bm["variables"].(map[string]any)["id"]
		for _, pr := range s.prs {
			if pr["node_id"] == id {
				pr["draft"] = strings.Contains(bm["query"].(string), "convertPullRequestToDraft")
			}
		}
		return ok(map[string]any{"data": map[string]any{}})
	}
	if strings.HasPrefix(rest, "pulls/") {
		p := strings.TrimPrefix(strings.SplitN(rest, "?", 2)[0], "pulls/")
		num, tail, _ := strings.Cut(p, "/")
		n, err := strconv.Atoi(num)
		if err == nil {
			if tail != "" {
				tail = "/" + tail
			}
			pr := s.prs[n]
			switch {
			case tail == "" && method == "GET":
				head := pr["head"].(map[string]any)
				head["sha"] = s.heads[head["ref"].(string)]
				return ok(deepCopy(pr))
			case tail == "" && method == "PATCH":
				for k, v := range b.(map[string]any) {
					pr[k] = v
				}
				return ok(deepCopy(pr))
			case tail == "/requested_reviewers":
				return ok(map[string]any{})
			case tail == "/reviews":
				return ok(deepCopy(s.reviews))
			case tail == "/comments":
				return ok(deepCopy(s.comments))
			case tail == "/merge":
				if s.refuseMerges > 0 {
					s.refuseMerges--
					return refuse(405, `Required status check "test" is expected.`)
				}
				pr["state"], pr["merged"] = "closed", true
				return ok(map[string]any{"merged": true, "sha": strings.Repeat("f", 40)})
			}
		}
	}
	if strings.HasPrefix(rest, "issues/") && strings.HasSuffix(rest, "/comments") {
		return ok(map[string]any{})
	}
	if method == "DELETE" && strings.HasPrefix(rest, "git/refs/heads/") {
		return ok(map[string]any{})
	}
	s.unscriptedErr = append(s.unscriptedErr, method+" "+path)
	return refuse(500, "unscripted GitHub request "+method+" "+path)
}

func toAny(v jsonx.Value) any {
	if v.IsNull() {
		return nil
	}
	var out any
	if err := json.Unmarshal(jsonx.EncodePython(v, -1), &out); err != nil {
		panic(err)
	}
	return out
}

func deepCopy(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// tokenSource hands out a fixed installation token.
type tokenSource struct {
	token string
	err   error
	n     int
}

func (s *tokenSource) Token(context.Context) (string, error) {
	s.n++
	return s.token, s.err
}

// fakeGit is a project's branches: their heads and every push.
type fakeGit struct {
	heads   map[string]string
	pushes  []push
	pushErr error
	headErr error
}

type push struct{ Repo, Branch, Token string }

func (g *fakeGit) Head(_ context.Context, _ config.Project, branch string) (string, error) {
	if g.headErr != nil {
		return "", g.headErr
	}
	h, ok := g.heads["refs/heads/"+branch]
	if !ok {
		return "", fmt.Errorf("[git -C /repo rev-parse] failed (128): fatal: ambiguous argument 'refs/heads/%s'", branch)
	}
	return h, nil
}

func (g *fakeGit) BranchExists(_ context.Context, _ config.Project, branch string) (bool, error) {
	_, ok := g.heads["refs/heads/"+branch]
	return ok, nil
}

func (g *fakeGit) Push(_ context.Context, p config.Project, branch, token string) error {
	g.pushes = append(g.pushes, push{Repo: p.GitHub, Branch: branch, Token: token})
	return g.pushErr
}

// fakeChecks is the merge queue's check (gate.Runner.StillHoldsOnMain).
// With run set, it starts the checks (in.Started) and then calls run.
type fakeChecks struct {
	ok    bool
	out   string
	err   error
	calls int
	run   func()
}

func (c *fakeChecks) StillHoldsOnMain(_ context.Context, in gate.HoldIn) (bool, string, error) {
	c.calls++
	if c.run != nil {
		if in.Started != nil {
			in.Started()
		}
		c.run()
	}
	return c.ok, c.out, c.err
}

// memBoard is the board in memory, with store.Board's semantics: a save
// stamps updated and writes the queued log lines; EachLocked re-reads each
// wanted ticket and hands a clone to fn.
type memBoard struct {
	clock    clock.Clock
	files    map[ticket.ID][]byte
	logs     []string
	saves    []ticket.ID
	saveErr  error
	changing []string
}

func newBoard(c clock.Clock) *memBoard { return &memBoard{clock: c, files: map[ticket.ID][]byte{}} }

func (b *memBoard) Save(t *ticket.Ticket) error {
	if b.saveErr != nil {
		return b.saveErr
	}
	t.Updated = clock.Stamp(b.clock.Now())
	b.files[t.ID] = ticket.Encode(t)
	b.logs = append(b.logs, t.TakeLogs()...)
	b.saves = append(b.saves, t.ID)
	return nil
}

func (b *memBoard) load(tb testing.TB, id ticket.ID) *ticket.Ticket {
	tb.Helper()
	raw, ok := b.files[id]
	if !ok {
		tb.Fatalf("no ticket %s", id)
	}
	t, _, err := ticket.Decode(raw)
	if err != nil {
		tb.Fatal(err)
	}
	return t
}

func (b *memBoard) put(tb testing.TB, t *ticket.Ticket) {
	tb.Helper()
	b.files[t.ID] = ticket.Encode(t)
}

func (b *memBoard) all(tb testing.TB) []*ticket.Ticket {
	tb.Helper()
	ids := make([]string, 0, len(b.files))
	for id := range b.files {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	out := make([]*ticket.Ticket, len(ids))
	for i, id := range ids {
		out[i] = b.load(tb, ticket.ID(id))
	}
	return out
}

func (b *memBoard) EachLocked(ctx context.Context, ts []*ticket.Ticket, pred func(*ticket.Ticket) bool, fn func(*ticket.Ticket) error) error {
	for i, t := range ts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !pred(t) {
			continue
		}
		fresh := t
		if raw, ok := b.files[t.ID]; ok {
			d, _, err := ticket.Decode(raw)
			if err != nil {
				return err
			}
			fresh = d
		}
		ts[i] = fresh
		if !pred(fresh) {
			continue
		}
		work := ticket.Clone(fresh)
		if err := fn(work); err != nil {
			return err
		}
		ts[i] = work
	}
	return nil
}

func (b *memBoard) Change(ctx context.Context, name string, fn func(*ticket.Ticket) error) error {
	b.changing = append(b.changing, name)
	id, ok := ticket.ParseID(name)
	if !ok {
		return fmt.Errorf("no ticket %s", name)
	}
	raw, ok := b.files[id]
	if !ok {
		return fmt.Errorf("no ticket %s", id)
	}
	t, _, err := ticket.Decode(raw)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.Save(t)
}

// logger records the factory log.
type logger struct{ lines []string }

func (l *logger) Printf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// configSource is a config.Source stand-in.
type configSource struct{ cfg *config.Config }

func (s configSource) Current() (*config.Config, error) { return s.cfg, nil }

// harness is one test's flow with its fakes.
type harness struct {
	t      *testing.T
	clock  *testkit.Step
	gh     *fakeGitHub
	git    *fakeGit
	checks *fakeChecks
	board  *memBoard
	log    *logger
	token  *tokenSource
	flow   *Flow
	logs   string
}

// start is the clock the tests run at.
var startTime = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, clock: testkit.NewStep(startTime, 0), gh: &fakeGitHub{}, git: &fakeGit{heads: map[string]string{}},
		checks: &fakeChecks{ok: true}, log: &logger{}, token: &tokenSource{token: "ghs_secret"}, logs: t.TempDir()}
	h.board = newBoard(h.clock)
	h.flow = &Flow{
		GitHub:  &github.Client{BaseURL: "https://api.github.test", HTTP: h.gh, Auth: h.token},
		Git:     h.git,
		Checks:  h.checks,
		Board:   h.board,
		LogsDir: h.logs,
		Clock:   h.clock,
		Log:     h.log,
	}
	return h
}

func (h *harness) routes(rs ...route) { h.gh.routes = rs }

// cfg is the config of GithubOutTest: the App, one reviewer and a footer,
// and the project gem on o/gem.
const testConfig = `
[github]
app_client_id = "Iv23test"
installation_id = 42
reviewers = ["maintainer-login"]

[identity]
pr_footer = "Opened by a bot."

[projects.gem]
path = "/nonexistent/gem"
base = "main"
github = "o/gem"
install = "bundle install"
test = "bundle exec rake test"
lint = "bundle exec standardrb"

[projects.local]
path = "/nonexistent/local"
base = "main"
`

func parseConfig(t testing.TB, text string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func project(t testing.TB, cfg *config.Config, key string) config.Project {
	t.Helper()
	p, ok := cfg.Project(key)
	if !ok {
		t.Fatalf("no project %s", key)
	}
	return *p
}

func decode(t testing.TB, text string) *ticket.Ticket {
	t.Helper()
	tk, problems, err := ticket.Decode([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("problems decoding the fixture: %v", problems)
	}
	return tk
}

func encodeAny(t testing.TB, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func sha(c byte) string { return strings.Repeat(string(c), 40) }

// readyTicket is GithubOutTest.ready_ticket: through the gate, waiting for
// its PR, on bot/x at head.
func readyTicket(t testing.TB, auto bool, head string) *ticket.Ticket {
	t.Helper()
	blockers := []string{}
	if !auto {
		blockers = []string{"12 changed lines (limit 10)"}
	}
	return decode(t, `{"id": "T0042", "project": "gem", "title": "Tidy README", "body": "", "door": "github",
		"column": "ready_for_pr", "branch": "bot/x", "history": [], "source": {"issue": 7},
		"reports": {"work": {"summary": "Clarify the install steps."},
		            "review": {"verdict": "pass", "runtime_behaviour": "no"},
		            "security": {"answers": [{"concern": "none"}]}},
		"security_tier": 0,
		"gate": {"auto_eligible": `+strconv.FormatBool(auto)+`, "blockers": `+encodeAny(t, blockers)+`, "head": "`+head+`"}}`)
}

// prTicket is GithubOutTest.pr_ticket: its PR #5 open, the gate at a*40.
func prTicket(t testing.TB, auto bool) *ticket.Ticket {
	t.Helper()
	tk := readyTicket(t, auto, sha('a'))
	tk.Column = "pr_open"
	tk.PR = ticket.Some(ticket.PR{Number: 5, URL: "https://github.com/o/gem/pull/5", Draft: ticket.Some(!auto)})
	return tk
}

func lastEntry(t *ticket.Ticket) ticket.Entry {
	if len(t.History) == 0 {
		return ticket.Entry{}
	}
	return t.History[len(t.History)-1]
}

func details(t *ticket.Ticket, event string) []string {
	var out []string
	for _, e := range t.History {
		if e.Event == event {
			out = append(out, e.Detail)
		}
	}
	return out
}

var errNoAnswer = errors.New("dial tcp: connection refused")
