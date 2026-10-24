// Package hostrun runs agents on a host (ARCHITECTURE.md §2.3, §2.4.6):
// Local runs them here through Scion, Jobs on a helper host through
// factory-remote. They are concrete types with no shared exported
// interface; the pipeline declares what it consumes and keeps every
// decision (timeouts, lost agents, limits, finishing) in Python's order.
// This package does the mechanics: launching, polling, collecting,
// archiving into logs/<ticket>/<agent>/, removing, and the screen.
package hostrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// ErrRemote marks an error a helper host answered with (or a verb that
// timed out): the tick takes it as that host being down for this pass and
// tries another (Python's RemoteError). The error's text is the helper's.
var ErrRemote = errors.New("helper host error")

// ErrUnknown is a host that could not say which agents it runs (a failed
// scion list is not an empty one): the tick then counts it as full.
var ErrUnknown = errors.New("could not tell which agents run here")

// MaxArchivedBytes is MAX_ARCHIVED_BYTES: what is kept of a screen or of a
// helper job's files in logs/.
const MaxArchivedBytes = 2_000_000

// ScreenLines is how much of an agent's screen remove keeps (keep_screen).
const ScreenLines = 200

// ArchivedFiles are the files an agent's run leaves in logs/<id>/<name>/.
func ArchivedFiles() []string {
	return []string{jobproto.MemberReport, jobproto.MemberLog, jobproto.MemberPrompt}
}

// Repo is git on a project checkout, for a helper job's start: gitread's
// Repo and gitx's Writer between them have both methods.
type Repo interface {
	BranchExists(ctx context.Context, branch string) (bool, error)
	// Bundle is a git bundle of refs/heads/<r> for each r.
	Bundle(ctx context.Context, refs []string) ([]byte, error)
}

// Job is one stage run to start.
type Job struct {
	Ticket   *ticket.Ticket
	Stage    model.Stage
	Name     string // t0042-w1
	Prompt   string
	Model    string // "" leaves the template's or the helper's default
	Template string // config.Template(stage)
	// Project is the checkout the agent works in: a project, or the
	// manager's workspace for manage. nil is a task with no repository,
	// which only a helper runs.
	Project *config.Project
	Repo    Repo // git on Project; nil with no project
	// TimeoutMinutes is hosts.Timeout in minutes (helper jobs only).
	TimeoutMinutes int
	// RunConfig is the local memory mount (run/<name>.yaml), "" for none.
	RunConfig string
	// Pinned is what a local run mounts read-only into its workspace of
	// the project's pinned_untracked (scion.PinnedVolumes); a helper job
	// ships its own copy instead.
	Pinned []string
	// Memory is a helper run's memory, by its name under memory/
	// (memory.Service.JobFiles); nil for none.
	Memory map[string][]byte
	// Gate makes the job a gate job (a pull host only): Gate 1's checks
	// instead of an agent. Stage, Prompt, Model and Template are unused.
	Gate *GateRun
}

// GateRun is a gate job's content: the commit to check and the commands.
type GateRun struct {
	Head   string // the gated commit (the ticket's branch head)
	Script string // gate.Script: "set -e; <install>; <test>; <lint>"
}

func (j Job) info() (model.StageInfo, error) {
	info, ok := model.StageInfoOf(j.Stage)
	if !ok {
		return info, fmt.Errorf("start %s: unknown stage %q", j.Name, j.Stage)
	}
	return info, nil
}

// branch is the branch a branch stage works on.
func (j Job) branch() (string, error) {
	b := j.Ticket.Branch.Or("")
	if b == "" {
		return "", fmt.Errorf("start %s: %s has no branch", j.Name, j.Ticket.ID)
	}
	return b, nil
}

// scionNoise is SCION_NOISE.match(line), ^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d WARN
// (with a final space; regex.json "scion_noise"), written by hand: \d is
// any Unicode decimal, as in Python.
func scionNoise(line string) bool {
	const shape = "dddd/dd/dd dd:dd:dd WARN "
	rs := []rune(line)
	if len(rs) < len(shape) {
		return false
	}
	for i, c := range shape {
		if c == 'd' && !textx.IsDecimal(rs[i]) || c != 'd' && rs[i] != c {
			return false
		}
	}
	return true
}

// ScreenText is screen_text (F:4523): Scion's own WARN lines dropped, each
// line's trailing whitespace and the trailing blank lines cut, and the
// last `lines` lines kept (all of them for lines <= 0, as [-0:]).
func ScreenText(text string, lines int) string {
	var keep []string
	for _, line := range textx.SplitLines(text) {
		if !scionNoise(line) {
			keep = append(keep, textx.RStrip(line))
		}
	}
	for len(keep) > 0 && keep[len(keep)-1] == "" {
		keep = keep[:len(keep)-1]
	}
	if lines > 0 && len(keep) > lines {
		keep = keep[len(keep)-lines:]
	}
	return strings.Join(keep, "\n")
}

// lastRunes is s[-n:] in code points.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func lastBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

// archiveDir is logs/<id>/<name>, created.
func archiveDir(logs string, id ticket.ID, name string) (string, error) {
	if _, ok := ticket.ParseID(string(id)); !ok || !jobproto.JobNameOK(name) {
		return "", fmt.Errorf("archive %s/%s: bad name", id, name)
	}
	dest := filepath.Join(logs, string(id), name)
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	return dest, nil
}

// writeNoFollow writes data at path, refusing to write through a link.
func writeNoFollow(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// openRegular opens a regular file without following a link in its last
// component (an agent's home is the agent's to plant links in). ok is
// false when there is no regular file there.
func openRegular(path string) (f *os.File, ok bool, err error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	switch {
	case err == syscall.ENOENT || err == syscall.ELOOP || err == syscall.ENOTDIR:
		return nil, false, nil
	case err != nil:
		return nil, false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f = os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, false, nil
	}
	return f, true, nil
}

// copyRegular is shutil.copy2 of a regular file (bytes, permission bits
// and times), never through a link. ok is false when src is no regular
// file.
func copyRegular(src, dst string) (ok bool, err error) {
	in, ok, err := openRegular(src)
	if !ok || err != nil {
		return false, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return false, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, err
	}
	if err := out.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
		return false, err
	}
	atime := st.ModTime()
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		atime = time.Unix(sys.Atim.Sec, sys.Atim.Nsec)
	}
	return true, os.Chtimes(dst, atime, st.ModTime())
}

// readTail reads at most the last n bytes of a regular file.
func readTail(path string, n int64) ([]byte, bool, error) {
	f, ok, err := openRegular(path)
	if !ok || err != nil {
		return nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if off := st.Size() - n; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, false, err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, n))
	return b, err == nil, err
}

// remoteErr marks a helper's error with ErrRemote, keeping its text.
type remoteErr struct{ err error }

func (e remoteErr) Error() string   { return e.err.Error() }
func (e remoteErr) Unwrap() []error { return []error{e.err, ErrRemote} }

func markRemote(err error) error {
	var re *jobproto.RemoteError
	if errors.As(err, &re) {
		return remoteErr{err}
	}
	return err
}
