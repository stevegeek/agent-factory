package stage

import (
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
)

func TestWorkOutcomes(t *testing.T) {
	cases := []struct {
		name, port string
		report     string
		facts      Facts
		column     string
		reason     model.Reason
		detail     string
	}{
		{"done with commits", "TF TransitionTest.test_worker_done_with_commits_goes_to_review",
			`{"status": "done"}`, Facts{BranchExists: true, CommitsAhead: 1}, "review", model.ReasonWorkDone, "working -> review"},
		{"done without commits", "TF TransitionTest.test_worker_done_without_commits_needs_human",
			`{"status": "done"}`, Facts{BranchExists: true}, "needs_human", model.ReasonNoCommits,
			"working -> needs_human: worker reported done but the branch has no commits"},
		{"done with no branch", "", `{"status": "done"}`, Facts{CommitsAhead: 3}, "needs_human", model.ReasonNoCommits,
			"working -> needs_human: worker reported done but the branch has no commits"},
		{"blocked", "TF TransitionTest.test_worker_blocked_needs_human",
			`{"status": "blocked", "summary": "cannot build"}`, Facts{BranchExists: true, CommitsAhead: 1}, "needs_human",
			model.ReasonWorkerBlocked, "working -> needs_human: worker blocked: cannot build"},
		{"no status, summary not a string", "", `{"summary": ["a", 1]}`, Facts{BranchExists: true, CommitsAhead: 1},
			"needs_human", model.ReasonWorkerBlocked, "working -> needs_human: worker blocked: ['a', 1]"},
		{"garbage report", "", "not json", Facts{BranchExists: true, CommitsAhead: 1}, "needs_human", model.ReasonWorkerBlocked,
			"working -> needs_human: worker blocked: report.json is not valid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// port markers for the table rows:
			// port: TF TransitionTest.test_worker_done_with_commits_goes_to_review
			// port: TF TransitionTest.test_worker_done_without_commits_needs_human
			// port: TF TransitionTest.test_worker_blocked_needs_human
			tk := running(t, "T0001", "work", "working", "")
			out := finish(t, FinishIn{Ticket: tk, Report: report(c.report), Facts: c.facts})
			got := out.Ticket
			if got.Column != c.column || reason(got) != c.reason || last(got).Detail != c.detail {
				t.Errorf("%s %s %q", got.Column, reason(got), last(got).Detail)
			}
			if out.DeleteBranch || got.Agent.Present() || got.AgentLastHost.Or("") != "local" {
				t.Errorf("delete %v agent %v last host %v", out.DeleteBranch, got.Agent, got.AgentLastHost)
			}
			if _, ok := got.Reports.Lookup("work"); !ok {
				t.Error("the report is kept")
			}
		})
	}
}

func TestWorkerSummaryIsCutAt200Characters(t *testing.T) {
	long := ""
	for range 250 {
		long += "é"
	}
	tk := running(t, "T0001", "work", "working", "")
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "blocked", "summary": "` + long + `"}`)})
	if want := "working -> needs_human: worker blocked: " + long[:400]; last(out.Ticket).Detail != want {
		t.Errorf("detail %q", last(out.Ticket).Detail)
	}
}

func TestFinishRecordsTheRunBeforeTheOutcome(t *testing.T) {
	tk := running(t, "T0001", "work", "working", "")
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "done"}`), Facts: Facts{BranchExists: true, CommitsAhead: 2}})
	h := out.Ticket.History
	if len(h) != 3 || h[1].Event != "finished" || h[1].Detail != "work t0001-w1" || h[2].Event != "move" {
		t.Fatalf("history %+v", h)
	}
	logs := out.Ticket.Logs()
	if len(logs) != 2 || logs[0] != "T0001 finished work t0001-w1" || logs[1] != "T0001 move working -> review" {
		t.Errorf("logs %q", logs)
	}
	if _, err := Finish(FinishIn{Ticket: decode(t, `{"id": "T0002", "history": []}`), Config: cfg(t, ""), Now: now}); err == nil {
		t.Error("a ticket with no agent cannot finish")
	}
}
