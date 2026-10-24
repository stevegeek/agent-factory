package ops

import (
	"context"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Intents is Foreman's PRActions (§6.3): Foreman holds no GitHub
// credentials, so instead of doing the work it records on the ticket's pr
// what the tick should do (hold_requested, open_requested, each a stamp),
// and the save that records it wakes the tick, whose GitHub pass drains it
// first (prflow.DrainIntents). Nothing here talks to GitHub.
type Intents struct {
	Clock clock.Clock // nil: the system clock
}

func (in Intents) stamp() string {
	if in.Clock == nil {
		return clock.Stamp(time.Now())
	}
	return clock.Stamp(in.Clock.Now())
}

// Hold records that the tick should turn the PR back to draft. It records
// nothing where hold_pr would do nothing: no PR number, no GitHub
// repository for the project, or GitHub out switched off.
func (in Intents) Hold(_ context.Context, cfg *config.Config, t *ticket.Ticket) error {
	pr, ok := t.PR.Get()
	p, _ := projectOf(cfg, t.Project)
	if !ok || pr.Number == 0 || p.GitHub == "" || !cfg.HasGitHub() {
		return nil
	}
	pr.HoldRequested = ticket.Some(in.stamp())
	t.PR = ticket.Some(pr)
	return nil
}

// Open records that the tick should open or update the PR. It refuses what
// open_pr would refuse at once: a ticket not ready for a PR, a project with
// no GitHub repository, GitHub out switched off. The tick checks the rest
// (the branch has not moved since the gate) and says on the ticket if it
// cannot.
func (in Intents) Open(_ context.Context, cfg *config.Config, t *ticket.Ticket) (string, error) {
	if t.Column != string(model.ColReadyForPR) || !t.Gate.Present() {
		return "", refuse(Conflict, "%s is not ready for a PR", t.ID)
	}
	if p, _ := projectOf(cfg, t.Project); p.GitHub == "" {
		return "", refuse(Conflict, "%s has no GitHub repository", t.Project)
	}
	if !cfg.HasGitHub() {
		return "", refuse(Conflict, "GitHub is not set up here ([github] installation_id)")
	}
	pr := t.PR.V
	if !t.PR.Present() {
		pr = ticket.PR{}
	}
	pr.OpenRequested = ticket.Some(in.stamp())
	t.PR = ticket.Some(pr)
	return "", nil
}
