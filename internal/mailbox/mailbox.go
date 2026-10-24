// Package mailbox is the pull transport: the jobs of a host the
// dispatcher cannot reach wait in ~/factory/mailbox/<host>/<job>/, and the
// host calls in to take them. Two sides share the directory, both as the
// factory user:
//
//   - the dispatcher, through Box.Do, which answers the job verbs of a
//     helper (start, status, collect, tail, remove, list) from files, so a
//     pull host is a hostrun.Jobs like any helper, and Box.Vitals, the
//     host's last vitals for sysinfo;
//   - the host's forced command, `factory-remote mailbox <host>`, through
//     Box.Serve: claim, beat, done, release and vitals
//     (jobproto.MailVerbs).
//
// A job's life, by its files (jobproto.Mail*): start writes start.tar,
// deadline and window into a new directory and renames it into place
// (queued); claim creates `claimed` exclusively (running); each beat
// rewrites `beat` and `beat.log`; done writes done.tar (done); release
// takes the claim back and sets a new deadline (queued again). A queued job
// past its deadline, or a claimed one with no beat for Box.BeatWindow, is
// missing: the tick removes it and counts the agent lost, as for a helper
// job that disappeared. remove renames the directory away and deletes it,
// which the host's next beat reads as cancel.
//
// Times are unix seconds written in the files by this package, never file
// mtimes, so a test's clock decides them. Nothing here follows a link in a
// file's last component, and every input is capped.
package mailbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// Box is the mailbox directory, ~/factory/mailbox.
type Box struct {
	Dir   string
	Clock clock.Clock
	// ClaimWindow is how long a queued job waits for a claim ([hosts] claim_minutes).
	ClaimWindow time.Duration
	// BeatWindow is how long the host may go without calling in ([hosts]
	// beat_minutes): past it the host is away, and its claimed jobs that
	// have not beaten are missing.
	BeatWindow time.Duration
}

// ErrNoJob is a claim with nothing queued for the host.
var ErrNoJob = errors.New("no job queued")

// Refusal is a call the mailbox turns down (a bad name, a job in the wrong
// state, an input over its cap): Do returns it as a *jobproto.RemoteError,
// Serve as exit 2.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

func refuse(format string, args ...any) error { return &Refusal{fmt.Sprintf(format, args...)} }

func (b Box) hostDir(host string) (string, error) {
	if !jobproto.JobNameOK(host) {
		return "", refuse("bad host name: %q", host)
	}
	return filepath.Join(b.Dir, host), nil
}

func (b Box) jobDir(host, job string) (string, error) {
	d, err := b.hostDir(host)
	if err != nil {
		return "", err
	}
	if !jobproto.JobNameOK(job) {
		return "", refuse("bad job name: %q", job)
	}
	return filepath.Join(d, job), nil
}

func (b Box) now() time.Time { return b.Clock.Now() }

// isDir reports a real directory at p (never one reached through a link).
func isDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// isFile reports a regular file at p (never through a link).
func isFile(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// readUnix reads a time file; false when there is none or it is not one.
func readUnix(p string) (time.Time, bool) {
	b, err := fsx.ReadNoFollow(p, 64)
	if err != nil {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0).UTC(), true
}

func writeUnix(p string, t time.Time) error {
	return fsx.WriteAtomic(p, []byte(strconv.FormatInt(t.Unix(), 10)+"\n"), 0o600)
}

// Away reports a pull host that has not called in (claim or beat) for
// b.BeatWindow: the tick sends it no new jobs.
func (b Box) Away(host string) bool {
	d, err := b.hostDir(host)
	if err != nil {
		return true
	}
	seen, ok := readUnix(filepath.Join(d, jobproto.MailSeen))
	return !ok || b.now().Sub(seen) > b.BeatWindow
}

// seen records that the host called in.
func (b Box) seen(host string) error {
	d, err := b.hostDir(host)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	return writeUnix(filepath.Join(d, jobproto.MailSeen), b.now())
}

// State is a job's state: jobproto.StateQueued, StateRunning, StateDone or
// StateMissing.
func (b Box) State(host, job string) (string, error) {
	d, err := b.jobDir(host, job)
	if err != nil {
		return "", err
	}
	now := b.now()
	switch {
	case !isDir(d):
		return jobproto.StateMissing, nil
	case isFile(filepath.Join(d, jobproto.MailDoneTar)):
		return jobproto.StateDone, nil
	}
	if claimed, ok := readUnix(filepath.Join(d, jobproto.MailClaimed)); ok {
		last := claimed
		if beat, ok := readUnix(filepath.Join(d, jobproto.MailBeatAt)); ok && beat.After(last) {
			last = beat
		}
		if now.Sub(last) > b.BeatWindow {
			return jobproto.StateMissing, nil
		}
		return jobproto.StateRunning, nil
	}
	deadline, ok := readUnix(filepath.Join(d, jobproto.MailDeadline))
	if !ok || now.After(deadline) {
		return jobproto.StateMissing, nil
	}
	return jobproto.StateQueued, nil
}

// Start queues job for host: the start tar, checked as factory-remote's
// start checks its input (within MaxInputBytes, a meta.json that passes),
// lands whole or not at all.
func (b Box) Start(host, job string, tar []byte) error {
	d, err := b.jobDir(host, job)
	if err != nil {
		return err
	}
	if len(tar) > jobproto.MaxInputBytes {
		return refuse("job input too large")
	}
	files, err := jobproto.UntarFlat(tar)
	if err != nil {
		return refuse("bad job input: %v", err)
	}
	meta, ok := files[jobproto.MemberMeta]
	if !ok {
		return refuse("bad job input: no %s", jobproto.MemberMeta)
	}
	if _, err := jobproto.ParseMeta(meta); err != nil {
		return refuse("bad job input: %v", err)
	}
	if _, err := os.Lstat(d); err == nil {
		return refuse("%s already exists", job)
	}
	parent := filepath.Dir(d)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".new-"+job+"-")
	if err != nil {
		return err
	}
	now := b.now()
	err = errors.Join(
		fsx.WriteAtomic(filepath.Join(tmp, jobproto.MailStartTar), tar, 0o600),
		writeUnix(filepath.Join(tmp, jobproto.MailDeadline), now.Add(b.ClaimWindow)),
		fsx.WriteAtomic(filepath.Join(tmp, jobproto.MailWindow), []byte(strconv.Itoa(int(b.ClaimWindow/time.Second))+"\n"), 0o600),
	)
	if err == nil {
		err = os.Rename(tmp, d)
	}
	if err != nil {
		fsx.RemoveTree(tmp)
		return err
	}
	return nil
}

// Collect is the collect tar the host posted, rebuilt as factory-remote's
// collect builds one (jobproto.PackCollect): only the collect members,
// each within its cap.
func (b Box) Collect(host, job string) ([]byte, error) {
	d, err := b.jobDir(host, job)
	if err != nil {
		return nil, err
	}
	raw, err := fsx.ReadNoFollow(filepath.Join(d, jobproto.MailDoneTar), jobproto.MaxCollectTar)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, refuse("%s is not done", job)
		}
		return nil, err
	}
	files, err := jobproto.UntarFlat(raw)
	if err != nil {
		return nil, refuse("%s: %v", job, err)
	}
	return jobproto.PackCollect(files)
}

// Remove deletes the job: its directory is renamed away first, so the
// host sees it gone at once (its next beat answers cancel).
func (b Box) Remove(host, job string) error {
	d, err := b.jobDir(host, job)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	gone := filepath.Join(filepath.Dir(d), fmt.Sprintf(".gone-%s-%d", job, b.now().UnixNano()))
	if err := os.Rename(d, gone); err != nil {
		return err
	}
	return fsx.RemoveTree(gone)
}

// Jobs is the host's job names, sorted.
func (b Box) Jobs(host string) ([]string, error) {
	d, err := b.hostDir(host)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && jobproto.JobNameOK(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// Claim takes the queued job with the earliest deadline for host: its
// `claimed` file is created exclusively, so two claims never get the same
// job. It returns the job's name and start tar, or ErrNoJob. A claim
// records that the host called in, job or not.
func (b Box) Claim(host string) (string, []byte, error) {
	if err := b.seen(host); err != nil {
		return "", nil, err
	}
	d, _ := b.hostDir(host)
	names, err := b.Jobs(host)
	if err != nil {
		return "", nil, err
	}
	type queued struct {
		name     string
		deadline time.Time
	}
	var qs []queued
	now := b.now()
	for _, n := range names {
		jd := filepath.Join(d, n)
		if _, err := os.Lstat(filepath.Join(jd, jobproto.MailClaimed)); err == nil {
			continue
		}
		if _, err := os.Lstat(filepath.Join(jd, jobproto.MailDoneTar)); err == nil {
			continue
		}
		if dl, ok := readUnix(filepath.Join(jd, jobproto.MailDeadline)); ok && !now.After(dl) {
			qs = append(qs, queued{n, dl})
		}
	}
	slices.SortFunc(qs, func(a, b queued) int {
		if c := a.deadline.Compare(b.deadline); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	for _, q := range qs {
		jd := filepath.Join(d, q.name)
		f, err := os.OpenFile(filepath.Join(jd, jobproto.MailClaimed), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			continue // claimed meanwhile, or removed
		}
		_, werr := fmt.Fprintf(f, "%d\n", now.Unix())
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		tar, err := fsx.ReadNoFollow(filepath.Join(jd, jobproto.MailStartTar), jobproto.MaxInputBytes)
		if werr != nil || err != nil {
			os.Remove(filepath.Join(jd, jobproto.MailClaimed))
			continue
		}
		return q.name, tar, nil
	}
	return "", nil, ErrNoJob
}

// Beat records that the host is still on job, with the tail of its log
// (the last jobproto.TailBytes of log are kept, for `tail`). It answers
// jobproto.BeatCancel when the job is gone or no longer claimed, so the
// host stops it; BeatOK otherwise.
func (b Box) Beat(host, job string, log []byte) (string, error) {
	d, err := b.jobDir(host, job)
	if err != nil {
		return "", err
	}
	if err := b.seen(host); err != nil {
		return "", err
	}
	if !isDir(d) || !isFile(filepath.Join(d, jobproto.MailClaimed)) {
		return jobproto.BeatCancel, nil
	}
	if len(log) > jobproto.TailBytes {
		log = log[len(log)-jobproto.TailBytes:]
	}
	if err := fsx.WriteAtomic(filepath.Join(d, jobproto.MailLog), log, 0o600); err != nil {
		return "", err
	}
	if err := writeUnix(filepath.Join(d, jobproto.MailBeatAt), b.now()); err != nil {
		return "", err
	}
	return jobproto.BeatOK, nil
}

// Done stores what the host's job left: a collect tar that passes
// jobproto.CheckCollect, for a job it holds the claim of and has not
// posted yet.
func (b Box) Done(host, job string, tar []byte) error {
	d, err := b.jobDir(host, job)
	if err != nil {
		return err
	}
	if err := b.seen(host); err != nil {
		return err
	}
	switch {
	case !isDir(d):
		return refuse("%s is gone", job)
	case !isFile(filepath.Join(d, jobproto.MailClaimed)):
		return refuse("%s is not claimed", job)
	case isFile(filepath.Join(d, jobproto.MailDoneTar)):
		return refuse("%s is already done", job)
	}
	if err := jobproto.CheckCollect(tar); err != nil {
		return refuse("%v", err)
	}
	return fsx.WriteAtomic(filepath.Join(d, jobproto.MailDoneTar), tar, 0o600)
}

// Release gives a claimed job back to the queue, with a new deadline one
// claim window from now.
func (b Box) Release(host, job string) error {
	d, err := b.jobDir(host, job)
	if err != nil {
		return err
	}
	if err := b.seen(host); err != nil {
		return err
	}
	switch {
	case !isDir(d):
		return refuse("%s is gone", job)
	case isFile(filepath.Join(d, jobproto.MailDoneTar)):
		return refuse("%s is already done", job)
	case !isFile(filepath.Join(d, jobproto.MailClaimed)):
		return refuse("%s is not claimed", job)
	}
	window := b.ClaimWindow
	if w, err := fsx.ReadNoFollow(filepath.Join(d, jobproto.MailWindow), 64); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(w))); err == nil && n > 0 {
			window = time.Duration(n) * time.Second
		}
	}
	if err := writeUnix(filepath.Join(d, jobproto.MailDeadline), b.now().Add(window)); err != nil {
		return err
	}
	for _, f := range []string{jobproto.MailBeatAt, jobproto.MailLog, jobproto.MailClaimed} {
		if err := os.Remove(filepath.Join(d, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
