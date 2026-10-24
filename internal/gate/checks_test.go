package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/podman"
)

// port: TF ConfigTest.test_gate_uses_configured_env_mounts_and_memory
//
// Security: the gate's container has no network, runs as
// the keep-id user, and gets the project's gate_memory; the real podman
// argv is checked.
func TestGateUsesConfiguredEnvMountsAndMemory(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	cfg, p := r.config("", "[gate]\nenv = { A = \"1\" }\nmounts = [\"{root}/c:/c:ro\"]\nmemory = \"300m\"\n")
	var argv []string
	var seen string
	wt := filepath.Join(r.paths.Gate, "T0009")
	r.runner.Podman = podman.Client{Exec: execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
		argv = c.Argv
		b, _ := os.ReadFile(filepath.Join(wt, "README.md"))
		seen = string(b)
		return execx.Result{Argv: c.Argv, Stdout: []byte("checks ran\n")}, nil
	})}
	ok, tail, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b"})
	if err != nil || !ok || tail != "checks ran\n" {
		t.Fatalf("Checks = %v %q %v", ok, tail, err)
	}
	want := []string{"podman", "run", "--rm", "--name", "factory-gate-T0009", "--entrypoint", "", "--network", "none",
		"--userns=keep-id:uid=1000,gid=1000", "--user", "1000:1000", "--memory", "300m",
		"-e", "A=1", "-v", r.paths.Root + "/c:/c:ro", "-v", wt + ":/work", "-w", "/work", "localhost/gate:test",
		"sh", "-c", "set -e; bundle install; rake test; rake lint"}
	if !slices.Equal(argv, want) {
		t.Errorf("podman argv\n got %q\nwant %q", argv, want)
	}
	if seen != "x\n" {
		t.Errorf("the gate checked out %q, not the branch", seen)
	}
	if exists(wt) {
		t.Error("the checkout is still there")
	}

	// A project's own gate_memory wins over [gate] memory.
	cfg, p = r.config(`gate_memory = "1g"`, "[gate]\nmemory = \"300m\"\n")
	if _, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b"}); err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(argv, "--memory"); i < 0 || argv[i+1] != "1g" {
		t.Errorf("gate_memory not used: %q", argv)
	}
	// And with neither, 900m.
	cfg, p = r.config("", "")
	if _, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b"}); err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(argv, "--memory"); i < 0 || argv[i+1] != "900m" {
		t.Errorf("default memory: %q", argv)
	}
}

// The checkout is a fresh clone: the project's hooks and config never come
// with it, and the image is the one asked for.
func TestChecksRunOnAFreshCloneOfTheRef(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	r.repo.Write(".git/hooks/post-checkout", "#!/bin/sh\ntouch \"$HOME/pwned\"\n")
	if err := os.Chmod(filepath.Join(r.repo.Dir, ".git/hooks/post-checkout"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config("", "")
	wt := filepath.Join(r.paths.Gate, "rebuild-gem")
	r.podman.inspect = func(g podman.Gate) {
		b, _ := os.ReadFile(filepath.Join(wt, "README.md"))
		if string(b) != "hello\n" {
			t.Errorf("checked out %q, not main", b)
		}
		if exists(filepath.Join(wt, ".git", "hooks", "post-checkout")) {
			t.Error("the project's hook came with the clone")
		}
	}
	ok, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "rebuild-gem", Image: "localhost/candidate", Ref: first})
	if err != nil || !ok {
		t.Fatalf("Checks = %v %v", ok, err)
	}
	g := r.podman.runs[0]
	if g.Image != "localhost/candidate" || g.Name != "factory-gate-rebuild-gem" || g.Workdir != wt {
		t.Errorf("gate run %+v", g)
	}
	if exists(filepath.Join(r.env.Home, "pwned")) {
		t.Error("a hook ran")
	}
}

// Security: pinned_untracked files and directories are copied
// into the checkout, a directory's inner links as links; a link is never
// followed, neither in the project (so a planted link cannot pull a secret
// into the container) nor in the checkout (so a link the branch commits
// cannot redirect the copy outside it).
func TestPinnedUntrackedAreCopiedWithoutFollowingLinks(t *testing.T) {
	r := newRig(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The branch commits a link where a pinned file will be copied.
	r.git(r.repo.Dir, "checkout", "-q", "-b", "b")
	if err := os.Symlink(victim, filepath.Join(r.repo.Dir, "Gemfile.lock")); err != nil {
		t.Fatal(err)
	}
	r.git(r.repo.Dir, "add", "-A")
	r.git(r.repo.Dir, "commit", "-q", "-m", "link")
	r.git(r.repo.Dir, "checkout", "-q", "main")
	// Untracked in the project: a lockfile, a shard directory with a link
	// inside, and a link to a secret.
	r.repo.Write("Gemfile.lock", "GEM\n  rake (13)\n")
	r.repo.Write("lib/shards/ameba/src/a.cr", "puts 1\n")
	if err := os.Chmod(filepath.Join(r.repo.Dir, "lib/shards/ameba/src/a.cr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../ameba/src", filepath.Join(r.repo.Dir, "lib/shards/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.repo.Dir, "secret.lock")); err != nil {
		t.Fatal(err)
	}
	cfg, p := r.config(`pinned_untracked = ["Gemfile.lock", "lib/shards", "secret.lock", "missing.lock"]`, "")
	wt := filepath.Join(r.paths.Gate, "T0001")
	r.podman.inspect = func(podman.Gate) {
		b, err := os.ReadFile(filepath.Join(wt, "Gemfile.lock"))
		if err != nil || string(b) != "GEM\n  rake (13)\n" {
			t.Errorf("Gemfile.lock in the checkout: %q %v", b, err)
		}
		if fi, err := os.Lstat(filepath.Join(wt, "Gemfile.lock")); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("Gemfile.lock is not a plain file: %v %v", fi, err)
		}
		b, _ = os.ReadFile(filepath.Join(wt, "lib/shards/ameba/src/a.cr"))
		if string(b) != "puts 1\n" {
			t.Errorf("shard file: %q", b)
		}
		if fi, err := os.Stat(filepath.Join(wt, "lib/shards/ameba/src/a.cr")); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("shard file mode: %v %v", fi, err)
		}
		if target, err := os.Readlink(filepath.Join(wt, "lib/shards/link")); err != nil || target != "../ameba/src" {
			t.Errorf("inner link: %q %v", target, err)
		}
		fi, err := os.Lstat(filepath.Join(wt, "secret.lock"))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("secret.lock was followed: %v %v", fi, err)
		}
		if exists(filepath.Join(wt, "missing.lock")) {
			t.Error("a missing pinned file appeared")
		}
		// The checkout itself is still the branch's.
		if b, _ := os.ReadFile(filepath.Join(wt, "README.md")); string(b) != "hello\n" {
			t.Errorf("README.md: %q", b)
		}
	}
	ok, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0001", Branch: "b"})
	if err != nil || !ok {
		t.Fatalf("Checks = %v %v", ok, err)
	}
	if len(r.podman.runs) != 1 {
		t.Fatalf("%d gate runs", len(r.podman.runs))
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep me\n" {
		t.Errorf("the copy wrote through the branch's link: %q", b)
	}
}

// A pinned path that leaves the project is refused, not read.
func TestPinnedUntrackedMustStayInTheProject(t *testing.T) {
	for _, bad := range []string{"../escape", "/etc/passwd", "a/../../b", ""} {
		r := newRig(t)
		r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
		cfg, p := r.config(`pinned_untracked = [`+strings.ReplaceAll(`"X"`, "X", bad)+`]`, "")
		_, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0001", Branch: "b"})
		if err == nil || !strings.Contains(err.Error(), "pinned_untracked") {
			t.Errorf("pinned %q: %v", bad, err)
		}
		if len(r.podman.runs) != 0 {
			t.Errorf("pinned %q: the checks ran", bad)
		}
	}
}

// port: TF GithubOutTest.test_the_merge_check_reports_a_conflict_without_running_anything
//
// The merge queue's check: a conflict with the base is a failed check, and
// nothing runs.
func TestMergeConflictRunsNothing(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "branch\n"})
	r.repo.Write("README.md", "main\n")
	r.repo.Commit("main moves")
	head := r.git(r.repo.Dir, "rev-parse", "b")
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config("", "")
	ok, tail, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0001", Ref: head, Merge: base})
	if err != nil || ok {
		t.Fatalf("Checks = %v %v", ok, err)
	}
	if !strings.HasPrefix(tail, "merging "+base[:12]+" conflicts:\n") || !strings.Contains(tail, "CONFLICT") {
		t.Errorf("tail %q", tail)
	}
	if len(r.podman.runs) != 0 {
		t.Error("the checks ran on a conflict")
	}
	if exists(filepath.Join(r.paths.Gate, "T0001")) {
		t.Error("the checkout is still there")
	}
}

func TestMergeCheckRunsWithTheBaseMergedIn(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"lib/new.rb": "x\n"})
	r.repo.Write("README.md", "main moved\n")
	r.repo.Commit("main moves")
	head := r.git(r.repo.Dir, "rev-parse", "b")
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config("", "")
	wt := filepath.Join(r.paths.Gate, "T0001")
	r.podman.inspect = func(podman.Gate) {
		a, _ := os.ReadFile(filepath.Join(wt, "lib/new.rb"))
		b, _ := os.ReadFile(filepath.Join(wt, "README.md"))
		if string(a) != "x\n" || string(b) != "main moved\n" {
			t.Errorf("the checkout is not the merge: %q %q", a, b)
		}
	}
	ok, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0001", Ref: head, Merge: base})
	if err != nil || !ok {
		t.Fatalf("Checks = %v %v", ok, err)
	}
	if r.podman.runs[0].Name != "factory-merge-T0001" {
		t.Errorf("container %q", r.podman.runs[0].Name)
	}
}

// A missing install, test or lint is a dispatcher error naming the key,
// not the string "None" in the gate's shell.
func TestMissingCommandIsRefused(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	cfg := mustConfig(t, "image = \"img\"\n[projects.gem]\npath = \""+r.repo.Dir+"\"\nbase = \"main\"\ninstall = \"true\"\ntest = \"true\"\n")
	p, _ := cfg.Project("gem")
	_, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: *p, ID: "T0001", Branch: "b"})
	if err == nil || !strings.Contains(err.Error(), "lint") {
		t.Errorf("err = %v", err)
	}
	if len(r.podman.runs) != 0 || exists(filepath.Join(r.paths.Gate, "T0001")) {
		t.Error("the gate went ahead")
	}
	// An empty command is still a command (Python runs "set -e; ; ..." and the shell fails it).
	cfg = mustConfig(t, "image = \"img\"\n[projects.gem]\npath = \""+r.repo.Dir+"\"\nbase = \"main\"\ninstall = \"\"\ntest = \"true\"\nlint = \"true\"\n")
	p, _ = cfg.Project("gem")
	if _, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: *p, ID: "T0001", Branch: "b"}); err != nil {
		t.Fatal(err)
	}
	if got := r.podman.runs[0].Script; got != "set -e; ; true; true" {
		t.Errorf("script %q", got)
	}
}

func TestNoImageIsRefused(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	cfg := mustConfig(t, "[projects.gem]\npath = \""+r.repo.Dir+"\"\nbase = \"main\"\ninstall = \"true\"\ntest = \"true\"\nlint = \"true\"\n")
	p, _ := cfg.Project("gem")
	_, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: *p, ID: "T0001", Branch: "b"})
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("err = %v", err)
	}
}

// The guard runs before git touches the project, and its refusal stops
// the gate.
func TestTheGuardRunsBeforeTheClone(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	g := &recordingGuard{}
	r.runner.Guard = g
	cfg, p := r.config("", "")
	_, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0001", Branch: "b"})
	if err == nil || !strings.Contains(err.Error(), "refused by the guard") {
		t.Errorf("err = %v", err)
	}
	if len(g.asked) == 0 || g.asked[0] != r.repo.Dir || len(r.podman.runs) != 0 || exists(filepath.Join(r.paths.Gate, "T0001")) {
		t.Errorf("guard %q, runs %d", g.asked, len(r.podman.runs))
	}
}

// A run that did not finish (a timeout) is an error, and the checkout goes.
func TestAGateThatTimesOutIsAnError(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	r.podman.err = execx.ErrTimeout
	cfg, p := r.config("", "")
	_, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0001", Branch: "b"})
	if !errors.Is(err, execx.ErrTimeout) {
		t.Errorf("err = %v", err)
	}
	if exists(filepath.Join(r.paths.Gate, "T0001")) {
		t.Error("the checkout is still there")
	}
}

// A stale checkout from a run that died is replaced, even one an agent
// made unreadable.
func TestAStaleCheckoutIsReplaced(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	stale := filepath.Join(r.paths.Gate, "T0001", "locked")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0); err != nil {
		t.Fatal(err)
	}
	cfg, p := r.config("", "")
	ok, _, err := r.runner.Checks(context.Background(), CheckIn{Config: cfg, Project: p, ID: "T0001", Branch: "b"})
	if err != nil || !ok {
		t.Fatalf("Checks = %v %v", ok, err)
	}
}

func TestContainerNames(t *testing.T) {
	for id, want := range map[string]string{"T0001": "factory-gate-T0001", "rebuild-my.gem": "factory-gate-rebuild-my.gem",
		"rebuild-café gem": "factory-gate-rebuild-caf--gem"} {
		if got := containerName("factory-gate-", id); got != want {
			t.Errorf("containerName(%q) = %q, want %q", id, got, want)
		}
	}
}
