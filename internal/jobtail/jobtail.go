// Package jobtail is read-only access to helper jobs: the tail of a job's
// output, its state, and the list of jobs. It is Foreman's only path to a
// helper host, so each function names its verb as a constant and no caller
// can name another.
package jobtail

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// Reader reads helper jobs over ssh.
type Reader struct {
	Exec execx.Execer
	Env  []string // ssh's environment; nil passes this process's on
}

// Tail is `tail <job>`: the last 64 KiB of the job's agent.log, decoded as
// Python's errors="replace" does (look). A failure is a
// *jobproto.RemoteError; the caller decides how to show it.
func (r Reader) Tail(ctx context.Context, h jobproto.Host, job string) (string, error) {
	out, err := r.run(ctx, h, jobproto.VerbTail, job)
	if err != nil {
		return "", err
	}
	return jobproto.DecodeText(out), nil
}

// Status is `status <job>`: jobproto.StateRunning, StateDone, StateMissing,
// or whatever else the helper said (which the tick reads as gone).
func (r Reader) Status(ctx context.Context, h jobproto.Host, job string) (string, error) {
	out, err := r.run(ctx, h, jobproto.VerbStatus, job)
	if err != nil {
		return "", err
	}
	return jobproto.ParseStatus(out)
}

// List is `list`: the helper's job names, one per line.
func (r Reader) List(ctx context.Context, h jobproto.Host) ([]string, error) {
	out, err := r.run(ctx, h, jobproto.VerbList, "")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range bytes.Split(out, []byte("\n")) {
		if s := string(bytes.TrimSpace(line)); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// run stays unexported, so the verb is always one of the three above.
func (r Reader) run(ctx context.Context, h jobproto.Host, verb, job string) ([]byte, error) {
	argv, err := jobproto.SSHArgv(h, verb, job)
	if err != nil {
		return nil, err
	}
	res, err := r.Exec.Run(ctx, execx.Cmd{Argv: argv, Env: r.Env, Timeout: jobproto.Timeout(verb), MaxOut: jobproto.OutputCap(verb)})
	switch {
	case errors.Is(err, execx.ErrTimeout):
		return nil, jobproto.TimedOut(h.SSH, verb)
	case err != nil:
		return nil, fmt.Errorf("%s %s: %w", h.SSH, verb, err)
	case res.Code != 0:
		return nil, jobproto.Failed(h.SSH, verb, res.Code, res.Stderr)
	case res.Truncated:
		return nil, jobproto.TooMuch(h.SSH, verb)
	}
	return res.Stdout, nil
}
