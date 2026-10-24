package testkit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRootIsHermetic(t *testing.T) {
	e := Root(t)
	for _, dir := range []string{e.Path("board", "tickets"), e.Path("logs"), filepath.Join(e.Home, ".config", "factory"), filepath.Join(e.Home, ".local", "bin")} {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			t.Errorf("%s: %v", dir, err)
		}
	}
	st, err := os.Stat(filepath.Join(e.Home, ".config", "factory"))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode = %v, %v", st.Mode().Perm(), err)
	}
	if e.Vars["FACTORY_ROOT"] != e.Root || e.Vars["HOME"] != e.Home {
		t.Errorf("Vars = %v", e.Vars)
	}
	if !strings.HasPrefix(e.Vars["FACTORY_API_TOKENS"], e.Home) {
		t.Errorf("token store outside the temp home: %s", e.Vars["FACTORY_API_TOKENS"])
	}
	// Environ overrides HOME and FACTORY_ROOT exactly once each.
	n := 0
	for _, kv := range e.Environ() {
		if strings.HasPrefix(kv, "HOME=") {
			n++
			if kv != "HOME="+e.Home {
				t.Errorf("Environ HOME = %s", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("HOME appears %d times in Environ", n)
	}
	if got := e.WriteConfig(t, "[github]\n"); got != e.Path("config.toml") {
		t.Errorf("WriteConfig = %s", got)
	}
	b, err := os.ReadFile(e.WriteFile(t, "a/b/c.txt", "x"))
	if err != nil || string(b) != "x" {
		t.Errorf("WriteFile: %q %v", b, err)
	}
	if other := Root(t); other.Root == e.Root {
		t.Error("two roots share a directory")
	}
}

func TestClocks(t *testing.T) {
	f := Fixed{Stamp()}
	if !f.Now().Equal(Stamp()) || !f.Now().Equal(f.Now()) {
		t.Fatal("Fixed moved")
	}
	s := NewStep(Stamp(), time.Second)
	if a, b := s.Now(), s.Now(); b.Sub(a) != time.Second || !a.Equal(Stamp()) {
		t.Fatalf("Step: %v then %v", a, b)
	}
	s.Advance(time.Hour)
	if got := s.Now(); !got.Equal(Stamp().Add(time.Hour + 2*time.Second)) {
		t.Fatalf("after Advance: %v", got)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); s.Now() }()
	}
	wg.Wait()
	if got := s.Now(); !got.Equal(Stamp().Add(time.Hour + 11*time.Second)) {
		t.Fatalf("concurrent steps lost: %v", got)
	}
}

func TestRepoLikeRepoCase(t *testing.T) {
	r := NewRepo(t)
	if got := r.Git("rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("branch = %q", got)
	}
	if got := r.Git("log", "--format=%s"); got != "init" {
		t.Fatalf("log = %q", got)
	}
	r.BranchWith("bot/t0001", map[string]string{"lib/new.rb": "x\n"})
	if got := r.Git("diff", "--name-only", "main", "bot/t0001"); got != "lib/new.rb" {
		t.Fatalf("branch diff = %q", got)
	}
	if got := r.Git("rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("not back on main: %q", got)
	}
	if got := r.Git("config", "user.email"); got != "t@example.com" {
		t.Fatalf("identity = %q", got)
	}
}

func TestGoldenAndDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	Golden(t, path, []byte("a\nb\nc\n"))

	ft := &fakeT{TB: t}
	Golden(ft, path, []byte("a\nB\nc\n"))
	if !ft.failed || !strings.Contains(ft.msg, "- b\n+ B\n") {
		t.Fatalf("Golden mismatch not reported: failed=%v msg=%q", ft.failed, ft.msg)
	}
	if Diff("x", "x") != "" {
		t.Fatal("Diff of equal strings")
	}
	if got := Diff("1\n2\n3\n4", "1\n2\nX\n4"); got != "  2\n- 3\n+ X\n  4\n" {
		t.Fatalf("Diff = %q", got)
	}
	if got := Diff("a", "a\nb"); got != "  a\n+ b\n" {
		t.Fatalf("Diff append = %q", got)
	}
}

func TestLiveSnapshotSkipsWhenUnset(t *testing.T) {
	t.Setenv("FACTORY_LIVE_SNAPSHOT", "")
	ft := &fakeT{TB: t}
	func() {
		defer func() { _ = recover() }()
		LiveSnapshot(ft)
	}()
	if !ft.skipped {
		t.Fatal("not skipped")
	}
	dir := t.TempDir()
	t.Setenv("FACTORY_LIVE_SNAPSHOT", dir)
	if got := LiveSnapshot(t); got != dir {
		t.Fatalf("LiveSnapshot = %q", got)
	}
}

// fakeT records failures and skips instead of ending the real test.
type fakeT struct {
	testing.TB
	failed, skipped bool
	msg             string
}

func (f *fakeT) Helper() {}
func (f *fakeT) Errorf(format string, args ...any) {
	f.failed = true
	f.msg += fmt.Sprintf(format, args...)
}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	panic("fatal")
}
func (f *fakeT) Skip(args ...any) {
	f.skipped = true
	panic("skip")
}
func (f *fakeT) Skipf(format string, args ...any) { f.Skip() }

// Environ and WithoutGitIdentity drop the caller's git identity, so a
// test's commits never depend on the environment it runs in.
func TestGitIdentityIsCleared(t *testing.T) {
	for _, k := range GitIdentityVars() {
		t.Setenv(k, "x")
	}
	t.Setenv("FACTORY_KEEP", "1")
	e := Root(t)
	got := strings.Join(e.Environ(), "\n")
	for _, k := range GitIdentityVars() {
		if strings.Contains(got, k+"=") {
			t.Errorf("Environ keeps %s", k)
		}
	}
	if !strings.Contains(got, "FACTORY_KEEP=1") {
		t.Error("Environ dropped an unrelated variable")
	}
	r := NewRepo(t)
	if who := r.Git("log", "-1", "--format=%an <%ae> %cn"); who != "t <t@example.com> t" {
		t.Errorf("commit by %q", who)
	}
}
