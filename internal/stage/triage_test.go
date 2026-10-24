package stage

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func triaging(t *testing.T, door string) *ticket.Ticket {
	tk := running(t, "T0002", "triage", "triage", "")
	tk.Title, tk.Body, tk.Door, tk.Branch = "issue title", "issue body", door, ticket.Nil[string]()
	return tk
}

func TestTriageAgentBranchIsDeleted(t *testing.T) {
	// port: TF TriageTest.test_triage_agent_branch_is_deleted
	out := finish(t, FinishIn{Ticket: triaging(t, "github"), Report: report(`{"decision": "reject", "reason": "dup"}`)})
	if !out.DeleteBranch || KeepBranch(model.StageTriage) {
		t.Error("triage keeps no branch")
	}
}

func TestTriageRejectCloses(t *testing.T) {
	// port: TF TriageTest.test_reject_closes
	out := finish(t, FinishIn{Ticket: triaging(t, "github"), Report: report(`{"decision": "reject", "reason": "duplicate"}`)})
	e := last(out.Ticket)
	if out.Ticket.Column != "rejected" || e.Detail != "triage -> rejected: triage: duplicate" || e.Rule != "triage rejected it" ||
		reason(out.Ticket) != model.ReasonTriageRejected {
		t.Errorf("%s %q %q", out.Ticket.Column, e.Detail, e.Rule)
	}
}

func TestAcceptedGitHubIssueWaitsAndUsesTriageWording(t *testing.T) {
	// port: TF TriageTest.test_accepted_github_issue_waits_for_human_and_uses_triage_wording
	c := cfg(t, "[triage.auto_accept]\ngithub = false\n")
	out := finish(t, FinishIn{Ticket: triaging(t, "github"), Report: report(`{"decision": "accept", "title": "Fix x", "task": "Do y"}`), Config: c})
	got := out.Ticket
	if got.Column != "needs_human" || got.Title != "Fix x" || got.Body != "Do y" {
		t.Errorf("%s %q %q", got.Column, got.Title, got.Body)
	}
	if o, _ := got.Original.Get(); o != (ticket.Original{Title: "issue title", Body: "issue body"}) {
		t.Errorf("original %+v", o)
	}
	e := last(got)
	if e.Detail != "triage -> needs_human: triage accepted; move to ready to start work" ||
		e.Rule != "triage accepted; GitHub issues wait for a person ([triage.auto_accept] github = false)" {
		t.Errorf("%q %q", e.Detail, e.Rule)
	}
	if a, _ := got.Ask.Get(); a.Kind != string(model.AskStart) {
		t.Errorf("ask %+v", a)
	}
}

func TestAcceptedClockTicketCanGoStraightToReady(t *testing.T) {
	// port: TF TriageTest.test_accepted_clock_ticket_can_go_straight_to_ready
	c := cfg(t, "[triage.auto_accept]\nclock = true\n")
	out := finish(t, FinishIn{Ticket: triaging(t, "clock"), Report: report(`{"decision": "accept", "title": "Fix x", "task": "Do y"}`), Config: c})
	if e := last(out.Ticket); out.Ticket.Column != "ready" || e.Detail != "triage -> ready: triage accepted" ||
		e.Rule != "triage accepted; clock tickets start straight away ([triage.auto_accept] clock = true)" {
		t.Errorf("%s %q %q", out.Ticket.Column, e.Detail, e.Rule)
	}
}

func TestAcceptWithoutTaskNeedsHuman(t *testing.T) {
	// port: TF TriageTest.test_accept_without_task_needs_human
	c := cfg(t, "[triage.auto_accept]\nclock = true\n")
	out := finish(t, FinishIn{Ticket: triaging(t, "clock"), Report: report(`{"decision": "accept"}`), Config: c})
	if e := last(out.Ticket); out.Ticket.Column != "needs_human" || e.Rule != "triage accepted it without writing a task" ||
		e.Detail != "triage -> needs_human: triage unsure: " || reason(out.Ticket) != model.ReasonTriageUnsure {
		t.Errorf("%s %q %q", out.Ticket.Column, e.Detail, e.Rule)
	}
	out = finish(t, FinishIn{Ticket: triaging(t, "clock"), Report: report(`{"decision": "maybe", "reason": "vague"}`), Config: c})
	if e := last(out.Ticket); e.Rule != "triage could not decide" || e.Detail != "triage -> needs_human: triage unsure: vague" {
		t.Errorf("%q %q", e.Detail, e.Rule)
	}
}

func TestTriageFoldsTheTitleItWrites(t *testing.T) {
	// port: TL MarkdownTest.test_triage_folds_the_title_it_writes
	out := finish(t, FinishIn{Ticket: triaging(t, "github"), Report: report(`{"decision": "accept", "task": "Do it", "title": "New\n## Body\r\nforged"}`)})
	if out.Ticket.Title != "New ## Body forged" {
		t.Errorf("title %q", out.Ticket.Title)
	}
	long := strings.Repeat("ab ", 60)
	out = finish(t, FinishIn{Ticket: triaging(t, "github"), Report: report(`{"decision": "accept", "task": "x", "title": "` + long + `"}`)})
	if n := len(out.Ticket.Title); n != 120 {
		t.Errorf("title of %d", n)
	}
	out = finish(t, FinishIn{Ticket: triaging(t, "github"), Report: report(`{"decision": "accept", "task": 7, "title": "  \n "}`)})
	if out.Ticket.Title != "issue title" || out.Ticket.Body != "7" {
		t.Errorf("blank title keeps the old one: %q, a task is str()'d: %q", out.Ticket.Title, out.Ticket.Body)
	}
}

func TestTriageOfAnUnknownDoorNamesIt(t *testing.T) {
	out := finish(t, FinishIn{Ticket: triaging(t, "pigeon"), Report: report(`{"decision": "accept", "task": "x"}`)})
	if e := last(out.Ticket); e.Rule != "triage accepted; pigeon tickets wait for a person ([triage.auto_accept] pigeon = false)" {
		t.Errorf("%q", e.Rule)
	}
}
