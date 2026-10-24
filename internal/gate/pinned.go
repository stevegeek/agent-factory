package gate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/config"
)

// pinnedPaths are the project's pinned_untracked entries, refused unless
// each names a path inside the project ("lib", "Gemfile.lock"; not
// "../x", "/etc/x" or "").
func pinnedPaths(p config.Project) ([]string, error) {
	out := make([]string, 0, len(p.PinnedUntracked))
	for _, f := range p.PinnedUntracked {
		if f == "" || !filepath.IsLocal(f) {
			return nil, fmt.Errorf("gate: [projects.%s] pinned_untracked %q is not a path inside the project", p.Key, f)
		}
		out = append(out, filepath.Clean(f))
	}
	return out, nil
}

// copyPinned copies the pinned untracked files and directories (a library's
// lockfile, a Crystal shard's installed lib/) from the project into the
// gate's checkout, which has no network to fetch them.
//
// Python used shutil: copytree(symlinks=True) for a directory, copy2 (which
// follows links) for anything else. Here no link is ever followed, on
// either side. A link in the project is copied as a link, so one planted at
// a pinned path cannot pull a file from elsewhere into the container. In the
// checkout, whatever the branch committed at a pinned path that is not a
// directory is replaced rather than written through, and every write is
// confined to the checkout (os.Root), so a link the branch commits cannot
// redirect the copy outside it. Fifos, sockets and devices are skipped.
func copyPinned(project, checkout string, pinned []string) error {
	if len(pinned) == 0 {
		return nil
	}
	src, err := os.OpenRoot(project)
	if err != nil {
		return fmt.Errorf("pinned_untracked: %w", err)
	}
	defer src.Close()
	dst, err := os.OpenRoot(checkout)
	if err != nil {
		return fmt.Errorf("pinned_untracked: %w", err)
	}
	defer dst.Close()
	for _, f := range pinned {
		fi, err := src.Lstat(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue // Python: os.path.exists() is false, so nothing is copied
		}
		if err != nil {
			return fmt.Errorf("pinned_untracked %s: %w", f, err)
		}
		if dir := path.Dir(filepath.ToSlash(f)); dir != "." {
			if err := dst.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("pinned_untracked %s: %w", f, err)
			}
		}
		if err := copyEntry(src, dst, filepath.ToSlash(f), fi); err != nil {
			return fmt.Errorf("pinned_untracked %s: %w", f, err)
		}
	}
	return nil
}

// copyEntry copies one entry, a directory recursively, without following
// links.
func copyEntry(src, dst *os.Root, name string, fi fs.FileInfo) error {
	switch mode := fi.Mode(); {
	case mode.IsDir():
		if err := dirAt(dst, name, mode.Perm()); err != nil {
			return err
		}
		d, err := src.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		entries, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			child := name + "/" + e.Name()
			cfi, err := src.Lstat(child)
			if err != nil {
				return err
			}
			if err := copyEntry(src, dst, child, cfi); err != nil {
				return err
			}
		}
		return nil
	case mode&fs.ModeSymlink != 0:
		target, err := src.Readlink(name)
		if err != nil {
			return err
		}
		if err := clearAt(dst, name); err != nil {
			return err
		}
		return dst.Symlink(target, name)
	case mode.IsRegular():
		return copyFile(src, dst, name, mode.Perm())
	}
	return nil // a fifo, socket or device: not copied
}

// dirAt makes name a real directory in dst: an existing directory is kept
// (copytree's dirs_exist_ok), anything else there is replaced.
func dirAt(dst *os.Root, name string, perm fs.FileMode) error {
	fi, err := dst.Lstat(name)
	if err == nil && fi.IsDir() {
		return nil
	}
	if err := clearAt(dst, name); err != nil {
		return err
	}
	if err := dst.Mkdir(name, perm|0o700); err != nil {
		return err
	}
	return dst.Chmod(name, perm|0o700)
}

// clearAt removes whatever is at name in dst, so the copy never writes
// through it.
func clearAt(dst *os.Root, name string) error {
	fi, err := dst.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return dst.RemoveAll(name)
	}
	return dst.Remove(name)
}

func copyFile(src, dst *os.Root, name string, perm fs.FileMode) error {
	in, err := src.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s changed while it was copied", name)
	}
	if err := clearAt(dst, name); err != nil {
		return err
	}
	out, err := dst.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(perm); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
