package pipeline

import (
	"context"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// heldAfter is what still keeps a ready ticket from starting: the tickets
// it waits for that are not done yet, and those that never will be. A
// ticket whose file could not be read this pass is not gone: it holds.
func heldAfter(t *ticket.Ticket, byID map[ticket.ID]*ticket.Ticket, unread map[ticket.ID]bool) (open, gone []ticket.ID) {
	if t.Column != string(model.ColReady) || len(t.After) == 0 {
		return nil, nil
	}
	open, gone = ticket.Blockers(t, byID)
	for _, id := range gone {
		if _, ok := byID[id]; !ok && unread[id] {
			open = append(open, id)
		}
	}
	gone = slices.DeleteFunc(gone, func(id ticket.ID) bool { return slices.Contains(open, id) })
	return open, gone
}

// unreadIDs are the tickets whose files the pass skipped.
func unreadIDs(problems []boardread.Problem) map[ticket.ID]bool {
	m := map[ticket.ID]bool{}
	for _, p := range problems {
		if p.Err != nil && p.ID != "" {
			m[p.ID] = true
		}
	}
	return m
}

// dropAfter sends to the maintainer each ready ticket that waits for a
// closed or missing ticket. The gone ids leave its list, so starting it
// from the desk is not held again.
func (d Deps) dropAfter(ctx context.Context, ts []*ticket.Ticket, byID map[ticket.ID]*ticket.Ticket, unread map[ticket.ID]bool, startable func(*ticket.Ticket) bool) error {
	dropped := func(t *ticket.Ticket) bool {
		_, gone := heldAfter(t, byID, unread)
		return len(gone) > 0 && startable(t)
	}
	var queue []*ticket.Ticket
	for _, t := range ts {
		if dropped(t) {
			queue = append(queue, t)
		}
	}
	return d.Board.EachLocked(ctx, queue, dropped, func(t *ticket.Ticket) error {
		_, gone := heldAfter(t, byID, unread)
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
