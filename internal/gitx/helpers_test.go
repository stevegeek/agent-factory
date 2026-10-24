package gitx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// recorder runs commands for real and keeps every Cmd it ran.
type recorder struct {
	mu   sync.Mutex
	cmds []execx.Cmd
}

func (r *recorder) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	return execx.OS{}.Run(ctx, c)
}

func (r *recorder) argvs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.cmds))
	for i, c := range r.cmds {
		out[i] = c.Argv
	}
	return out
}

func (r *recorder) count(sub string) int {
	n := 0
	for _, a := range r.argvs() {
		if strings.Contains(strings.Join(a, " "), sub) {
			n++
		}
	}
	return n
}

// logs collects guard log lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) Printf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

type fixture struct {
	repo  *testkit.Repo
	env   []string
	rec   *recorder
	log   *logs
	guard *Guarder
	r     *gitread.Repo
	w     Writer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo := testkit.NewRepo(t)
	f := &fixture{repo: repo, env: testkit.Root(t).Environ(), rec: &recorder{}, log: &logs{}}
	f.guard = NewGuarder(f.rec, f.env, f.log)
	f.r = &gitread.Repo{Path: repo.Dir, Base: "main", Exec: f.rec, Env: f.env, Guard: f.guard}
	f.w = Writer{Repo: f.r}
	return f
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// plainGit runs git in dir with env and no protection, for test set-up.
func plainGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
