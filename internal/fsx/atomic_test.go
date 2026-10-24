package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestWriteAtomicReplacesAndLeavesNoTemp(t *testing.T) {
	for _, write := range []struct {
		name string
		fn   func(string, []byte, fs.FileMode) error
	}{{"plain", WriteAtomic}, {"sync", WriteAtomicSync}} {
		t.Run(write.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "T0001.json")
			if err := write.fn(path, []byte("one"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := write.fn(path, []byte("two"), 0o644); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != "two" {
				t.Fatalf("read %q, %v", b, err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Errorf("left behind: %v", entries)
			}
		})
	}
}

func TestWriteAtomicTempNameIsPythons(t *testing.T) {
	if got, want := TempName("/b/T0001.json"), fmt.Sprintf("/b/T0001.json.%d.tmp", os.Getpid()); got != want {
		t.Errorf("TempName = %q, want %q", got, want)
	}
}

func TestWriteAtomicPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := WriteAtomicSync(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}
}

// A stale temp file (a crashed writer whose pid came round again) is never
// written through, even when it is a link somewhere else.
func TestWriteAtomicNeverFollowsAPlantedTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, TempName(path)); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim overwritten: %q", b)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Errorf("path holds %q", b)
	}
}

func TestWriteAtomicConcurrentWritersNeverTear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := make([]byte, 64<<10)
			for j := range data {
				data[j] = byte('a' + i)
			}
			if err := WriteAtomic(path, data, 0o644); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range b {
		if c != b[0] {
			t.Fatal("torn write")
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestWriteAtomicFailureLeavesTheOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := WriteAtomic(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a directory where the file should be: the rename fails
	if err := WriteAtomic(filepath.Join(dir, "missing", "f"), []byte("x"), 0o644); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err %v, want not-exist", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "d", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(filepath.Join(dir, "d"), []byte("x"), 0o644); err == nil {
		t.Error("replacing a non-empty directory succeeded")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestTouch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tick.again")
	if err := Touch(path); err != nil {
		t.Fatal(err)
	}
	if err := Touch(path); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 0 {
		t.Fatalf("stat %v %v", st, err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	if err := Touch(link); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("touch through a link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "elsewhere")); err == nil {
		t.Error("touch followed the link")
	}
}
