package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/gitread"
)

// port: TF BranchTest.test_base_follows_origin_whether_or_not_it_is_checked_out
func TestBaseFollowsOriginWhetherOrNotItIsCheckedOut(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	clone := filepath.Join(t.TempDir(), "clone")
	plainGit(t, "/", f.env, "clone", "-q", f.repo.Dir, clone)
	cr := &gitread.Repo{Path: clone, Base: "main", Exec: f.rec, Env: f.env, Guard: f.guard}
	cw := Writer{Repo: cr}
	upstream := &gitread.Repo{Path: f.repo.Dir, Base: "main", Exec: f.rec, Env: f.env, Guard: noGuard{}}

	f.repo.Write("README.md", "merged upstream\n")
	f.repo.Commit("merged PR")
	if err := cw.RefreshBase(ctx); err != nil {
		t.Fatal(err)
	}
	same(t, ctx, cr, upstream)

	plainGit(t, clone, f.env, "checkout", "-q", "-b", "other")
	f.repo.Write("README.md", "merged again\n")
	f.repo.Commit("another PR")
	if err := cw.RefreshBase(ctx); err != nil {
		t.Fatal(err)
	}
	same(t, ctx, cr, upstream)

	var fetches []string
	for _, a := range f.rec.argvs() {
		if len(a) > 3 && a[3] == "fetch" || len(a) > 3 && a[3] == "merge" {
			fetches = append(fetches, strings.Join(a[3:], " "))
		}
	}
	want := []string{"fetch -q origin main", "merge -q --ff-only origin/main", "fetch -q origin main", "fetch -q origin main:main"}
	if strings.Join(fetches, "|") != strings.Join(want, "|") {
		t.Errorf("git calls %q; want %q", fetches, want)
	}
}

// same asserts that both repositories' main is the same commit.
func same(t *testing.T, ctx context.Context, a, b *gitread.Repo) {
	t.Helper()
	ha, err1 := a.Head(ctx, "main")
	hb, err2 := b.Head(ctx, "main")
	if err1 != nil || err2 != nil || ha != hb {
		t.Errorf("main: %s (%v) vs %s (%v)", ha, err1, hb, err2)
	}
}

// runIgnoring runs git with no protection and ignores its outcome.
func runIgnoring(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Run()
}

// §10.7 S7a (security): protocol.ext.allow=never outranks a config that
// allows the ext:: transport, which would run any command.
func TestExtTransportIsNeverAllowed(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	global := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(global, []byte("[protocol \"ext\"]\n\tallow = always\n"), 0o644)
	env := append(withoutVar(f.env, "GIT_CONFIG_GLOBAL"), "GIT_CONFIG_GLOBAL="+global)
	plainGit(t, f.repo.Dir, env, "remote", "add", "origin", "ext::sh -c touch% "+marker)

	// the control: plain git with that config runs the command
	ctl := filepath.Join(t.TempDir(), "control")
	plainGit(t, f.repo.Dir, env, "config", "remote.ctl.url", "ext::sh -c touch% "+ctl)
	runIgnoring(t, f.repo.Dir, env, "fetch", "-q", "ctl", "main")
	if !exists(ctl) {
		t.Skip("this git does not run ext:: even when allowed; the test proves nothing here")
	}
	plainGit(t, f.repo.Dir, env, "config", "--unset", "remote.ctl.url")

	r := &gitread.Repo{Path: f.repo.Dir, Base: "main", Exec: f.rec, Env: env, Guard: f.guard}
	err := Writer{Repo: r}.RefreshBase(ctx)
	if err == nil {
		t.Fatal("a fetch over ext:: succeeded")
	}
	if exists(marker) {
		t.Fatal("the ext:: transport ran a command")
	}
}

func withoutVar(env []string, name string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}
