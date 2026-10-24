package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Tick is tick(cfg) (F:2120): one run of the pipeline. Another tick holding
// the tick lock gets .tick.again touched and this one returns at once;
// that tick then goes round once more. Otherwise the watches run, then up
// to three passes, each clearing .tick.again first and reading the config
// again (from the second on), until no wake arrived during a pass.
//
// The error is a config that cannot be read at the start, a save that
// failed, or the context ending (nothing unsaved is kept then).
func Tick(ctx context.Context, d Deps) error {
	cfg, err := d.Config.Current()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.Paths.Tickets, 0o777); err != nil {
		return fmt.Errorf("tick: %w", err)
	}
	lock, err := d.Board.TickLock(ctx, store.NoWait)
	if errors.Is(err, fsx.ErrBusy) {
		if err := d.Board.Again(); err != nil {
			return fmt.Errorf("tick: %w", err)
		}
		d.logf("tick already running; it goes round once more")
		return nil
	}
	if err != nil {
		return fmt.Errorf("tick: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	deadline := d.Deadline
	if deadline <= 0 {
		deadline = DefaultDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	if d.Doors != nil {
		if err := d.Doors.Watches(ctx, cfg); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			d.logf("watches: %s", textx.Prefix(err.Error(), 300))
		}
	}
	for n := 0; n < maxPasses; n++ {
		if err := d.Board.ClearAgain(); err != nil {
			return fmt.Errorf("tick: %w", err)
		}
		if n > 0 {
			// a deploy may have landed while the last pass ran (a new
			// project, say): read the config again, keeping the old one
			// when the new one does not read
			if c, err := d.Config.Current(); err == nil {
				cfg = c
			} else {
				d.logf("config: %s; this pass keeps the last one", textx.Prefix(err.Error(), 300))
			}
		}
		if err := d.pass(ctx, cfg); err != nil {
			return err
		}
		if !d.Board.AgainSet() {
			break
		}
	}
	return nil
}

func (d Deps) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log.Printf(format, args...)
	}
}

func (d Deps) now() time.Time { return d.Clock.Now() }

// reserve is what a gate needs left of the tick's time.
func (d Deps) reserve() time.Duration {
	if d.Reserve > 0 {
		return d.Reserve
	}
	return DefaultReserve
}

// timeLeft reports whether ctx's deadline leaves at least the reserve.
func (d Deps) timeLeft(ctx context.Context) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) >= d.reserve()
}

// notice files a notice for each ticket file the board cannot read
// (quirk 1, §4.5): the tick passes it over instead of failing whole.
// The reader has logged it; the notice is deduplicated by its title.
func (d Deps) notice(ctx context.Context, problems []boardread.Problem) error {
	if d.Doors == nil {
		return nil
	}
	for _, p := range problems {
		if p.Err == nil {
			continue
		}
		name := string(p.ID)
		if name == "" {
			name = filepath.Base(p.File)
		}
		title := "Ticket file " + name + " cannot be read"
		body := "The tick passes over " + p.File + " because it does not read as a ticket (" + textx.Prefix(p.Err.Error(), 300) +
			"). Fix the file or move it out of the board; until then nothing happens to that ticket."
		if _, err := d.Doors.Notice(ctx, title, body); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			d.logf("notice %s: %s", title, textx.Prefix(err.Error(), 300))
		}
	}
	return nil
}

// cfgOf is a project's config, or nil.
func cfgOf(cfg *config.Config, key string) *config.Project {
	p, ok := cfg.Project(key)
	if !ok {
		return nil
	}
	return p
}
