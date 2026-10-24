package prflow

import (
	"context"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// HoldPR is hold_pr: a ticket sent back for rework has its open PR
// put back to draft, so the old version cannot be merged meanwhile, with a
// "pr" event. A ticket with no PR, a project with no GitHub repository, a
// factory with no GitHub App and a PR that is no longer open have nothing to
// hold.
//
// A failure is recorded on the ticket as an error event, as Python records
// it, and returned as a *NotHeldError (the CLI warns with it).
func (f *Flow) HoldPR(ctx context.Context, cfg *config.Config, t *ticket.Ticket) error {
	return f.hold(ctx, cfg, t)
}

func (f *Flow) hold(ctx context.Context, cfg *config.Config, t *ticket.Ticket) error {
	n, err := f.holdOnce(ctx, cfg, t)
	if err == nil {
		return nil
	}
	holdError(t, n, err, f.now())
	return &NotHeldError{Number: n, Err: err}
}

func holdFailure(n int, err error) string {
	return "github: could not turn PR #" + itoa(n) + " back to draft: " + textx.Prefix(pyErr(err), 200)
}

// holdError records a hold's failure as an error event.
func holdError(t *ticket.Ticket, n int, err error, now time.Time) {
	ticket.Event(t, "error", holdFailure(n, err), "", ticket.Errored{Text: textx.Prefix(pyErr(err), 200)}, now)
}

// holdOnce does the hold and returns the PR's number and what went wrong,
// recording only the success.
func (f *Flow) holdOnce(ctx context.Context, cfg *config.Config, t *ticket.Ticket) (int, error) {
	n := t.PR.V.Number
	if !t.PR.Present() || n == 0 || !cfg.HasGitHub() {
		return n, nil
	}
	p, ok := cfg.Project(t.Project)
	if !ok || p.GitHub == "" {
		return n, nil
	}
	pr, err := f.GitHub.PR(ctx, p.GitHub, int64(n))
	if err != nil {
		return n, err
	}
	if pr.State != "open" {
		return n, nil
	}
	if !pr.Draft {
		if err := f.GitHub.SetDraft(ctx, pr.NodeID, true); err != nil {
			return n, err
		}
	}
	v := t.PR.V
	v.Draft = ticket.Some(true)
	t.PR = ticket.Some(v)
	ticket.Event(t, "pr", "PR #"+itoa(n)+" back to draft while it is reworked", "",
		ticket.PRNote{Action: ticket.PRDraft, Number: n}, f.now())
	return n, nil
}
