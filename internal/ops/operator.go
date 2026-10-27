package ops

import (
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// requestApply is a person's "Apply it" on a manager ticket: every waiting or failed item is stamped requested, and the
// ticket moves needs_human -> needs_human with operator_settled so that
// its ask follows and the tick knows the tap for a person's
// (ticket.Tapped matches the stamp to this move). Foreman only records
// the request and wakes the tick: the tick judges each draft again on the
// current config main and carries it out, or refuses it.
func (c *call) requestApply(t *ticket.Ticket) error {
	if !c.person() {
		return refuse(Conflict, "only [board] people apply the manager's changes; add %s there", c.by())
	}
	if n := ticket.Items(t); n.Waiting+n.Failed == 0 {
		return refuse(Conflict, "%s: nothing waits for you to apply", t.ID)
	}
	stamp := clockStamp(c)
	tap := func(state string) bool { return state == model.ItemWaiting || state == model.ItemFailed }
	for i := range t.Drafts {
		if tap(ticket.DraftState(t.Drafts[i])) {
			t.Drafts[i].State, t.Drafts[i].Requested = model.ItemRequested, stamp
		}
	}
	for i := range t.Actions {
		if tap(ticket.ActionState(t.Actions[i])) {
			t.Actions[i].State, t.Actions[i].Requested = model.ItemRequested, stamp
		}
	}
	// the tally after it says what the person still has to do (a by-hand
	// draft), for the desk's metrics
	if !ticket.SettleMove(t, "apply requested by "+c.by()+"; "+ticket.SettledDetail(ticket.Items(t)), c.by(), c.now) {
		return refuse(Conflict, "%s is not waiting on the desk", t.ID)
	}
	c.wake = true
	c.print("the next tick applies the manager's changes under the rights tiers (the ticket shows each result)")
	return nil
}
