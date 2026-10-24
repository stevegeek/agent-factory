// Package execx runs subprocesses (ARCHITECTURE.md §5.3). Every command
// runs in its own process group; a timeout or a cancelled context sends
// SIGTERM to the group and SIGKILL after WaitDelay, so no child outlives
// its run. Output is captured with an optional tail-kept cap.
//
// Execer is the seam every adapter takes: OS runs for real, and Func turns
// a function into a fake.
package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrTimeout is returned when a command ran past its Cmd.Timeout.
var ErrTimeout = errors.New("timed out")

// Cmd is one command to run. Argv is a list, never a shell string.
type Cmd struct {
	Argv []string
	Dir  string
	// Env is the whole environment of the command. nil inherits this
	// process's environment (Python run(env=None)); callers build it
	// explicitly wherever credentials or a repository are involved.
	Env []string
	// Stdin is streamed to the command; nil gives it /dev/null.
	Stdin io.Reader
	// Timeout bounds the run; zero means only ctx bounds it.
	Timeout time.Duration
	// MaxOut caps each of stdout and stderr, keeping the tail; zero keeps
	// everything, as Python does.
	MaxOut int
	// Cleanup runs after a run that timed out or was cancelled, for what
	// outlives the process group (a container: podman rm -f <name>). It
	// gets no context: it must bound itself.
	Cleanup func()
}

// Result is what a command did. Code is its exit status, or -signum when a
// signal ended it (Python's returncode).
type Result struct {
	Argv      []string
	Code      int
	Stdout    []byte
	Stderr    []byte
	Truncated bool // a stream went over MaxOut and lost its head
}

// Out is stdout as a string.
func (r Result) Out() string { return string(r.Stdout) }

// Execer runs commands. A non-zero exit is not an error: it is in
// Result.Code (CheckErr turns it into one). Errors are for a command that
// could not start, timed out or was cancelled; the Result then holds what
// output there was.
type Execer interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Func adapts a function to an Execer, for fakes.
type Func func(ctx context.Context, c Cmd) (Result, error)

// Run calls f.
func (f Func) Run(ctx context.Context, c Cmd) (Result, error) { return f(ctx, c) }

// ExitError is a command's non-zero exit, as CheckErr reports it.
type ExitError struct {
	Argv   []string
	Code   int
	Stderr string // the last 400 characters of the stripped stderr
}

// Error formats like Python run(check=True):
// "<argv[:4]> failed (<rc>): <last 400 chars of stderr.strip()>".
func (e *ExitError) Error() string {
	head := e.Argv
	if len(head) > 4 {
		head = head[:4]
	}
	return fmt.Sprintf("%s failed (%d): %s", strings.Join(head, " "), e.Code, e.Stderr)
}

// CheckErr is nil for a zero exit, else an *ExitError.
func CheckErr(r Result) error {
	if r.Code == 0 {
		return nil
	}
	return &ExitError{Argv: r.Argv, Code: r.Code, Stderr: lastChars(pyStrip(r.Stderr), 400)}
}

// pyStrip is Python str.strip(): Unicode whitespace plus the separators
// \x1c-\x1f, which Python counts as whitespace and Go does not.
func pyStrip(b []byte) string {
	s := strings.ToValidUTF8(string(b), "�")
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// lastChars keeps the last n code points (Python s[-n:]).
func lastChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := len(s)
	for range n {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}
