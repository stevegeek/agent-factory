package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// The removal half of TF test_nothing_the_agent_plants_runs_or_leaves_the_copy
// (the removal half: the copy goes even where the agent locked directories,
// and nothing outside it is touched through a link).
func TestRemoveTreeDeletesLockedDirectoriesAndNeverFollowsALink(t *testing.T) {
	base := t.TempDir()
	top := filepath.Join(base, "copy")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(top, "a", "b", "c"), filepath.Join(top, "ro"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(top, "a", "b", "c", "f"), filepath.Join(top, "ro", "f"), filepath.Join(outside, "keep")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(outside, 0o755) })
	if err := os.Symlink(outside, filepath.Join(top, "a", "out")); err != nil {
		t.Fatal(err)
	}
	// the agent locks directories at every depth, the top too
	for _, d := range []string{filepath.Join(top, "a", "b", "c"), filepath.Join(top, "a", "b"), filepath.Join(top, "a"), filepath.Join(top, "ro"), top} {
		if err := os.Chmod(d, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveTree(top); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(top); !os.IsNotExist(err) {
		t.Errorf("copy still there: %v", err)
	}
	st, err := os.Stat(outside)
	if err != nil || st.Mode().Perm() != 0o500 {
		t.Errorf("outside changed through the link: %v %v", st.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("outside file gone: %v", err)
	}
}

func TestRemoveTreeOfALinkRemovesOnlyTheLink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Error("link still there")
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("target changed: %v %v", st, err)
	}
}

func TestRemoveTreeOfNothingOrAFile(t *testing.T) {
	base := t.TempDir()
	if err := RemoveTree(filepath.Join(base, "none")); err != nil {
		t.Errorf("missing: %v", err)
	}
	f := filepath.Join(base, "f")
	if err := os.WriteFile(f, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f); !os.IsNotExist(err) {
		t.Error("file still there")
	}
}
