package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// fakeDeploySh stands in for core's deploy.sh in the test core repo. It
// installs as the real one would: new binaries naming --commit (unless
// FAKE_DEPLOY=config-only, the binaries the host runs already), the
// config from --config, a new foreman.env. FAKE_DEPLOY=early fails before
// it changes anything, late after.
const fakeDeploySh = `#!/bin/bash
set -eu
commit="" config="" args="$*"
while [ $# -gt 0 ]; do
  case "$1" in --commit) commit=$2; shift ;; --config) config=$2; shift ;; esac
  shift
done
F=$HOME/factory
echo "fake deploy.sh: $commit $args"
[ "${FACTORY_DEPLOY_SELF:-}" = 1 ] || { echo "not a self-deploy"; exit 3; }
mode=${FAKE_DEPLOY:-ok}
if [ "$mode" = early ]; then echo "the builder is down"; exit 1; fi
if [ "$mode" != config-only ]; then
  printf '#!/bin/sh\necho "%s go1.test"\n' "$commit" >"$F/bin/factory.t" && chmod 755 "$F/bin/factory.t" && mv "$F/bin/factory.t" "$F/bin/factory"
  printf 'foreman %s\n' "$commit" >"$F/bin/foreman.t" && chmod 755 "$F/bin/foreman.t" && mv "$F/bin/foreman.t" "$F/bin/foreman"
  printf 'remote %s\n' "$commit" >"$F/bin/factory-remote.t" && chmod 755 "$F/bin/factory-remote.t" && mv "$F/bin/factory-remote.t" "$F/bin/factory-remote"
  (cd "$F/bin" && sha256sum factory foreman factory-remote | awk '{ print $1, $2 }' >BUILD.t && echo "commit $commit go go1.test" >>BUILD.t && mv BUILD.t BUILD)
fi
cp "$config/config.toml" "$F/config.toml"
echo "FOREMAN_ARGS=\"--new $commit\"" >"$F/foreman.env"
if [ "$mode" = late ]; then echo "install failed late"; exit 1; fi
exit 0
`

// fixture is a master with a factory root under HOME, a core repo (two
// commits on main) and a config repo served over file://, a fake Foreman
// whose /healthz names the commit in bin/BUILD (and fails while
// config.toml says "# broken"), and recording fakes for the rest.
type fixture struct {
	t       *testing.T
	p       paths.Paths
	env     map[string]string
	core    *testkit.Repo
	conf    *testkit.Repo
	a, b    string // core's two commits, 12 digits: the master runs a
	listen  string
	exec    *fakeExec
	notices *fakeNotices
	ssh     *fakeSSH
	cfg     *config.Config
	s       Service
	stays   atomic.Bool // the fake Foreman is not restarted by deploy.sh
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	tk := testkit.Root(t)
	env := map[string]string{}
	for k, v := range tk.Vars {
		env[k] = v
	}
	env["HOME"] = base
	env["FACTORY_ROOT"] = filepath.Join(base, "factory")
	env["PATH"] = os.Getenv("PATH")
	p := paths.FromEnv(env)
	for _, d := range []string{p.Tickets, p.Logs, p.Bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{t: t, p: p, env: env, exec: &fakeExec{}, notices: &fakeNotices{}, ssh: &fakeSSH{fail: map[string]string{}}}

	srv := httptest.NewServer(http.HandlerFunc(f.healthz))
	t.Cleanup(srv.Close)
	f.listen = strings.TrimPrefix(srv.URL, "http://")

	f.core = testkit.NewRepo(t)
	f.core.Write("deploy.sh", fakeDeploySh)
	f.core.Write("host/foreman-system.sh", "root\n")
	f.core.Write("README.md", "one\n")
	f.core.Commit("A")
	f.a = f.core.Git("rev-parse", "--short=12", "HEAD")
	f.core.Write("README.md", "two\n")
	f.core.Commit("B")
	f.b = f.core.Git("rev-parse", "--short=12", "HEAD")

	f.conf = testkit.NewRepo(t)
	f.conf.Write("config/config.toml", f.configToml(""))
	f.conf.Write("config/context/demo/INDEX.md", "# demo\n")
	f.conf.Commit("config")

	served := t.TempDir()
	if err := os.MkdirAll(filepath.Join(served, "example"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*testkit.Repo{"agent-factory": f.core, "factory-config": f.conf} {
		if err := os.Symlink(filepath.Join(r.Dir, ".git"), filepath.Join(served, "example", name+".git")); err != nil {
			t.Fatal(err)
		}
	}

	// the master runs a, with the config as it was
	f.install(f.a)
	f.write("config.toml", f.configToml(""))
	f.write("foreman.env", "FOREMAN_ARGS=\"--old\"\n")
	f.write("bin/pi-run", "#!/bin/sh\n")

	f.cfg = parseConfig(t, f.configToml(""))
	f.s = Service{
		Paths: p, Exec: f.exec, Clock: testkit.Fixed{T: testkit.Stamp()}, Log: &memLog{}, Notices: f.notices,
		Board: &store.Board{Reader: &boardread.Reader{Paths: p}, Paths: p, Clock: testkit.Fixed{T: testkit.Stamp()}},
		SSH:   f.ssh, Env: env,
		// a short wait per answer: a first connection can stall where a
		// firewall vets new programs
		HTTP:       &http.Client{Timeout: 500 * time.Millisecond},
		MirrorBase: "file://" + served + "/", HealthWait: 5 * time.Second, HealthEvery: 50 * time.Millisecond,
		TickLockWait: time.Second, RunLockWait: 100 * time.Millisecond, SystemUnit: filepath.Join(base, "no-system-unit"),
	}
	return f
}

func (f *fixture) configToml(extra string) string {
	return fmt.Sprintf(`[foreman]
listen = %q

[deploy]
core = "example/agent-factory"
config = "example/factory-config"

[hosts.helper1]
ssh = "factory@helper1"

[hosts.helper2]
ssh = "factory@helper2"

[hosts.desktop]
transport = "pull"
%s`, f.listen, extra)
}

// install puts commit's binaries in bin/ as a deploy would.
func (f *fixture) install(commit string) {
	f.t.Helper()
	bins := map[string]string{
		"factory":        fmt.Sprintf("#!/bin/sh\necho \"%s go1.test\"\n", commit),
		"foreman":        "foreman " + commit + "\n",
		"factory-remote": "remote " + commit + "\n",
	}
	var build strings.Builder
	for _, b := range []string{"factory", "foreman", "factory-remote"} {
		path := filepath.Join(f.p.Bin, b)
		if err := os.WriteFile(path+".t", []byte(bins[b]), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Rename(path+".t", path); err != nil {
			f.t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(bins[b]))
		fmt.Fprintf(&build, "%s %s\n", hex.EncodeToString(sum[:]), b)
	}
	fmt.Fprintf(&build, "commit %s go go1.test\n", commit)
	f.write("bin/BUILD", build.String())
}

func (f *fixture) write(rel, data string) {
	f.t.Helper()
	path := filepath.Join(f.p.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if strings.HasPrefix(rel, "bin/") && rel != "bin/BUILD" {
		mode = 0o755
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.p.Root, rel))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// healthz is the fake Foreman: ok, the commit bin/BUILD names and its
// start, unless config.toml is marked broken. It started an hour after
// the fixture's clock (after any deploy began), or, with stays set, an
// hour before until a systemctl restart (a restore's) restarts it.
func (f *fixture) healthz(w http.ResponseWriter, _ *http.Request) {
	conf, _ := os.ReadFile(filepath.Join(f.p.Root, "config.toml"))
	if strings.Contains(string(conf), "# broken") {
		http.Error(w, "broken", http.StatusServiceUnavailable)
		return
	}
	started := f.s.now().Add(time.Hour)
	if f.stays.Load() && !slices.ContainsFunc(f.exec.ran("systemctl"), func(argv []string) bool { return slices.Contains(argv, "restart") }) {
		started = f.s.now().Add(-time.Hour)
	}
	fmt.Fprintf(w, "ok\nversion %s\nstarted %s\n", buildCommit(f.p), started.UTC().Format(time.RFC3339))
}

// setEnv sets a variable deploy.sh sees.
func (f *fixture) setEnv(k, v string) { f.s.Env[k] = v }

func (f *fixture) state() deployrec.State {
	f.t.Helper()
	st, err := deployrec.ReadState(f.p.DeployState())
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

// runRequest writes a request and runs it.
func (f *fixture) runRequest(r deployrec.Request) deployrec.State {
	f.t.Helper()
	if err := deployrec.WriteRequest(f.p.DeployRequest(), r); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Run(context.Background(), f.cfg); err != nil {
		f.t.Fatal(err)
	}
	return f.state()
}

func parseConfig(t *testing.T, doc string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeExec runs everything for real except systemd-run and systemctl,
// which it records and answers from results.
type fakeExec struct {
	mu      sync.Mutex
	cmds    []execx.Cmd
	results map[string]execx.Result // by argv[0] and argv[len-1]
}

func (e *fakeExec) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	e.mu.Lock()
	e.cmds = append(e.cmds, c)
	e.mu.Unlock()
	switch c.Argv[0] {
	case "systemd-run", "systemctl":
		key := c.Argv[0] + " " + c.Argv[len(c.Argv)-2]
		if r, ok := e.results[key]; ok {
			r.Argv = c.Argv
			return r, nil
		}
		return execx.Result{Argv: c.Argv}, nil
	}
	return execx.OS{}.Run(ctx, c)
}

func (e *fakeExec) ran(name string) [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out [][]string
	for _, c := range e.cmds {
		if c.Argv[0] == name {
			out = append(out, c.Argv)
		}
	}
	return out
}

type fakeNotices struct{ titles, bodies []string }

func (n *fakeNotices) Notice(_ context.Context, title, body string) error {
	n.titles = append(n.titles, title)
	n.bodies = append(n.bodies, body)
	return nil
}

type fakeSSH struct {
	calls []string // "<host> <verb> <job>"
	sizes []int
	fail  map[string]string
}

func (s *fakeSSH) Do(_ context.Context, h jobproto.Host, verb, job string, stdin []byte, _ time.Duration) ([]byte, error) {
	s.calls = append(s.calls, h.Name+" "+verb+" "+job)
	s.sizes = append(s.sizes, len(stdin))
	if msg, ok := s.fail[h.Name]; ok {
		return nil, fmt.Errorf("%s", msg)
	}
	return []byte("installed " + job + "\n"), nil
}

type fakeAsker struct{ asks []string }

func (a *fakeAsker) DeployAsk(_ context.Context, rev, why string) error {
	a.asks = append(a.asks, rev+": "+why)
	return nil
}

type memLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *memLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// userBus makes XDG_RUNTIME_DIR/bus a socket, as the user manager's is.
func (f *fixture) userBus() {
	f.t.Helper()
	dir, err := os.MkdirTemp("", "bus")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "bus"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { l.Close() })
	f.s.Env["XDG_RUNTIME_DIR"] = dir
}

func TestOfferHeldFilesTheAsk(t *testing.T) {
	f := newFixture(t)
	a := &fakeAsker{}
	if err := f.s.Offer(context.Background(), f.cfg, a, "abc123abc123", "PR #4 merged", "", true); err != nil {
		t.Fatal(err)
	}
	if err := deployrec.SetHold(f.p.DeployHold(), &deployrec.Hold{By: "maintainer", At: "now"}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Offer(context.Background(), f.cfg, a, "def456def456", "PR #5 merged", "", false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"abc123abc123: PR #4 merged", "def456def456: PR #5 merged"}; !slices.Equal(a.asks, want) {
		t.Errorf("asks %q, want %q", a.asks, want)
	}
	if _, err := os.Stat(f.p.DeployRequest()); !os.IsNotExist(err) {
		t.Errorf("a held offer wrote a request: %v", err)
	}
}

func TestOfferStartsMainWhenNotHeld(t *testing.T) {
	f := newFixture(t)
	f.userBus()
	a := &fakeAsker{}
	if err := f.s.Offer(context.Background(), f.cfg, a, "abc123abc123", "PR #4 merged", "prflow", false); err != nil {
		t.Fatal(err)
	}
	if len(a.asks) != 0 {
		t.Errorf("asked %q", a.asks)
	}
	r, ok, err := deployrec.TakeRequest(f.p.DeployRequest())
	if err != nil || !ok || r.Rev != "main" || r.Why != "PR #4 merged" || r.By != "prflow" {
		t.Errorf("request %+v, %v, %v; want main", r, ok, err)
	}
	if len(f.exec.ran("systemd-run")) != 1 {
		t.Errorf("systemd-run ran %d times", len(f.exec.ran("systemd-run")))
	}
}

func TestStartWritesTheRequestAndStartsTheUnit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.s.Start(ctx, f.cfg, deployrec.Request{Rev: "main", By: "maintainer"}); err == nil || err.Error() != "no user bus: run factory deploy run" {
		t.Errorf("no bus: %v", err)
	}
	if _, ok, _ := deployrec.TakeRequest(f.p.DeployRequest()); !ok {
		t.Error("with no bus the request is not left for factory deploy run")
	}
	f.userBus()
	f.s.Env["FACTORY_GH_TOKEN"] = "ghs_secret"
	f.s.Env["FACTORY_TEST_X"] = "1"
	if err := f.s.Start(ctx, f.cfg, deployrec.Request{Rev: "main", By: "maintainer"}); err != nil {
		t.Fatal(err)
	}
	argv := f.exec.ran("systemd-run")[0]
	want := []string{"systemd-run", "--user", "--no-block", "--collect", "--unit=factory-deploy", "--property=RuntimeMaxSec=3h"}
	if !slices.Equal(argv[:len(want)], want) {
		t.Errorf("argv %q", argv)
	}
	if tail := argv[len(argv)-3:]; !slices.Equal(tail, []string{f.p.FactoryBin(), "deploy", "run"}) {
		t.Errorf("argv ends %q", tail)
	}
	if !slices.Contains(argv, "--setenv=FACTORY_ROOT="+f.p.Root) || !slices.Contains(argv, "--setenv=FACTORY_TEST_X=1") {
		t.Errorf("argv %q lacks the factory settings", argv)
	}
	if strings.Contains(strings.Join(argv, " "), "ghs_secret") {
		t.Errorf("a token went to the unit: %q", argv)
	}
	r, ok, err := deployrec.TakeRequest(f.p.DeployRequest())
	if err != nil || !ok || r.By != "maintainer" || r.At == "" {
		t.Errorf("request %+v, %v, %v", r, ok, err)
	}
	// a deploy that runs already: the unit takes the request when done
	f.exec.results = map[string]execx.Result{"systemd-run deploy": {Code: 1, Stderr: []byte("Failed to start transient service unit: Unit factory-deploy.service was already loaded or has a fragment file.\n")}}
	if err := f.s.Start(ctx, f.cfg, deployrec.Request{Rev: "main"}); err != nil {
		t.Errorf("already loaded: %v", err)
	}
	f.exec.results = map[string]execx.Result{"systemd-run deploy": {Code: 1, Stderr: []byte("Failed to connect to bus\n")}}
	if err := f.s.Start(ctx, f.cfg, deployrec.Request{Rev: "main"}); err == nil {
		t.Error("a failed systemd-run passed")
	}
}

func TestStartRefusedWithoutDeployConfig(t *testing.T) {
	f := newFixture(t)
	f.userBus()
	err := f.s.Start(context.Background(), parseConfig(t, ""), deployrec.Request{Rev: "main"})
	if err == nil || !strings.Contains(err.Error(), "no [deploy]") {
		t.Errorf("Start = %v", err)
	}
	if _, err := os.Stat(f.p.DeployRequest()); !os.IsNotExist(err) {
		t.Errorf("wrote a request: %v", err)
	}
}

// deployTicket files a Deploy? ticket on project, from door, answered
// Deploy it.
func (f *fixture) deployTicket(project, door, rev string) *ticket.Ticket {
	f.t.Helper()
	id, err := f.s.Board.NextID(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	t := ticket.New(id, ticket.NewIn{Project: project, Title: "Deploy the factory?", Door: door,
		Column: string(model.ColNeedsHuman)}, testkit.Stamp())
	t.Deploy = ticket.Some(ticket.Deploy{Rev: rev, Why: []string{}, Requested: ticket.Some("2026-10-24T10:00:00+00:00")})
	ticket.Move(t, string(model.ColDone), model.ReasonDeployRequested, "deploy main", "", "", testkit.Stamp())
	if err := f.s.Board.Save(t); err != nil {
		f.t.Fatal(err)
	}
	return t
}

func (f *fixture) tickets() []*ticket.Ticket {
	f.t.Helper()
	ts, _ := f.s.Board.Reader.List()
	return ts
}

func TestDrainStartsMainForARequestedTicket(t *testing.T) {
	f := newFixture(t)
	f.userBus()
	tk := f.deployTicket(model.NoticeProject, string(model.DoorClock), "0123456789ab")
	ctx := context.Background()
	for range 2 {
		if err := f.s.Drain(ctx, f.cfg, f.tickets()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.exec.ran("systemd-run")); n != 1 {
		t.Errorf("started %d deploys, want 1", n)
	}
	r, ok, _ := deployrec.TakeRequest(f.p.DeployRequest())
	if !ok || r.Rev != "main" || r.Ticket != string(tk.ID) {
		t.Errorf("request %+v, want main from %s", r, tk.ID)
	}
	got, err := f.s.Board.Reader.Load(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deploy.V.Started.Present() || got.Column != string(model.ColDone) {
		t.Errorf("ticket %+v in %s, want started and done", got.Deploy.V, got.Column)
	}
	if last := got.History[len(got.History)-1]; last.Data == nil || last.Data.(ticket.Moved).Reason != model.ReasonDeployStarted {
		t.Errorf("last event %+v, want deploy_started", last)
	}
}

func TestDrainIgnoresATicketOffTheNoticeProject(t *testing.T) {
	f := newFixture(t)
	f.userBus()
	f.deployTicket("demo", string(model.DoorClock), "0123456789ab")
	f.deployTicket(model.NoticeProject, string(model.DoorHuman), "0123456789ab")
	if err := f.s.Drain(context.Background(), f.cfg, f.tickets()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.exec.ran("systemd-run")); n != 0 {
		t.Errorf("started %d deploys", n)
	}
}

func TestDrainMarksAnInterruptedDeploy(t *testing.T) {
	f := newFixture(t)
	if err := deployrec.WriteState(f.p.DeployState(), deployrec.State{Phase: deployrec.PhaseRunning, Step: "deploy.sh", Rev: "main", Commit: f.b, Snapshotted: true}); err != nil {
		t.Fatal(err)
	}
	// a runner at work holds the lock: nothing is marked
	held, err := lockFile(f.p.DeployLock())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Drain(context.Background(), f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if st := f.state(); st.Phase != deployrec.PhaseRunning {
		t.Fatalf("a held lock: state %+v", st)
	}
	held.Unlock()
	if err := f.s.Drain(context.Background(), f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	st := f.state()
	if st.Phase != deployrec.PhaseDone || st.Result != deployrec.ResultInterrupted || st.Finished == "" {
		t.Errorf("state %+v, want interrupted", st)
	}
	want := "Deploy of " + f.b + " was interrupted: check System status; factory deploy restore puts the snapshot back"
	if !slices.Equal(f.notices.titles, []string{want}) {
		t.Errorf("notices %q", f.notices.titles)
	}
	if _, err := os.Stat(f.p.DeployBefore()); !os.IsNotExist(err) {
		t.Error("restored or snapshotted by itself")
	}
	// once only
	if err := f.s.Drain(context.Background(), f.cfg, nil); err != nil || len(f.notices.titles) != 1 {
		t.Errorf("second drain: %v, %q", err, f.notices.titles)
	}
}

// TestDrainInterruptedBeforeTheSnapshot: a deploy cut off before its
// snapshot installed nothing, and deploys/before is an older deploy's: the
// notice does not offer the restore.
func TestDrainInterruptedBeforeTheSnapshot(t *testing.T) {
	f := newFixture(t)
	if err := deployrec.WriteState(f.p.DeployState(), deployrec.State{Phase: deployrec.PhaseRunning, Step: "fetching", Rev: "main", From: f.a}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Drain(context.Background(), f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Deploy of main was interrupted before it installed anything"}; !slices.Equal(f.notices.titles, want) {
		t.Fatalf("notices %q, want %q", f.notices.titles, want)
	}
	if b := f.notices.bodies[0]; strings.Contains(b, "factory deploy restore") || !strings.Contains(b, "fetching") {
		t.Errorf("body %q", b)
	}
}

func lockFile(path string) (*fsx.Locked, error) { return fsx.TryLock(path) }
