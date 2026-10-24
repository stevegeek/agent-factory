package fsx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"syscall"
)

var (
	// ErrNotRegular is returned for a read of anything but a regular file:
	// a link, a fifo, a socket, a device or a directory.
	ErrNotRegular = errors.New("not a regular file")
	// ErrTooLarge is returned for a read of a file over its cap.
	ErrTooLarge = errors.New("file too large")
)

// oPath is Linux O_PATH (010000000 on amd64, arm64 and the other common
// architectures); package syscall does not export it.
const oPath = 0o10000000

// Entry is one name in a directory, typed by a no-follow stat.
type Entry struct {
	Name string
	Path string      // slash-separated, relative to the walk's top
	Mode fs.FileMode // type and permission bits of the entry itself (lstat)
}

// Dir is one directory of a walk, as os.fwalk yields it: Dirs holds the
// entries that are directories once links are followed (so a link to a
// directory is listed here, though it is never entered), Files the rest.
// Both are in the directory's own order (getdents, as scandir gives).
type Dir struct {
	Path  string // "." for the top
	Dirs  []Entry
	Files []Entry
	// Cut says the listing stopped at the walk's cap on names: the
	// directory has more than Dirs and Files hold.
	Cut bool
	fd  int
}

// WalkNoFollow walks the tree at top, top-down, calling fn once per
// directory. Every open is relative to its parent's descriptor and refuses
// links, so nothing outside the tree is reached however it was arranged. fn
// may cut d.Dirs to prune the walk; only entries that are real directories
// are entered, and one that cannot be opened (mode 000) is passed over
// silently, as os.fwalk does. fs.SkipAll from fn ends the walk with nil; any
// other error ends it with that error. A top that is a link or cannot be
// opened is an error.
func WalkNoFollow(top string, fn func(d *Dir) error) error { return WalkNoFollowMax(top, 0, fn) }

// WalkNoFollowMax is WalkNoFollow listing at most max names of each
// directory (0 for no cap), so a directory of millions of names costs no
// more than max of them: a directory with more is passed to fn with Cut
// set and the first max names in its order.
func WalkNoFollowMax(top string, max int, fn func(d *Dir) error) error {
	fd, err := syscall.Open(top, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("walk %s: %w", top, &fs.PathError{Op: "open", Path: top, Err: err})
	}
	err = walkDir(fd, ".", max, fn)
	if errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

// walkDir owns fd and closes it.
func walkDir(fd int, rel string, max int, fn func(d *Dir) error) error {
	d := &Dir{Path: rel, fd: fd}
	defer syscall.Close(fd)
	names, err := readNames(fd, max)
	if err != nil {
		return fmt.Errorf("walk %s: %w", rel, err)
	}
	if max > 0 && len(names) > max {
		names, d.Cut = names[:max], true
	}
	for _, name := range names {
		e := Entry{Name: name, Path: join(rel, name)}
		mode, err := statAt(fd, name, true)
		if err != nil {
			continue // gone since the listing
		}
		e.Mode = mode
		isDir := mode.IsDir()
		if mode&fs.ModeSymlink != 0 {
			if m, err := statAt(fd, name, false); err == nil && m.IsDir() {
				isDir = true
			}
		}
		if isDir {
			d.Dirs = append(d.Dirs, e)
		} else {
			d.Files = append(d.Files, e)
		}
	}
	if err := fn(d); err != nil {
		return err
	}
	for _, e := range d.Dirs {
		if !e.Mode.IsDir() {
			continue
		}
		sub, err := syscall.Openat(fd, e.Name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		if err := walkDir(sub, e.Path, max, fn); err != nil {
			return err
		}
	}
	return nil
}

func join(rel, name string) string {
	if rel == "." {
		return name
	}
	return path.Join(rel, name)
}

// readNames lists a directory in its own order, from a duplicate of fd so
// the os.File can be closed without closing fd: all of it, or with max
// set, max+1 names at most (one more than a cut listing keeps, to tell it
// was cut).
func readNames(fd, max int) ([]string, error) {
	dup, err := syscall.Dup(fd)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(dup)
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	if max <= 0 {
		return f.Readdirnames(-1)
	}
	names, err := f.Readdirnames(max + 1)
	if errors.Is(err, io.EOF) {
		err = nil // an empty directory
	}
	return names, err
}

// statAt stats name in the directory fd through an O_PATH open, which
// neither reads the file nor (with nofollow) follows a link.
func statAt(dirfd int, name string, nofollow bool) (fs.FileMode, error) {
	flags := oPath | syscall.O_CLOEXEC
	if nofollow {
		flags |= syscall.O_NOFOLLOW
	}
	fd, err := syscall.Openat(dirfd, name, flags, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return 0, err
	}
	return modeOf(st.Mode), nil
}

func modeOf(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & syscall.S_IFMT {
	case syscall.S_IFDIR:
		mode |= fs.ModeDir
	case syscall.S_IFLNK:
		mode |= fs.ModeSymlink
	case syscall.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case syscall.S_IFSOCK:
		mode |= fs.ModeSocket
	case syscall.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case syscall.S_IFBLK:
		mode |= fs.ModeDevice
	}
	if m&syscall.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if m&syscall.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if m&syscall.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// Read reads one of the directory's entries: opened relative to the
// directory with O_NOFOLLOW|O_NONBLOCK (a link or a fifo cannot stall or
// redirect it), checked to be a regular file by fstat, and read up to max
// bytes. A longer file is ErrTooLarge. Valid only inside the walk's fn.
func (d *Dir) Read(e Entry, max int64) ([]byte, error) {
	if !e.Mode.IsRegular() {
		return nil, fmt.Errorf("read %s: %w", e.Path, ErrNotRegular)
	}
	fd, err := syscall.Openat(d.fd, e.Name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ELOOP {
			return nil, fmt.Errorf("read %s: %w", e.Path, ErrNotRegular)
		}
		return nil, fmt.Errorf("read %s: %w", e.Path, err)
	}
	return readCapped(fd, e.Path, max)
}

// Open opens one of the directory's entries for a caller that streams it,
// with Read's checks: relative to the directory, no link followed, no fifo
// stall, a regular file only. Valid only inside the walk's fn; the file
// stays open after it, and the caller closes it.
func (d *Dir) Open(e Entry) (*os.File, error) {
	if !e.Mode.IsRegular() {
		return nil, fmt.Errorf("open %s: %w", e.Path, ErrNotRegular)
	}
	fd, err := syscall.Openat(d.fd, e.Name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ELOOP {
			return nil, fmt.Errorf("open %s: %w", e.Path, ErrNotRegular)
		}
		return nil, fmt.Errorf("open %s: %w", e.Path, err)
	}
	f := os.NewFile(uintptr(fd), e.Path)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, fmt.Errorf("open %s: %w", e.Path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		f.Close()
		return nil, fmt.Errorf("open %s: %w", e.Path, ErrNotRegular)
	}
	return f, nil
}

// ReadNoFollow reads the regular file at path up to max bytes, refusing a
// link in the last component, a fifo, a device or a socket. Directories
// on the way are followed as usual: use WalkNoFollow where they may be
// planted.
func ReadNoFollow(path string, max int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ELOOP {
			return nil, fmt.Errorf("read %s: %w", path, ErrNotRegular)
		}
		return nil, fmt.Errorf("read %s: %w", path, &fs.PathError{Op: "open", Path: path, Err: err})
	}
	return readCapped(fd, path, max)
}

// readCapped owns fd.
func readCapped(fd int, name string, max int64) ([]byte, error) {
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, fmt.Errorf("read %s: %w", name, ErrNotRegular)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("read %s: over %d bytes: %w", name, max, ErrTooLarge)
	}
	return b, nil
}
