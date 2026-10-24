package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
)

var errRefused = errors.New("refused")

func mkdir(p string) error { return os.MkdirAll(p, 0o755) }

func TestBareEnvDropsEveryGitVariable(t *testing.T) {
	base := []string{"PATH=/bin", "GIT_DIR=/elsewhere", "GIT_INDEX_FILE=/x", "GIT_CONFIG_GLOBAL=/home/u/.gitconfig", "HOME=/h"}
	got := BareEnv(base, []string{"GIT_INDEX_FILE=/tmp/index"})
	want := []string{"PATH=/bin", "HOME=/h", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_INDEX_FILE=/tmp/index"}
	if !slices.Equal(got, want) {
		t.Errorf("BareEnv:\n%q\nwant\n%q", got, want)
	}
}

func TestBareArgvAndErrors(t *testing.T) {
	cap := &captured{}
	b := Bare{GitDir: "/m/p/worker.git", Exec: cap, Env: []string{"PATH=/bin"}}
	if _, err := b.Run(context.Background(), BareCmd{Args: []string{"rev-parse", "refs/heads/main"}}); err != nil {
		t.Fatal(err)
	}
	c := cap.cmds[0]
	want := []string{"git", "--git-dir=/m/p/worker.git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "user.name=factory", "-c", "user.email=factory@localhost", "-c", "commit.gpgsign=false", "rev-parse", "refs/heads/main"}
	if !slices.Equal(c.Argv, want) || c.Dir != "/m/p/worker.git" {
		t.Errorf("argv %q dir %q", c.Argv, c.Dir)
	}
	failing := execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
		return execx.Result{Argv: c.Argv, Code: 1, Stderr: []byte("  " + strings.Repeat("x", 400) + "fatal: bad\n")}, nil
	})
	b.Exec = failing
	_, err := b.Run(context.Background(), BareCmd{Args: []string{"merge-tree", "--write-tree"}})
	if err == nil || err.Error() != "memory git merge-tree: "+strings.Repeat("x", 290)+"fatal: bad" {
		t.Errorf("error %v", err)
	}
	// rc 1 is a result, not an error, where the caller says so (merge-tree conflicts)
	res, err := b.Run(context.Background(), BareCmd{Args: []string{"merge-tree"}, OK: []int{0, 1}})
	if err != nil || res.Code != 1 {
		t.Errorf("ok=(0,1): %v, %v", res.Code, err)
	}
}

// A store made, written and read the way memory does it, with real git.
func TestBareStoreRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	dir := filepath.Join(t.TempDir(), "worker.git")
	b := Bare{GitDir: dir, Exec: f.rec, Env: f.env}
	if err := mkdir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, BareCmd{Args: []string{"init", "-q", "--bare", "-b", "main", "--template="}}); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"README.md": "# Memory\n", "knowledge/INDEX.md": "nothing\n", "tickets/.keep": ""}
	var lines []string
	shas := map[string]string{}
	for _, p := range []string{"README.md", "knowledge/INDEX.md", "tickets/.keep"} {
		sha, err := b.Out(ctx, BareCmd{Args: []string{"hash-object", "-w", "--no-filters", "--stdin"}, Stdin: []byte(files[p])})
		if err != nil {
			t.Fatal(err)
		}
		shas[p] = sha
		lines = append(lines, "100644 "+sha+"\t"+p+"\x00")
	}
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
	if _, err := b.Run(ctx, BareCmd{Args: []string{"update-index", "--add", "-z", "--index-info"}, Stdin: []byte(strings.Join(lines, "")), Env: index}); err != nil {
		t.Fatal(err)
	}
	tree, err := b.Out(ctx, BareCmd{Args: []string{"write-tree"}, Env: index})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := b.Out(ctx, BareCmd{Args: []string{"commit-tree", tree, "-m", "memory"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, BareCmd{Args: []string{"update-ref", "refs/heads/main", commit}}); err != nil {
		t.Fatal(err)
	}
	got, err := b.CatFileBatch(ctx, []string{shas["README.md"], shas["tickets/.keep"], shas["knowledge/INDEX.md"]})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got[0]) != "# Memory\n" || string(got[1]) != "" || string(got[2]) != "nothing\n" {
		t.Errorf("CatFileBatch = %q", got)
	}
	if _, err := b.CatFileBatch(ctx, []string{strings.Repeat("0", 40)}); err == nil {
		t.Error("a missing object was read")
	}
	if got, err := b.CatFileBatch(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("empty batch: %q, %v", got, err)
	}
}
