package prflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/testkit/pyticket"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The strongest parity check this package has: Python's real tick_pass,
// driven by tools/goldens.py with GitHub faked, wrote every ticket file of
// two scenarios step by step (internal/ticket/testdata/py/history-order).
// Here the GitHub pass replays the steps that are pure PR flow, from the
// files Python wrote just before, against the same fake GitHub, at the same
// time, and must write the same bytes, send the same requests and log the
// same lines.

const historyDir = "../ticket/testdata/py/history-order"

// lifeConfig is goldens.py's life_config, in TOML.
const lifeConfig = `
max_agents = 2
image = "localhost/factory-agent:dev"

[github]
installation_id = 1
app_client_id = "Iv1test"
open_prs = "auto"
reviewers = ["maintainer-login"]

[gate]
max_changed_lines = 50
allowed_paths = ["lib/*", "test/*"]

[triage.auto_accept]
github = true

[memory]
enabled = false

[identity]
pr_footer = "Opened by the factory."

[projects.gem]
path = "/nonexistent/gem"
base = "main"
github = "example/gem"
install = "bundle install"
test = "bundle exec rake test"
lint = "bundle exec standardrb"
`

type lifeStep struct {
	Name   string
	Now    string
	GitHub []request
	Log    []string
	Files  []string
}

type life struct {
	t     *testing.T
	dir   string
	steps []lifeStep
}

func loadLife(t *testing.T, scenario string) *life {
	t.Helper()
	dir := filepath.Join(historyDir, scenario)
	raw, err := os.ReadFile(filepath.Join(dir, "steps.json"))
	if err != nil {
		t.Skipf("no history goldens: %v", err)
	}
	v, err := jsonx.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	l := &life{t: t, dir: dir}
	steps, _ := v.Get("steps").Array()
	for _, s := range steps {
		st := lifeStep{}
		st.Name, _ = s.Get("step").Str()
		st.Now, _ = s.Get("now").Str()
		reqs, _ := s.Get("github").Array()
		for _, r := range reqs {
			m, _ := r.Get("method").Str()
			p, _ := r.Get("path").Str()
			st.GitHub = append(st.GitHub, request{Method: m, Path: p, Body: r.Get("body")})
		}
		logs, _ := s.Get("log").Array()
		for _, x := range logs {
			line, _ := x.Str()
			st.Log = append(st.Log, line)
		}
		files, _ := s.Get("changed").Array()
		for _, x := range files {
			f, _ := x.Str()
			st.Files = append(st.Files, f)
		}
		l.steps = append(l.steps, st)
	}
	return l
}

func (l *life) step(name string) (int, lifeStep) {
	for i, s := range l.steps {
		if strings.HasSuffix(s.Name, "-"+name) {
			return i, s
		}
	}
	l.t.Fatalf("no step %s", name)
	return 0, lifeStep{}
}

// file is the ticket file as Python last wrote it at or before step i.
func (l *life) file(i int, name string) []byte {
	l.t.Helper()
	for ; i >= 0; i-- {
		raw, err := os.ReadFile(filepath.Join(l.dir, l.steps[i].Name, name))
		if err == nil {
			return raw
		}
	}
	l.t.Fatalf("no %s up to step %d", name, i)
	return nil
}

// lifeHarness is a flow at the step's time over a stateful fake GitHub.
func lifeHarness(t *testing.T, gh *stateGitHub, now string) *harness {
	t.Helper()
	h := newHarness(t)
	at, err := clock.Parse(now)
	if err != nil {
		t.Fatal(err)
	}
	h.flow.Clock, h.board.clock = fixedClock(at), fixedClock(at)
	h.gh.state = gh
	return h
}

func (h *harness) loadFile(raw []byte) *ticket.Ticket {
	h.t.Helper()
	tk, _, err := ticket.Decode(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	h.board.files[tk.ID] = raw
	return tk
}

// replay sends recorded requests to the fake, to bring its PRs to the state
// they had at that point.
func replay(gh *stateGitHub, reqs []request) {
	for _, r := range reqs {
		gh.answer(r.Method, r.Path, r.Body)
	}
}

func (h *harness) checkStep(l *life, i int, names ...string) {
	h.t.Helper()
	s := l.steps[i]
	for _, name := range names {
		want := l.file(i, name)
		id := ticket.ID(strings.TrimSuffix(name, ".json"))
		if got := pyticket.History(string(h.board.files[id])); got != string(want) {
			h.t.Errorf("%s/%s differs (-python +go):\n%s", s.Name, name, testkit.Diff(string(want), got))
		}
	}
	got := h.gh.calls()
	if len(got) != len(s.GitHub) {
		h.t.Errorf("%s: requests %v, python sent %v", s.Name, got, s.GitHub)
	} else {
		for k := range got {
			if got[k].String() != s.GitHub[k].String() || !jsonx.Equal(got[k].Body, s.GitHub[k].Body) {
				h.t.Errorf("%s: request %d: %s %s, python %s %s", s.Name, k, got[k], jsonx.EncodePython(got[k].Body, -1),
					s.GitHub[k], jsonx.EncodePython(s.GitHub[k].Body, -1))
			}
		}
	}
	var mine []string
	for _, line := range s.Log {
		for _, name := range names {
			if strings.HasPrefix(line, strings.TrimSuffix(name, ".json")+" ") {
				mine = append(mine, line)
			}
		}
	}
	if strings.Join(h.board.logs, "\n") != strings.Join(mine, "\n") {
		h.t.Errorf("%s: log\n%s\npython\n%s", s.Name, strings.Join(h.board.logs, "\n"), strings.Join(mine, "\n"))
	}
}

func gateHead(t *testing.T, raw []byte) (branch, head string) {
	t.Helper()
	tk, _, err := ticket.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Branch.Or(""), tk.Gate.V.Head
}

// A change through its PR: opened, approved while checks were pending, then
// merged on the go-ahead.
func TestHistoryAChangeThroughItsPR(t *testing.T) {
	l := loadLife(t, "change-through-a-pr")
	heads := map[string]string{}
	gh := newStateGitHub(heads)
	cfg := parseConfig(t, lifeConfig)

	// 06: the gate passed last tick; the PR opens
	i, s := l.step("pr-opens")
	h := lifeHarness(t, gh, s.Now)
	tk := h.loadFile(l.file(i-1, "T0001.json"))
	branch, head := tk.Branch.V, tk.Gate.V.Head
	heads[branch] = head
	h.setHead(branch, head)
	if err := h.flow.Pass(context.Background(), cfg, h.board.all(t)); err != nil {
		t.Fatal(err)
	}
	h.checkStep(l, i, "T0001.json")

	// the reviews of step 07 and the rework of step 10, as Python's run made them
	// (a GET only refreshes the PR's head sha, so the final head will do)
	_, heads[branch] = gateHead(t, l.file(l.mustStep("security-gate-pr-updated"), "T0001.json"))
	for _, name := range []string{"changes-requested-work-restarts", "security-gate-pr-updated"} {
		_, st := l.step(name)
		replay(gh, st.GitHub)
	}
	gh.reviews = []map[string]any{
		{"id": 101, "state": "CHANGES_REQUESTED", "user": map[string]any{"login": "Maintainer-Login"}, "body": "Please add a test.", "commit_id": head},
		{"id": 99, "state": "COMMENTED", "user": map[string]any{"login": "maintainer-login"}, "body": "hm"},
		{"id": 100, "state": "APPROVED", "user": map[string]any{"login": "someone-else"}, "body": ""},
		{"id": 102, "state": "APPROVED", "user": map[string]any{"login": "maintainer-login"}, "body": "", "commit_id": heads[branch]},
	}
	gh.refuseMerges = 1

	// 11: your approval, while the required checks still run
	i, s = l.step("approved-checks-pending")
	h = lifeHarness(t, gh, s.Now)
	h.loadFile(l.file(i-1, "T0001.json"))
	h.setHead(branch, heads[branch])
	if err := h.flow.Pass(context.Background(), cfg, h.board.all(t)); err != nil {
		t.Fatal(err)
	}
	h.checkStep(l, i, "T0001.json")

	// 13: the go-ahead (12, cmd_merge) is merged by the tick
	i, s = l.step("merged")
	h = lifeHarness(t, gh, s.Now)
	h.loadFile(l.file(i-1, "T0001.json"))
	h.setHead(branch, heads[branch])
	if err := h.flow.Pass(context.Background(), cfg, h.board.all(t)); err != nil {
		t.Fatal(err)
	}
	h.checkStep(l, i, "T0001.json")
	if len(gh.unscriptedErr) > 0 {
		t.Errorf("unscripted requests: %v", gh.unscriptedErr)
	}
}

func (l *life) mustStep(name string) int {
	i, _ := l.step(name)
	return i
}
