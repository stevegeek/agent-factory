package present

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

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

func TestActionFlagsWallsOnDraft(t *testing.T) {
	x := tk(t, `{"id": "T0050", "project": "factory", "kind": "manage", "title": "Gate", "column": "needs_human",
		"ask": {"kind": "draft", "question": "Apply the manager's 1 change?", "since": "2026-10-16T10:00:00Z"},
		"drafts": [{"title": "Gate limit", "why": "w", "patch": "+x\n", "tier": "tap", "state": "waiting", "walls": ["gate.max_changed_lines"]}]}`)
	a := AskFor(x)
	if got := Action(x, a); got != "Apply it? · 1 change · touches the walls" {
		t.Errorf("action: %q", got)
	}
	if got := DraftWalls(x); !reflect.DeepEqual(got, []string{"gate.max_changed_lines"}) {
		t.Errorf("walls %v", got)
	}
	if v := DeskPage([]DeskItem{{T: x, Ask: a}}, "", "", time.Now()); len(v.Items) != 1 || len(v.Items[0].Walls) != 1 {
		t.Errorf("desk row walls %+v", v.Items)
	}
	x.Drafts[0].Walls = nil
	if got := Action(x, a); got != "Apply it? · 1 change" {
		t.Errorf("no walls: %q", got)
	}
	x.Drafts[0].State = model.ItemRequested
	if got := Action(x, a); got != "Applying · 1 change" {
		t.Errorf("applying: %q", got)
	}
	if !strings.Contains(WaitingWhat(x, a), "next tick") {
		t.Errorf("applying: %q", WaitingWhat(x, a))
	}
	x.Drafts[0].State = model.ItemWaiting
	if w := WaitingWhat(x, a); strings.Contains(w, "on your machine") || !strings.Contains(w, "Apply it") {
		t.Errorf("waiting: %q", w)
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
	if !strings.Contains(WaitingWhat(x, a), "on your machine") {
		t.Errorf("a by-hand draft: %q", WaitingWhat(x, a))
	}
}

func TestStoryTellsOperatorSettled(t *testing.T) {
	x := tk(t, `{"id": "T0050", "project": "factory", "kind": "manage", "title": "A", "column": "needs_human", "history": []}`)
	at := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	ticket.Move(x, "needs_human", model.ReasonOperatorSettled, "operator: 1 applied", "", "", at)
	ticket.Move(x, "needs_human", model.ReasonOperatorSettled, "apply requested by sam", "", "sam", at)
	var whats, whos []string
	for _, s := range BuildStory(x, nil).Steps {
		whats, whos = append(whats, s.What), append(whos, s.Who)
	}
	joined := strings.Join(whats, " | ")
	if !strings.Contains(joined, "Operator: 1 applied.") || !strings.Contains(joined, "Asked the factory to apply the manager's changes.") {
		t.Errorf("story %q (%v)", joined, whos)
	}
	if len(whos) < 2 || whos[0] != "Dispatcher" || whos[1] != "You" {
		t.Errorf("who %v", whos)
	}
}

// A tapped item beside a by-hand draft: the desk says the factory is
// applying, not "apply the diff on your machine".
func TestApplyingBesideByHandPresents(t *testing.T) {
	x := tk(t, `{"id": "T0050", "project": "factory", "kind": "manage", "title": "A", "column": "needs_human",
		"drafts": [{"title": "Old", "why": "w", "patch": "+y\n"}], "actions": [{"kind": "rebuild", "state": "requested"}]}`)
	a := &ticket.Ask{Kind: string(model.AskDraft)}
	if got := Action(x, a); got != "Applying · 1 change" {
		t.Errorf("action %q", got)
	}
	if w := WaitingWhat(x, a); !strings.Contains(w, "next tick") || !strings.Contains(w, "by-hand diff") {
		t.Errorf("waiting %q", w)
	}
}
