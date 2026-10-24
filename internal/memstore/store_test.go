package memstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/paths"
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

// gitArgs is each recorded call's git arguments after mgit's fixed prefix.
func (r *recorder) gitArgs(t *testing.T, gitDir string) [][]string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := []string{"git", "--git-dir=" + gitDir,
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "user.name=factory", "-c", "user.email=factory@localhost", "-c", "commit.gpgsign=false"}
	var out [][]string
	for _, c := range r.cmds {
		if len(c.Argv) < len(prefix) || !reflect.DeepEqual(c.Argv[:len(prefix)], prefix) {
			t.Fatalf("not mgit's argv: %q", c.Argv)
		}
		if c.Dir != gitDir {
			t.Errorf("%q ran in %s, want %s", c.Argv, c.Dir, gitDir)
		}
		for _, kv := range c.Env {
			if strings.HasPrefix(kv, "GIT_") && kv != "GIT_CONFIG_GLOBAL=/dev/null" && kv != "GIT_CONFIG_NOSYSTEM=1" &&
				!strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
				t.Errorf("%q: %s in its environment", c.Argv, kv)
			}
		}
		out = append(out, c.Argv[len(prefix):])
	}
	r.cmds = nil
	return out
}

func newStore(t *testing.T, ex execx.Execer) *Store {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	env := testkit.Root(t)
	// a hostile environment: none of it may reach memory git
	base := append(env.Environ(), "GIT_DIR=/nonexistent", "GIT_WORK_TREE=/", "GIT_INDEX_FILE=/nonexistent/index",
		"GIT_CONFIG_PARAMETERS='core.hooksPath'='/tmp'")
	return &Store{Paths: paths.FromEnv(env.Vars), Exec: ex, Env: base}
}

func snapshotText(t *testing.T, r *Repo, rev string) map[string]string {
	t.Helper()
	files, err := r.Snapshot(t.Context(), rev)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for p, d := range files {
		out[p] = string(d)
	}
	return out
}

func TestEnsureMakesAStoreOnceWithAReadmeAndEmptyFolders(t *testing.T) {
	s := newStore(t, nil)
	ctx := t.Context()
	r, err := s.Ensure(ctx, "p", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if r.GitDir != filepath.Join(s.Paths.Memory, "p", "worker.git") {
		t.Errorf("GitDir %s", r.GitDir)
	}
	head, err := r.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"README.md":          strings.ReplaceAll(strings.ReplaceAll(readme, "{role}", "worker"), "{project}", "p"),
		"knowledge/INDEX.md": "# Knowledge index\n\n(nothing yet)\n",
		"tickets/.keep":      "",
	}
	if got := snapshotText(t, r, head); !reflect.DeepEqual(got, want) {
		t.Errorf("first commit holds %q", got)
	}
	if !strings.HasPrefix(want["README.md"], "# Memory: worker on p\n\nWhat agents in this role") {
		t.Errorf("README %q", want["README.md"])
	}
	if _, err := os.Stat(r.GitDir + ".new"); !os.IsNotExist(err) {
		t.Errorf("the temp store is left: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(r.GitDir, "hooks")); len(ents) != 0 {
		t.Errorf("sample hooks in the store: %v", ents)
	}
	msg := gitOut(t, r, "log", "-1", "--format=%s%n%an <%ae>", "refs/heads/main")
	if msg != "memory for worker on p\nfactory <factory@localhost>" {
		t.Errorf("first commit %q", msg)
	}
	again, err := s.Ensure(ctx, "p", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := again.Head(ctx); h != head {
		t.Errorf("a second Ensure made a new store: %s != %s", h, head)
	}
	for _, bad := range [][2]string{{"../x", "worker"}, {"p", ""}, {"", "worker"}, {"p", "a/b"}, {"p", ".git"}} {
		_, err := s.Ensure(ctx, bad[0], bad[1])
		if err == nil {
			t.Errorf("Ensure(%q, %q) made a store", bad[0], bad[1])
			continue
		}
		want := "no memory for " + PyRepr(bad[0]) + "/" + PyRepr(bad[1]) + ": names must be plain"
		if err.Error() != want || !errors.Is(err, ErrName) {
			t.Errorf("Ensure(%q, %q): %v, want %s", bad[0], bad[1], err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Paths.Root, "x")); !os.IsNotExist(err) {
		t.Error("an odd name escaped the memory directory")
	}
}

func gitOut(t *testing.T, r *Repo, args ...string) string {
	t.Helper()
	out, err := r.git.Out(t.Context(), bareCmd(args...))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Every git call is Python's, word for word.
func TestTheGitCallsArePythons(t *testing.T) {
	rec := &recorder{}
	s := newStore(t, rec)
	ctx := t.Context()
	r, err := s.Ensure(ctx, "p", "worker")
	if err != nil {
		t.Fatal(err)
	}
	tmp := r.GitDir + ".new"
	got := rec.gitArgs(t, tmp)
	want := [][]string{
		{"init", "-q", "--bare", "-b", "main", "--template="},
		{"hash-object", "-w", "--no-filters", "--stdin"},
		{"hash-object", "-w", "--no-filters", "--stdin"},
		{"hash-object", "-w", "--no-filters", "--stdin"},
		{"update-index", "--add", "-z", "--index-info"},
		{"write-tree"},
		{"commit-tree", "-", "-m", "memory for worker on p"},
		{"update-ref", "refs/heads/main", "-"},
	}
	maskShas(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Ensure ran\n%q\nwant\n%q", got, want)
	}

	base, _ := r.Head(ctx)
	listing, _ := r.Listing(ctx, base)
	files, _ := r.Snapshot(ctx, base)
	files["tickets/T0001.md"] = []byte("x\n")
	tree, err := r.WriteTree(ctx, files, listing)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := r.CommitTree(ctx, tree, []string{base}, "T0001 t0001-w1")
	if _, _, err := r.Land(ctx, base, run, "T0001 t0001-w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Changed(ctx, base, run); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Short(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TreeOf(ctx, base); err != nil {
		t.Fatal(err)
	}
	got = rec.gitArgs(t, r.GitDir)
	maskShas(got)
	want = [][]string{
		{"rev-parse", "refs/heads/main"},
		{"ls-tree", "-r", "-z", "-"},
		{"ls-tree", "-r", "-z", "-"},
		{"cat-file", "--batch"},
		{"hash-object", "-w", "--no-filters", "--stdin"}, // only the new file: the others are known
		{"update-index", "--add", "-z", "--index-info"},
		{"write-tree"},
		{"commit-tree", "-", "-m", "T0001 t0001-w1", "-p", "-"},
		{"rev-parse", "refs/heads/main"},
		{"update-ref", "refs/heads/main", "-", "-"},
		{"diff-tree", "-r", "--name-only", "-z", "--no-renames", "-", "-"},
		{"rev-parse", "--short", "-"},
		{"rev-parse", "-^{tree}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a settle ran\n%q\nwant\n%q", got, want)
	}
}

// maskShas replaces object names (hex of 7 to 40) with "-".
func maskShas(argvs [][]string) {
	for _, a := range argvs {
		for i, w := range a {
			sha, suffix, _ := strings.Cut(w, "^")
			if len(sha) >= 7 && len(sha) <= 40 && strings.Trim(sha, "0123456789abcdef") == "" {
				a[i] = "-"
				if suffix != "" {
					a[i] += "^" + suffix
				}
			}
		}
	}
}

func TestListingAndSnapshotKeepOnlyPlainFiles(t *testing.T) {
	s := newStore(t, nil)
	ctx := t.Context()
	r, err := s.Ensure(ctx, "p", "worker")
	if err != nil {
		t.Fatal(err)
	}
	// a commit with a link, an executable and a gitlink, made with plumbing
	blob := strings.TrimSpace(gitIn(t, r, "secret\n", "hash-object", "-w", "--stdin"))
	index := filepath.Join(t.TempDir(), "index")
	info := "120000 " + blob + "\tlink.md\x00" +
		"100755 " + blob + "\trun.sh\x00" +
		"160000 " + strings.Repeat("1", 40) + "\tsub\x00" +
		"100644 " + blob + "\tplain.md\x00" +
		"100644 " + blob + "\tz/é.md\x00"
	gitEnv(t, r, []string{"GIT_INDEX_FILE=" + index}, info, "update-index", "--add", "-z", "--index-info")
	tree := strings.TrimSpace(gitEnv(t, r, []string{"GIT_INDEX_FILE=" + index}, "", "write-tree"))
	commit, err := r.CommitTree(ctx, tree, nil, "odd")
	if err != nil {
		t.Fatal(err)
	}
	listing, err := r.Listing(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"plain.md": blob, "z/é.md": blob}; !reflect.DeepEqual(listing, want) {
		t.Errorf("Listing %v, want %v", listing, want)
	}
	if got := snapshotText(t, r, commit); !reflect.DeepEqual(got, map[string]string{"plain.md": "secret\n", "z/é.md": "secret\n"}) {
		t.Errorf("Snapshot %q", got)
	}
	// an empty tree is an empty snapshot, with no cat-file
	empty, _ := r.WriteTree(ctx, Files{}, nil)
	c, _ := r.CommitTree(ctx, empty, nil, "empty")
	if got := snapshotText(t, r, c); len(got) != 0 {
		t.Errorf("empty commit: %q", got)
	}
}

func gitIn(t *testing.T, r *Repo, stdin string, args ...string) string {
	t.Helper()
	return gitEnv(t, r, nil, stdin, args...)
}

func gitEnv(t *testing.T, r *Repo, env []string, stdin string, args ...string) string {
	t.Helper()
	c := bareCmd(args...)
	c.Env = env
	c.Stdin = []byte(stdin)
	res, err := r.git.Run(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	return string(res.Stdout)
}

// port: TF MemoryTest.test_two_runs_of_one_role_merge_instead_of_overwriting
func TestLandMergesTwoRunsAndKeepsBothSidesOfAConflict(t *testing.T) {
	s := newStore(t, nil)
	ctx := t.Context()
	r, err := s.Ensure(ctx, "p", "worker")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := r.Head(ctx)
	run := func(id, index string) string {
		files, _ := r.Snapshot(ctx, base)
		files["tickets/"+id+".md"] = []byte("from " + id)
		files["knowledge/INDEX.md"] = []byte(index)
		listing, _ := r.Listing(ctx, base)
		tree, err := r.WriteTree(ctx, files, listing)
		if err != nil {
			t.Fatal(err)
		}
		c, err := r.CommitTree(ctx, tree, []string{base}, id)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := run("A", "A's index\n"), run("B", "B's index\n")
	headA, conflicted, err := r.Land(ctx, base, a, "A")
	if err != nil || conflicted || headA != a {
		t.Fatalf("first run: %s %v %v", headA, conflicted, err)
	}
	headB, conflicted, err := r.Land(ctx, base, b, "B")
	if err != nil || !conflicted {
		t.Fatalf("second run: %v %v", conflicted, err)
	}
	if h, _ := r.Head(ctx); h != headB {
		t.Errorf("head %s, want %s", h, headB)
	}
	files := snapshotText(t, r, headB)
	if files["tickets/A.md"] != "from A" || files["tickets/B.md"] != "from B" {
		t.Errorf("a run's notes were lost: %q", files)
	}
	if !strings.Contains(files["knowledge/INDEX.md"], "A's index") || !strings.Contains(files["knowledge/INDEX.md"], "B's index") {
		t.Errorf("both sides not kept: %q", files["knowledge/INDEX.md"])
	}
	if got := gitOut(t, r, "log", "-1", "--format=%s %P", headB); got != "B (merged; both sides kept) "+headA+" "+b {
		t.Errorf("merge commit %q", got)
	}
	// a clean merge says "(merged)"
	c := run("C", "A's index\n")
	files2, _ := r.Snapshot(ctx, headB)
	listing, _ := r.Listing(ctx, headB)
	files2["tickets/D.md"] = []byte("d")
	tree, _ := r.WriteTree(ctx, files2, listing)
	d, _ := r.CommitTree(ctx, tree, []string{headB}, "D")
	if _, _, err := r.Land(ctx, headB, d, "D"); err != nil {
		t.Fatal(err)
	}
	_ = c
	e := run("E", "# Knowledge index\n\n(nothing yet)\n")
	headE, conflicted, err := r.Land(ctx, base, e, "E")
	if err != nil || conflicted {
		t.Fatalf("clean merge: %v %v", conflicted, err)
	}
	if got := gitOut(t, r, "log", "-1", "--format=%s", headE); got != "E (merged)" {
		t.Errorf("merge commit %q", got)
	}
	// head moved under the update: the old-value check refuses
	if err := r.UpdateRef(ctx, a, base); err == nil {
		t.Error("update-ref with a stale old value succeeded")
	}
}

// port: TF MemoryTest.test_undo_a_memory_change
func TestRevertUndoesOneChangeOrRefusesCleanly(t *testing.T) {
	s := newStore(t, nil)
	ctx := t.Context()
	r, _ := s.Ensure(ctx, "p", "worker")
	root, _ := r.Head(ctx)
	commit := func(base string, files map[string]string) string {
		f, _ := r.Snapshot(ctx, base)
		for p, d := range files {
			if d == "" {
				delete(f, p)
				continue
			}
			f[p] = []byte(d)
		}
		tree, _ := r.WriteTree(ctx, f, nil)
		c, _ := r.CommitTree(ctx, tree, []string{base}, "x")
		if _, _, err := r.Land(ctx, base, c, "x"); err != nil {
			t.Fatal(err)
		}
		h, _ := r.Head(ctx)
		return h
	}
	bad := commit(root, map[string]string{"knowledge/bad.md": "wrong\n", "knowledge/edit.md": "one\n"})
	later := commit(bad, map[string]string{"tickets/T1.md": "later\n"})
	short, _ := r.Short(ctx, bad)
	if err := r.Revert(ctx, short); err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head(ctx)
	files := snapshotText(t, r, head)
	if _, ok := files["knowledge/bad.md"]; ok {
		t.Error("the change is still there")
	}
	if files["tickets/T1.md"] != "later\n" {
		t.Error("a later change was undone too")
	}
	if got := gitOut(t, r, "log", "-1", "--format=%s %P", head); got != "undo "+short+" "+later {
		t.Errorf("undo commit %q", got)
	}

	if err := r.Revert(ctx, root); err == nil || err.Error() != "that is the store's first commit; there is nothing before it to go back to" {
		t.Errorf("root commit: %v", err)
	}
	// a change a later one built on cannot be undone on its own
	e1 := commit(head, map[string]string{"knowledge/edit.md": "two\n"})
	commit(e1, map[string]string{"knowledge/edit.md": "three\n"})
	before, _ := r.Head(ctx)
	err := r.Revert(ctx, e1)
	if err == nil || err.Error() != "undoing "+e1+" conflicts with later changes; nothing changed" {
		t.Errorf("conflict: %v", err)
	}
	if h, _ := r.Head(ctx); h != before {
		t.Error("a refused undo changed the store")
	}
	if err := r.Revert(ctx, "nope"); err == nil {
		t.Error("an unknown commit was undone")
	}
	// the message keeps the first 12 characters of what was asked for
	long := commit(before, map[string]string{"knowledge/l.md": "l\n"})
	if err := r.Revert(ctx, long); err != nil {
		t.Fatal(err)
	}
	h, _ := r.Head(ctx)
	if got := gitOut(t, r, "log", "-1", "--format=%s", h); got != "undo "+long[:12] {
		t.Errorf("undo message %q", got)
	}
}

func TestOpenRefusesAMissingStore(t *testing.T) {
	s := newStore(t, nil)
	_, err := s.Open("p", "worker")
	if !errors.Is(err, ErrNoStore) || err.Error() != "no memory store for p/worker" {
		t.Errorf("Open of a missing store: %v", err)
	}
	if _, err := s.Open("..", "worker"); !errors.Is(err, ErrName) {
		t.Errorf("Open(..): %v", err)
	}
	if _, err := s.Ensure(t.Context(), "p", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open("p", "worker"); err != nil {
		t.Errorf("Open: %v", err)
	}
}

func TestACancelledContextStopsGit(t *testing.T) {
	s := newStore(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Ensure(ctx, "p", "worker"); err == nil {
		t.Error("Ensure ran on a cancelled context")
	}
	if _, err := os.Stat(filepath.Join(s.Paths.Memory, "p", "worker.git")); !os.IsNotExist(err) {
		t.Error("a half-made store was put in place")
	}
}
