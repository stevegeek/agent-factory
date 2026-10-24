// Package logx writes the factory log: Python's "<stamp> <msg>" line,
// echoed to stderr and appended to logs/factory.log (Foreman's to
// foreman-state/foreman.log) with one write per line, rotated at a size
// under a leaf flock. A rotated file is renamed factory.log.<stamp> and
// gzip-compressed beside it as factory.log.<stamp>.gz; every one is kept
// (nothing is deleted).
package logx

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
)

const (
	// DefaultMaxSize is the size at which factory.log is rotated.
	DefaultMaxSize = 8 << 20
	// RotateWait bounds the wait for the rotation lock: a writer that
	// cannot have it appends without rotating (another writer is).
	RotateWait = 2 * time.Second
	// rotatedLayout is a rotated file's stamp: UTC, no ":" in a file name.
	rotatedLayout = "20060102T150405Z"
	// FileName is the log's name in its directory.
	FileName = "factory.log"
	// LockName is the rotation lock's name in the log directory.
	LockName = ".log.lock"
)

// Clock is the time source.
type Clock = clock.Clock

// Stamp is the factory's time format (clock.Stamp).
func Stamp(t time.Time) string { return clock.Stamp(t) }

// Log is the factory log. The zero value of MaxSize means the default; a
// nil Stderr echoes nowhere. A Log is safe for concurrent use, and several
// Logs (or processes) may share Dir.
type Log struct {
	Dir     string // the logs directory, made on first use
	Clock   Clock
	Stderr  io.Writer
	MaxSize int64
	// Name is the log's file in Dir; "" is FileName. Foreman's is
	// foreman-state/foreman.log, because logs/ is read-only to it. It is
	// rotated and kept as factory.log is, with the lock in Dir.
	Name string
}

// Printf logs a formatted line. A failure to write the file is reported on
// Stderr, never returned: logging never stops the caller.
func (l *Log) Printf(format string, args ...any) {
	if err := l.Print(fmt.Sprintf(format, args...)); err != nil && l.Stderr != nil {
		fmt.Fprintf(l.Stderr, "%s log: %v\n", Stamp(l.Clock.Now()), err)
	}
}

// Print logs msg as given (a newline inside it stays, as in Python): to
// Stderr first, then appended to the file in one write.
func (l *Log) Print(msg string) error {
	line := Stamp(l.Clock.Now()) + " " + msg + "\n"
	if l.Stderr != nil {
		io.WriteString(l.Stderr, line)
	}
	name := l.Name
	if name == "" {
		name = FileName
	}
	path := filepath.Join(l.Dir, name)
	if err := l.rotateIfFull(path); err != nil {
		// a failed rotation never loses the line
		if werr := appendLine(l.Dir, path, line); werr != nil {
			return errors.Join(err, werr)
		}
		return err
	}
	return appendLine(l.Dir, path, line)
}

// logFlags: never through a symlink. Nothing untrusted should be able to write logs/, but if it
// could, a planted link must not redirect the tick, which runs with more reach.
const logFlags = os.O_WRONLY | os.O_APPEND | os.O_CREATE | syscall.O_NOFOLLOW

func appendLine(dir, path, line string) error {
	f, err := os.OpenFile(path, logFlags, 0o666)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(dir, 0o777); err == nil {
			f, err = os.OpenFile(path, logFlags, 0o666)
		}
	}
	if err != nil {
		return fmt.Errorf("log %s: %w", path, err)
	}
	_, err = f.WriteString(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("log %s: %w", path, err)
	}
	return nil
}

// rotateIfFull renames factory.log to factory.log.<stamp> once it has
// reached MaxSize, then compresses that file to factory.log.<stamp>.gz.
// The lock is a leaf, taken only when rotating, waited for at most
// RotateWait (a writer that cannot have it appends without rotating), and
// the size is checked again under it, so of several writers only one
// rotates. The rename is under the lock; the compression is after it, so
// no writer waits for gzip. Nothing is ever deleted but the uncompressed
// copy, once its .gz is complete.
func (l *Log) rotateIfFull(path string) error {
	max := l.MaxSize
	if max <= 0 {
		max = DefaultMaxSize
	}
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() || st.Size() < max {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), RotateWait)
	defer cancel()
	lock, err := fsx.Lock(ctx, filepath.Join(l.Dir, LockName), fsx.Wait(RotateWait))
	if errors.Is(err, fsx.ErrBusy) || errors.Is(err, context.DeadlineExceeded) {
		return nil // another writer is rotating: this line goes in the current file
	}
	if err != nil {
		return fmt.Errorf("rotate %s: %w", path, err)
	}
	rotated, err := l.rename(path, max)
	var older []string
	if err == nil && rotated != "" {
		older = uncompressed(path, rotated)
	}
	lock.Unlock()
	if err != nil || rotated == "" {
		return err
	}
	// The file just renamed stays as it is until the next rotation: a
	// writer that opened factory.log before the rename may still append to
	// it. The ones before it have had a whole rotation to settle.
	var errs []error
	for _, f := range older {
		if err := compress(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// uncompressed are the rotated files not yet compressed, but for newest.
func uncompressed(path, newest string) []string {
	matches, _ := filepath.Glob(path + ".2*")
	var out []string
	for _, m := range matches {
		if m == newest || strings.HasSuffix(m, ".gz") || strings.HasSuffix(m, ".tmp") {
			continue
		}
		if st, err := os.Lstat(m); err != nil || !st.Mode().IsRegular() {
			continue
		}
		out = append(out, m)
	}
	return out
}

// rename moves a full log aside under a name no rotated file has.
func (l *Log) rename(path string, max int64) (string, error) {
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() || st.Size() < max {
		return "", nil
	}
	base := path + "." + l.Clock.Now().UTC().Format(rotatedLayout)
	dst := base
	for n := 1; exists(dst) || exists(dst+".gz"); n++ {
		dst = base + "-" + strconv.Itoa(n)
	}
	if err := os.Rename(path, dst); err != nil {
		return "", fmt.Errorf("rotate %s: %w", path, err)
	}
	return dst, nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// compress writes src.gz (through a temporary file and a rename) and then
// removes src. On any failure src stays as it is: the log is never lost.
// A file another writer compressed meanwhile is no error, and one that
// grew while it was read is left for the next rotation.
func compress(src string) (err error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil // compressed by another writer
	}
	if err != nil {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	defer in.Close()
	// only a plain file of its own: a hard link to something else (a key on the same filesystem)
	// would be copied into the readable .gz
	if fi, serr := in.Stat(); serr != nil || !fi.Mode().IsRegular() {
		return nil
	} else if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink != 1 {
		return nil
	}
	out, err := os.CreateTemp(filepath.Dir(src), filepath.Base(src)+".gz.*.tmp")
	if err != nil {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	tmp := out.Name()
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(tmp)
		}
	}()
	gz := gzip.NewWriter(out)
	gz.Name = filepath.Base(src)
	n, err := io.Copy(gz, in)
	if err != nil {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	if err = gz.Close(); err != nil {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	if err = out.Chmod(0o644); err != nil {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	if err = out.Sync(); err != nil {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	if err = out.Close(); err != nil {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	if st, serr := in.Stat(); serr != nil || st.Size() != n {
		os.Remove(tmp)
		return nil // it grew: the next rotation compresses it whole
	}
	if err = os.Rename(tmp, src+".gz"); err != nil {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	if err := os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("compress %s: %w", src, err)
	}
	return nil
}
