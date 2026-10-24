package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/gitread"
)

type noGuard struct{}

func (noGuard) Check(context.Context, string, string) error { return nil }

// A bundle of the base and the start ref goes out; the helper's work comes
// back as out.bundle, and only the work branch is updated from it.
func TestBundleRoundTripUpdatesOnlyTheBranch(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	f.repo.BranchWith("bot/w", map[string]string{"lib/w.rb": "w\n"})
	out, err := f.w.Bundle(ctx, []string{"main", "bot/w"})
	if err != nil || len(out) == 0 {
		t.Fatalf("Bundle: %d bytes, %v", len(out), err)
	}
	var create []string
	for _, a := range f.rec.argvs() {
		if slices.Contains(a, "bundle") {
			create = a
		}
	}
	if len(create) != 9 || !slices.Equal(create[:6], []string{"git", "-C", f.repo.Dir, "bundle", "create", "-q"}) ||
		!slices.Equal(create[7:], []string{"refs/heads/main", "refs/heads/bot/w"}) {
		t.Errorf("bundle argv %q", create)
	}
	if exists(create[6]) {
		t.Errorf("the bundle file %s was left behind", create[6])
	}

	// the helper side: a clone of the bundle, a commit on the branch, and main moved too
	work := filepath.Join(t.TempDir(), "work")
	in := filepath.Join(t.TempDir(), "in.bundle")
	os.WriteFile(in, out, 0o644)
	plainGit(t, "/", f.env, "clone", "-q", "-b", "bot/w", in, work)
	id := []string{"-c", "user.name=a", "-c", "user.email=a@a", "-c", "commit.gpgsign=false"}
	os.WriteFile(filepath.Join(work, "lib", "w.rb"), []byte("w2\n"), 0o644)
	plainGit(t, work, f.env, append(id, "commit", "-q", "-am", "more")...)
	plainGit(t, work, f.env, "checkout", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("agent moved main\n"), 0o644)
	plainGit(t, work, f.env, append(id, "commit", "-q", "-am", "main")...)
	back := filepath.Join(t.TempDir(), "out.bundle")
	plainGit(t, work, f.env, "bundle", "create", "-q", back, "refs/heads/bot/w", "refs/heads/main")
	data, _ := os.ReadFile(back)

	mainBefore := f.repo.Git("rev-parse", "main")
	if err := f.w.FetchBundle(ctx, "bot/w", data); err != nil {
		t.Fatal(err)
	}
	if got, want := f.repo.Git("rev-parse", "bot/w"), plainGit(t, work, f.env, "rev-parse", "bot/w"); got != want {
		t.Errorf("bot/w = %s; want %s", got, want)
	}
	if got := f.repo.Git("rev-parse", "main"); got != mainBefore {
		t.Errorf("main moved to %s: only the work branch may come back", got)
	}
	var fetch []string
	for _, a := range f.rec.argvs() {
		if slices.Contains(a, "fetch") {
			fetch = a
		}
	}
	if len(fetch) != 9 || !slices.Equal(fetch[3:7], []string{"-c", "transfer.fsckObjects=true", "fetch", "-q"}) ||
		fetch[8] != "+refs/heads/bot/w:refs/heads/bot/w" {
		t.Errorf("fetch argv %q", fetch)
	}
}

// §10.7 S7a (security): a bundle with a malformed object is refused.
func TestFetchBundleChecksObjects(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	evil := t.TempDir()
	plainGit(t, evil, f.env, "init", "-q", "-b", "main")
	tree := hashObject(t, evil, f.env, "tree", "")
	bad := hashObject(t, evil, f.env, "commit", "tree "+tree+"\nauthor no email 1 +0000\ncommitter no email 1 +0000\n\nbad\n")
	plainGit(t, evil, f.env, "update-ref", "refs/heads/bot/w", bad)
	path := filepath.Join(t.TempDir(), "evil.bundle")
	plainGit(t, evil, f.env, "bundle", "create", "-q", path, "refs/heads/bot/w")
	data, _ := os.ReadFile(path)

	// the control: without fsck the same fetch is taken
	ctl := filepath.Join(t.TempDir(), "ctl")
	plainGit(t, "/", f.env, "init", "-q", "-b", "main", ctl)
	plainGit(t, ctl, f.env, "fetch", "-q", path, "+refs/heads/bot/w:refs/heads/bot/w")

	err := f.w.FetchBundle(ctx, "bot/w", data)
	if err == nil {
		t.Fatal("a bundle with a malformed commit was fetched")
	}
	if !strings.Contains(err.Error(), "fsck") {
		t.Errorf("error %v", err)
	}
	if ok, _ := (&gitread.Repo{Path: f.repo.Dir, Exec: f.rec, Env: f.env, Guard: f.guard}).BranchExists(ctx, "bot/w"); ok {
		t.Error("the refused bundle created the branch")
	}
}

func hashObject(t *testing.T, dir string, env []string, kind, body string) string {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "obj")
	os.WriteFile(tmp, []byte(body), 0o644)
	return plainGit(t, dir, env, "hash-object", "-t", kind, "-w", "--literally", tmp)
}
