package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The fakes the pipeline's tests share. Git is real (testkit.NewRepo, the
// Python suite's RepoCase); the board is the real store on a temp root;
// Scion, the helper hosts, the gate's containers, GitHub and the doors are
// small in-memory fakes that record what the tick asked of them.

// nowClock is a clock a test moves by hand.
type nowClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *nowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *nowClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// logBuf is the factory log.
type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) Printf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logBuf) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// calls counts every adapter call, for the cancel injection: hook runs
// before the call is answered.
type calls struct {
	mu   sync.Mutex
	log  []string
	hook func(op string)
}

func (c *calls) note(op string) {
	c.mu.Lock()
	c.log = append(c.log, op)
	hook := c.hook
	c.mu.Unlock()
	if hook != nil {
		hook(op)
	}
}

func (c *calls) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log...)
}

func (c *calls) count(prefix string) int {
	n := 0
	for _, s := range c.names() {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

type removal struct {
	Name       string
	KeepBranch bool
}

// fakeLocal is this host's Scion.
type fakeLocal struct {
	c        *calls
	mu       sync.Mutex
	running  map[string]bool
	runErr   error
	onList   func(n int) // called with the number of the Running call
	lists    int
	reports  map[string][]byte
	quiet    map[string]bool
	screens  map[string]string
	startErr error
	onStart  func(j hostrun.Job)
	started  []hostrun.Job
	removed  []removal
	dropped  []string
	// alive: a started agent joins the running set
	alive bool
}

func newLocal(c *calls) *fakeLocal {
	return &fakeLocal{c: c, running: map[string]bool{}, reports: map[string][]byte{}, quiet: map[string]bool{}, screens: map[string]string{}}
}

func (f *fakeLocal) Running(ctx context.Context, cfg *config.Config) (map[string]bool, error) {
	f.c.note("local.running")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.lists++
	n, hook := f.lists, f.onList
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return nil, f.runErr
	}
	out := map[string]bool{}
	for k, v := range f.running {
		out[k] = v
	}
	return out, nil
}

func (f *fakeLocal) Report(name string) ([]byte, bool, error) {
	f.c.note("local.report")
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.reports[name]
	return b, ok, nil
}

func (f *fakeLocal) Start(ctx context.Context, j hostrun.Job) error {
	f.c.note("local.start")
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.onStart != nil {
		f.onStart(j)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, j)
	if f.startErr != nil {
		return f.startErr
	}
	if f.alive {
		f.running[j.Name] = true
	}
	return nil
}

func (f *fakeLocal) Remove(ctx context.Context, dir string, id ticket.ID, name string, keepBranch bool) error {
	f.c.note("local.remove")
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, removal{name, keepBranch})
	delete(f.running, name)
	delete(f.reports, name)
	return nil
}

func (f *fakeLocal) Quiet(name string, d time.Duration) bool {
	f.c.note("local.quiet")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quiet[name]
}

func (f *fakeLocal) Look(ctx context.Context, dir, name string, lines int) (string, error) {
	f.c.note("local.look")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screens[name], nil
}

func (f *fakeLocal) DropRunConfig(name string) error {
	f.c.note("local.drop")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, name)
	return nil
}

// fakeHelper is one helper host.
type fakeHelper struct {
	c        *calls
	name     string
	mu       sync.Mutex
	startErr error
	started  []hostrun.Job
	state    string
	pollErr  error
	files    map[string][]byte
	removed  []string
	removeEr error
	onRemove func(id ticket.ID, name string) // what the removal archives (the usage a helper sends back)
	away     bool                            // a pull host that has not called in
	capacity *int                            // a pull host's running workers, when it reports fewer than its slots
	gaveBack string                          // why a pull host gave its job back ("" for none)
}

func (f *fakeHelper) Start(ctx context.Context, j hostrun.Job) error {
	f.c.note(f.name + ".start")
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, j)
	return f.startErr
}

func (f *fakeHelper) Poll(ctx context.Context, name string) (string, error) {
	f.c.note(f.name + ".poll")
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.pollErr
}

func (f *fakeHelper) Collect(ctx context.Context, id ticket.ID, name string) (*hostrun.Result, error) {
	f.c.note(f.name + ".collect")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &hostrun.Result{Files: f.files}, nil
}

func (f *fakeHelper) Remove(ctx context.Context, id ticket.ID, name string) error {
	f.c.note(f.name + ".remove")
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	if f.onRemove != nil && f.removeEr == nil {
		f.onRemove(id, name)
	}
	return f.removeEr
}

type fakeHelpers map[string]*fakeHelper

func (f fakeHelpers) Helper(cfg *config.Config, name string) (Helper, bool) {
	h, ok := f[name]
	if !ok {
		return nil, false
	}
	if _, known := cfg.HostNamed(name); !known {
		return nil, false
	}
	return h, true
}

// Away is the fake helpers marked away.
func (f fakeHelpers) Away(cfg *config.Config) []string {
	var away []string
	for _, h := range cfg.Hosts {
		if fh, ok := f[h.Name]; ok && fh.away {
			away = append(away, h.Name)
		}
	}
	return away
}

// Capacity is the fake helpers with a reported capacity.
func (f fakeHelpers) Capacity(cfg *config.Config) map[string]int {
	out := map[string]int{}
	for name, fh := range f {
		if fh.capacity != nil {
			out[name] = *fh.capacity
		}
	}
	return out
}

// GivenBack is the fake helper's give-back reason.
func (f fakeHelpers) GivenBack(cfg *config.Config, host, job string) (string, bool) {
	fh, ok := f[host]
	if !ok || fh.gaveBack == "" {
		return "", false
	}
	return fh.gaveBack, true
}

// downError is what hostrun gives for a helper that cannot be reached:
// its message is the helper's, and it is hostrun.ErrRemote.
type downError struct{ msg string }

func (e downError) Error() string        { return e.msg }
func (e downError) Is(target error) bool { return target == hostrun.ErrRemote }

func remoteDown(host string) error {
	return downError{"factory@" + host + " start failed (255): down"}
}

// fakeGate is Gate 1.
type fakeGate struct {
	c       *calls
	mu      sync.Mutex
	result  ticket.Gate
	err     error
	ran     []ticket.ID
	decided []gate.Remote
	kept    *gate.Remote // what Kept gives, nil for none
	keptErr error
}

// Kept is the early checks' result the fake was given, used once.
func (f *fakeGate) Kept(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (gate.Remote, bool, error) {
	f.c.note("gate.kept")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keptErr != nil {
		return gate.Remote{}, false, f.keptErr
	}
	if f.kept == nil {
		return gate.Remote{}, false, nil
	}
	k := *f.kept
	f.kept = nil
	return k, true, nil
}

// earlyStart is one start of the early checks.
type earlyStart struct {
	ID   ticket.ID
	Head string
}

// fakeEarly records the early checks the tick starts; running is the
// ones it says run now (nil for none), which a start sets when marks is
// set, as gate.Early's does.
type fakeEarly struct {
	c       *calls
	mu      sync.Mutex
	started []earlyStart
	err     error
	running *earlyStart
	marks   bool
}

func (f *fakeEarly) Running() (ticket.ID, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		return "", "", false
	}
	return f.running.ID, f.running.Head, true
}

func (f *fakeEarly) Start(ctx context.Context, id ticket.ID, head string) error {
	f.c.note("early.start")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, earlyStart{id, head})
	if f.marks && f.err == nil {
		f.running = &earlyStart{id, head}
	}
	return f.err
}

func (f *fakeGate) Run(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (ticket.Gate, error) {
	f.c.note("gate.run")
	if err := ctx.Err(); err != nil {
		return ticket.Gate{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, t.ID)
	return f.result, f.err
}

// Decide is the decision on checks a gate job ran: the fake's result, with
// the remote's checks and host.
func (f *fakeGate) Decide(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, in gate.Remote) (ticket.Gate, error) {
	f.c.note("gate.decide")
	if err := ctx.Err(); err != nil {
		return ticket.Gate{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decided = append(f.decided, in)
	g := f.result
	g.ChecksPass, g.Head, g.Base, g.Host = in.ChecksPass, in.Head, in.Base, in.Host
	return g, f.err
}

// fakeGitHub is the PR flow.
type fakeGitHub struct {
	c         *calls
	mu        sync.Mutex
	passes    int
	deadlines []time.Duration
	opened    []ticket.ID
	openErr   error
	passErr   error // what Pass returns
	clock     clock.Clock
	// cut ends the tick's context inside OpenPR (the deadline mid-push)
	cut context.CancelFunc
}

func (f *fakeGitHub) Pass(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error {
	f.c.note("github.pass")
	if f.passErr != nil {
		return f.passErr
	}
	if !cfg.HasGitHub() {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passes++
	if dl, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(dl))
	}
	return ctx.Err()
}

func (f *fakeGitHub) OpenPR(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (ticket.PR, error) {
	f.c.note("github.open")
	if f.cut != nil {
		f.cut()
	}
	if err := ctx.Err(); err != nil {
		return ticket.PR{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, t.ID)
	if f.openErr != nil {
		return ticket.PR{}, f.openErr
	}
	pr := t.PR.V
	pr.Draft = ticket.Some(false)
	t.PR = ticket.Some(pr)
	ticket.Move(t, string(model.ColPROpen), model.ReasonPROpened, "PR https://example.test/pull/1 updated", "", "", f.clock.Now())
	return pr, nil
}

// fakeDoors are the watches and the notices.
type fakeDoors struct {
	c        *calls
	board    *store.Board
	mu       sync.Mutex
	watches  int
	watchErr error
	notices  []string
}

func (f *fakeDoors) Watches(ctx context.Context, cfg *config.Config) error {
	f.c.note("doors.watches")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watches++
	return f.watchErr
}

func (f *fakeDoors) Notice(ctx context.Context, title, body string) (*ticket.Ticket, error) {
	f.c.note("doors.notice")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.notices {
		if n == title {
			return nil, nil
		}
	}
	f.notices = append(f.notices, title)
	return nil, nil
}

// realRepos is git on the projects' clones, as gate.Runner gives it.
type realRepos struct {
	env   []string
	guard *gitx.Guarder
}

func (r realRepos) Repo(p config.Project) *gitread.Repo {
	return &gitread.Repo{Path: p.Path, Base: p.Base, GitHub: p.GitHub, Exec: execx.OS{}, Env: r.env, Guard: r.guard}
}

// harness is one test's factory: a root, a project repository and the
// fakes.
type harness struct {
	t       *testing.T
	env     *testkit.Env
	paths   paths.Paths
	clock   *nowClock
	log     *logBuf
	board   *store.Board
	repo    *testkit.Repo
	calls   *calls
	local   *fakeLocal
	helpers fakeHelpers
	gate    *fakeGate
	early   *fakeEarly
	github  *fakeGitHub
	doors   *fakeDoors
	repos   realRepos
}

// defaultConfig is the config most tests run with: one factory project on
// the test repository, two local slots and one helper for triage and
// review.
const defaultConfig = `max_agents = 2
agent_timeout_minutes = 30

[projects.p]
path = "{repo}"
base = "main"
install = "true"
test = "true"
lint = "true"

[hosts.pi]
ssh = "factory@pi"
slots = 1
stages = ["triage", "review"]
`

func newHarness(t *testing.T) *harness {
	t.Helper()
	env := testkit.Root(t)
	vars := map[string]string{}
	for k, v := range env.Vars {
		vars[k] = v
	}
	p := paths.FromEnv(vars)
	c := &calls{}
	clk := &nowClock{t: testkit.Stamp()}
	log := &logBuf{}
	board := &store.Board{Reader: &boardread.Reader{Paths: p, Log: log}, Paths: p, Clock: clk, Log: log}
	h := &harness{
		t: t, env: env, paths: p, clock: clk, log: log, board: board, calls: c,
		local:   newLocal(c),
		helpers: fakeHelpers{"pi": {c: c, name: "pi", state: jobproto.StateRunning}},
		gate:    &fakeGate{c: c},
		early:   &fakeEarly{c: c},
		github:  &fakeGitHub{c: c, clock: clk},
		doors:   &fakeDoors{c: c, board: board},
	}
	h.repos = realRepos{env: env.Environ(), guard: gitx.NewGuarder(execx.OS{}, env.Environ(), nil)}
	h.repo = testkit.NewRepo(t)
	// the guard strips the clone's own identity from .git/config (it is not
	// one a plain clone has), so the test's commits after a tick use the
	// repository helper's global one
	h.repo.Git("config", "--global", "user.email", "t@example.com")
	h.repo.Git("config", "--global", "user.name", "t")
	h.repo.Git("config", "--global", "commit.gpgsign", "false")
	h.config(defaultConfig)
	return h
}

// config writes config.toml; {repo} stands for the project's repository.
func (h *harness) config(toml string) {
	h.t.Helper()
	h.env.WriteConfig(h.t, strings.ReplaceAll(toml, "{repo}", h.repo.Dir))
}

func (h *harness) deps() Deps {
	return Deps{
		Board:   h.board,
		Config:  &config.Source{Path: h.paths.Config, Switches: h.paths.ProjectSwitches(), Log: h.log},
		Paths:   h.paths,
		Clock:   h.clock,
		Log:     h.log,
		Local:   h.local,
		Screen:  h.local,
		Helpers: h.helpers,
		Git:     h.repos,
		Gate:    h.gate,
		Early:   h.early,
		GitHub:  h.github,
		Doors:   h.doors,
	}
}

func (h *harness) tick() {
	h.t.Helper()
	if err := Tick(context.Background(), h.deps()); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
}

// file files a ticket and applies change to it before it is saved again.
func (h *harness) file(project, title, column string, change func(t *ticket.Ticket)) ticket.ID {
	h.t.Helper()
	t, err := h.board.FileTicket(context.Background(), ticket.NewIn{Project: project, Title: title, Body: "b", Door: "human", Column: column})
	if err != nil {
		h.t.Fatal(err)
	}
	if change != nil {
		h.change(t.ID, change)
	}
	return t.ID
}

func (h *harness) change(id ticket.ID, change func(t *ticket.Ticket)) {
	h.t.Helper()
	err := h.board.Changing(context.Background(), string(id), store.NoWait, func(t *ticket.Ticket) error {
		change(t)
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) load(id ticket.ID) *ticket.Ticket {
	h.t.Helper()
	t, err := h.board.Reader.Load(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

func (h *harness) raw(id ticket.ID) string {
	h.t.Helper()
	b, err := os.ReadFile(h.paths.TicketFile(string(id)))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// agent puts a running agent on a ticket.
func agentOn(t *ticket.Ticket, name, stage, host, started string) {
	t.Agent = ticket.Some(ticket.Agent{Name: name, Stage: stage, Started: started, Host: host})
}

func stamp(t time.Time) string { return clock.Stamp(t) }

// last is the ticket's last history entry.
func last(t *ticket.Ticket) ticket.Entry {
	if len(t.History) == 0 {
		return ticket.Entry{}
	}
	return t.History[len(t.History)-1]
}

// events are the ticket's history entries of a kind.
func events(t *ticket.Ticket, kind string) []ticket.Entry {
	var out []ticket.Entry
	for _, e := range t.History {
		if e.Event == kind {
			out = append(out, e)
		}
	}
	return out
}

func moveReason(e ticket.Entry) model.Reason {
	if m, ok := e.Data.(ticket.Moved); ok {
		return m.Reason
	}
	if m, ok := e.Data.(*ticket.Moved); ok {
		return m.Reason
	}
	return ""
}

// lastMove is the ticket's last move.
func lastMove(t *ticket.Ticket) ticket.Entry {
	for i := len(t.History) - 1; i >= 0; i-- {
		if t.History[i].Event == "move" {
			return t.History[i]
		}
	}
	return ticket.Entry{}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lockHeld holds a ticket's lock for the test's whole life.
func (h *harness) lockHeld(id ticket.ID) func() {
	l, err := h.board.Lock(context.Background(), id, fsx.NoWait)
	if err != nil {
		h.t.Fatal(err)
	}
	return func() { _ = l.Unlock() }
}

var errBoom = errors.New("no space left on device")

func h0() time.Time { return testkit.Stamp() }

// gitIn runs git in dir with the test's environment and returns its
// trimmed output.
func (h *harness) gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, h.env.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h *harness) git(t *testing.T, args ...string) string { return h.gitIn(t, h.env.Root, args...) }

// mustConfig is the config the test wrote.
func mustConfig(t *testing.T, h *harness) *config.Config {
	t.Helper()
	cfg, err := config.Load(h.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
