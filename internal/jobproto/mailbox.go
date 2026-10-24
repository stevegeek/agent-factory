package jobproto

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

// A pull host (`transport = "pull"`) is one the dispatcher cannot reach:
// its jobs wait in a mailbox on the dispatcher, ~/factory/mailbox/<host>/,
// and the host calls in over ssh with a key forced to
// `factory-remote mailbox <host>` to claim them, beat while they run, and
// post what they left. These are the names both sides share.

// StateQueued is a pull job not yet claimed: the tick waits on it as on a
// running one.
const StateQueued = "queued"

// The mailbox verbs a pull host sends (SSH_ORIGINAL_COMMAND: `<verb> <host>
// [<job>]`), and the argv word that puts factory-remote in mailbox mode.
const (
	MailMode    = "mailbox"
	MailClaim   = "claim"   // claim <host>: the oldest queued job, "<job>\n" then its start tar
	MailBeat    = "beat"    // beat <host> <job>: stdin a log tail; answers BeatOK or BeatCancel
	MailDone    = "done"    // done <host> <job>: stdin the collect tar
	MailRelease = "release" // release <host> <job>: back to the queue
	MailVitals  = "vitals"  // vitals <host>: stdin the host's vitals JSON (at most MaxVitals); answers "ok"
)

// MailVerbs are the verbs of mailbox mode.
func MailVerbs() []string { return []string{MailClaim, MailBeat, MailDone, MailRelease, MailVitals} }

// What a beat answers: carry on, or stop (the job was removed or is no
// longer this host's).
const (
	BeatOK     = "ok"
	BeatCancel = "cancel"
)

// ExitNoJob is claim's exit status when nothing is queued.
const ExitNoJob = 3

// Limits of the mailbox verbs.
const (
	MaxBeatInput    = 1 << 20   // a beat's stdin; its last TailBytes are kept
	MaxCollectTar   = 128 << 20 // done's stdin, as OutputCap(VerbCollect)
	MailVerbTimeout = 60 * time.Second
	MailBulkTimeout = 300 * time.Second // claim and done move a whole tar
)

// The files of one mailbox job, mailbox/<host>/<job>/, and of its host.
const (
	MailStartTar   = "start.tar"   // the start tar, as factory-remote would get it
	MailDeadline   = "deadline"    // unix seconds: unclaimed past it, the job is lost
	MailWindow     = "window"      // seconds a claim may take, for a released job's new deadline
	MailClaimed    = "claimed"     // unix seconds of the claim; made exclusively
	MailBeatAt     = "beat"        // unix seconds of the last beat
	MailLog        = "beat.log"    // the last beat's log tail (what `tail` shows)
	MailDoneTar    = "done.tar"    // the collect tar the host posted
	MailSeen       = "last_seen"   // per host: unix seconds the host last called in
	MailVitalsFile = "vitals.json" // per host: its last vitals, stamped with the dispatcher's clock
)

// MailTimeout is how long a pull host waits on one mailbox verb.
func MailTimeout(verb string) time.Duration {
	if verb == MailClaim || verb == MailDone {
		return MailBulkTimeout
	}
	return MailVerbTimeout
}

// MailArgv is the OpenSSH argv of one mailbox verb, from a pull host to the
// dispatcher: BatchMode, a 10 s connect timeout, the key when one is given
// (else ssh's own config for dest decides), then the verb, the host and,
// for every verb but claim and vitals, the job.
func MailArgv(ssh, dest, key, verb, host, job string) ([]string, error) {
	if dest == "" || dest[0] == '-' {
		return nil, fmt.Errorf("bad ssh destination %q", dest)
	}
	if !slices.Contains(MailVerbs(), verb) {
		return nil, fmt.Errorf("%q is not a mailbox verb", verb)
	}
	if !JobNameOK(host) {
		return nil, fmt.Errorf("bad host name %q", host)
	}
	argv := []string{ssh, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if key != "" {
		argv = append(argv, "-o", "IdentitiesOnly=yes", "-i", key)
	}
	argv = append(argv, dest, verb, host)
	if verb == MailClaim || verb == MailVitals {
		if job != "" {
			return nil, fmt.Errorf("%s takes no job", verb)
		}
		return argv, nil
	}
	if !JobNameOK(job) {
		return nil, fmt.Errorf("bad job name %q", job)
	}
	return append(argv, job), nil
}

// capOf is a collect member's cap, and whether the name is one.
func capOf(name string) (int64, bool) {
	for _, c := range CollectCaps() {
		if c.Name == name {
			return c.Max, true
		}
	}
	return 0, false
}

// CheckCollect is what a mailbox takes as a posted collect tar: a tar
// whose every member is a regular file named as one of CollectCaps, once,
// within its cap. Anything else (a link, a directory, a path, an unknown
// or oversized member, a tar cut short) refuses the whole tar.
func CheckCollect(b []byte) error {
	if len(b) == 0 {
		return ErrEmptyTar
	}
	if len(b) > MaxCollectTar {
		return fmt.Errorf("collect tar over %d bytes", MaxCollectTar)
	}
	seen := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("collect tar: %w", err)
		}
		max, ok := capOf(h.Name)
		switch {
		case !ok:
			return fmt.Errorf("collect tar: %q is not a collect member", h.Name)
		case !regular(h.Typeflag):
			return fmt.Errorf("collect tar: %s is not a regular file", h.Name)
		case seen[h.Name]:
			return fmt.Errorf("collect tar: %s twice", h.Name)
		case h.Size > max:
			return fmt.Errorf("collect tar: %s is over %d bytes", h.Name, max)
		}
		seen[h.Name] = true
		if n, err := io.Copy(io.Discard, tr); err != nil || n != h.Size {
			return fmt.Errorf("collect tar: %s cut short", h.Name)
		}
	}
}

// PackCollect is a collect tar of files, as factory-remote's collect
// builds it: the CollectCaps members present, in that order, each within
// its cap; a member over its cap is left out, except agent.log, which is
// cut to its tail. Other names are dropped.
func PackCollect(files map[string][]byte) ([]byte, error) {
	var out []File
	for _, c := range CollectCaps() {
		data, ok := files[c.Name]
		if !ok {
			continue
		}
		if int64(len(data)) > c.Max {
			if c.Name != MemberLog {
				continue
			}
			data = data[int64(len(data))-c.Max:]
		}
		out = append(out, File{Name: c.Name, Data: data})
	}
	return TarFlat(out, time.Unix(0, 0))
}
