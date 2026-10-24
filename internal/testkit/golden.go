package testkit

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Golden compares got with the golden file at path. When the test binary
// has a true -update flag, it rewrites the file instead. testkit holds no
// flag of its own (no package state); a package that wants -update declares
// it in a _test.go file:
//
//	var _ = flag.Bool("update", false, "rewrite golden files")
func Golden(t testing.TB, path string, got []byte) {
	t.Helper()
	if Updating() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run the test with -update to create it)", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden %s differs (-want +got):\n%s", path, Diff(string(want), string(got)))
	}
}

// Updating reports whether the test binary was run with -update.
func Updating() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}

// Diff is a small line diff for failure messages: the common head and tail
// are cut, and the differing middle is shown as "-" and "+" lines with a
// line of context on each side.
func Diff(want, got string) string {
	if want == got {
		return ""
	}
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	var out strings.Builder
	if head > 0 {
		out.WriteString("  " + a[head-1] + "\n")
	}
	for _, l := range a[head : len(a)-tail] {
		out.WriteString("- " + l + "\n")
	}
	for _, l := range b[head : len(b)-tail] {
		out.WriteString("+ " + l + "\n")
	}
	if tail > 0 {
		out.WriteString("  " + a[len(a)-tail] + "\n")
	}
	return out.String()
}

// LiveSnapshot returns the directory of a private snapshot of the live
// board, named by FACTORY_LIVE_SNAPSHOT, and skips the test when it is unset
// or missing. The snapshot is never copied into the repository.
func LiveSnapshot(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("FACTORY_LIVE_SNAPSHOT")
	if dir == "" {
		t.Skip("FACTORY_LIVE_SNAPSHOT not set")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Skipf("FACTORY_LIVE_SNAPSHOT %s is not a directory", dir)
	}
	return dir
}
