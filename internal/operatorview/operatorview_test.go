package operatorview

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func tk(t *testing.T, src string) *ticket.Ticket {
	t.Helper()
	x, problems, err := ticket.Decode([]byte(src))
	if err != nil || len(problems) > 0 {
		t.Fatalf("test ticket: %v %v", err, problems)
	}
	return x
}

func TestOperatorItemsPresent(t *testing.T) {
	x := tk(t, `{"id": "T0050", "project": "factory", "kind": "manage", "title": "Tidy the schedules", "column": "needs_human",
		"drafts": [
			{"title": "Lower the poll interval", "why": "Less noise.", "patch": "+every_days = 14\n", "tier": "auto", "state": "applied",
			 "places": ["schedules[bug-hunt].every_days"], "pr": 7, "pr_url": "https://github.com/example/config/pull/7", "commit": "abc"},
			{"title": "Gate limit", "why": "w", "patch": "+x\n", "tier": "tap", "state": "waiting",
			 "places": ["gate.max_changed_lines"], "walls": ["gate.max_changed_lines"], "pr_url": "javascript:alert(1)"},
			{"title": "Old", "why": "w", "patch": "+y\n"}],
		"actions": [{"kind": "pause_host", "target": "local", "hours": 2, "why": "Busy.", "tier": "auto", "state": "refused", "note": "no such host"}]}`)
	got := Operator(x)
	if len(got) != 4 {
		t.Fatalf("%d items", len(got))
	}
	d := got[0]
	if d.N != 1 || d.Kind != "draft" || d.Title != "Lower the poll interval" || d.Tier != "auto" || d.TierLabel != "applies by itself" ||
		d.State != model.ItemApplied || d.StateLabel != "Applied" || d.Patch != "+every_days = 14\n" || d.Why != "Less noise." ||
		d.PRURL != "https://github.com/example/config/pull/7" || !reflect.DeepEqual(d.Places, []string{"schedules[bug-hunt].every_days"}) {
		t.Errorf("draft 1: %+v", d)
	}
	if w := got[1]; w.TierLabel != "needs your tap" || w.StateLabel != "Waits for your tap" || w.PRURL != "" ||
		!reflect.DeepEqual(w.Walls, []string{"gate.max_changed_lines"}) {
		t.Errorf("draft 2: %+v", w)
	}
	if b := got[2]; b.State != model.ItemByHand || b.StateLabel != "Yours to apply by hand" || !b.ByHand || b.Tier != "" {
		t.Errorf("draft 3, saved before the operator: %+v", b)
	}
	if a := got[3]; a.N != 4 || a.Kind != "action" || a.Title != "pause_host local · 2 h" || a.State != model.ItemRefused ||
		a.StateLabel != "Refused" || a.Note != "no such host" || a.Why != "Busy." || a.Patch != "" {
		t.Errorf("action: %+v", a)
	}
}

func TestByHandLineOnlyForByHand(t *testing.T) {
	x := tk(t, `{"id": "T0050", "project": "factory", "kind": "manage", "title": "A", "column": "needs_human",
		"drafts": [{"title": "Old", "why": "w", "patch": "+y\n"}, {"title": "New", "why": "w", "patch": "+y\n", "state": "waiting"}]}`)
	got := Operator(x)
	if !got[0].ByHand || got[1].ByHand {
		t.Errorf("by hand: %v %v", got[0].ByHand, got[1].ByHand)
	}
	a := &ticket.Ask{Kind: string(model.AskDraft)}
	if !strings.Contains(present.WaitingWhat(x, a), "on your machine") {
		t.Errorf("a by-hand draft: %q", present.WaitingWhat(x, a))
	}
}
