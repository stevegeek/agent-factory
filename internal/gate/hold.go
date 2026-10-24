package gate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// HoldIn is a ticket about to merge.
type HoldIn struct {
	Config  *config.Config
	Project config.Project
	Ticket  *ticket.Ticket // changed on a pass: its gate's base, and a gate event
}

// StillHoldsOnMain is still_holds_on_main, the merge queue's check: when the
// base has moved since the gate, the checks run again on the gated head with
// the base as it is now merged in, because two changes that pass alone can
// break the base together. A ticket gated before the base was recorded
// holds, as before.
//
// The base is refreshed from origin first; when that fails the local base
// is used, and the failure is logged. On a pass the gate records the new
// base, with a gate event. out is the checks' output when they ran.
func (r Runner) StillHoldsOnMain(ctx context.Context, in HoldIn) (ok bool, out string, err error) {
	t, p := in.Ticket, in.Project
	g, has := t.Gate.Get()
	if !has || g.Base == "" {
		return true, "", nil
	}
	repo := r.Repo(p)
	if err := (gitx.Writer{Repo: repo}).RefreshBase(ctx); err != nil {
		if ctx.Err() != nil {
			return false, "", ctx.Err()
		}
		r.logf("merge check for %s: could not refresh %s, using the local one: %s", t.ID, p.Base, textx.Prefix(err.Error(), 200))
	}
	now, err := repo.Head(ctx, p.Base)
	if err != nil {
		return false, "", fmt.Errorf("merge check %s: %w", t.ID, err)
	}
	if now == g.Base {
		return true, "", nil
	}
	if err := r.room(ctx, in.Config, p); err != nil {
		return false, "", err
	}
	ok, out, err = r.Checks(ctx, CheckIn{Config: in.Config, Project: p, ID: string(t.ID), Branch: t.Branch.Or(""), Ref: g.Head, Merge: now})
	if err != nil {
		return false, "", err
	}
	if ok {
		g.Base = now
		t.Gate = ticket.Some(g)
		ticket.Event(t, "gate", fmt.Sprintf("%s moved; the checks pass again with it merged in (%s)", p.Base, textx.Prefix(now, 7)), "",
			ticket.Gated{Base: p.Base, Commit: now}, r.now())
	}
	return ok, out, nil
}

// ErrNoRoom is a merge check that did not run because this host has no
// room for it now (Fits); the merge waits for a later tick.
var ErrNoRoom = errors.New("no room on this host for the merge check")

// Fits reports whether a gate of project p may run on this host beside
// agents running agents: it takes one of max_agents' slots, and
// a heavy one (config.HeavyGate) needs the host to itself.
func Fits(cfg *config.Config, p config.Project, agents int) bool {
	if agents >= cfg.MaxAgents {
		return false
	}
	return agents == 0 || !cfg.HeavyGate(p)
}

// room is nil when the merge check fits beside the agents running here
// (Agents), and ErrNoRoom otherwise, or when they cannot be counted.
func (r Runner) room(ctx context.Context, cfg *config.Config, p config.Project) error {
	if r.Agents == nil {
		return nil
	}
	n, err := r.Agents(ctx, cfg)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return fmt.Errorf("%w: the agents running here cannot be counted: %v", ErrNoRoom, err)
	}
	if !Fits(cfg, p, n) {
		return fmt.Errorf("%w: %d agents run here (max_agents %d, gate memory %s)", ErrNoRoom, n, cfg.MaxAgents, cfg.GateMemory(p))
	}
	return nil
}

// now is the Clock's time, or the system's when the Runner has none.
func (r Runner) now() time.Time {
	if r.Clock == nil {
		return clock.System{}.Now()
	}
	return r.Clock.Now()
}
