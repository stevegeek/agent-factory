// Package sshjobs is the dispatcher's client for factory-remote: one helper
// verb over OpenSSH (Python remote()). The helper's authorized_keys
// forces the key to factory-remote, so the verbs of jobproto.SSHVerbs are
// all a call can do there.
//
// Foreman never links this package; its read-only path to a
// helper is jobtail.
package sshjobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// Host is one helper host: its [hosts.<name>] key, ssh destination and key.
type Host = jobproto.Host

// RemoteError is a verb that failed on the helper or timed out.
type RemoteError = jobproto.RemoteError

// Client runs verbs on helpers.
type Client struct {
	Exec execx.Execer
	// Env is ssh's environment; nil passes this process's on, as Python's
	// subprocess.run does (ssh needs HOME for known_hosts).
	Env []string
}

// Do runs verb with job on h, sending stdin, and returns what the helper
// wrote to stdout. timeout 0 is the verb's (jobproto.Timeout). A non-zero
// exit, a timeout or an answer over jobproto.OutputCap is a *RemoteError;
// a cancelled context, or an ssh that would not start, is not one, so the
// tick does not take it as the host being down.
func (c Client) Do(ctx context.Context, h Host, verb, job string, stdin []byte, timeout time.Duration) ([]byte, error) {
	argv, err := jobproto.SSHArgv(h, verb, job)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = jobproto.Timeout(verb)
	}
	cmd := execx.Cmd{Argv: argv, Env: c.Env, Timeout: timeout, MaxOut: jobproto.OutputCap(verb)}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	res, err := c.Exec.Run(ctx, cmd)
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
