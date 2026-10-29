package pipeline

import (
	"context"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// heldAfter is what still keeps a ready ticket from starting: the tickets
// it waits for that are not done yet, and those that never will be.
func heldAfter(t *ticket.Ticket, byID map[ticket.ID]*ticket.Ticket) (open, gone []ticket.ID) {
	if t.Column != string(model.ColReady) || len(t.After) == 0 {
		return nil, nil
	}
	return ticket.Blockers(t, byID)
}

// dropAfter sends to the maintainer each ready ticket that waits for a
// rejected or missing ticket. The gone ids leave its list, so starting it
// from the desk is not held again.
func (d Deps) dropAfter(ctx context.Context, ts []*ticket.Ticket, byID map[ticket.ID]*ticket.Ticket) error {
	dropped := func(t *ticket.Ticket) bool {
		_, gone := heldAfter(t, byID)
		return !t.Agent.Present() && len(gone) > 0
	}
	var queue []*ticket.Ticket
	for _, t := range ts {
		if dropped(t) {
			queue = append(queue, t)
		}
	}
	return d.Board.EachLocked(ctx, queue, dropped, func(t *ticket.Ticket) error {
		_, gone := heldAfter(t, byID)
		names := make([]string, len(gone))
		for i, g := range gone {
			names[i] = string(g)
		}
		t.After = slices.DeleteFunc(t.After, func(id ticket.ID) bool { return slices.Contains(gone, id) })
		verb := " was dropped"
		if len(gone) > 1 {
			verb = " were dropped"
		}
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonAfterDropped, strings.Join(names, ", ")+verb, "", "", d.now())
		return d.save(ctx, t)
	})
}
