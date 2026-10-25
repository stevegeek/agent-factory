package gocache

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
)

func commit(c byte) string { return strings.Repeat(string(c), 40) }

func newDir(t *testing.T) (Dir, string) {
	t.Helper()
	root := t.TempDir()
	d, err := For(root, "example-lib")
	if err != nil {
		t.Fatal(err)
	}
	return d, root
}

// compiler is a Compile that records the directories it was handed and
// writes one entry named after the base into each.
type compiler struct {
	dirs []string
	fail error
}

func (c *compiler) compile(ctx context.Context, gen string) error {
	c.dirs = append(c.dirs, gen)
	if c.fail != nil {
		return c.fail
	}
	return os.WriteFile(filepath.Join(gen, "entry-"+fmt.Sprint(len(c.dirs))), []byte("compiled"), 0o644)
}

// tree is every name under dir with its type, mode, mtime and, for a
// file, a hash of its content: what must not change.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v %d", strings.TrimPrefix(p, dir), fi.Mode(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			line += fmt.Sprintf(" %x", sha256.Sum256(b))
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestFor(t *testing.T) {
	root := t.TempDir()
	d, err := For(root, "example-lib")
	if err != nil || d.Path() != filepath.Join(root, "cache", "go", "example-lib") {
		t.Errorf("For: %q %v", d.Path(), err)
	}
	for _, bad := range []string{"", "..", "../x", "a/b", ".x", "-x", "a..b"} {
		if _, err := For(root, bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
	if _, err := For("", "x"); err == nil {
		t.Error("no root: accepted")
	}
}

// The first run for a base warms a new generation, in a directory that is
// not yet the generation, and only a warm that succeeded becomes current,
// by a rename. A later run for the same base finds it and compiles nothing.
func TestEnsureWarmsOnceForABase(t *testing.T) {
	d, _ := newDir(t)
	if gen, ok := d.Current(); ok || gen != "" {
		t.Fatalf("a new dir has a current: %q", gen)
	}
	c := &compiler{}
	l := d.Ensure(t.Context(), commit('a'), c.compile)
	want := filepath.Join(d.Path(), "aaaaaaaaaaaa")
	if l.Gen != want || !l.Warmed {
		t.Fatalf("lend %+v, want %s warmed", l, want)
	}
	if len(c.dirs) != 1 || c.dirs[0] == want || filepath.Dir(c.dirs[0]) != d.Path() {
		t.Errorf("compiled in %q: want a scratch dir beside the generations", c.dirs)
	}
	if link, err := os.Readlink(filepath.Join(d.Path(), "current")); err != nil || link != "aaaaaaaaaaaa" {
		t.Errorf("current -> %q %v", link, err)
	}
	if gen, ok := d.Current(); !ok || gen != want {
		t.Errorf("Current = %q %v", gen, ok)
	}
	if !onDisk(filepath.Join(want, "entry-1")) {
		t.Error("the compiled entry is not in the generation")
	}
	for _, p := range []string{filepath.Join(d.Path(), ".."), d.Path(), want} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v %v, want 0700", p, fi.Mode().Perm(), err)
		}
	}
	again := d.Ensure(t.Context(), commit('a'), c.compile)
	if again.Gen != want || again.Warmed || len(c.dirs) != 1 {
		t.Errorf("second run: %+v, compiled %d times", again, len(c.dirs))
	}
}

// A new base starts from a copy of the current generation (files, modes
// and times), so only what changed compiles; current moves to it, the one
// before is kept, and older generations and scratch dirs go. The warm
// never writes into a generation that exists: each one stays as it was.
func TestEnsureSeedsAndPrunes(t *testing.T) {
	d, _ := newDir(t)
	c := &compiler{}
	la := d.Ensure(t.Context(), commit('a'), c.compile)
	la.Release()
	a := la.Gen
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(a, "entry-1"), old, old); err != nil {
		t.Fatal(err)
	}
	before := tree(t, a)
	if err := os.Mkdir(filepath.Join(d.Path(), ".warm-cccccccccccc"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := d.Ensure(t.Context(), commit('b'), c.compile)
	if !b.Warmed || b.Gen != filepath.Join(d.Path(), "bbbbbbbbbbbb") {
		t.Fatalf("lend %+v", b)
	}
	b.Release()
	if got := tree(t, a); !slices.Equal(got, before) {
		t.Errorf("the warm changed the generation before it:\n got %q\nwant %q", got, before)
	}
	fi, err := os.Stat(filepath.Join(b.Gen, "entry-1"))
	if err != nil || !fi.ModTime().Equal(old) {
		t.Errorf("the seed lost the entry or its time: %v %v", fi, err)
	}
	if !onDisk(filepath.Join(b.Gen, "entry-2")) {
		t.Error("the new compile is not in the new generation")
	}
	if got := names(t, d.Path()); !slices.Equal(got, []string{".lease-aaaaaaaaaaaa", ".lease-bbbbbbbbbbbb", ".lock", "aaaaaaaaaaaa", "bbbbbbbbbbbb", "current"}) {
		t.Errorf("after b: %q", got)
	}
	lc := d.Ensure(t.Context(), commit('c'), c.compile)
	defer lc.Release()
	if got := names(t, d.Path()); !slices.Equal(got, []string{".lease-bbbbbbbbbbbb", ".lease-cccccccccccc", ".lock", "bbbbbbbbbbbb", "cccccccccccc", "current"}) {
		t.Errorf("after c: %q", got)
	}
}

// A base whose generation is still on disk (the base moved back) becomes
// current again without a compile.
func TestEnsureReusesAKeptGeneration(t *testing.T) {
	d, _ := newDir(t)
	c := &compiler{}
	d.Ensure(t.Context(), commit('a'), c.compile)
	d.Ensure(t.Context(), commit('b'), c.compile)
	l := d.Ensure(t.Context(), commit('a'), c.compile)
	if len(c.dirs) != 2 || l.Gen != filepath.Join(d.Path(), "aaaaaaaaaaaa") {
		t.Errorf("lend %+v after %d compiles", l, len(c.dirs))
	}
	if gen, _ := d.Current(); gen != l.Gen {
		t.Errorf("current is %q", gen)
	}
}

// A warm that fails leaves nothing new: current stays where it was and is
// lent, or with none the run is cold. Never an error, never a broken run.
func TestEnsureFailedWarm(t *testing.T) {
	d, _ := newDir(t)
	bad := &compiler{fail: errors.New("compile failed")}
	l := d.Ensure(t.Context(), commit('a'), bad.compile)
	if l.Gen != "" || l.Warmed || !strings.Contains(l.Note, "compile failed") {
		t.Errorf("a failed first warm lent %+v", l)
	}
	if _, ok := d.Current(); ok {
		t.Error("a failed warm made a current")
	}
	if got := names(t, d.Path()); !slices.Equal(got, []string{".lock"}) {
		t.Errorf("left %q", got)
	}
	good := &compiler{}
	la := d.Ensure(t.Context(), commit('a'), good.compile)
	la.Release()
	a := la.Gen
	l = d.Ensure(t.Context(), commit('b'), bad.compile)
	l.Release()
	if l.Gen != a || l.Warmed || !strings.Contains(l.Note, "compile failed") {
		t.Errorf("a failed warm over a: %+v", l)
	}
	if gen, _ := d.Current(); gen != a {
		t.Errorf("current moved to %q", gen)
	}
	if got := names(t, d.Path()); !slices.Equal(got, []string{".lease-aaaaaaaaaaaa", ".lock", "aaaaaaaaaaaa", "current"}) {
		t.Errorf("left %q", got)
	}
}

// A generation a run is lent stays on disk, whatever the later warms
// prune, until the run releases it (its overlay's lower layer must not
// change under a live mount); the prune after that removes it and its
// lease. While a prune removes a generation, a run is not lent it.
func TestALeasedGenerationOutlivesPrunes(t *testing.T) {
	d, _ := newDir(t)
	c := &compiler{}
	held := d.Ensure(t.Context(), commit('a'), c.compile)
	if held.Gen == "" {
		t.Fatalf("lend %+v", held)
	}
	before := tree(t, held.Gen)
	for _, b := range []byte("bcd") {
		l := d.Ensure(t.Context(), commit(b), c.compile)
		l.Release()
	}
	if got := tree(t, held.Gen); !slices.Equal(got, before) {
		t.Errorf("the leased generation changed:\n got %q\nwant %q", got, before)
	}
	if onDisk(filepath.Join(d.Path(), "bbbbbbbbbbbb")) || !onDisk(filepath.Join(d.Path(), "cccccccccccc")) {
		t.Errorf("an unleased generation was kept, or the one before current went: %q", names(t, d.Path()))
	}
	held.Release()
	held.Release() // twice is fine
	l := d.Ensure(t.Context(), commit('e'), c.compile)
	l.Release()
	if got := names(t, d.Path()); !slices.Equal(got, []string{".lease-dddddddddddd", ".lease-eeeeeeeeeeee", ".lock", "current", "dddddddddddd", "eeeeeeeeeeee"}) {
		t.Errorf("after release: %q", got)
	}

	// a prune holds the lease exclusively while it removes: no lend then
	x, err := fsx.TryLock(filepath.Join(d.Path(), ".lease-eeeeeeeeeeee"))
	if err != nil {
		t.Fatal(err)
	}
	l = d.Ensure(t.Context(), commit('e'), c.compile)
	x.Unlock()
	if l.Gen != "" || !strings.Contains(l.Note, "running cold") {
		t.Errorf("lent a generation being removed: %+v", l)
	}
}

// While another run warms (it holds the lock), a run does not wait and does
// not compile: it gets the current generation, or runs cold with none.
func TestEnsureLocked(t *testing.T) {
	d, _ := newDir(t)
	c := &compiler{}
	if err := os.MkdirAll(d.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := fsx.TryLock(filepath.Join(d.Path(), ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	l := d.Ensure(t.Context(), commit('a'), c.compile)
	if l.Gen != "" || len(c.dirs) != 0 || !strings.Contains(l.Note, "warming") {
		t.Errorf("locked with no current: %+v, %d compiles", l, len(c.dirs))
	}
	held.Unlock()
	a := d.Ensure(t.Context(), commit('a'), c.compile).Gen
	held, err = fsx.TryLock(filepath.Join(d.Path(), ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	l = d.Ensure(t.Context(), commit('b'), c.compile)
	if l.Gen != a || l.Warmed || len(c.dirs) != 1 {
		t.Errorf("locked over a: %+v, %d compiles", l, len(c.dirs))
	}
}

// Only a current that names a generation of this dir, which is a real
// directory, is lent: a link anywhere else, to a file, or to a link is
// none.
func TestCurrentRefusesWhatIsNotAGeneration(t *testing.T) {
	d, root := newDir(t)
	if err := os.MkdirAll(d.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "elsewhere")
	os.Mkdir(outside, 0o700)
	os.WriteFile(filepath.Join(d.Path(), "dddddddddddd"), []byte("x"), 0o600)
	os.Symlink(outside, filepath.Join(d.Path(), "eeeeeeeeeeee"))
	for _, target := range []string{outside, "../elsewhere", "current", "dddddddddddd", "eeeeeeeeeeee", "ffffffffffff", "not-a-commit"} {
		cur := filepath.Join(d.Path(), "current")
		os.Remove(cur)
		if err := os.Symlink(target, cur); err != nil {
			t.Fatal(err)
		}
		if gen, ok := d.Current(); ok {
			t.Errorf("current -> %q lent %q", target, gen)
		}
	}
	os.Remove(filepath.Join(d.Path(), "current"))
	os.Mkdir(filepath.Join(d.Path(), "current"), 0o700)
	if gen, ok := d.Current(); ok {
		t.Errorf("a directory named current lent %q", gen)
	}
}

// A project dir that is a link is never used: nothing is warmed through
// it and nothing is lent.
func TestEnsureRefusesALinkedDir(t *testing.T) {
	d, root := newDir(t)
	outside := filepath.Join(root, "elsewhere")
	os.MkdirAll(outside, 0o700)
	os.MkdirAll(filepath.Dir(d.Path()), 0o700)
	if err := os.Symlink(outside, d.Path()); err != nil {
		t.Fatal(err)
	}
	c := &compiler{}
	l := d.Ensure(t.Context(), commit('a'), c.compile)
	if l.Gen != "" || len(c.dirs) != 0 {
		t.Errorf("lend %+v, %d compiles", l, len(c.dirs))
	}
	if got := names(t, outside); len(got) != 0 {
		t.Errorf("wrote %q through the link", got)
	}
}

func TestEnsureRefusesABadBase(t *testing.T) {
	d, _ := newDir(t)
	c := &compiler{}
	for _, base := range []string{"", "main", commit('a')[:39], strings.ToUpper(commit('a')), "../" + commit('a')} {
		if l := d.Ensure(t.Context(), base, c.compile); l.Gen != "" || len(c.dirs) != 0 {
			t.Errorf("%q: %+v", base, l)
		}
	}
}

// Mount lends a generation as a podman overlay: the container's writes go
// to a throwaway upper layer, never into the generation.
func TestMount(t *testing.T) {
	if got := Mount("/f/cache/go/p/aaaaaaaaaaaa"); got != "/f/cache/go/p/aaaaaaaaaaaa:"+Target+":O" {
		t.Errorf("Mount = %q", got)
	}
	if Env() != "GOCACHE="+Target {
		t.Errorf("Env = %q", Env())
	}
}

// A warm gets only the Go settings of the environment its consumers run
// with (their cache keys depend on them): never a credential, never another
// GOCACHE.
func TestWarmEnv(t *testing.T) {
	got := WarmEnv([]string{"GOFLAGS=-mod=readonly", "CGO_ENABLED=0", "GOCACHE=/elsewhere", "ANTHROPIC_API_KEY=k",
		"CLAUDE_CODE_OAUTH_TOKEN=t", "GITHUB_TOKEN=g", "BUNDLE_PATH=/b", "GOAMD64=v3", "GO=x", "go=y", "PATH=/bin"})
	want := []string{"GOFLAGS=-mod=readonly", "CGO_ENABLED=0", "GOAMD64=v3"}
	if !slices.Equal(got, want) {
		t.Errorf("WarmEnv = %q, want %q", got, want)
	}
}

// From a job's bundle the warm clones, where the job's checkout will be,
// and checks out exactly the base head,
// refuses to compile anything else, then runs Compile.
func TestBundleScript(t *testing.T) {
	base := commit('a')
	s, err := BundleScript(base, "/job/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"clone -q --no-checkout repo.bundle /job/repo; cd /job/repo;", "checkout -q --detach " + base, `"$(git rev-parse HEAD)" = ` + base, Compile} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if !strings.HasPrefix(s, "set -e") {
		t.Errorf("script does not stop at the first failure:\n%s", s)
	}
	if _, err := BundleScript("main; rm -rf /", "/job/repo"); err == nil {
		t.Error("a base that is not a commit id was accepted")
	}
	for _, dir := range []string{"", "job/repo", "/job/repo; rm -rf /", "/a b", "/$(x)"} {
		if _, err := BundleScript(base, dir); err == nil {
			t.Errorf("checkout dir %q accepted", dir)
		}
	}
}

func TestReaches(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "cache", "go", "p"), 0o700)
	os.MkdirAll(filepath.Join(root, "cache", "bundle"), 0o700)
	os.Symlink(filepath.Join(root, "cache"), filepath.Join(root, "link"))
	for m, want := range map[string]bool{
		root + "/cache/go:/x": true, root + "/cache/go/p:/x:O": true, root + "/cache:/x:ro": true, root + ":/x": true,
		"/:/x": true, root + "/./cache//go/:/x": true, root + "/link:/x": true, root + "/link/go/p:/x": true,
		root + "/cache/bundle:/x": false, root + "/cache/gopher:/x": false, "/elsewhere:/x": false, "named:/x": false,
	} {
		if got := Reaches(root, m); got != want {
			t.Errorf("Reaches(%q) = %v, want %v", m, got, want)
		}
	}
}

// ReachesDir is Reaches for another directory under the root (the gate's
// kept early results), with the root's links resolved the same way, even
// before the directory exists.
func TestReachesDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gate-early")
	link := filepath.Join(t.TempDir(), "r")
	os.Symlink(root, link)
	for m, want := range map[string]bool{
		dir + ":/x": true, dir + "/T0001.json:/x:ro": true, root + ":/x": true, "/:/x": true, link + "/gate-early:/x": true,
		root + "/gate:/x": false, root + "/gate-earlyx:/x": false, root + "/cache/go:/x": false, "named:/x": false,
	} {
		if got := ReachesDir(root, dir, m); got != want {
			t.Errorf("ReachesDir(%q) = %v, want %v", m, got, want)
		}
	}
	if !Reaches(root, root+"/cache/go:/x") || Reaches(root, dir+":/x") {
		t.Error("Reaches is not ReachesDir of the caches")
	}
}

// Compile, run by the go on this host in a read-only module whose ./...
// is one main package (as the gate mounts /work), succeeds and writes
// nothing into the source. It uses this host's own build cache, as any go
// test does, so it compiles little.
func TestCompileInAReadOnlySingleMainModule(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	src := filepath.Join(t.TempDir(), "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go.mod":       "module example.com/single\n\ngo 1.21\n",
		"main.go":      "package main\n\nfunc main() { println(f()) }\n\nfunc f() int { return 1 }\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tif f() != 1 {\n\t\tt.Fatal()\n\t}\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(src, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(src, 0o755) })
	cmd := exec.Command("/bin/sh", "-c", Compile)
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Compile: %v\n%s", err, out)
	}
	if got := names(t, src); !slices.Equal(got, []string{"go.mod", "main.go", "main_test.go"}) {
		t.Errorf("the source now holds %q", got)
	}
}
