// Package screens is the screen relay: Foreman's sandbox hides podman and
// the user manager, so it cannot run `scion look` itself. `factory
// screens`, a user unit outside the sandbox (factory-screens.path starts
// it), looks at each running local agent's terminal while someone has Live
// open and writes it to Root/screens/<agent>.txt, which Foreman only reads
// (Reader).
//
// Foreman's one input to the relay is the mtime of foreman-state/watching,
// which it touches on every Live screen poll: the relay never reads what
// is in that file, and runs nothing named by anything Foreman wrote. It
// writes only under Root/screens, never under logs/ or foreman-state/, and
// never through a link.
package screens

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
)

const (
	// Round is the pause between the relay's rounds.
	Round = 2500 * time.Millisecond
	// Fresh is how long a touch of the marker keeps the relay going: Live
	// polls every 5 s, so a page left open keeps it well inside this.
	Fresh = 20 * time.Second
	// LookTimeout is the limit on one look, Foreman's own look's limit.
	LookTimeout = 25 * time.Second
	// Slots is how many looks run at once: a small dispatcher may have only 4 slow cores.
	Slots = 2
	// Lines and MaxBytes cap a screen: the most a look shows (ops.MaxLookLines)
	// and the bytes of agent.log a look falls back to.
	Lines    = 400
	MaxBytes = 64 << 10
)

// Ext is a screen file's extension.
const Ext = ".txt"

// Agent is a running local agent and the workspace Scion runs it in.
type Agent struct {
	Name string
	Dir  string
}

// Looker reads an agent's terminal (scionread.Reader.Look): read-only.
type Looker interface {
	Look(ctx context.Context, dir, name string, lines int) (string, error)
}

// Logger is the factory log (*logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Relay is `factory screens`.
type Relay struct {
	Dir    string // paths.Paths.Screens(): where the screens go
	Marker string // paths.Paths.Watching(): only its mtime is read
	// Agents lists the board's running local agents.
	Agents func(ctx context.Context) ([]Agent, error)
	Scion  Looker
	Clock  clock.Clock                                      // nil: the system clock
	Sleep  func(ctx context.Context, d time.Duration) error // nil: a timer
	Log    Logger                                           // nil: nothing is logged
}

// Run relays the screens, a round every Round, while the marker is under
// Fresh old, and returns when it is not (Foreman's Live tab has closed) or
// ctx ends. A round that fails is logged, once while it fails the same
// way; the next one tries again.
func (r *Relay) Run(ctx context.Context) error {
	last := ""
	for r.Watched() {
		err := r.Round(ctx)
		if msg := fmt.Sprint(err); err != nil && msg != last {
			r.logf("screens: %s", msg)
		}
		last = fmt.Sprint(err)
		if err := r.sleep(ctx, Round); err != nil {
			return nil
		}
	}
	return nil
}

// Watched reports whether someone has Live open: the marker is a regular
// file (never a link) touched less than Fresh ago.
func (r *Relay) Watched() bool {
	st, err := os.Lstat(r.Marker)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	return r.now().Sub(st.ModTime()) < Fresh
}

// Round looks at every running local agent, at most Slots at once, writes
// each screen it gets atomically, and deletes what is not a running
// agent's screen. A failed look leaves that agent's last screen to go
// stale; a board that cannot be listed changes nothing.
func (r *Relay) Round(ctx context.Context) error {
	agents, err := r.Agents(ctx)
	if err != nil {
		return fmt.Errorf("list the agents: %w", err)
	}
	if err := r.ensureDir(); err != nil {
		return err
	}
	keep := map[string]bool{}
	var mu sync.Mutex
	var errs []error
	slots := make(chan struct{}, Slots)
	var wg sync.WaitGroup
	for _, a := range agents {
		if !ValidName(a.Name) || keep[a.Name+Ext] {
			continue
		}
		keep[a.Name+Ext] = true
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			if err := r.relay(ctx, a); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	errs = append(errs, r.prune(keep))
	return errors.Join(errs...)
}

// relay writes one agent's screen, when Scion shows one.
func (r *Relay) relay(ctx context.Context, a Agent) error {
	ctx, cancel := context.WithTimeout(ctx, LookTimeout)
	defer cancel()
	out, err := r.Scion.Look(ctx, a.Dir, a.Name, Lines)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil // Foreman shows the agent's log until a screen comes
	}
	if err := fsx.WriteAtomic(filepath.Join(r.Dir, a.Name+Ext), []byte(Cap(out)), 0o600); err != nil {
		return fmt.Errorf("screen of %s: %w", a.Name, err)
	}
	return nil
}

// ensureDir makes the screens directory, which must be a directory of its
// own, not a link to one.
func (r *Relay) ensureDir() error {
	if err := os.Mkdir(r.Dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("screens: %w", err)
	}
	st, err := os.Lstat(r.Dir)
	if err != nil {
		return fmt.Errorf("screens: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("screens: %s is not a directory", r.Dir)
	}
	return nil
}

// prune deletes every entry of the directory that is not in keep, but for
// a directory: a stopped agent's screen, a crashed write's temp file. A
// link is removed as itself.
func (r *Relay) prune(keep map[string]bool) error {
	es, err := os.ReadDir(r.Dir)
	if err != nil {
		return fmt.Errorf("screens: %w", err)
	}
	var errs []error
	for _, e := range es {
		if keep[e.Name()] || e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(r.Dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Cap is a screen cut to its last Lines lines and, past MaxBytes, to the
// whole lines that fit, with invalid UTF-8 replaced.
func Cap(text string) string {
	text = strings.ToValidUTF8(text, "�")
	trail := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > Lines {
		lines = lines[len(lines)-Lines:]
	}
	text = strings.Join(lines, "\n")
	if trail {
		text += "\n"
	}
	if len(text) > MaxBytes {
		text = text[len(text)-MaxBytes:]
		if i := strings.IndexByte(text, '\n'); i >= 0 && i < len(text)-1 {
			text = text[i+1:]
		}
		text = strings.ToValidUTF8(text, "")
	}
	return text
}

// ValidName is an agent name a screen file may be named after: letters,
// digits, - and _, not starting with -, at most 128 bytes (the factory's
// names are like t0042-w1). No dot: it can be neither a path nor hidden.
func ValidName(name string) bool {
	if name == "" || name[0] == '-' || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (r *Relay) now() time.Time {
	if r.Clock == nil {
		return clock.System{}.Now()
	}
	return r.Clock.Now()
}

func (r *Relay) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Relay) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// Reader is Foreman's side: the screens the relay wrote, read-only.
type Reader struct {
	Dir string // paths.Paths.Screens()
}

// Read is an agent's relayed screen and when it was written. The name must
// be ValidName; the file is read without following a link, and refused
// over MaxBytes.
func (d Reader) Read(name string) (string, time.Time, error) {
	if !ValidName(name) {
		return "", time.Time{}, fmt.Errorf("bad agent name %q", name)
	}
	path := filepath.Join(d.Dir, name+Ext)
	st, err := os.Lstat(path)
	if err != nil {
		return "", time.Time{}, err
	}
	b, err := fsx.ReadNoFollow(path, MaxBytes)
	if err != nil {
		return "", time.Time{}, err
	}
	return string(b), st.ModTime(), nil
}
