package stage

import (
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// reworking is a ticket whose open PR was sent back to work for reason,
// after a first run that reported summary.
func reworking(t *testing.T, reason model.Reason, pr int) *ticket.Ticket {
	t.Helper()
	tk := running(t, "T0001", "work", "working", "")
	tk.Reports.Set("work", report(`{"status": "done", "summary": "Add the parser."}`))
	if pr != 0 {
		tk.PR = ticket.Some(ticket.PR{Number: pr})
	}
	ticket.Move(tk, string(model.ColReady), reason, "sent back", "", "", now)
	tk.Column = string(model.ColWorking)
	return tk
}

func TestReworkWithoutSummaryKeepsTheDescriptionAndAddsAnUpdate(t *testing.T) {
	tk := reworking(t, model.ReasonConflicts, 7)
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "done", "rework": "Rebased on the base branch."}`),
		Facts: Facts{BranchExists: true, CommitsAhead: 1}})
	got := out.Ticket
	if s, _ := got.Reports.Get("work").Get("summary").Str(); s != "Add the parser." {
		t.Errorf("summary %q", s)
	}
	want := []ticket.Update{{At: "2026-10-17T09:30:00+00:00", Why: "to resolve conflicts with the base branch", Text: "Rebased on the base branch."}}
	if len(got.Updates) != 1 || got.Updates[0] != want[0] {
		t.Errorf("updates %+v", got.Updates)
	}
}

func TestReworkThatChangesBehaviourReplacesTheSummary(t *testing.T) {
	tk := reworking(t, model.ReasonChangesRequested, 7)
	out := finish(t, FinishIn{Ticket: tk,
		Report: report(`{"status": "done", "summary": "Add the parser and reject empty input.", "rework": "Empty input is now an error."}`),
		Facts:  Facts{BranchExists: true, CommitsAhead: 1}})
	got := out.Ticket
	if s, _ := got.Reports.Get("work").Get("summary").Str(); s != "Add the parser and reject empty input." {
		t.Errorf("summary %q", s)
	}
	if len(got.Updates) != 1 || got.Updates[0].Why != "after changes were requested in review" || got.Updates[0].Text != "Empty input is now an error." {
		t.Errorf("updates %+v", got.Updates)
	}
}

func TestReworkIsRecordedOnlyForAnOpenPR(t *testing.T) {
	tk := reworking(t, model.ReasonReviewChanges, 0)
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "done", "rework": "Renamed a helper."}`),
		Facts: Facts{BranchExists: true, CommitsAhead: 1}})
	if len(out.Ticket.Updates) != 0 {
		t.Errorf("updates %+v", out.Ticket.Updates)
	}
}
