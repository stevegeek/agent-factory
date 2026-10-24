package present

import (
	"strconv"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// OperatorItem is one of a manager ticket's proposals as the desk shows it:
// a config draft or a typed action, with the tier the tick found,
// where it stands, the checker's places and walls, and the PR it landed
// as. N counts drafts first, then actions, from 1.
type OperatorItem struct {
	N                                                     int
	Kind, Title, Tier, TierLabel, State, StateLabel, Note string
	Patch, Why, PRURL                                     string
	Walls, Places                                         []string
	ByHand                                                bool // a draft the maintainer applies on their machine
}

// ItemStateLabel is an item's state in a few words.
func ItemStateLabel(state string) string {
	switch state {
	case model.ItemNew:
		return "Being checked"
	case model.ItemWaiting:
		return "Waits for your tap"
	case model.ItemRequested:
		return "Being applied"
	case model.ItemApplied:
		return "Applied"
	case model.ItemRefused:
		return "Refused"
	case model.ItemFailed:
		return "Failed"
	case model.ItemByHand:
		return "Yours to apply by hand"
	}
	return state
}

func tierLabel(tier string) string {
	if tier == "" {
		return ""
	}
	return model.TierLabel(model.Tier(tier))
}

// Operator is a manager ticket's drafts and actions, for its page and the
// desk's detail.
func Operator(t *ticket.Ticket) []OperatorItem {
	var out []OperatorItem
	for _, d := range t.Drafts {
		st := ticket.DraftState(d)
		out = append(out, OperatorItem{N: len(out) + 1, Kind: "draft", Title: d.Title, Tier: d.Tier, TierLabel: tierLabel(d.Tier),
			State: st, StateLabel: ItemStateLabel(st), Note: d.Note, Patch: d.Patch, Why: d.Why, PRURL: SafeURL(d.PRURL),
			Walls: d.Walls, Places: d.Places, ByHand: st == model.ItemByHand})
	}
	for _, a := range t.Actions {
		st := ticket.ActionState(a)
		title := a.ItemLabel()
		if a.Hours > 0 && (a.Kind == string(model.ActionPauseHost)) {
			title += " · " + strconv.Itoa(a.Hours) + " h"
		}
		out = append(out, OperatorItem{N: len(out) + 1, Kind: "action", Title: title, Tier: a.Tier, TierLabel: tierLabel(a.Tier),
			State: st, StateLabel: ItemStateLabel(st), Note: a.Note, Why: a.Why})
	}
	return out
}

// DraftWalls are the wall places a manager's drafts touch, as the tick's
// checker found them, each once.
func DraftWalls(t *ticket.Ticket) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range t.Drafts {
		for _, w := range d.Walls {
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
	}
	return out
}
