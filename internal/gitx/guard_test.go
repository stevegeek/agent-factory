package gitx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stevegeek/agent-factory/internal/gitread"
)

// port: TF GitGuardTest.test_nothing_an_agent_plants_in_dot_git_runs_on_the_host
func TestNothingAnAgentPlantsInDotGitRunsOnTheHost(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	gitdir := filepath.Join(f.repo.Dir, ".git")
	for _, hook := range []string{"pre-push", "post-checkout", "reference-transaction", "pre-commit", "post-merge"} {
		if err := os.WriteFile(filepath.Join(gitdir, "hooks", hook), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	planted := [][2]string{
		{"core.fsmonitor", "touch " + marker + "; false"},
		{"url.https://evil.example/.insteadOf", "https://github.com/"},
		{"filter.x.smudge", "touch " + marker},
		{"include.path", "/tmp/elsewhere"},
		{"alias.st", "!touch x"},
		{"remote.origin.url", "https://evil.example/owner/gem.git"},
	}
	for _, kv := range planted {
		f.repo.Git("config", kv[0], kv[1])
	}
	f.r.GitHub = "o/gem"
	// any host git: the config is checked first
	if _, err := f.r.Head(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	// the factory's own git: hooks (post-checkout, pre-commit, reference-transaction…) would fire here
	steps := [][]string{
		{"checkout", "-q", "-b", "bot/z"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "z"},
		{"checkout", "-q", "main"},
		{"branch", "-D", "bot/z"},
	}
	for i, args := range steps {
		if i == 1 {
			f.repo.Write("README.md", "z\n")
		}
		if _, err := f.w.git(ctx, 0, args...); err != nil {
			t.Fatalf("git %q: %v", args, err)
		}
	}
	if exists(marker) {
		t.Fatal("something planted in .git ran on the host")
	}
	cfg := plainGit(t, "/", f.env, "config", "--file", filepath.Join(gitdir, "config"), "--list")
	for _, k := range []string{"fsmonitor", "insteadof", "filter.x", "include.path", "alias.st", "evil.example"} {
		if strings.Contains(strings.ToLower(cfg), k) {
			t.Errorf("%s still in .git/config:\n%s", k, cfg)
		}
	}
	if !strings.Contains(cfg, "core.repositoryformatversion") {
		t.Errorf("core.repositoryformatversion lost:\n%s", cfg)
	}
	if len(f.log.lines) != 1 || !strings.HasPrefix(f.log.lines[0], f.repo.Dir+": removed from .git/config (an agent can write it): ") {
		t.Errorf("log %q", f.log.lines)
	}
	// a clean config is left exactly as it is
	before, _ := os.ReadFile(filepath.Join(gitdir, "config"))
	fresh := NewGuarder(f.rec, f.env, f.log)
	removed, err := fresh.Guard(ctx, f.repo.Dir, "o/gem")
	if err != nil || len(removed) != 0 {
		t.Errorf("clean config: removed %q, %v", removed, err)
	}
	after, _ := os.ReadFile(filepath.Join(gitdir, "config"))
	if string(before) != string(after) {
		t.Errorf("a clean config was rewritten:\n%s\n->\n%s", before, after)
	}
}

func TestGuardLogsWhatItRemovedAndKeepsWhatAPlainCloneHas(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	f.repo.Git("config", "branch.main.remote", "origin")
	f.repo.Git("config", "branch.main.merge", "refs/heads/main")
	f.repo.Git("config", "remote.origin.url", "https://github.com/o/r.git")
	f.repo.Git("config", "diff.external", "touch x")
	removed, err := f.guard.Guard(ctx, f.repo.Dir, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"user.email", "user.name", "commit.gpgsign", "diff.external"}; !slices.Equal(removed, want) {
		t.Errorf("removed %q; want %q", removed, want)
	}
	want := f.repo.Dir + ": removed from .git/config (an agent can write it): user.email, user.name, commit.gpgsign, diff.external"
	if !slices.Equal(f.log.lines, []string{want}) {
		t.Errorf("log %q", f.log.lines)
	}
	cfg := plainGit(t, "/", f.env, "config", "--file", filepath.Join(f.repo.Dir, ".git", "config"), "--list")
	for _, k := range []string{"branch.main.remote=origin", "branch.main.merge=refs/heads/main", "remote.origin.url=https://github.com/o/r.git", "core.bare=false"} {
		if !strings.Contains(cfg, k) {
			t.Errorf("%s lost:\n%s", k, cfg)
		}
	}
}

func TestGuardLogsAtMostEightKeys(t *testing.T) {
	f := newFixture(t)
	for i := range 10 {
		f.repo.Git("config", "alias.a"+string(rune('a'+i)), "x")
	}
	removed, err := f.guard.Guard(ctxT(t), f.repo.Dir, "")
	if err != nil || len(removed) != 13 {
		t.Fatalf("removed %q, %v", removed, err)
	}
	if got := strings.Count(f.log.lines[0], ", ") + 1; got != 8 {
		t.Errorf("log names %d keys; want 8: %s", got, f.log.lines[0])
	}
}

// §10.7 S7a (security): a symlinked .git/config is replaced, not followed.
func TestGuardReplacesASymlinkedConfig(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	cfg := filepath.Join(f.repo.Dir, ".git", "config")
	elsewhere := filepath.Join(t.TempDir(), "evil-config")
	evil := "[core]\n\tfsmonitor = touch pwned\n"
	os.WriteFile(elsewhere, []byte(evil), 0o644)
	os.Remove(cfg)
	if err := os.Symlink(elsewhere, cfg); err != nil {
		t.Fatal(err)
	}
	removed, err := f.guard.Guard(ctx, f.repo.Dir, "")
	if err != nil || !slices.Equal(removed, []string{gitread.NotPlain}) {
		t.Fatalf("removed %q, %v", removed, err)
	}
	fi, err := os.Lstat(cfg)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("config after the guard: %v, %v", fi, err)
	}
	if got, _ := os.ReadFile(cfg); string(got) != "[core]\n\trepositoryformatversion = 0\n" {
		t.Errorf("config %q", got)
	}
	if got, _ := os.ReadFile(elsewhere); string(got) != evil {
		t.Errorf("the symlink's target was written: %q", got)
	}
	// the repository still works
	if _, err := f.r.Head(ctx, "main"); err != nil {
		t.Error(err)
	}
}

// §10.7 S7a (security): a config that is not a regular file is replaced.
func TestGuardReplacesANonRegularConfig(t *testing.T) {
	ctx := ctxT(t)
	t.Run("fifo", func(t *testing.T) {
		f := newFixture(t)
		cfg := filepath.Join(f.repo.Dir, ".git", "config")
		os.Remove(cfg)
		if err := syscall.Mkfifo(cfg, 0o644); err != nil {
			t.Skip(err)
		}
		removed, err := f.guard.Guard(ctx, f.repo.Dir, "")
		if err != nil || !slices.Equal(removed, []string{gitread.NotPlain}) {
			t.Fatalf("removed %q, %v", removed, err)
		}
		if fi, err := os.Lstat(cfg); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("config is %v, %v", fi, err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		f := newFixture(t)
		cfg := filepath.Join(f.repo.Dir, ".git", "config")
		os.Remove(cfg)
		os.MkdirAll(filepath.Join(cfg, "sub"), 0o755)
		removed, err := f.guard.Guard(ctx, f.repo.Dir, "")
		if err != nil || !slices.Equal(removed, []string{gitread.NotPlain}) {
			t.Fatalf("removed %q, %v", removed, err)
		}
		if fi, err := os.Lstat(cfg); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("config is %v, %v", fi, err)
		}
	})
}

// A planted symlink at the guard's temp name is never written through.
func TestGuardDoesNotFollowAPlantedTempFile(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("keep\n"), 0o644)
	os.Symlink(target, filepath.Join(f.repo.Dir, ".git", "config.factory-new"))
	if _, err := f.guard.Guard(ctxT(t), f.repo.Dir, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep\n" {
		t.Errorf("target written: %q", got)
	}
}

// §10.7 S7a (security): the (ino, mtime, size) cache skips an unchanged
// config, and any change to it is checked again.
func TestGuardStatCache(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	listing := "--no-includes --list"
	if _, err := f.guard.Guard(ctx, f.repo.Dir, ""); err != nil {
		t.Fatal(err)
	}
	if n := f.rec.count(listing); n != 1 {
		t.Fatalf("%d listings after the first guard", n)
	}
	for range 3 {
		if removed, err := f.guard.Guard(ctx, f.repo.Dir, ""); err != nil || removed != nil {
			t.Fatalf("cached guard: %q, %v", removed, err)
		}
	}
	if n := f.rec.count(listing); n != 1 {
		t.Errorf("an unchanged config was listed again (%d listings)", n)
	}
	// an agent writes the config: the next guard reads and cleans it
	f.repo.Git("config", "core.fsmonitor", "touch x")
	removed, err := f.guard.Guard(ctx, f.repo.Dir, "")
	if err != nil || !slices.Equal(removed, []string{"core.fsmonitor"}) {
		t.Errorf("after a change: %q, %v", removed, err)
	}
	// replaced by a symlink to a copy of the clean config, same size: never cached
	cfg := filepath.Join(f.repo.Dir, ".git", "config")
	clean, _ := os.ReadFile(cfg)
	copyPath := filepath.Join(t.TempDir(), "c")
	os.WriteFile(copyPath, clean, 0o644)
	os.Remove(cfg)
	os.Symlink(copyPath, cfg)
	removed, err = f.guard.Guard(ctx, f.repo.Dir, "")
	if err != nil || !slices.Equal(removed, []string{gitread.NotPlain}) {
		t.Errorf("symlink after the cache: %q, %v", removed, err)
	}
}

// quirk 11 (§13.2): a .git that is a file (a gitdir pointer) is refused.
func TestGuardRefusesADotGitFile(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(f.repo.Dir, ".git")+"\n"), 0o644)
	if _, err := f.guard.Guard(ctxT(t), dir, ""); err == nil || !strings.Contains(err.Error(), "unexpected .git file") {
		t.Errorf("guard of a .git file: %v", err)
	}
	r := &gitread.Repo{Path: dir, Base: "main", Exec: f.rec, Env: f.env, Guard: f.guard}
	if _, err := r.Head(ctxT(t), "main"); err == nil || !strings.Contains(err.Error(), "unexpected .git file") {
		t.Errorf("a read through a .git file: %v", err)
	}
	if n := f.rec.count("rev-parse"); n != 0 {
		t.Errorf("git ran in a refused repository")
	}
}

func TestGuardOfARepoWithoutDotGit(t *testing.T) {
	f := newFixture(t)
	removed, err := f.guard.Guard(ctxT(t), t.TempDir(), "")
	if err != nil || removed != nil {
		t.Errorf("no .git: %q, %v", removed, err)
	}
	if len(f.rec.argvs()) != 0 {
		t.Errorf("ran %q", f.rec.argvs())
	}
}

// The rewrite goes through git config --add on the temp file, cwd /, with
// the safe environment, as guard_repo does.
func TestGuardRewriteArgv(t *testing.T) {
	f := newFixture(t)
	f.repo.Git("config", "alias.x", "y")
	if _, err := f.guard.Guard(ctxT(t), f.repo.Dir, ""); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(f.repo.Dir, ".git", "config.factory-new")
	adds := 0
	for _, c := range f.rec.cmds {
		if len(c.Argv) > 4 && c.Argv[1] == "config" && c.Argv[4] == "--add" {
			adds++
			if c.Argv[3] != tmp || c.Dir != "/" || !slices.Equal(c.Env, gitread.SafeEnv(f.env)) {
				t.Errorf("rewrite call %q dir %q", c.Argv, c.Dir)
			}
		}
	}
	if adds == 0 {
		t.Error("no git config --add ran")
	}
	if exists(tmp) {
		t.Error("the temp config was left behind")
	}
}
