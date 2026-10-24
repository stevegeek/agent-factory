package gitx

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// mirrorFixture is a source repo served at <Base>example/agent-factory.git
// over file://, and a Mirror of it not yet fetched.
type mirrorFixture struct {
	src *testkit.Repo
	rec *recorder
	m   Mirror
}

func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	src := testkit.NewRepo(t)
	src.Write("deploy.sh", "#!/bin/bash\necho deploy\n")
	src.Write("config/config.toml", "max_agents = 2\n")
	src.Write("config/context/gem/notes.md", "notes\n")
	src.Write("host/setup.sh", "root\n")
	src.Commit("deployable")
	served := t.TempDir()
	if err := os.MkdirAll(filepath.Join(served, "example"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src.Dir, ".git"), filepath.Join(served, "example", "agent-factory.git")); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	env := testkit.Root(t).Environ()
	return &mirrorFixture{src: src, rec: rec, m: Mirror{
		Dir: filepath.Join(t.TempDir(), "deploys", "core.git"), Base: "file://" + served + "/", Exec: rec, Env: env,
	}}
}

func (f *mirrorFixture) fetchCmd(t *testing.T) []string {
	t.Helper()
	for _, c := range f.rec.cmds {
		if slices.Contains(c.Argv, "fetch") {
			return c.Argv
		}
	}
	t.Fatalf("no fetch in %q", f.rec.argvs())
	return nil
}

func TestMirrorFetchesMain(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := ctxT(t)
	if err := f.m.Fetch(ctx, "example/agent-factory", "ghs_secret"); err != nil {
		t.Fatal(err)
	}
	head := f.src.Git("rev-parse", "main")
	if got, err := f.m.Resolve(ctx, "main"); err != nil || got != head {
		t.Errorf("Resolve(main) = %q, %v; want %s", got, err, head)
	}
	argv := f.fetchCmd(t)
	if strings.Contains(strings.Join(argv, " "), "ghs_secret") {
		t.Errorf("the token is in argv: %q", argv)
	}
	if !slices.Contains(argv, "credential.helper=") || !slices.Contains(argv, "credential.helper="+credentialHelper) {
		t.Errorf("fetch argv %q has no env credential helper", argv)
	}
	if want := []string{"fetch", "--quiet", "--prune", f.m.Base + "example/agent-factory.git", "+refs/heads/main:refs/heads/main"}; !slices.Equal(argv[len(argv)-len(want):], want) {
		t.Errorf("fetch argv %q", argv)
	}
	for _, c := range f.rec.cmds {
		if slices.Contains(c.Argv, "fetch") {
			if !slices.Contains(c.Env, "FACTORY_GH_TOKEN=ghs_secret") || !slices.Contains(c.Env, "GIT_TERMINAL_PROMPT=0") || !slices.Contains(c.Env, "GIT_CONFIG_NOSYSTEM=1") || c.Timeout != FetchTimeout {
				t.Errorf("fetch env %q timeout %v", c.Env, c.Timeout)
			}
		}
	}

	// A second fetch picks up a new commit on main.
	f.src.Write("README.md", "moved\n")
	f.src.Commit("moved")
	if err := f.m.Fetch(ctx, "example/agent-factory", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.m.Resolve(ctx, "main"); got != f.src.Git("rev-parse", "main") {
		t.Errorf("after a second fetch main is %s", got)
	}
}

func TestMirrorFetchWithoutTokenSetsNoHelper(t *testing.T) {
	f := newMirrorFixture(t)
	if err := f.m.Fetch(ctxT(t), "example/agent-factory", ""); err != nil {
		t.Fatal(err)
	}
	argv := f.fetchCmd(t)
	if strings.Contains(strings.Join(argv, " "), "credential") {
		t.Errorf("fetch argv %q has a credential helper", argv)
	}
	for _, c := range f.rec.cmds {
		for _, kv := range c.Env {
			if strings.HasPrefix(kv, "FACTORY_GH_TOKEN=") {
				t.Errorf("%q has %s", c.Argv, kv)
			}
		}
		if slices.Contains(c.Argv, "fetch") && !slices.Contains(c.Env, "GIT_TERMINAL_PROMPT=0") {
			t.Errorf("fetch env %q", c.Env)
		}
	}
}

func TestMirrorFetchRefusesABadRepo(t *testing.T) {
	f := newMirrorFixture(t)
	for _, repo := range []string{"", "agent-factory", "../x/y", "a/b/c", "-u/x"} {
		if err := f.m.Fetch(ctxT(t), repo, ""); err == nil {
			t.Errorf("Fetch(%q) went ahead", repo)
		}
	}
	if len(f.rec.cmds) > 0 {
		t.Errorf("git ran: %q", f.rec.argvs())
	}
}

func TestResolveAndOnMain(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := ctxT(t)
	first := f.src.Git("rev-parse", "main")
	f.src.Write("README.md", "second\n")
	f.src.Commit("second")
	f.src.BranchWith("side", map[string]string{"side.txt": "x\n"})
	side := f.src.Git("rev-parse", "side") // only main is fetched: the mirror lacks it
	if err := f.m.Fetch(ctx, "example/agent-factory", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := f.m.Resolve(ctx, first[:12]); err != nil || got != first {
		t.Errorf("Resolve(%s) = %q, %v", first[:12], got, err)
	}
	if on, err := f.m.OnMain(ctx, first); err != nil || !on {
		t.Errorf("OnMain(first) = %v, %v", on, err)
	}
	if _, err := f.m.Resolve(ctx, side); err == nil {
		t.Errorf("Resolve(side) resolved a commit the mirror lacks")
	}
	if _, err := f.m.Resolve(ctx, "--all"); err == nil {
		t.Errorf("Resolve(--all) resolved")
	}

	// A side-branch commit the mirror has (fetched into another ref) is
	// not on main.
	plainGit(t, f.m.Dir, f.m.Env, "--git-dir="+f.m.Dir, "fetch", "-q", f.m.Base+"example/agent-factory.git", "+refs/heads/side:refs/heads/side")
	if got, err := f.m.Resolve(ctx, side); err != nil || got != side {
		t.Fatalf("Resolve(side) = %q, %v", got, err)
	}
	if on, err := f.m.OnMain(ctx, side); err != nil || on {
		t.Errorf("OnMain(side) = %v, %v", on, err)
	}
}

func TestHasUnknownCommit(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := ctxT(t)
	if f.m.Has(ctx, f.src.Git("rev-parse", "main")) {
		t.Error("Has before any fetch")
	}
	if err := f.m.Fetch(ctx, "example/agent-factory", ""); err != nil {
		t.Fatal(err)
	}
	if !f.m.Has(ctx, f.src.Git("rev-parse", "main")) {
		t.Error("Has(main) is false")
	}
	for _, sha := range []string{strings.Repeat("0", 40), "", "--all", f.src.Git("rev-parse", "main:README.md")} {
		if f.m.Has(ctx, sha) {
			t.Errorf("Has(%q)", sha)
		}
	}
}

func TestArchiveTreeHasNoPaxHeader(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := ctxT(t)
	if err := f.m.Fetch(ctx, "example/agent-factory", ""); err != nil {
		t.Fatal(err)
	}
	sha, _ := f.m.Resolve(ctx, "main")
	b, err := f.m.ArchiveTree(ctx, sha, "config", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			t.Errorf("a pax global header: %q", h.Name)
		}
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := fsx.Extract(root, bytes.NewReader(b)); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "config.toml")); string(got) != "max_agents = 2\n" {
		t.Errorf("config.toml = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "context", "gem", "notes.md")); string(got) != "notes\n" {
		t.Errorf("notes = %q", got)
	}
	if _, err := f.m.ArchiveTree(ctx, sha, "config", 100); err == nil {
		t.Error("an archive over max came back")
	}
	if _, err := f.m.ArchiveTree(ctx, sha, "nope", 1<<20); err == nil {
		t.Error("an archive of a missing directory came back")
	}
}

func TestShowCaps(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := ctxT(t)
	if err := f.m.Fetch(ctx, "example/agent-factory", ""); err != nil {
		t.Fatal(err)
	}
	sha, _ := f.m.Resolve(ctx, "main")
	want := "#!/bin/bash\necho deploy\n"
	if got, err := f.m.Show(ctx, sha, "deploy.sh", int64(len(want))); err != nil || string(got) != want {
		t.Errorf("Show = %q, %v", got, err)
	}
	if got, err := f.m.Show(ctx, sha, "deploy.sh", int64(len(want)-1)); err == nil {
		t.Errorf("Show over max = %q", got)
	}
	if _, err := f.m.Show(ctx, sha, "missing.sh", 1<<20); err == nil {
		t.Error("Show of a missing file")
	}
	if _, err := f.m.Show(ctx, sha, "config", 1<<20); err == nil {
		t.Error("Show of a directory")
	}
}

func TestChangedListsOnlyGivenPaths(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := ctxT(t)
	from := f.src.Git("rev-parse", "main")
	f.src.Write("host/setup.sh", "root changed\n")
	f.src.Write("systemd/factory-tick.timer", "[Timer]\n")
	f.src.Write("systemd/foreman.service", "[Service]\n")
	f.src.Write("README.md", "docs\n")
	f.src.Commit("root and docs")
	if err := f.m.Fetch(ctx, "example/agent-factory", ""); err != nil {
		t.Fatal(err)
	}
	to, _ := f.m.Resolve(ctx, "main")
	got, err := f.m.Changed(ctx, from, to, "host/", "systemd/*.timer", "systemd/factory@.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"host/setup.sh", "systemd/factory-tick.timer"}; !slices.Equal(got, want) {
		t.Errorf("Changed = %q, want %q", got, want)
	}
	if got, err := f.m.Changed(ctx, to, to, "host/"); err != nil || len(got) != 0 {
		t.Errorf("Changed(to, to) = %q, %v", got, err)
	}
}
