package present

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

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
