package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// managerItems files a manager ticket on the desk whose drafts and actions
// are as given, with the ask the finish (or a settle) stores.
func managerItems(w *world, drafts []ticket.Draft, actions []ticket.Action) *ticket.Ticket {
	w.t.Helper()
	return w.new(ticket.NewIn{Project: model.NoticeProject, Title: "Space out the hunt", Body: "b", Door: "human", Column: "working",
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("manage")}}}, func(t *ticket.Ticket) {
		t.Drafts, t.Actions = drafts, actions
		ticket.Move(t, "needs_human", model.ReasonManagerDrafted, "manager proposed", "", "", now)
	})
}

func TestDraftAnswersForWaiting(t *testing.T) {
	tk := &ticket.Ticket{Column: "needs_human", Drafts: []ticket.Draft{{Title: "a", State: model.ItemWaiting}},
		Actions: []ticket.Action{{Kind: "rebuild", State: model.ItemFailed}}}
	as := ops.Answers(tk, &ticket.Ask{Kind: string(model.AskDraft)})
	if values(as) != "apply,send_back,drop" || as[0].Label != "Apply it" || !as[0].Primary || !as[2].Danger ||
		!strings.Contains(as[0].Sub, "config main as a PR") || as[2].Sub != "anything already applied stays; the ticket closes" {
		t.Errorf("answers %+v", as)
	}
	// nothing waits: only send back and drop
	tk.Drafts[0].State, tk.Actions[0].State = model.ItemApplied, model.ItemRequested
	if as := ops.Answers(tk, &ticket.Ask{Kind: string(model.AskDraft)}); values(as) != "send_back,drop" {
		t.Errorf("nothing waiting: %+v", as)
	}
}

func TestDraftAnswersByHandUnchanged(t *testing.T) {
	tk := &ticket.Ticket{Column: "needs_human", Drafts: []ticket.Draft{{Title: "a"}}}
	as := ops.Answers(tk, &ticket.Ask{Kind: string(model.AskDraft)})
	if values(as) != "applied,send_back,drop" || as[0].Label != "I've applied it" || !as[0].Primary {
		t.Errorf("by hand: %+v", as)
	}
	// a waiting action beside a by-hand draft: Apply it before Send back
	tk.Actions = []ticket.Action{{Kind: "pause_host", Target: "local", State: model.ItemWaiting}}
	if as := ops.Answers(tk, &ticket.Ask{Kind: string(model.AskDraft)}); values(as) != "applied,apply,send_back,drop" {
		t.Errorf("by hand and waiting: %+v", as)
	}
}

func TestApplyIsMaintainerOnly(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := managerItems(w, []ticket.Draft{{Title: "Weekly", Patch: "+x\n", State: model.ItemWaiting, Tier: "tap"}}, nil).ID
	before := w.files()
	if _, err := answerDeploy(w, id, "apply", "robin"); err == nil || !strings.Contains(err.Error(), "only the maintainer") {
		t.Errorf("a Foreman user applied: %v", err)
	}
	if after := w.files(); after[string(id)+".json"] != before[string(id)+".json"] {
		t.Error("a refused apply saved something")
	}
	if w.wakes.count() != 0 {
		t.Errorf("%d wakes", w.wakes.count())
	}
}

func TestApplyRequestsWaitingAndFailedAndMoves(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := managerItems(w,
		[]ticket.Draft{{Title: "Weekly", Patch: "+x\n", State: model.ItemWaiting, Run: 1},
			{Title: "Old", Patch: "+y\n", State: model.ItemApplied, Run: 1},
			{Title: "Broke", Patch: "+z\n", State: model.ItemFailed, Note: "PR #4 open", Run: 1},
			{Title: "No", Patch: "+n\n", State: model.ItemRefused, Run: 1}},
		[]ticket.Action{{Kind: "rebuild", State: model.ItemWaiting, Run: 1}, {Kind: "pause_host", Target: "local", State: model.ItemApplied, Run: 1}}).ID
	out, err := answerDeploy(w, id, "apply", "")
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Error("nothing printed")
	}
	got := w.load(id)
	stamp := "2026-10-18T10:00:00+00:00"
	for i, want := range []string{model.ItemRequested, model.ItemApplied, model.ItemRequested, model.ItemRefused} {
		d := got.Drafts[i]
		if d.State != want || (want == model.ItemRequested) != (d.Requested == stamp) {
			t.Errorf("draft %d: %+v", i, d)
		}
	}
	if a := got.Actions[0]; a.State != model.ItemRequested || a.Requested != stamp {
		t.Errorf("action 0: %+v", a)
	}
	if a := got.Actions[1]; a.State != model.ItemApplied || a.Requested != "" {
		t.Errorf("action 1: %+v", a)
	}
	last := got.History[len(got.History)-1]
	m, _ := last.Data.(ticket.Moved)
	if got.Column != "needs_human" || m.Reason != model.ReasonOperatorSettled || m.By != "sam" || last.At != stamp ||
		!strings.HasSuffix(last.Detail, "apply requested by sam; operator: 2 applied, 3 requested, 1 refused") {
		t.Errorf("column %s last %+v", got.Column, last)
	}
	// the tick will take the tap as the maintainer's
	if !ticket.Tapped(got, 1, stamp, "sam") {
		t.Error("the tap is not the maintainer's to the tick")
	}
	if a, _ := got.Ask.Get(); a.Kind != string(model.AskDraft) || !strings.HasPrefix(a.Question, "The factory is applying") {
		t.Errorf("ask %+v", a)
	}
	if w.wakes.count() != 1 {
		t.Errorf("%d wakes", w.wakes.count())
	}
}

func TestApplyWithNothingWaitingRefused(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := managerItems(w, []ticket.Draft{{Title: "Weekly", Patch: "+x\n", State: model.ItemRequested}}, nil).ID
	before := w.files()
	if _, err := answerDeploy(w, id, "apply", ""); err == nil || !strings.Contains(err.Error(), "nothing waits") {
		t.Errorf("apply with nothing waiting: %v", err)
	}
	if after := w.files(); after[string(id)+".json"] != before[string(id)+".json"] {
		t.Error("a refused apply saved something")
	}
	// nor "I've applied it" where nothing is by hand
	if _, err := answerDeploy(w, id, "applied", ""); err == nil {
		t.Error("I've applied it closed a ticket with no by-hand draft")
	}
}

func TestApplyStaleAskRefused(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := managerItems(w, []ticket.Draft{{Title: "Weekly", Patch: "+x\n", State: model.ItemWaiting}}, nil).ID
	kind, _ := askOf(w.load(id))
	_, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: string(id), Value: "apply", Kind: kind, Since: "2026-10-01T00:00:00+00:00"})
	if err == nil || !strings.Contains(err.Error(), ops.ErrStale.Error()) {
		t.Errorf("a stale apply: %v", err)
	}
	if got := w.load(id); got.Drafts[0].State != model.ItemWaiting {
		t.Errorf("a stale apply changed the draft: %+v", got.Drafts[0])
	}
}

// "I've applied it" while the factory still has a tapped (or new) item to
// run is refused: closing the ticket would drop that item without a word.
func TestAppliedRefusedWhileApplying(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := managerItems(w, []ticket.Draft{{Title: "Gate", Patch: "+g\n", Run: 1}},
		[]ticket.Action{{Kind: "rebuild", State: model.ItemWaiting, Run: 1}}).ID
	if _, err := answerDeploy(w, id, "apply", ""); err != nil {
		t.Fatal(err)
	}
	got := w.load(id)
	a, _ := got.Ask.Get()
	if as := ops.Answers(got, &a); values(as) != "send_back,drop" {
		t.Errorf("answers while applying %+v", as)
	}
	before := w.files()
	if _, err := answerDeploy(w, id, "applied", ""); err == nil || !strings.Contains(err.Error(), "still applying") {
		t.Errorf("I've applied it while applying: %v", err)
	}
	if after := w.files(); after[string(id)+".json"] != before[string(id)+".json"] {
		t.Error("a refused answer saved something")
	}
	if got := w.load(id); got.Column != "needs_human" || got.Actions[0].State != model.ItemRequested {
		t.Errorf("column %s action %+v", got.Column, got.Actions[0])
	}
}

// "I've applied it" beside a waiting item closes the ticket and says which
// items it leaves unapplied.
func TestAppliedNamesWhatItLeaves(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := managerItems(w, []ticket.Draft{{Title: "Gate", Patch: "+g\n", Run: 1}},
		[]ticket.Action{{Kind: "rebuild", State: model.ItemWaiting, Run: 1}}).ID
	as := ops.Answers(w.load(id), &ticket.Ask{Kind: string(model.AskDraft)})
	if as[0].Value != "applied" || !strings.Contains(as[0].Sub, "not applied") {
		t.Errorf("answers %+v", as)
	}
	if _, err := answerDeploy(w, id, "applied", ""); err != nil {
		t.Fatal(err)
	}
	got := w.load(id)
	last := got.History[len(got.History)-1]
	if got.Column != "done" || !strings.Contains(last.Detail, "not applied: rebuild") {
		t.Errorf("column %s last %+v", got.Column, last)
	}
}

// After the maintainer's tap, a Foreman user cannot drop the ticket or send
// it back before the tick runs what was tapped; the maintainer can.
func TestTappedTicketIsTheMaintainersToCancel(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := managerItems(w, []ticket.Draft{{Title: "Weekly", Patch: "+x\n", State: model.ItemWaiting, Run: 1}}, nil).ID
	if _, err := answerDeploy(w, id, "apply", ""); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"drop", "send_back"} {
		kind, since := askOf(w.load(id))
		_, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: string(id), Value: v, Kind: kind, Since: since, By: "alex", Note: "no"})
		if err == nil || !strings.Contains(err.Error(), "only the maintainer") {
			t.Errorf("%s by alex after the tap: %v", v, err)
		}
	}
	if got := w.load(id); got.Column != "needs_human" || got.Drafts[0].State != model.ItemRequested {
		t.Errorf("column %s draft %+v", got.Column, got.Drafts[0])
	}
	if _, err := answerDeploy(w, id, "drop", ""); err != nil {
		t.Errorf("the maintainer's drop: %v", err)
	}
	// before any tap, anyone may still drop
	other := managerItems(w, []ticket.Draft{{Title: "Weekly", Patch: "+x\n", State: model.ItemWaiting, Run: 1}}, nil).ID
	if _, err := answerDeploy(w, other, "drop", "alex"); err != nil {
		t.Errorf("a drop with nothing tapped: %v", err)
	}
}

// The tap is recorded as the configured maintainer's, whoever that is, so
// the tick (which asks cfg.Maintainer()) takes it as theirs.
func TestApplyTapIsTheConfiguredMaintainers(t *testing.T) {
	cfgText := strings.Replace(deskConfig, `people = ["sam", "alex"]`, `people = ["jo", "sam"]`, 1)
	w := newWorld(t, cfgText)
	id := managerItems(w, []ticket.Draft{{Title: "Weekly", Patch: "+x\n", State: model.ItemWaiting, Run: 1}}, nil).ID
	if _, err := answerDeploy(w, id, "apply", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	got := w.load(id)
	d := got.Drafts[0]
	if cfg.Maintainer() != "jo" || !ticket.Tapped(got, d.Run, d.Requested, cfg.Maintainer()) {
		t.Errorf("the tap is not %s's to the tick: %+v", cfg.Maintainer(), got.History[len(got.History)-1])
	}
	if ticket.Tapped(got, d.Run, d.Requested, "sam") {
		t.Error("the tap counts as someone else's")
	}
}
