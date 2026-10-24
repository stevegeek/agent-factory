package gitx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The gate's fresh checkout: a clone with no checkout, the branch head
// detached, and the base merged in for the merge queue's check.
func TestCloneCheckoutAndMerge(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	f.repo.BranchWith("bot/a", map[string]string{"lib/a.rb": "a\n"})
	f.repo.Write("lib/b.rb", "b\n")
	f.repo.Commit("base moved")
	planted := filepath.Join(f.repo.Dir, ".git", "hooks", "post-checkout")
	marker := filepath.Join(t.TempDir(), "pwned")
	os.WriteFile(planted, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)

	dest := filepath.Join(t.TempDir(), "gate", "T0001")
	c, err := f.w.CloneNoCheckout(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := f.r.Head(ctx, "bot/a")
	if err := c.CheckoutDetach(ctx, head); err != nil {
		t.Fatal(err)
	}
	base, _ := f.r.Head(ctx, "main")
	ok, out, err := c.Merge(ctx, base)
	if err != nil || !ok {
		t.Fatalf("merge: %v %q %v", ok, out, err)
	}
	for _, p := range []string{"lib/a.rb", "lib/b.rb"} {
		if !exists(filepath.Join(dest, p)) {
			t.Errorf("%s missing after the merge", p)
		}
	}
	if exists(marker) {
		t.Error("the project's hook ran in the gate's clone")
	}
	var clone []string
	for _, a := range f.rec.argvs() {
		if slices.Contains(a, "clone") {
			clone = a
		}
	}
	if want := []string{"git", "clone", "-q", "--no-hardlinks", "--no-checkout", f.repo.Dir, dest}; !slices.Equal(clone, want) {
		t.Errorf("clone argv %q", clone)
	}
	last := f.rec.argvs()[len(f.rec.argvs())-1]
	if want := []string{"git", "-C", dest, "-c", "user.name=factory", "-c", "user.email=factory@localhost", "merge", "-q", "--no-edit", base}; !slices.Equal(last, want) {
		t.Errorf("merge argv %q", last)
	}
}

func TestMergeConflictIsAnOutcome(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	f.repo.BranchWith("bot/a", map[string]string{"README.md": "branch\n"})
	f.repo.Write("README.md", "base\n")
	f.repo.Commit("base moved")
	c, err := f.w.CloneNoCheckout(ctx, filepath.Join(t.TempDir(), "g"))
	if err != nil {
		t.Fatal(err)
	}
	head, _ := f.r.Head(ctx, "bot/a")
	base, _ := f.r.Head(ctx, "main")
	if err := c.CheckoutDetach(ctx, head); err != nil {
		t.Fatal(err)
	}
	ok, out, err := c.Merge(ctx, base)
	if err != nil || ok || !strings.Contains(out, "CONFLICT") {
		t.Errorf("conflict: ok %v, out %q, err %v", ok, out, err)
	}
	if err := c.CheckoutDetach(ctx, "no-such-ref"); err == nil {
		t.Error("checkout of a missing ref succeeded")
	}
}

// The manager's workspace: git init, a README committed as "workspace",
// gpg signing off, and nothing done when it exists.
func TestInitWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	dir := filepath.Join(t.TempDir(), "manager")
	r := *f.r
	r.Path = dir
	w := Writer{Repo: &r}
	readme := "The factory manager's scratch workspace. Nothing here is used by anything else.\n"
	if err := w.InitWorkspace(ctx, readme); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(got) != readme {
		t.Errorf("README %q", got)
	}
	log := plainGit(t, dir, f.env, "log", "--format=%s %an <%ae>")
	if log != "workspace factory <factory@localhost>" {
		t.Errorf("log %q", log)
	}
	if err := w.InitWorkspace(ctx, "other\n"); err != nil {
		t.Fatal(err)
	}
	if n := plainGit(t, dir, f.env, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("%s commits after a second init", n)
	}
	var init []string
	for _, a := range f.rec.argvs() {
		if slices.Contains(a, "init") {
			init = a
		}
	}
	if want := []string{"git", "init", "-q", "-b", "main", dir}; !slices.Equal(init, want) {
		t.Errorf("init argv %q", init)
	}
}
