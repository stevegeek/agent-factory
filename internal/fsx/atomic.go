// Package fsx holds the factory's file primitives (ARCHITECTURE.md §4.3,
// §4.4): atomic writes with Python's temp names, flock(2) locks with waits
// and inherited descriptors, a walk and reads that never follow a link, and
// the removal of a tree an agent may have made unreadable.
package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// TempName is the temp file Python's save_ticket writes before its rename:
// "<path>.<pid>.tmp". Tools that look for *.tmp see the same names.
func TempName(path string) string { return fmt.Sprintf("%s.%d.tmp", path, os.Getpid()) }

// WriteAtomic writes data to path through a temp file in the same
// directory and a rename, as Python does: a reader sees the old file or the
// new one, never a part. perm is the mode of a new file, less the umask.
func WriteAtomic(path string, data []byte, perm fs.FileMode) error {
	return writeAtomic(path, data, perm, false)
}

// WriteAtomicSync is WriteAtomic with fsync of the file before the rename
// and of the directory after it, so a crash cannot lose the write (ticket
// saves, the id counter, the token store). The bytes are the same.
func WriteAtomicSync(path string, data []byte, perm fs.FileMode) error {
	return writeAtomic(path, data, perm, true)
}

func writeAtomic(path string, data []byte, perm fs.FileMode, sync bool) (err error) {
	f, tmp, err := createTemp(path, perm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if sync {
		if err = f.Sync(); err != nil {
			return fmt.Errorf("sync %s: %w", tmp, err)
		}
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if sync {
		if err := syncDir(filepath.Dir(path)); err != nil {
			return fmt.Errorf("sync directory of %s: %w", path, err)
		}
	}
	return nil
}

// createTemp makes the temp file with O_EXCL, so it never writes through
// a planted file or link. Python's name is tried first; if it is taken (a
// concurrent writer in this process, or a crashed one whose pid came round
// again) a numbered name beside it is used.
func createTemp(path string, perm fs.FileMode) (*os.File, string, error) {
	base := TempName(path)
	name := base
	for n := 1; ; n++ {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) || n > 100 {
			return nil, "", fmt.Errorf("create temp for %s: %w", path, err)
		}
		name = fmt.Sprintf("%s.%d.%d.tmp", path, os.Getpid(), n)
	}
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Touch creates path empty, or truncates it (Python open(path, "w")). It
// never follows a link.
func Touch(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return fmt.Errorf("touch %s: %w", path, err)
	}
	return f.Close()
}
