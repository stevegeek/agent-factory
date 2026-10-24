package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/logx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// baseConfig is config/config.toml on the test config repo's main. The
// two comment lines in alpha are where a draft's context sits, so that a
// later main can move them under [gate].
const baseConfig = `max_agents = 2

[host]
factory_home = "/home/factory"

[gate]
max_changed_lines = 10

[projects.alpha]
path = "/home/factory/projects/alpha"
github = "example/alpha"
base = "main"
test = "rake"
# operator-zone
# end-zone

[[schedules]]
name = "bug-hunt"
every_days = 30
title = "Hunt"
body = "Look"
projects = ["alpha"]

[[schedules]]
name = "tidy"
every_days = 3
title = "Tidy"
body = "Tidy up"
`

const testRepo = "example/config"

var testNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// fixture is a factory root, a config repo (a seed work tree that pushes
// to a bare origin, and the factory's clone of it), a fake GitHub that
// checks each merge against the head pushed to the bare origin, and a
// recording deployer.
type fixture struct {
	t      *testing.T
	p      paths.Paths
	board  *store.Board
	seed   *testkit.Repo
	bare   string
	clone  string
	env    []string
	gh     *fakeGitHub
	dep    *fakeDeployer
	sw     *fakeSwitcher
	mu     sync.Mutex
	pushed []string // refs pushed to "GitHub"
	runs   [][]string
	sleeps int
	// unitThere and unitFails make systemd-run refuse the unit: as one
	// already there, or with no bus
	unitThere, unitFails bool
	cfg                  *config.Config
	s                    Service
}

type noGuard struct{}

func (noGuard) Check(context.Context, string, string) error { return nil }

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tk := testkit.Root(t)
	f := &fixture{t: t, p: paths.FromEnv(tk.Vars)}
	f.env = tk.Environ()
	f.seed = testkit.NewRepo(t)
	f.seed.Write("config/config.toml", baseConfig)
	f.seed.Write("config/rules.md", "# Rules\n")
	f.seed.Commit("config")
	f.bare = filepath.Join(t.TempDir(), "origin.git")
	f.seed.Git("clone", "-q", "--bare", f.seed.Dir, f.bare)
	f.seed.Git("remote", "add", "origin", f.bare)
	f.clone = filepath.Join(tk.Root, "projects", "config-repo")
	f.seed.Git("clone", "-q", f.bare, f.clone)

	f.gh = newFakeGitHub(t, f.bare)
	f.dep = &fakeDeployer{}
	f.sw = &fakeSwitcher{}
	f.cfg = f.config(true, true)
	clk := clock.Fixed{T: testNow}
	f.board = &store.Board{Reader: &boardread.Reader{Paths: f.p}, Paths: f.p, Clock: clk}
	ex := execx.Func(f.run)
	f.s = Service{
		Paths:  f.p,
		Board:  f.board,
		GitHub: &github.Client{BaseURL: "https://api.github.test", Auth: staticToken("tok"), HTTP: f.gh},
		Token:  func(context.Context) (string, error) { return "tok", nil },
		Repos: func(p config.Project) *gitread.Repo {
			return &gitread.Repo{Path: p.Path, Base: p.Base, GitHub: p.GitHub, Exec: ex, Env: f.env, Guard: noGuard{}}
		},
		Exec:     ex,
		Deploys:  f.dep,
		Switches: f.sw,
		Clock:    clk,
		Log:      &logx.Log{Dir: f.p.Logs, Clock: clk},
		Env:      map[string]string{"XDG_RUNTIME_DIR": "/run/user/1", "PATH": "/usr/bin", "FACTORY_X": "1"},
		Sleep:    func(context.Context, time.Duration) error { f.sleeps++; return nil },
	}
	f.board.Log = f.s.Log
	return f
}

// config is the running config: the config project and [deploy] when
// asked for.
func (f *fixture) config(project, deploy bool) *config.Config {
	f.t.Helper()
	src := `[host]
factory_home = "/home/factory"

[identity]
git_name = "Factory Bot"
git_email = "bot@example.com"
pr_footer = "Opened by the factory."

[hosts.helper1]
ssh = "factory@helper1"

[projects.alpha]
path = "/home/factory/projects/alpha"
github = "example/alpha"
base = "main"

[projects.beta]
path = "/home/factory/projects/beta"
github = "example/beta"
base = "main"
enabled = false

[projects.board1]
mode = "board"
`
	if deploy {
		src += "\n[deploy]\ncore = \"example/core\"\nconfig = \"" + testRepo + "\"\n"
	}
	if project {
		src += "\n[projects.config-repo]\npath = \"" + f.clone + "\"\ngithub = \"" + testRepo + "\"\nbase = \"main\"\nself_deploy = \"config\"\n"
	}
	cfg, err := config.Parse([]byte(src))
	if err != nil {
		f.t.Fatal(err)
	}
	return cfg
}

// run runs commands for real, except that a push to GitHub goes to the
// bare origin (and is recorded) and systemd-run is only recorded.
func (f *fixture) run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	f.mu.Lock()
	f.runs = append(f.runs, c.Argv)
	f.mu.Unlock()
	if len(c.Argv) > 0 && c.Argv[0] == "systemd-run" {
		switch {
		case f.unitThere:
			return execx.Result{Argv: c.Argv, Code: 1, Stderr: []byte("Failed to start transient service unit: Unit factory-rebuild-now.service was already loaded or has a fragment file.")}, nil
		case f.unitFails:
			return execx.Result{Argv: c.Argv, Code: 1, Stderr: []byte("Failed to connect to bus: No medium found")}, nil
		}
		return execx.Result{Argv: c.Argv}, nil
	}
	argv := slices.Clone(c.Argv)
	if i := slices.Index(argv, "push"); i >= 0 {
		for j := i; j < len(argv); j++ {
			if argv[j] == "https://github.com/"+testRepo+".git" {
				argv[j] = f.bare
				if j+1 < len(argv) {
					f.mu.Lock()
					f.pushed = append(f.pushed, argv[j+1])
					f.mu.Unlock()
				}
			}
		}
	}
	c.Argv = argv
	return execx.OS{}.Run(ctx, c)
}

// moveMain commits files on the seed and pushes them to origin main.
func (f *fixture) moveMain(files map[string]string) {
	f.t.Helper()
	for p, text := range files {
		f.seed.Write(p, text)
	}
	f.seed.Commit("main moves")
	f.seed.Git("push", "-q", "origin", "main")
}

// patch is the diff from main's files to these, with one line of context.
func (f *fixture) patch(files map[string]string) string {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "w")
	f.git("", "clone", "-q", f.bare, dir)
	for p, text := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.git(dir, "add", "-A")
	return f.git(dir, "diff", "--cached", "-U1") + "\n"
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = f.env
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimRight(out.String(), "\n")
}

// edit is baseConfig with one replacement.
func edit(t *testing.T, from, to string) string {
	t.Helper()
	if !strings.Contains(baseConfig, from) {
		t.Fatalf("baseConfig has no %q", from)
	}
	return strings.Replace(baseConfig, from, to, 1)
}

// autoPatch spaces bug-hunt out (auto); tapPatch raises max_agents (tap);
// neverPatch adds a gate mount (only the maintainer).
func (f *fixture) autoPatch() string {
	return f.patch(map[string]string{"config/config.toml": edit(f.t, "every_days = 30", "every_days = 60")})
}

func (f *fixture) tapPatch() string {
	return f.patch(map[string]string{"config/config.toml": edit(f.t, "max_agents = 2", "max_agents = 3")})
}

func (f *fixture) neverPatch() string {
	return f.patch(map[string]string{"config/config.toml": edit(f.t, "max_changed_lines = 10\n", "max_changed_lines = 10\nmounts = [\"/home/factory/.config/factory:/k\"]\n")})
}

// ticket files the maintainer's manager ticket, waiting on the desk with
// these items.
func (f *fixture) ticket(ds []ticket.Draft, as []ticket.Action) *ticket.Ticket {
	f.t.Helper()
	tk, err := f.board.FileTicket(context.Background(), ticket.NewIn{Project: model.NoticeProject, Title: "Tidy the schedules",
		Door: string(model.DoorHuman), Column: string(model.ColNeedsHuman), By: "maintainer"})
	if err != nil {
		f.t.Fatal(err)
	}
	tk.Kind = ticket.Some(string(model.KindManage))
	tk.Drafts, tk.Actions = ds, as
	if err := f.board.Save(tk); err != nil {
		f.t.Fatal(err)
	}
	return tk
}

func draftOf(title, patch, state string) ticket.Draft {
	return ticket.Draft{Title: title, Why: "because " + title, Patch: patch, Run: 1, State: state}
}

// settle runs one pass over the tickets and returns them as saved.
func (f *fixture) settle(ts ...*ticket.Ticket) []*ticket.Ticket {
	f.t.Helper()
	if err := f.s.Settle(context.Background(), f.cfg, ts); err != nil {
		f.t.Fatalf("Settle: %v", err)
	}
	return f.reload(ts...)
}

func (f *fixture) reload(ts ...*ticket.Ticket) []*ticket.Ticket {
	f.t.Helper()
	out := make([]*ticket.Ticket, len(ts))
	for i, t := range ts {
		got, err := f.board.Reader.Load(t.ID)
		if err != nil {
			f.t.Fatal(err)
		}
		out[i] = got
	}
	return out
}

// request is the maintainer's tap as Foreman gives it: every waiting or
// failed item requested, and the settle move stamped the same.
func (f *fixture) request(t *ticket.Ticket) *ticket.Ticket {
	f.t.Helper()
	t = f.stamp(t)
	ticket.Move(t, string(model.ColNeedsHuman), model.ReasonOperatorSettled, "apply requested by maintainer", "", "maintainer", testNow)
	if err := f.board.Save(t); err != nil {
		f.t.Fatal(err)
	}
	return t
}

// stamp marks every waiting or failed item requested, with no tap: what a
// board edit, or a path that is not the maintainer's, could write.
func (f *fixture) stamp(t *ticket.Ticket) *ticket.Ticket {
	f.t.Helper()
	t = f.reload(t)[0]
	for i := range t.Drafts {
		if s := ticket.DraftState(t.Drafts[i]); s == model.ItemWaiting || s == model.ItemFailed {
			t.Drafts[i].State, t.Drafts[i].Requested = model.ItemRequested, clock.Stamp(testNow)
		}
	}
	for i := range t.Actions {
		if s := ticket.ActionState(t.Actions[i]); s == model.ItemWaiting || s == model.ItemFailed {
			t.Actions[i].State, t.Actions[i].Requested = model.ItemRequested, clock.Stamp(testNow)
		}
	}
	if err := f.board.Save(t); err != nil {
		f.t.Fatal(err)
	}
	return t
}

func (f *fixture) workLeft() []string {
	f.t.Helper()
	es, err := os.ReadDir(f.p.OperatorWork())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func (f *fixture) logText() string {
	b, _ := os.ReadFile(filepath.Join(f.p.Logs, logx.FileName))
	return string(b)
}

type fakeDeployer struct {
	whys []string
	bys  []string
	err  error
}

func (d *fakeDeployer) Offer(_ context.Context, _ *config.Config, why, by string) error {
	d.whys, d.bys = append(d.whys, why), append(d.bys, by)
	return d.err
}

type fakeSwitcher struct{ calls []string }

func (s *fakeSwitcher) SwitchProject(_ context.Context, key, state, by string) (string, error) {
	s.calls = append(s.calls, key+" "+state+" "+by)
	return key + " switched " + state, nil
}

// fakePR is one pull request on the fake GitHub.
type fakePR struct {
	Number  int64
	Title   string
	Head    string
	Base    string
	BaseSHA string // main when the PR was opened
	State   string
	polls   int
}

// fakeGitHub serves the pull request calls the operator makes. Mergeable
// is the mergeable_state sequence a new PR shows on each read (the last
// repeats); MergeFails is how many merges are refused with a 405 (-1:
// every one). A merge whose sha is not the head on the bare origin is a
// 409, as GitHub does when the head moved.
type fakeGitHub struct {
	t          *testing.T
	bare       string
	mu         sync.Mutex
	prs        []*fakePR
	creates    int
	merges     []string // "#n sha"
	deleted    []string
	Mergeable  []string
	MergeFails int
	BaseRef    string // the base a new PR reports; "" is the asked one
	// LateMerge lets a merge through while the mergeability is unknown
	// (the wait ran out; GitHub then decides).
	LateMerge bool
	// ExtraCommits is added to every PR's commit count; OnRead runs on
	// each read of a PR (with the PR's read count), and AfterMerge after a
	// merge, outside the lock.
	ExtraCommits int
	OnRead       func(polls int)
	AfterMerge   func()
}

func newFakeGitHub(t *testing.T, bare string) *fakeGitHub {
	return &fakeGitHub{t: t, bare: bare, Mergeable: []string{"clean"}}
}

// Do serves a request in process (a github.Doer), recorded by httptest.
func (g *fakeGitHub) Do(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	g.serve(rec, r)
	return rec.Result(), nil
}

func (g *fakeGitHub) prJSON(p *fakePR) map[string]any {
	state := g.Mergeable[min(p.polls, len(g.Mergeable)-1)]
	head := gitOut(g.bare, "rev-parse", "refs/heads/"+p.Head)
	commits, _ := strconv.Atoi(gitOut(g.bare, "rev-list", "--count", "refs/heads/main.."+head))
	return map[string]any{"number": p.Number, "title": p.Title, "html_url": "https://github.com/" + testRepo + "/pull/" + strconv.FormatInt(p.Number, 10),
		"state": p.State, "draft": false, "mergeable_state": state, "node_id": "N" + strconv.FormatInt(p.Number, 10),
		"commits": commits + g.ExtraCommits,
		"head":    map[string]any{"ref": p.Head, "sha": head}, "base": map[string]any{"ref": p.Base, "sha": p.BaseSHA}}
}

// mergeMain merges head into the bare origin's main as GitHub's merge
// button does (a merge commit), and is the merge commit; "" on a conflict.
func (g *fakeGitHub) mergeMain(n int, head string) string {
	main := gitOut(g.bare, "rev-parse", "refs/heads/main")
	tree := gitOut(g.bare, "merge-tree", "--write-tree", main, head)
	if tree == "" {
		return ""
	}
	sha := gitOut(g.bare, "-c", "user.name=GitHub", "-c", "user.email=noreply@github.com",
		"commit-tree", tree, "-p", main, "-p", head, "-m", fmt.Sprintf("Merge pull request #%d", n))
	if sha == "" || exec.Command("git", "--git-dir="+g.bare, "update-ref", "refs/heads/main", sha, main).Run() != nil {
		return ""
	}
	return sha
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	locked := true
	defer func() {
		if locked {
			g.mu.Unlock()
		}
	}()
	pre := "/repos/" + testRepo + "/pulls"
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == "GET" && r.URL.Path == pre:
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "example:")
		out := []any{}
		for _, p := range g.prs {
			if p.State == "open" && p.Head == head {
				out = append(out, g.prJSON(p))
			}
		}
		reply(200, out)
	case r.Method == "POST" && r.URL.Path == pre:
		var in struct{ Title, Body, Head, Base string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, p := range g.prs {
			if p.State == "open" && p.Head == in.Head {
				reply(422, map[string]any{"message": "A pull request already exists"})
				return
			}
		}
		g.creates++
		base := in.Base
		if g.BaseRef != "" {
			base = g.BaseRef
		}
		p := &fakePR{Number: int64(len(g.prs) + 1), Title: in.Title, Head: in.Head, Base: base, State: "open",
			BaseSHA: gitOut(g.bare, "rev-parse", "refs/heads/main")}
		g.prs = append(g.prs, p)
		reply(201, g.prJSON(p))
	case strings.HasPrefix(r.URL.Path, pre+"/"):
		rest := strings.TrimPrefix(r.URL.Path, pre+"/")
		num, tail, _ := strings.Cut(rest, "/")
		n, _ := strconv.Atoi(num)
		if n < 1 || n > len(g.prs) {
			reply(404, map[string]any{"message": "Not Found"})
			return
		}
		p := g.prs[n-1]
		switch {
		case r.Method == "GET" && tail == "":
			p.polls++
			if g.OnRead != nil {
				g.OnRead(p.polls)
			}
			reply(200, g.prJSON(p))
		case r.Method == "PUT" && tail == "merge":
			var in struct {
				SHA string `json:"sha"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if s := g.Mergeable[min(p.polls, len(g.Mergeable)-1)]; s == "unknown" && !g.LateMerge {
				g.t.Errorf("PR #%d merged while its mergeability is unknown", n)
			}
			head := gitOut(g.bare, "rev-parse", "refs/heads/"+p.Head)
			if in.SHA != head {
				reply(409, map[string]any{"message": "Head branch was modified"})
				return
			}
			if g.MergeFails != 0 {
				if g.MergeFails > 0 {
					g.MergeFails--
				}
				reply(405, map[string]any{"message": "Pull Request is not mergeable"})
				return
			}
			merge := g.mergeMain(n, in.SHA)
			if merge == "" {
				reply(405, map[string]any{"message": "Merge conflict"})
				return
			}
			g.merges = append(g.merges, fmt.Sprintf("#%d %s", n, in.SHA))
			p.State = "closed"
			reply(200, map[string]any{"merged": true, "message": "Pull Request successfully merged", "sha": merge})
			if g.AfterMerge != nil {
				locked = false
				g.mu.Unlock()
				g.AfterMerge()
			}
		default:
			reply(404, map[string]any{"message": "Not Found"})
		}
	case r.Method == "GET" && r.URL.Path == "/repos/"+testRepo+"/git/ref/heads/main":
		reply(200, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"type": "commit", "sha": gitOut(g.bare, "rev-parse", "refs/heads/main")}})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/"+testRepo+"/git/refs/heads/"):
		g.deleted = append(g.deleted, strings.TrimPrefix(r.URL.Path, "/repos/"+testRepo+"/git/refs/heads/"))
		w.WriteHeader(204)
	default:
		reply(404, map[string]any{"message": "Not Found"})
	}
}

func gitOut(gitDir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"--git-dir=" + gitDir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
