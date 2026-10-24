package gitread

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
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

func (r *recorder) reset() {
	r.mu.Lock()
	r.cmds = nil
	r.mu.Unlock()
}

// fixture is RepoCase: a real repository plus a gitread.Repo on it whose
// git runs with a hermetic HOME and global config.
type fixture struct {
	repo *testkit.Repo
	env  []string
	rec  *recorder
	r    *Repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo := testkit.NewRepo(t)
	env := testkit.Root(t).Environ()
	rec := &recorder{}
	return &fixture{repo: repo, env: env, rec: rec,
		// the guard has its own tests; testkit's repository keeps a committer
		// identity in .git/config, which a Verifier would refuse
		r: &Repo{Path: repo.Dir, Base: "main", Exec: rec, Env: env, Guard: nopGuard{}}}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}
