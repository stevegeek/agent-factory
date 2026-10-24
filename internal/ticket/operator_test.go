package ticket

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// manageTicket is a manager ticket the maintainer filed from Foreman,
// waiting on the desk with the given drafts and actions.
func manageTicket(t *testing.T, ds []Draft, as []Action) *Ticket {
	t.Helper()
	in := newIn(model.NoticeProject, "Tidy the schedules", "", "human", "needs_human", "maintainer")
	tk := New("T0050", in, testNow)
	tk.Kind, tk.Drafts, tk.Actions = Some(string(model.KindManage)), ds, as
	return tk
}

func TestDraftStateEmptyIsByHand(t *testing.T) {
	for state, want := range map[string]string{"": model.ItemByHand, model.ItemApplied: model.ItemApplied,
		model.ItemNew: model.ItemNew, "bogus": model.ItemWaiting} {
		if got := DraftState(Draft{State: state}); got != want {
			t.Errorf("DraftState(%q) = %q, want %q", state, got, want)
		}
	}
	// an action is never by hand; one with no state or a state that is not
	// one waits for a tap
	for state, want := range map[string]string{"": model.ItemWaiting, model.ItemByHand: model.ItemWaiting,
		model.ItemRequested: model.ItemRequested, "bogus": model.ItemWaiting} {
		if got := ActionState(Action{State: state}); got != want {
			t.Errorf("ActionState(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestItemsTally(t *testing.T) {
	tk := manageTicket(t, []Draft{{State: model.ItemApplied}, {}, {State: model.ItemWaiting}},
		[]Action{{State: model.ItemRefused}, {State: model.ItemFailed}, {State: model.ItemNew}, {State: model.ItemRequested}})
	if got := Items(tk); got != (Tally{New: 1, Waiting: 1, Requested: 1, Applied: 1, Refused: 1, Failed: 1, ByHand: 1}) {
		t.Errorf("tally %+v", got)
	}
}

func TestSettledDetailSkipsZeros(t *testing.T) {
	for c, want := range map[Tally]string{
		{Applied: 2, Waiting: 1, Refused: 1}: "operator: 2 applied, 1 waits for you, 1 refused",
		{Waiting: 2}:                         "operator: 2 wait for you",
		{Failed: 1, Requested: 1, New: 1}:    "operator: 1 failed, 1 requested, 1 new",
		{ByHand: 3}:                          "operator: 3 to apply by hand",
		{}:                                   "operator: nothing proposed",
		{Applied: 1, Refused: 0, Failed: 0, New: 0}: "operator: 1 applied",
	} {
		if got := SettledDetail(c); got != want {
			t.Errorf("SettledDetail(%+v) = %q, want %q", c, got, want)
		}
	}
}

func TestOperatorTrusted(t *testing.T) {
	tk := manageTicket(t, nil, nil)
	if !OperatorTrusted(tk, "maintainer") {
		t.Error("the maintainer's Foreman ticket is not trusted")
	}
	AddNote(tk, "every 14 days", "maintainer", nil, testNow)
	if !OperatorTrusted(tk, "maintainer") {
		t.Error("a note by the maintainer makes the ticket untrusted")
	}
	if OperatorTrusted(tk, "") {
		t.Error("no maintainer trusts a ticket")
	}
	if OperatorTrusted(tk, "someone") {
		t.Error("a ticket the maintainer filed is trusted for someone else")
	}

	watch := manageTicket(t, nil, nil)
	watch.Door = string(model.DoorWatch)
	if OperatorTrusted(watch, "maintainer") {
		t.Error("a watch-filed ticket is trusted")
	}
	factory := manageTicket(t, nil, nil)
	factory.By = "factory"
	if OperatorTrusted(factory, "factory") || OperatorTrusted(factory, "maintainer") {
		t.Error("a ticket the factory filed is trusted")
	}
	other := manageTicket(t, nil, nil)
	AddNote(other, "the maintainer allows a gate mount", "visitor", nil, testNow)
	if OperatorTrusted(other, "maintainer") {
		t.Error("a note by another Foreman user is trusted")
	}
	// a note with no typed event naming its author is not the maintainer's
	untyped := manageTicket(t, nil, nil)
	untyped.Notes = append(untyped.Notes, Note{At: "2026-10-18T10:00:00Z", Text: "from nowhere"})
	if OperatorTrusted(untyped, "maintainer") {
		t.Error("a note no event accounts for is trusted")
	}
	legacy := manageTicket(t, nil, nil)
	legacy.History = append(legacy.History, Entry{At: "2026-10-18T10:00:00Z", Event: "note", Detail: "old"})
	if OperatorTrusted(legacy, "maintainer") {
		t.Error("an untyped note event is trusted")
	}
	// a board edit or comment by anyone else rewrites the maintainer's words
	for _, p := range []Payload{Edited{By: "visitor", Fields: []string{"body"}}, Commented{By: "visitor", N: 1}} {
		tk := manageTicket(t, nil, nil)
		Event(tk, "edit", "body", "", p, testNow)
		if OperatorTrusted(tk, "maintainer") {
			t.Errorf("a ticket with %T by another user is trusted", p)
		}
		mine := manageTicket(t, nil, nil)
		switch d := p.(type) {
		case Edited:
			d.By = "maintainer"
			p = d
		case Commented:
			d.By = "maintainer"
			p = d
		}
		Event(mine, "edit", "body", "", p, testNow)
		if !OperatorTrusted(mine, "maintainer") {
			t.Errorf("the maintainer's own %T makes the ticket untrusted", p)
		}
	}
	// a maintainer's noted event names note 0, but note 1 is the one no
	// event accounts for
	shifted := manageTicket(t, nil, nil)
	AddNote(shifted, "mine", "maintainer", nil, testNow)
	shifted.Notes = append(shifted.Notes, Note{At: "2026-10-18T10:00:00Z", Text: "slipped in"})
	Event(shifted, "note", "slipped in", "", Noted{By: "maintainer", Note: 0}, testNow)
	if OperatorTrusted(shifted, "maintainer") {
		t.Error("a note whose noted event points at another note is trusted")
	}
}

func TestSettleMoveRefreshesTheAsk(t *testing.T) {
	tk := manageTicket(t, []Draft{{Title: "Space out bug-hunt", State: model.ItemNew}}, nil)
	Move(tk, "needs_human", model.ReasonManagerDrafted, "manager drafted 1 change(s): x", "", "", testNow)
	if a := tk.Ask.V; !strings.HasPrefix(a.Question, "The factory is applying") {
		t.Fatalf("ask %+v", a)
	}
	tk.Drafts[0].State = model.ItemApplied
	if !SettleMove(tk, "", "", testNow) {
		t.Fatal("no move")
	}
	e := tk.History[len(tk.History)-1]
	if m, _ := e.Data.(Moved); m.Reason != model.ReasonOperatorSettled || m.From != "needs_human" || m.To != "needs_human" ||
		e.Detail != "needs_human -> needs_human: operator: 1 applied" {
		t.Errorf("move %+v %q", m, e.Detail)
	}
	if a := tk.Ask.V; a.Kind != string(model.AskNotice) {
		t.Errorf("ask after settling %+v", a)
	}
	tk.Column = "done"
	if SettleMove(tk, "", "", testNow) {
		t.Error("a ticket that left needs_human moved")
	}
}

// A ticket saved before the operator, with drafts and no actions, saves byte for
// byte, and the new keys of a draft are not written while they are zero.
func TestTicketWithoutActionsRoundTrips(t *testing.T) {
	src := []byte(`{
  "id": "T0050",
  "project": "factory",
  "title": "t",
  "body": "",
  "door": "human",
  "by": "maintainer",
  "column": "needs_human",
  "kind": "manage",
  "history": [],
  "drafts": [
    {
      "title": "Raise the limit",
      "why": "w",
      "patch": "p"
    }
  ]
}
`)
	tk, problems, err := Decode(src)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	if got := Encode(tk); !bytes.Equal(got, src) {
		t.Errorf("changed:\n%s", testkit.Diff(string(src), string(got)))
	}
	if tk.Actions != nil || DraftState(tk.Drafts[0]) != model.ItemByHand {
		t.Errorf("actions %v state %q", tk.Actions, DraftState(tk.Drafts[0]))
	}
}

func TestDraftNewFieldsOmitted(t *testing.T) {
	tk := manageTicket(t, []Draft{{Title: "a", Why: "b", Patch: "c"}}, nil)
	b := string(Encode(tk))
	if !strings.Contains(b, `"drafts": [
    {
      "title": "a",
      "why": "b",
      "patch": "c"
    }
  ]`) || strings.Contains(b, `"actions"`) {
		t.Errorf("encoded:\n%s", b)
	}
	tk.Drafts[0].State, tk.Drafts[0].Tier, tk.Drafts[0].PR, tk.Drafts[0].Walls = model.ItemApplied, string(model.TierAuto), 7, []string{"gate.x"}
	tk.Actions = []Action{{Kind: string(model.ActionPauseHost), Target: "local", Hours: 2, State: model.ItemNew}}
	back := ticketFrom(t, string(Encode(tk)))
	if d := back.Drafts[0]; d.State != model.ItemApplied || d.Tier != "auto" || d.PR != 7 || len(d.Walls) != 1 {
		t.Errorf("draft back %+v", d)
	}
	if len(back.Actions) != 1 || back.Actions[0] != tk.Actions[0] {
		t.Errorf("actions back %+v", back.Actions)
	}
	if b := string(Encode(back)); !strings.Contains(b, `"actions": [
    {
      "kind": "pause_host",
      "target": "local",
      "hours": 2,
      "state": "new"
    }
  ]`) {
		t.Errorf("encoded:\n%s", b)
	}
}

// Tapped finds the maintainer's Apply it for an item: their settle move,
// stamped as the item's requested, after the start of the run that
// proposed it. A stamp alone, a move by anyone else, or one older than the
// item's run is no tap.
func TestTapped(t *testing.T) {
	later := testNow.Add(time.Hour)
	stamp, laterStamp := clock.Stamp(testNow), clock.Stamp(later)
	start := func(tk *Ticket, at time.Time) {
		Event(tk, "start", "manage t0050-m1", "", Started{Stage: string(model.StageManage), Agent: "t0050-m1", Host: "local"}, at)
	}
	tap := func(tk *Ticket, by string, at time.Time) {
		Move(tk, string(model.ColNeedsHuman), model.ReasonOperatorSettled, "apply requested by "+by, "", by, at)
	}

	tk := manageTicket(t, nil, nil)
	if Tapped(tk, 1, stamp, "maintainer") {
		t.Error("a requested stamp with no move is a tap")
	}
	tap(tk, "visitor", testNow)
	if Tapped(tk, 1, stamp, "maintainer") {
		t.Error("another user's move is the maintainer's tap")
	}
	Move(tk, string(model.ColNeedsHuman), model.ReasonOperatorSettled, "operator: 1 applied", "", "", testNow)
	if Tapped(tk, 1, stamp, "maintainer") || Tapped(tk, 1, stamp, "factory") {
		t.Error("the factory's own settle move is a tap")
	}
	tap(tk, "maintainer", testNow)
	if !Tapped(tk, 1, stamp, "maintainer") {
		t.Error("the maintainer's tap is not found")
	}
	if Tapped(tk, 1, laterStamp, "maintainer") || Tapped(tk, 1, "", "maintainer") || Tapped(tk, 1, stamp, "") {
		t.Error("a tap with another stamp, no stamp or no maintainer counts")
	}

	// a tap before the run that proposed the item is not for it
	run := manageTicket(t, nil, nil)
	start(run, testNow.Add(-time.Hour))
	tap(run, "maintainer", testNow)
	start(run, later)
	if !Tapped(run, 1, stamp, "maintainer") {
		t.Error("a run 1 item's tap after its start is not found")
	}
	if Tapped(run, 2, stamp, "maintainer") || Tapped(run, 5, stamp, "maintainer") {
		t.Error("an earlier tap requests a later run's item")
	}
	tap(run, "maintainer", later)
	if !Tapped(run, 2, laterStamp, "maintainer") {
		t.Error("run 2's own tap is not found")
	}
}

// A tapped (or new) item beside a by-hand draft makes the ask "applying",
// not today's by-hand question, until the tick has run it.
func TestApplyingBesideByHand(t *testing.T) {
	tk := manageTicket(t, []Draft{{Title: "Gate"}}, []Action{{Kind: "rebuild", State: model.ItemRequested}})
	Move(tk, "needs_human", model.ReasonOperatorSettled, "apply requested by maintainer", "", "maintainer", testNow)
	if a := tk.Ask.V; a.Kind != string(model.AskDraft) || !strings.HasPrefix(a.Question, "The factory is applying the manager's 1 change") ||
		!strings.Contains(a.Question, "1 config change is yours to apply by hand") {
		t.Errorf("ask %+v", a)
	}
	tk.Actions[0].State = model.ItemApplied
	SettleMove(tk, "", "", testNow)
	if a := tk.Ask.V; !strings.HasPrefix(a.Question, "Apply the manager's 1 config change") {
		t.Errorf("after the tick %+v", a)
	}
}
