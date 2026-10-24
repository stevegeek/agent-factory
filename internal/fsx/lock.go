package fsx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// ErrBusy is returned when a lock is held elsewhere and the wait ran out.
var ErrBusy = errors.New("lock is held elsewhere")

// PollInterval is how often a waiting Lock tries again (Python ticket_lock
// sleeps 0.2 s).
const PollInterval = 200 * time.Millisecond

// Wait is how long Lock waits for a lock held elsewhere.
type Wait time.Duration

const (
	// Forever waits until the lock is free or the context ends (the tick).
	Forever Wait = -1
	// NoWait tries once.
	NoWait Wait = 0
)

func (w Wait) String() string {
	switch {
	case w < 0:
		return "forever"
	case w == 0:
		return "no wait"
	}
	return time.Duration(w).String()
}

// Locked is a held flock(2). The lock belongs to its own open file
// description, so two Locked values exclude each other inside one process
// exactly as two processes do. Lock files are never deleted.
type Locked struct {
	f    *os.File
	path string
}

// Path is the lock file's path.
func (l *Locked) Path() string { return l.path }

// Unlock releases the lock by closing its descriptor.
func (l *Locked) Unlock() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("unlock %s: %w", l.path, err)
	}
	return nil
}

// TryLock takes the lock at path without waiting, or returns ErrBusy. The
// file is created (0666 less the umask) if missing; its directory must
// exist. A link at path is refused.
func TryLock(path string) (*Locked, error) {
	return Lock(context.Background(), path, NoWait)
}

// Lock takes an exclusive flock on path. While another holds it, Lock tries
// again every PollInterval until wait has passed (ErrBusy) or ctx ends (the
// context's error); Forever waits for as long as ctx allows.
func Lock(ctx context.Context, path string, wait Wait) (*Locked, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	var giveUp time.Time
	if wait >= 0 {
		giveUp = time.Now().Add(time.Duration(wait))
	}
	for {
		err := flockNB(f)
		if err == nil {
			return &Locked{f: f, path: path}, nil
		}
		if !errors.Is(err, ErrBusy) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if wait >= 0 && !time.Now().Before(giveUp) {
			f.Close()
			return nil, fmt.Errorf("lock %s (waited %v): %w", path, wait, ErrBusy)
		}
		t := time.NewTimer(PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-t.C:
		}
	}
}

func flockNB(f *os.File) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := raw.Control(func(fd uintptr) {
		for {
			ferr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
			if ferr != syscall.EINTR {
				return
			}
		}
	}); err != nil {
		return err
	}
	if ferr == syscall.EWOULDBLOCK {
		return ErrBusy
	}
	return ferr
}
