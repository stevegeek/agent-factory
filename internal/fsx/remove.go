package fsx

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// RemoveTree deletes the tree at path even where an agent made directories
// unreadable (Python remove_copy): each directory is opened with
// O_PATH|O_NOFOLLOW|O_DIRECTORY, which works on a mode-000 directory and
// never through a link, and chmodded 0700 through /proc/self/fd before it
// is entered; then the tree is removed without following links. A path that
// is a link or a file is removed itself. A missing path is not an error.
func RemoveTree(path string) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	if st.IsDir() {
		if fd, err := syscall.Open(path, oPath|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0); err == nil {
			unlockDirs(fd)
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// unlockDirs owns pathfd, an O_PATH descriptor of a directory: it makes the
// directory 0700, then does the same for each directory inside it.
func unlockDirs(pathfd int) {
	defer syscall.Close(pathfd)
	proc := "/proc/self/fd/" + strconv.Itoa(pathfd)
	_ = os.Chmod(proc, 0o700)
	fd, err := syscall.Open(proc, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer syscall.Close(fd)
	names, err := readNames(fd, 0)
	if err != nil {
		return
	}
	for _, name := range names {
		sub, err := syscall.Openat(fd, name, oPath|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue // not a directory, or a link
		}
		unlockDirs(sub)
	}
}
