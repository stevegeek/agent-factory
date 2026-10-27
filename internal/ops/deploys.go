package ops

import (
	"context"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// requestDeploy is the maintainer's Deploy it on a Deploy? ticket: it
// records the intent (Deploy.Requested) and closes the ticket at once, so
// no notice is left open with an intent on it; the tick's Drain starts
// the deploy of main (whatever Rev the ticket shows) and marks it
// Started. Only [board] people may, as the tick trusts only theirs.
func (c *call) requestDeploy(t *ticket.Ticket) error {
	if !c.person() {
		return refuse(Conflict, "only [board] people deploy the factory; add %s there", c.by())
	}
	d, ok := t.Deploy.Get()
	if !ok {
		return refuse(Conflict, "%s is not a deploy", t.ID)
	}
	if d.Requested.Present() {
		return refuse(Conflict, "%s: the deploy is already requested", t.ID)
	}
	d.Requested = ticket.Some(clockStamp(c))
	t.Deploy = ticket.Some(d)
	ticket.Move(t, string(model.ColDone), model.ReasonDeployRequested, "deploy main", "", c.by(), c.now)
	c.wake = true
	c.print("the next tick deploys main to the factory (factory deploy status shows it)")
	return nil
}

// HoldDeploys holds automatic deploys (held: a merge's deploy becomes a
// Deploy? ask) or releases them, in foreman-state/deploy-hold.json
// (deployrec.SetHold), and wakes the tick. by is who did ("" is the
// maintainer). A deploy started by hand (factory deploy start) ignores the
// hold. Each change is a line in the log; it returns a line for the
// person.
func (s *Service) HoldDeploys(ctx context.Context, held bool, note, by string) (string, error) {
	cfg, err := s.config()
	if err != nil {
		return "", err
	}
	if len(note) > MaxNote {
		return "", refuse(BadRequest, "the note is longer than %d characters", MaxNote)
	}
	for _, r := range note {
		if r < 0x20 && r != '\t' {
			return "", refuse(BadRequest, "the note has control characters")
		}
	}
	if s.Board == nil {
		return "", fmt.Errorf("ops: no board")
	}
	if by == "" {
		by = cfg.Maintainer()
	}
	var h *deployrec.Hold
	what, line := "released", "deploys released: a merge deploys main by itself"
	if held {
		h = &deployrec.Hold{By: by, At: s.stamp(), Note: note}
		what, line = "held", "deploys held: a merge leaves a Deploy? ask on the desk"
	}
	if err := deployrec.SetHold(s.Board.Paths.DeployHold(), h); err != nil {
		return "", err
	}
	s.logf("deploys %s by %s", what, by)
	s.wake(ctx)
	return line, nil
}
