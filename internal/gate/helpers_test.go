package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// rig is a factory root, a real project repository (testkit.Repo: main with
// README.md and lib/thing.rb) and a Runner whose git is real and whose
// podman is fake.
type rig struct {
	t      *testing.T
	env    *testkit.Env
	paths  paths.Paths
	repo   *testkit.Repo
	podman *fakePodman
	log    *fakeLog
	runner Runner
}

func newRig(t *testing.T) *rig {
	t.Helper()
	env := testkit.Root(t)
	r := &rig{t: t, env: env, paths: paths.FromEnv(env.Vars), repo: testkit.NewRepo(t),
		podman: &fakePodman{ok: true, out: "ok"}, log: &fakeLog{}}
	r.runner = Runner{Paths: r.paths, Exec: execx.OS{}, Env: env.Environ(), Guard: nopGuard{},
		Podman: r.podman, Log: r.log, Clock: testkit.Fixed{T: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}}
	return r
}

// config is a config whose project "gem" is the rig's repository, with
// extra TOML appended to [projects.gem] (before) and after it.
func (r *rig) config(project, rest string) (*config.Config, config.Project) {
	r.t.Helper()
	text := "image = \"localhost/gate:test\"\n" + rest + "\n[projects.gem]\npath = " + strconv.Quote(r.repo.Dir) +
		"\nbase = \"main\"\ninstall = \"bundle install\"\ntest = \"rake test\"\nlint = \"rake lint\"\n" + project + "\n"
	cfg := mustConfig(r.t, text)
	p, ok := cfg.Project("gem")
	if !ok {
		r.t.Fatal("no project gem")
	}
	return cfg, *p
}

// git runs git in dir with the rig's isolated environment.
func (r *rig) git(dir string, args ...string) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = r.env.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(trimNL(out))
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// fakePodman answers RunGate and records each run. inspect, when set,
// looks at the gate's checkout while the "container" runs.
type fakePodman struct {
	mu      sync.Mutex
	runs    []podman.Gate
	ok      bool
	out     string
	err     error
	inspect func(g podman.Gate)
}

func (f *fakePodman) RunGate(ctx context.Context, g podman.Gate) (bool, string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, g)
	f.mu.Unlock()
	if f.inspect != nil {
		f.inspect(g)
	}
	return f.ok, f.out, f.err
}

type fakeLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *fakeLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

type nopGuard struct{}

func (nopGuard) Check(context.Context, string, string) error { return nil }

// recordingGuard refuses, and remembers that it was asked.
type recordingGuard struct{ asked []string }

func (g *recordingGuard) Check(_ context.Context, path, _ string) error {
	g.asked = append(g.asked, path)
	return errors.New("refused by the guard")
}

// failExec fails the test if anything runs.
func failExec(t *testing.T) execx.Execer {
	return execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
		t.Errorf("unexpected command %q", c.Argv)
		return execx.Result{}, errors.New("no commands here")
	})
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
