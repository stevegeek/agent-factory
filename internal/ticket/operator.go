package ticket

import (
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
)

// knownItemState reports a state an operator item can be in.
func knownItemState(s string) bool {
	switch s {
	case model.ItemNew, model.ItemWaiting, model.ItemRequested, model.ItemApplied, model.ItemRefused, model.ItemFailed, model.ItemByHand:
		return true
	}
	return false
}

// DraftState is a draft's state: "" (a draft saved before the operator) is by hand,
// and a state that is not one waits for the maintainer's tap.
func DraftState(d Draft) string {
	switch {
	case d.State == "":
		return model.ItemByHand
	case knownItemState(d.State):
		return d.State
	}
	return model.ItemWaiting
}

// ActionState is an action's state. Only drafts are applied by hand, so an
// action with no state, with by_hand, or with a state that is not one waits
// for the maintainer's tap.
func ActionState(a Action) string {
	if a.State == model.ItemByHand || !knownItemState(a.State) {
		return model.ItemWaiting
	}
	return a.State
}

// Tally counts a ticket's operator items (drafts and actions) by state.
type Tally struct{ New, Waiting, Requested, Applied, Refused, Failed, ByHand int }

func (c *Tally) add(state string) {
	switch state {
	case model.ItemNew:
		c.New++
	case model.ItemRequested:
		c.Requested++
	case model.ItemApplied:
		c.Applied++
	case model.ItemRefused:
		c.Refused++
	case model.ItemFailed:
		c.Failed++
	case model.ItemByHand:
		c.ByHand++
	default:
		c.Waiting++
	}
}

// Items tallies the ticket's drafts and actions by state.
func Items(t *Ticket) Tally {
	var c Tally
	for _, d := range t.Drafts {
		c.add(DraftState(d))
	}
	for _, a := range t.Actions {
		c.add(ActionState(a))
	}
	return c
}

// SettledDetail is the tally in words, for the settle move and the desk:
// "operator: 2 applied, 1 waits for you, 1 refused". Zero counts are left
// out.
func SettledDetail(c Tally) string {
	var parts []string
	add := func(n int, words string) {
		if n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+words)
		}
	}
	add(c.Applied, "applied")
	waits := "wait for you"
	if c.Waiting == 1 {
		waits = "waits for you"
	}
	add(c.Waiting, waits)
	add(c.Failed, "failed")
	add(c.Requested, "requested")
	add(c.New, "new")
	add(c.Refused, "refused")
	add(c.ByHand, "to apply by hand")
	if len(parts) == 0 {
		return "operator: nothing proposed"
	}
	return "operator: " + strings.Join(parts, ", ")
}

// ItemLabel names an action: its kind, and its target when it has one.
func (a Action) ItemLabel() string {
	if a.Target == "" {
		return a.Kind
	}
	return a.Kind + " " + a.Target
}

// OperatorTrusted reports a manager ticket whose every word is the
// maintainer's: filed by the maintainer through a person's door, and every
// note on it written by the maintainer. Only then may the
// tick apply the manager's auto-tier items by itself; on any other ticket
// each item waits for a tap. A note whose author no typed event names, and
// a board edit or comment by anyone else, make it untrusted.
func OperatorTrusted(t *Ticket, maintainer string) bool {
	// "factory" is who the factory's own writes are by (Move, AddNote), so
	// it is never the maintainer here
	if maintainer == "" || maintainer == "factory" || t.Door != string(model.DoorHuman) || t.By != maintainer {
		return false
	}
	byMaintainer := map[int]bool{}
	for _, e := range t.History {
		if e.Event == "note" && e.Data == nil {
			return false
		}
		switch d := e.Data.(type) {
		case Noted:
			if d.By != maintainer {
				return false
			}
			byMaintainer[d.Note] = true
		case Edited:
			if d.By != maintainer {
				return false
			}
		case Commented:
			if d.By != maintainer {
				return false
			}
		}
	}
	for i := range t.Notes {
		if !byMaintainer[i] {
			return false
		}
	}
	return true
}

// Tapped reports the maintainer's Apply it for an item of the given run
// whose requested stamp is requested: a needs_human move with
// ReasonOperatorSettled by the maintainer, stamped requested, after the
// start of that run (the last manage start when the run is past them; the
// whole history when there is none). The tick acts on a requested item
// only then, so a requested state that anything else wrote (a board edit,
// a path that is not the maintainer's) can only make the item wait.
func Tapped(t *Ticket, run int, requested, maintainer string) bool {
	if maintainer == "" || maintainer == "factory" || requested == "" {
		return false
	}
	from, n := -1, 0
	for i, e := range t.History {
		if startOf(e, model.StageManage) && n < max(run, 1) {
			from, n = i, n+1
		}
	}
	for _, e := range t.History[from+1:] {
		m, ok := e.Data.(Moved)
		if ok && e.Event == "move" && e.At == requested && m.Reason == model.ReasonOperatorSettled &&
			m.By == maintainer && m.To == string(model.ColNeedsHuman) {
			return true
		}
	}
	return false
}

// SettleMove moves a manager ticket waiting on the desk to needs_human
// again with ReasonOperatorSettled, so that its stored ask follows its
// items. detail "" is the tally in words. It reports
// false, and does nothing, for a ticket not in needs_human.
func SettleMove(t *Ticket, detail, by string, now time.Time) bool {
	if t.Column != string(model.ColNeedsHuman) {
		return false
	}
	if detail == "" {
		detail = SettledDetail(Items(t))
	}
	Move(t, string(model.ColNeedsHuman), model.ReasonOperatorSettled, detail, "", by, now)
	return true
}

// operatorAsk is what a manager ticket with drafts or actions asks after
// the finish or a settle move; ok is false for today's by-hand question
// (drafts to apply by hand and nothing waiting for a tap, or nothing
// proposed). A waiting or failed item is asked for even beside by-hand
// drafts, which the question and the why then name too: with the landing
// off the actions still work.
func operatorAsk(t *Ticket, summary string) (k model.AskKind, question, why string, ok bool) {
	c := Items(t)
	if len(t.Drafts)+len(t.Actions) == 0 || (c.ByHand > 0 && c.Waiting+c.Failed+c.New+c.Requested == 0) {
		return "", "", "", false
	}
	changes := func(n int) string {
		if n == 1 {
			return "1 change"
		}
		return strconv.Itoa(n) + " changes"
	}
	byHandToo := func() string {
		switch c.ByHand {
		case 0:
			return ""
		case 1:
			return " 1 config change is yours to apply by hand."
		}
		return " " + strconv.Itoa(c.ByHand) + " config changes are yours to apply by hand."
	}
	// what the factory has still to run comes first: until the tick has run
	// it, nothing else on the ticket is the person's to close
	if n := c.New + c.Requested; n > 0 {
		question = "The factory is applying the manager's " + changes(n)
		if b := byHandToo(); b != "" {
			question += "." + b
		}
		return model.AskDraft, question, summary, true
	}
	if n := c.Waiting + c.Failed; n > 0 {
		var failed, waiting, byHand []string
		note := func(label, note string) string {
			if note == "" {
				return label
			}
			return label + " (" + note + ")"
		}
		for _, d := range t.Drafts {
			switch DraftState(d) {
			case model.ItemFailed:
				failed = append(failed, note(d.Title, d.Note))
			case model.ItemWaiting:
				waiting = append(waiting, d.Title)
			case model.ItemByHand:
				byHand = append(byHand, d.Title)
			}
		}
		for _, a := range t.Actions {
			switch ActionState(a) {
			case model.ItemFailed:
				failed = append(failed, note(a.ItemLabel(), a.Note))
			case model.ItemWaiting:
				waiting = append(waiting, a.ItemLabel())
			}
		}
		var parts []string
		if len(failed) > 0 {
			parts = append(parts, "failed: "+strings.Join(failed, ", "))
		}
		if len(waiting) > 0 {
			parts = append(parts, "waiting: "+strings.Join(waiting, ", "))
		}
		if len(byHand) > 0 {
			parts = append(parts, "by hand: "+strings.Join(byHand, ", "))
		}
		return model.AskDraft, "Apply the manager's " + changes(n) + "?" + byHandToo(), strings.Join(parts, "; ") + ". " + summary, true
	}
	return model.AskNotice, "Read what the manager did and close it?", SettledDetail(c) + ". " + orString(t.Result.Or(""), summary), true
}
