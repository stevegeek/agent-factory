package mailbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// Do is the dispatcher's side: one helper verb on h's mailbox, answered
// as factory-remote answers it, so hostrun.Jobs runs a pull host as it
// runs any helper. Every failure is a *jobproto.RemoteError, which the
// tick takes as that host being unusable for this pass. ctx and timeout
// are not needed: nothing here waits on another machine.
func (b Box) Do(_ context.Context, h jobproto.Host, verb, job string, stdin []byte, _ time.Duration) ([]byte, error) {
	out, err := b.do(h.Name, verb, job, stdin)
	if err != nil {
		return nil, &jobproto.RemoteError{SSH: "mailbox " + h.Name, Verb: verb,
			Msg: fmt.Sprintf("mailbox %s %s failed: %v", h.Name, verb, err)}
	}
	return out, nil
}

func (b Box) do(host, verb, job string, stdin []byte) ([]byte, error) {
	if verb == jobproto.VerbList {
		names, err := b.Jobs(host)
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			return nil, nil
		}
		return []byte(strings.Join(names, "\n") + "\n"), nil
	}
	if verb == jobproto.VerbVitals {
		return nil, refuse("a pull host posts its vitals (Box.Vitals reads them)")
	}
	if _, err := b.jobDir(host, job); err != nil {
		return nil, err
	}
	switch verb {
	case jobproto.VerbStart:
		if err := b.Start(host, job, stdin); err != nil {
			return nil, err
		}
		return fmt.Appendf(nil, "{\"started\": \"%s\"}\n", job), nil
	case jobproto.VerbStatus:
		st, err := b.State(host, job)
		if err != nil {
			return nil, err
		}
		return fmt.Appendf(nil, "{\"state\": \"%s\"}\n", st), nil
	case jobproto.VerbCollect:
		return b.Collect(host, job)
	case jobproto.VerbTail:
		s, err := b.tail(host, job)
		return []byte(s), err
	case jobproto.VerbRemove:
		if err := b.Remove(host, job); err != nil {
			return nil, err
		}
		return fmt.Appendf(nil, "{\"removed\": \"%s\"}\n", job), nil
	}
	return nil, refuse("%q is not a helper verb", verb)
}

// tail is the log tail the job's last beat left, "" when there is none
// (jobtail.MailTail is the same read for Foreman, which never links this
// package: it can start jobs).
func (b Box) tail(host, job string) (string, error) {
	d, err := b.jobDir(host, job)
	if err != nil {
		return "", err
	}
	data, err := fsx.ReadNoFollow(filepath.Join(d, jobproto.MailLog), jobproto.TailBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return jobproto.DecodeText(data), nil
}

// Tail is the job's last beat's log tail (hostrun's job reader).
func (b Box) Tail(_ context.Context, h jobproto.Host, job string) (string, error) {
	s, err := b.tail(h.Name, job)
	if err != nil {
		return "", &jobproto.RemoteError{SSH: "mailbox " + h.Name, Verb: jobproto.VerbTail, Msg: err.Error()}
	}
	return s, nil
}

// List is the host's queued, running and done jobs (hostrun's job
// reader).
func (b Box) List(_ context.Context, h jobproto.Host) ([]string, error) {
	names, err := b.Jobs(h.Name)
	if err != nil {
		return nil, &jobproto.RemoteError{SSH: "mailbox " + h.Name, Verb: jobproto.VerbList, Msg: err.Error()}
	}
	return names, nil
}
