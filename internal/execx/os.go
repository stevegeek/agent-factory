package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// DefaultWaitDelay is how long a group gets between SIGTERM and SIGKILL.
const DefaultWaitDelay = 5 * time.Second

// OS runs commands for real.
type OS struct {
	// WaitDelay is the grace between SIGTERM and SIGKILL; zero means
	// DefaultWaitDelay.
	WaitDelay time.Duration
}

func (o OS) waitDelay() time.Duration {
	if o.WaitDelay > 0 {
		return o.WaitDelay
	}
	return DefaultWaitDelay
}

// Run runs c in its own process group and waits for it and for its output
// to end. On timeout or cancellation the group gets SIGTERM, then SIGKILL
// after WaitDelay, and c.Cleanup runs.
func (o OS) Run(ctx context.Context, c Cmd) (Result, error) {
	res := Result{Argv: c.Argv}
	if len(c.Argv) == 0 {
		return res, errors.New("run: empty argv")
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("run %s: %w", c.Argv[0], err)
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, c.Timeout, ErrTimeout)
		defer cancel()
	}
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = c.Dir, c.Env, c.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	outR, outW, err := os.Pipe()
	if err != nil {
		return res, fmt.Errorf("run %s: %w", c.Argv[0], err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return res, fmt.Errorf("run %s: %w", c.Argv[0], err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		if errors.Is(err, exec.ErrNotFound) {
			err = fmt.Errorf("%w (%w)", err, fs.ErrNotExist)
		}
		return res, fmt.Errorf("run %s: %w", c.Argv[0], err)
	}
	pgid := cmd.Process.Pid

	stdout, stderr := &capBuf{max: c.MaxOut}, &capBuf{max: c.MaxOut}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); io.Copy(stdout, outR) }()
	go func() { defer readers.Done(); io.Copy(stderr, errR) }()
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	interrupted := false
	stop := func() {
		// TERM the group, give it WaitDelay, then KILL whatever is left
		interrupted = true
		syscall.Kill(-pgid, syscall.SIGTERM)
		t := time.NewTimer(o.waitDelay())
		defer t.Stop()
		select {
		case <-drained:
		case <-t.C:
		}
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
	var werr error
	select {
	case werr = <-exited:
		// the leader is gone; its group may still hold the pipes
		select {
		case <-drained:
		case <-ctx.Done():
			stop()
		}
	case <-ctx.Done():
		stop()
		werr = <-exited
	}
	// a member that left the group (setsid) may hold a pipe open: after a
	// stop, give the readers one more WaitDelay, then cut them off
	if interrupted {
		t := time.NewTimer(o.waitDelay())
		select {
		case <-drained:
		case <-t.C:
			outR.Close()
			errR.Close()
			<-drained
		}
		t.Stop()
	}
	outR.Close()
	errR.Close()

	res.Stdout, res.Stderr = stdout.bytes(), stderr.bytes()
	res.Truncated = stdout.trunc || stderr.trunc
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Code = -int(ws.Signal())
		} else {
			res.Code = ps.ExitCode()
		}
	}
	if interrupted {
		if c.Cleanup != nil {
			c.Cleanup()
		}
		if errors.Is(context.Cause(ctx), ErrTimeout) && c.Timeout > 0 {
			return res, fmt.Errorf("%s: %w after %v", c.Argv[0], ErrTimeout, c.Timeout)
		}
		return res, fmt.Errorf("%s: %w", c.Argv[0], ctx.Err())
	}
	var exitErr *exec.ExitError
	if werr != nil && !errors.As(werr, &exitErr) {
		return res, fmt.Errorf("run %s: %w", c.Argv[0], werr)
	}
	return res, nil
}

// Spawn starts c detached: in a new session, stdio on /dev/null, not
// waited for (a goroutine reaps it, so a long-lived caller collects no
// zombies). c.Timeout, c.Stdin and c.Cleanup are ignored.
func (OS) Spawn(c Cmd) error {
	if len(c.Argv) == 0 {
		return errors.New("spawn: empty argv")
	}
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env = c.Dir, c.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", c.Argv[0], err)
	}
	go cmd.Wait()
	return nil
}

// capBuf keeps everything, or with max > 0 the last max bytes.
type capBuf struct {
	max   int
	buf   []byte
	trunc bool
}

func (b *capBuf) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if b.max > 0 && len(b.buf) > 2*b.max {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.max:]...)
		b.trunc = true
	}
	return len(p), nil
}

func (b *capBuf) bytes() []byte {
	if b.max > 0 && len(b.buf) > b.max {
		b.trunc = true
		return b.buf[len(b.buf)-b.max:]
	}
	return b.buf
}
