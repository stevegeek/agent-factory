package mailbox

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// usage is what mailbox mode says to anything it does not take.
const usage = `factory-remote mailbox HOST: the forced command of a pull host's key. Verbs (SSH_ORIGINAL_COMMAND):
  claim HOST          the oldest queued job: "<job>\n" then its start tar on stdout; exit 3 if none
  beat HOST JOB       stdin: a log tail (at most 1 MiB); answers "ok" or "cancel"
  done HOST JOB       stdin: the collect tar (report.json, prompt.md, head, agent.log, out.bundle, memory.tar, usage.json)
  release HOST JOB    give the job back to the queue
  vitals HOST         stdin: this host's vitals as JSON (at most 64 KiB); answers "ok"
HOST must be the host this key is for.`

// maxCommand bounds SSH_ORIGINAL_COMMAND: a verb and two names.
const maxCommand = 128

// Serve is factory-remote's mailbox mode, `factory-remote mailbox <host>`:
// the forced command of a pull host's key, so the host named in
// authorized_keys is the only one the key can act for. command is
// SSH_ORIGINAL_COMMAND; it must be exactly `<verb> <host> [<job>]` (no
// job for claim and vitals), the host the same as the forced one, every
// name a job name. It returns the
// exit status: 0, 2 for a refusal (its message on stderr), 3 for a claim
// with nothing queued, 1 for a failure.
func (b Box) Serve(host, command string, stdin io.Reader, stdout, stderr io.Writer) int {
	err := b.serve(host, command, stdin, stdout)
	var r *Refusal
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrNoJob):
		return jobproto.ExitNoJob
	case errors.As(err, &r):
		fmt.Fprintln(stderr, r.Msg)
		return 2
	}
	fmt.Fprintf(stderr, "factory-remote mailbox: %v\n", err)
	return 1
}

func (b Box) serve(host, command string, stdin io.Reader, stdout io.Writer) error {
	if !jobproto.JobNameOK(host) {
		return refuse("mailbox: bad host name in the forced command")
	}
	if len(command) > maxCommand {
		return refuse("%s", usage)
	}
	args := strings.Split(command, " ")
	if len(args) < 2 || args[1] != host {
		return refuse("%s", usage)
	}
	for _, a := range args[1:] {
		if !jobproto.JobNameOK(a) {
			return refuse("%s", usage)
		}
	}
	verb := args[0]
	if verb == jobproto.MailClaim {
		if len(args) != 2 {
			return refuse("%s", usage)
		}
		job, tar, err := b.Claim(host)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "%s\n", job); err != nil {
			return err
		}
		_, err = stdout.Write(tar)
		return err
	}
	if verb == jobproto.MailVitals {
		if len(args) != 2 {
			return refuse("%s", usage)
		}
		data, err := readCapped(stdin, jobproto.MaxVitals)
		if err != nil {
			return err
		}
		if err := b.PostVitals(host, data); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "ok")
		return err
	}
	if len(args) != 3 {
		return refuse("%s", usage)
	}
	job := args[2]
	switch verb {
	case jobproto.MailBeat:
		log, err := readCapped(stdin, jobproto.MaxBeatInput)
		if err != nil {
			return err
		}
		answer, err := b.Beat(host, job, log)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, answer)
		return err
	case jobproto.MailDone:
		tar, err := readCapped(stdin, jobproto.MaxCollectTar)
		if err != nil {
			return err
		}
		if err := b.Done(host, job, tar); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "ok")
		return err
	case jobproto.MailRelease:
		if err := b.Release(host, job); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, "ok")
		return err
	}
	return refuse("%s", usage)
}

// readCapped reads all of r, refusing more than max bytes.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, refuse("input over %d bytes", max)
	}
	return b, nil
}
