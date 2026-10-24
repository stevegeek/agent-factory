package ops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const findingsConfig = `
[projects.idcodec]
path = "/x"
base = "main"
github = "acme/idcodec"

[[recipes]]
name = "bug-hunt"
kind = "task"
findings = true
title = "Bug hunt"
body = "Look for bugs in {github}."
`

// verifiedHunt is T0030 after its verifier: three findings, the second a
// behaviour change, the third a duplicate, waiting on the desk.
func verifiedHunt(w *world) *ticket.Ticket {
	w.t.Helper()
	return w.new(ticket.NewIn{Project: "idcodec", Title: "Bug hunt", Body: "Look for bugs.", Door: "clock", Column: "ready"},
		func(t *ticket.Ticket) {
			t.Kind = ticket.Some("task")
			t.Result = ticket.Some("## Confirmed bugs\n\n1. nil decodes ...")
			t.Findings = []ticket.Finding{
				{Title: "Raise IdCodecFormatError for nil input in decode", Severity: "low", Kind: "bug", Verdict: ticket.VerdictConfirmed,
					Evidence: "decode(nil): NoMethodError", Body: "`decode(nil)` raises NoMethodError."},
				{Title: "Preserve leading zeros in the hex round trip", Severity: "medium", Kind: "bug", BehaviourChange: true,
					Verdict: ticket.VerdictConfirmed, Evidence: "\"00ff\" comes back as \"ff\"", Body: "Leading zeros are lost."},
				{Title: "Make decode_hex errors consistent", Severity: "low", Kind: "bug", Verdict: ticket.VerdictDuplicate,
					Duplicate: "PR #27", Evidence: "PR #27 changes it", Body: "No max_length guard."},
			}
			ticket.Move(t, "verifying", model.ReasonVerifyRequested, "", "", "", now)
			ticket.Move(t, "needs_human", model.ReasonFindingsVerified, "findings verified: 2 of 3 confirmed", "", "", now)
		})
}

func answerOf(t *testing.T, w *world, id ticket.ID, in ops.AnswerIn) (string, error) {
	t.Helper()
	a := w.load(id).Ask.V
	in.ID, in.Kind, in.Since = string(id), a.Kind, a.Since
	return w.svc.Answer(context.Background(), in)
}

func TestARecipeWithFindingsAsksForThem(t *testing.T) {
	w := newWorld(t, findingsConfig)
	id, err := w.svc.Add(context.Background(), ops.AddIn{Project: "idcodec", Recipe: "bug-hunt"})
	if err != nil {
		t.Fatal(err)
	}
	tk := w.load(id)
	if tk.KindName() != "task" || !tk.FindingsAsked.Or(false) || !strings.Contains(tk.Body, "acme/idcodec") {
		t.Errorf("kind %q findings %v body %q", tk.KindName(), tk.FindingsAsked, tk.Body)
	}
	plain, err := w.svc.Add(context.Background(), ops.AddIn{Project: "idcodec", Title: "Summarise", Task: true})
	if err != nil {
		t.Fatal(err)
	}
	if w.load(plain).FindingsAsked.Present() {
		t.Error("a task asks for findings only when its recipe says so, or --findings")
	}
	asked, err := w.svc.Add(context.Background(), ops.AddIn{Project: "idcodec", Title: "Hunt", Task: true, Findings: true})
	if err != nil {
		t.Fatal(err)
	}
	if !w.load(asked).FindingsAsked.Or(false) {
		t.Error("--findings asks for them")
	}
	if _, err := w.svc.Add(context.Background(), ops.AddIn{Project: "idcodec", Title: "Hunt", Findings: true}); err == nil {
		t.Error("--findings without --task is refused")
	}
}

func TestTheFindingsAskOffersFileSelectedAndFileNone(t *testing.T) {
	w := newWorld(t, findingsConfig)
	tk := verifiedHunt(w)
	a := tk.Ask.V
	if a.Kind != "findings" {
		t.Fatalf("ask %+v", a)
	}
	var values []string
	for _, x := range ops.Answers(tk, &a) {
		values = append(values, x.Value)
	}
	if strings.Join(values, ",") != "file,file_none" {
		t.Errorf("answers %v", values)
	}
}

func TestFilingSelectedFindingsWithTheirKinds(t *testing.T) {
	w := newWorld(t, findingsConfig)
	hunt := verifiedHunt(w)
	out, err := answerOf(t, w, hunt.ID, ops.AnswerIn{Value: "file", Picks: []int{1, 2}, As: map[int]string{1: "intent", 2: "change"}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "T0002 T0003" {
		t.Errorf("printed %q", out)
	}
	h := w.load(hunt.ID)
	if h.Column != "done" || h.Findings[0].Filed != "T0002" || h.Findings[0].FiledAs != "intent" || h.Findings[1].FiledAs != "change" ||
		h.Findings[2].Filed != "" {
		t.Errorf("hunt in %s: %+v", h.Column, h.Findings)
	}
	if strings.Join(h.Children, ",") != "T0002,T0003" {
		t.Errorf("children %v", h.Children)
	}
	last := h.History[len(h.History)-1]
	if m, _ := last.Data.(ticket.Moved); m.Reason != model.ReasonFindingsFiled || !strings.Contains(last.Detail, "T0002 (intent), T0003 (change)") {
		t.Errorf("closing move %+v", last)
	}
	one, two := w.load("T0002"), w.load("T0003")
	if one.KindName() != "intent" || one.Column != "ready" || one.Door != "finding" || one.Parent.Or("") != "T0001" {
		t.Errorf("T0002: kind %q column %q door %q parent %v", one.KindName(), one.Column, one.Door, one.Parent)
	}
	if two.KindName() != "" || two.Column != "triage" || two.Door != "finding" || !strings.Contains(two.Body, "Found by T0001 (Bug hunt)") {
		t.Errorf("T0003: kind %q column %q door %q body %q", two.KindName(), two.Column, two.Door, two.Body)
	}
	if w.wakes.count() == 0 {
		t.Error("filing wakes the tick")
	}
	// answering again files nothing: the hunt has closed
	if _, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: "T0001", Value: "file", Picks: []int{1}}); err == nil {
		t.Error("a second answer was taken")
	}
	if n := len(w.files()); n != 4 { // three tickets and .last-id
		t.Errorf("%d files after a second answer", n)
	}
}

func TestFilingTakesEachFindingsDefaultKindAndRefusesTheUnconfirmed(t *testing.T) {
	w := newWorld(t, findingsConfig)
	hunt := verifiedHunt(w)
	if _, err := answerOf(t, w, hunt.ID, ops.AnswerIn{Value: "file", Picks: []int{3}}); err == nil || !strings.Contains(err.Error(), "duplicate of PR #27") {
		t.Errorf("a duplicate: %v", err)
	}
	if _, err := answerOf(t, w, hunt.ID, ops.AnswerIn{Value: "file"}); err == nil {
		t.Error("nothing ticked")
	}
	if _, err := answerOf(t, w, hunt.ID, ops.AnswerIn{Value: "file", Picks: []int{1}, As: map[int]string{1: "task"}}); err == nil {
		t.Error("a task is not a filing kind")
	}
	if _, err := answerOf(t, w, hunt.ID, ops.AnswerIn{Value: "file", Picks: []int{1, 2}}); err != nil {
		t.Fatal(err)
	}
	h := w.load(hunt.ID)
	if h.Findings[0].FiledAs != "change" || h.Findings[1].FiledAs != "intent" {
		t.Errorf("default kinds: %+v", h.Findings)
	}
}

// A filing cut short (the dispatcher killed after a finding's ticket was
// saved, before the hunt was) never files that finding twice.
func TestFilingIsIdempotentAcrossAnInterruptedAnswer(t *testing.T) {
	w := newWorld(t, findingsConfig)
	hunt := verifiedHunt(w)
	// the first answer got as far as finding 1's ticket
	copyOf := ticket.Clone(hunt)
	child, err := ticket.FileFinding("T0002", copyOf, 1, "change", "maintainer", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.board.Save(child); err != nil {
		t.Fatal(err)
	}
	out, err := answerOf(t, w, hunt.ID, ops.AnswerIn{Value: "file", Picks: []int{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if out != "T0002 T0003" {
		t.Errorf("printed %q", out)
	}
	h := w.load(hunt.ID)
	if h.Findings[0].Filed != "T0002" || h.Findings[1].Filed != "T0003" || strings.Join(h.Children, ",") != "T0002,T0003" {
		t.Errorf("findings %+v children %v", h.Findings, h.Children)
	}
	if _, err := w.board.Reader.Load("T0004"); err == nil {
		t.Error("finding 1 was filed twice")
	}
}

func TestFileNoneClosesTheHunt(t *testing.T) {
	w := newWorld(t, findingsConfig)
	hunt := verifiedHunt(w)
	if _, err := answerOf(t, w, hunt.ID, ops.AnswerIn{Value: "file_none", Note: "known already"}); err != nil {
		t.Fatal(err)
	}
	h := w.load(hunt.ID)
	last := h.History[len(h.History)-1]
	if m, _ := last.Data.(ticket.Moved); h.Column != "done" || m.Reason != model.ReasonFindingsDismissed || len(h.Notes) != 1 {
		t.Errorf("column %s last %+v notes %v", h.Column, last, h.Notes)
	}
	if len(w.files()) != 2 {
		t.Error("nothing is filed")
	}
}

// A task's result that holds findings only as prose (a hunt from before
// structured findings, like T0030) goes to the verifier on request.
func TestVerifyTheseFindingsSendsAProseResultToTheVerifier(t *testing.T) {
	w := newWorld(t, findingsConfig)
	tk := w.new(ticket.NewIn{Project: "idcodec", Title: "Bug hunt", Body: "Look.", Door: "clock", Column: "ready"}, func(t *ticket.Ticket) {
		t.Kind = ticket.Some("task")
		t.Result = ticket.Some("## Confirmed bugs\n\n### 1. Hex loses leading zeros")
		ticket.Move(t, "working", model.ReasonStarted, "", "", "", now)
		ticket.Move(t, "needs_human", model.ReasonTaskDone, "task done: six bugs", "", "", now)
	})
	a := tk.Ask.V
	if a.Kind != "notice" {
		t.Fatalf("ask %+v", a)
	}
	found := false
	for _, x := range ops.Answers(tk, &a) {
		found = found || x.Value == "verify" && x.Label == "Verify these findings"
	}
	if !found {
		t.Fatalf("no Verify these findings among %+v", ops.Answers(tk, &a))
	}
	if _, err := answerOf(t, w, tk.ID, ops.AnswerIn{Value: "verify"}); err != nil {
		t.Fatal(err)
	}
	h := w.load(tk.ID)
	last := h.History[len(h.History)-1]
	if m, _ := last.Data.(ticket.Moved); h.Column != "verifying" || m.Reason != model.ReasonVerifyRequested || m.By != "maintainer" {
		t.Errorf("column %s last %+v", h.Column, last)
	}
	if w.wakes.count() == 0 {
		t.Error("the verifier is started at once")
	}
	// a task with no repository has nothing to verify against
	bare := w.new(ticket.NewIn{Project: "factory", Title: "Summarise", Body: "x", Door: "human", Column: "needs_human"},
		func(t *ticket.Ticket) { t.Kind = ticket.Some("task") })
	ba := bare.Ask.V
	for _, x := range ops.Answers(bare, &ba) {
		if x.Value == "verify" {
			t.Error("a task on the factory project offers no verify")
		}
	}
	if _, err := answerOf(t, w, bare.ID, ops.AnswerIn{Value: "verify"}); err == nil {
		t.Error("verify without a project was taken")
	}
}

func TestFindingsVerbs(t *testing.T) {
	w := newWorld(t, findingsConfig)
	hunt := verifiedHunt(w)
	text, err := w.svc.Findings(context.Background(), string(hunt.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. Raise IdCodecFormatError", "confirmed · low bug · files as a change",
		"2. Preserve leading zeros", "changes behaviour · files as an intent", "3. Make decode_hex errors consistent", "duplicate of PR #27",
		"factory findings T0001 --file 1,2"} {
		if !strings.Contains(text, want) {
			t.Errorf("findings text lacks %q:\n%s", want, text)
		}
	}
	ids, err := w.svc.FileFindings(context.Background(), string(hunt.ID), []int{2}, map[int]string{2: "change"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || w.load(ids[0]).Column != "triage" {
		t.Errorf("filed %v", ids)
	}
	if _, err := w.svc.FileFindings(context.Background(), string(hunt.ID), []int{1}, nil); err == nil {
		t.Error("the hunt closed: nothing more is filed")
	}
	var r *ops.Refused
	if _, err := w.svc.Findings(context.Background(), "T0099"); err == nil || errors.As(err, &r) {
		// a missing ticket is not found, not a refusal
		if err == nil {
			t.Error("no T0099")
		}
	}
}
