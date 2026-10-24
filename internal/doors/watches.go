package doors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Watch limits: the output quoted in a ticket (clip's code points) and the
// failing command's stderr tail logged.
const (
	watchOutputMax = 6000
	watchStderrMax = 200
)

// Watches is watches(cfg) (F:2707), the watch door, run by the tick before
// its passes: each [[watches]] command that has not run within its
// every_minutes runs (no shell; only the maintainer's config names
// commands), and when its stripped output is non-empty and differs from the
// last run's it files a ticket, door watch, with the output in the body.
// The first run only learns the output, unless the watch sets file_first.
// A command that cannot start, times out or exits non-zero is logged and
// keeps its hash; its run is still recorded. A board project is logged and
// skipped. A watch's kind task, intent or manage is kept and goes to the
// watch's column (ready by default); anything else files a change, which
// goes through triage like the other untrusted doors unless the watch sets
// its column (D16; Python filed it to ready). watches.json is written after
// every watch has had its turn.
//
// The error is what Python raised out of watches() (which the tick logs as
// "watches: …"): output that is not UTF-8, a ticket that cannot be written,
// a state time that does not read. watches.json is then left as it was.
func (s Service) Watches(ctx context.Context, cfg *config.Config) error {
	if len(cfg.Watches) == 0 {
		return nil
	}
	state := s.Board.LoadWatches()
	for _, job := range cfg.Watches {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("watches: %w", err)
		}
		if at, _, _ := state.Get(job.Name); at != "" {
			last, err := clock.Parse(at)
			if err != nil {
				return fmt.Errorf("watch %s: watches.json at: %w", job.Name, err)
			}
			if s.Board.Clock.Now().Sub(last) < job.Every {
				continue
			}
		}
		state.SetAt(job.Name, clock.Stamp(s.Board.Clock.Now()))
		r, err := s.exec().Run(ctx, execx.Cmd{Argv: append([]string(nil), job.Command...), Timeout: job.Timeout})
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("watch %s: %w", job.Name, ctx.Err())
			}
			s.logf("watch %s: %s", job.Name, pyRunError(job, err))
			continue
		}
		stdout, err := textMode(r.Stdout)
		if err != nil {
			return fmt.Errorf("watch %s: stdout: %w", job.Name, err)
		}
		stderr, err := textMode(r.Stderr)
		if err != nil {
			return fmt.Errorf("watch %s: stderr: %w", job.Name, err)
		}
		if r.Code != 0 {
			s.logf("watch %s failed (%d): %s", job.Name, r.Code, lastRunes(textx.Strip(stderr), watchStderrMax))
			continue
		}
		out := textx.Strip(stdout)
		sum := sha256.Sum256([]byte(out))
		digest := hex.EncodeToString(sum[:])
		_, prev, hasHash := state.Get(job.Name)
		changed := out != "" && digest != prev
		// the first run only learns what is there now, unless the watch says otherwise
		if changed && (hasHash || job.FileFirst) {
			if mode, _ := cfg.ProjectMode(job.Project); mode == config.ModeBoard {
				s.logf("watch %s names %s, a board project; skipped", job.Name, job.Project)
				state.SetHash(job.Name, digest)
				continue
			}
			kind, keep := watchKind(job.Kind)
			column := job.Column
			if column == "" {
				column = string(model.ColReady)
				if !keep {
					column = string(model.ColTriage)
				}
			}
			_, err := s.file(ctx, ticket.NewIn{
				Project: job.Project, Title: job.Title,
				Body: strings.ReplaceAll(job.Body, "{output}", textx.Clip(out, watchOutputMax)),
				Door: string(model.DoorWatch), Column: column,
			}, func(t *ticket.Ticket) {
				if keep {
					t.Kind = ticket.Some(kind)
				}
			})
			if err != nil {
				return fmt.Errorf("watch %s: %w", job.Name, err)
			}
		}
		state.SetHash(job.Name, digest)
	}
	if err := s.Board.SaveWatches(state); err != nil {
		return fmt.Errorf("watches: %w", err)
	}
	return nil
}

// watchKind is the kind a watch's ticket keeps: task, intent and manage;
// any other is a change, which writes no kind.
func watchKind(kind string) (string, bool) {
	switch model.Kind(kind) {
	case model.KindTask, model.KindIntent, model.KindManage:
		return kind, true
	}
	return "", false
}

// textMode is what Python's subprocess.run(text=True) gives: the bytes
// decoded as UTF-8 (strictly: anything else raises) with universal
// newlines, so "\r\n" and a lone "\r" read as "\n".
func textMode(b []byte) (string, error) {
	if !utf8.Valid(b) {
		i := 0
		for i < len(b) {
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size <= 1 {
				break
			}
			i += size
		}
		return "", fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d", b[i], i)
	}
	s := string(b)
	if strings.IndexByte(s, '\r') < 0 {
		return s, nil
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), nil
}

// lastRunes is Python's s[-n:].
func lastRunes(s string, n int) string {
	count := utf8.RuneCountInString(s)
	if count <= n {
		return s
	}
	skip := count - n
	for i := range s {
		if skip == 0 {
			return s[i:]
		}
		skip--
	}
	return ""
}

// pyRunError is how Python's watches() logs a command that did not run:
// str() of the OSError subprocess raised, or of its TimeoutExpired.
func pyRunError(job config.Watch, err error) string {
	name := ""
	if len(job.Command) > 0 {
		name = job.Command[0]
	}
	var errno syscall.Errno
	switch {
	case errors.Is(err, execx.ErrTimeout):
		return fmt.Sprintf("Command '%s' timed out after %s seconds", pyReprList(job.Command), pySeconds(job.Timeout))
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return "[Errno 2] No such file or directory: " + jsonx.PyReprString(name)
	case errors.Is(err, fs.ErrPermission):
		return "[Errno 13] Permission denied: " + jsonx.PyReprString(name)
	case errors.As(err, &errno):
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), pyStrerror(errno), jsonx.PyReprString(name))
	}
	return err.Error()
}

// pyStrerror is the C library's message, capitalised as glibc's is.
func pyStrerror(e syscall.Errno) string {
	msg := e.Error()
	if msg == "" {
		return msg
	}
	r, size := utf8.DecodeRuneInString(msg)
	return strings.ToUpper(string(r)) + msg[size:]
}

// pySeconds prints timeout_seconds as Python printed the number.
func pySeconds(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10)
	}
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

// pyReprList is repr() of a list of strings.
func pyReprList(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = jsonx.PyReprString(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
