package hostrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/scion"
	"github.com/stevegeek/agent-factory/internal/scionread"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var now = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

// fakeExec answers commands by a function and records every one.
type fakeExec struct {
	mu     sync.Mutex
	calls  []execx.Cmd
	stdins [][]byte
	answer func(c execx.Cmd) (execx.Result, error)
}

func (f *fakeExec) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in []byte
	if c.Stdin != nil {
		b := new(strings.Builder)
		buf := make([]byte, 32<<10)
		for {
			n, err := c.Stdin.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		in = []byte(b.String())
	}
	f.calls = append(f.calls, c)
	f.stdins = append(f.stdins, in)
	if f.answer == nil {
		return execx.Result{Argv: c.Argv}, nil
	}
	res, err := f.answer(c)
	res.Argv = c.Argv
	return res, err
}

// args are the calls' argv after the binary.
func (f *fakeExec) args() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c.Argv[1:], " "))
	}
	return out
}

type fixture struct {
	env   *testkit.Env
	paths paths.Paths
	exec  *fakeExec
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	if err := os.WriteFile(p.OAuthToken, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fixture{env: env, paths: p, exec: &fakeExec{}}
}

func (f *fixture) local() *Local {
	return &Local{
		Paths: f.paths,
		Scion: scion.Client{Reader: scionread.Reader{Bin: f.paths.Scion, Exec: f.exec}, TokenFile: f.paths.OAuthToken, RunDir: f.paths.Run},
		Clock: clock.Fixed{T: now},
	}
}

// agentHome makes the Scion home of an agent, as Scion lays it out.
func (f *fixture) agentHome(t *testing.T, name string) string {
	t.Helper()
	home := strings.ReplaceAll(f.paths.AgentHomes, "*", "proj")
	home = strings.ReplaceAll(home, "{name}", name)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func cfgOf(t *testing.T, toml string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ticketOf(t *testing.T, js string) *ticket.Ticket {
	t.Helper()
	tk, _, err := ticket.Decode([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// gitRepo is the Repo the pipeline passes, done with git directly.
type gitRepo struct{ dir string }

// gitCmd runs git with no system or global config.
func gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(testkit.WithoutGitIdentity(os.Environ()), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	return cmd
}

func (g gitRepo) BranchExists(ctx context.Context, branch string) (bool, error) {
	err := gitCmd(ctx, "-C", g.dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run()
	return err == nil, nil
}

func (g gitRepo) Head(ctx context.Context, branch string) (string, error) {
	out, err := gitCmd(ctx, "-C", g.dir, "rev-parse", "refs/heads/"+branch).Output()
	return strings.TrimSpace(string(out)), err
}

func (g gitRepo) Bundle(ctx context.Context, refs []string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "hostrun-test-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "repo.bundle")
	args := []string{"-C", g.dir, "bundle", "create", "-q", path}
	for _, r := range refs {
		args = append(args, "refs/heads/"+r)
	}
	if out, err := gitCmd(ctx, args...).CombinedOutput(); err != nil {
		return nil, &os.PathError{Op: "bundle " + string(out), Path: path, Err: err}
	}
	return os.ReadFile(path)
}

// bundleHeads is `git bundle list-heads`: the refs a bundle holds.
func bundleHeads(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.bundle")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := gitCmd(context.Background(), "bundle", "list-heads", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	heads := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			heads[f[1]] = true
		}
	}
	return heads
}
