package stage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func designing(t *testing.T) *ticket.Ticket {
	tk := running(t, "T0001", "design", "working", `"parent": null`)
	tk.Title, tk.Body, tk.Kind, tk.Branch = "Add a Base62 encoder", "idea", ticket.Some("intent"), ticket.Nil[string]()
	return tk
}

func TestDesignWaitsForReviewWithTheProposedTickets(t *testing.T) {
	// the design:
	// port: TF IntentTest.test_design_waits_for_review_then_approve_files_the_chosen_tickets
	// the finish:
	// port: TF KindAskTest.test_designs_tasks_and_notices_each_ask_their_own_question
	tk := designing(t)
	tk.Column = "ready"
	if s, ok := ForTicket(tk); !ok || s != model.StageDesign {
		t.Errorf("stage for an intent in ready: %s", s)
	}
	tk.Column = "working"
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "done", "summary": "new encoder class", "design": "## Approach\nx",
		"tickets": [{"title": "Add encoder", "body": "do a"}, {"title": "Docs", "body": "do b"}, {"title": "", "body": "dropped"}]}`)})
	got := out.Ticket
	if !out.DeleteBranch || got.Column != "needs_human" || len(got.Proposed) != 2 || got.Design.Or("") != "## Approach\nx" {
		t.Fatalf("%v %s %+v", out.DeleteBranch, got.Column, got.Proposed)
	}
	if e := last(got); e.Detail != "working -> needs_human: design ready (2 tickets proposed): new encoder class; factory approve or redirect" ||
		reason(got) != model.ReasonDesignReady {
		t.Errorf("%q", e.Detail)
	}
	if a, _ := got.Ask.Get(); a.Kind != "design" || a.Why != "new encoder class." ||
		a.Question != "Approve the design for “Add a Base62 encoder” and its 2 proposed tickets, or send it back?" {
		t.Errorf("ask %+v", a)
	}
}

func TestDesignWithoutTicketsIsBlocked(t *testing.T) {
	// the finish:
	// port: TF IntentTest.test_design_without_tickets_is_blocked_and_cannot_be_approved
	out := finish(t, FinishIn{Ticket: designing(t), Report: report(`{"status": "done", "summary": "too vague", "design": "d", "tickets": []}`)})
	if !strings.Contains(last(out.Ticket).Detail, "designer blocked") || reason(out.Ticket) != model.ReasonDesignerBlocked {
		t.Errorf("%q", last(out.Ticket).Detail)
	}
	// a blocked designer's draft is not one to approve
	// the finish:
	// port: TF KindAskTest.test_a_blocked_designer_asks_to_be_unstuck_not_approved
	out = finish(t, FinishIn{Ticket: designing(t), Report: report(`{"status": "blocked", "summary": "two readings of the idea",
		"design": "half a draft", "tickets": [{"title": "A", "body": "a"}]}`)})
	if a, _ := out.Ticket.Ask.Get(); a.Kind != "unstick" || a.RetryColumn.Or("") != "ready" {
		t.Errorf("ask %+v", a)
	}
}

func TestDesignCaps(t *testing.T) {
	var tickets []string
	for i := range 8 {
		tickets = append(tickets, fmt.Sprintf(`{"title": "%s%d", "body": "b"}`, strings.Repeat("x", 130), i))
	}
	tickets = append(tickets, `"junk"`, `{"title": "  ", "body": "b"}`, `{"title": 5, "body": 6}`)
	out := finish(t, FinishIn{Ticket: designing(t), Report: report(`{"status": "done", "design": "d", "tickets": [` + strings.Join(tickets, ",") + `]}`)})
	if len(out.Ticket.Proposed) != 6 {
		t.Fatalf("proposed %d", len(out.Ticket.Proposed))
	}
	if n := len(out.Ticket.Proposed[0].Title); n != 120 {
		t.Errorf("title of %d", n)
	}
	out = finish(t, FinishIn{Ticket: designing(t), Report: report(`{"status": "done", "design": "d", "tickets": [{"title": 5, "body": 6}]}`)})
	if p := out.Ticket.Proposed; len(p) != 1 || p[0] != (ticket.Proposed{Title: "5", Body: "6"}) {
		t.Errorf("str()'d: %+v", p)
	}
	out = finish(t, FinishIn{Ticket: designing(t), Report: report(`{"status": "done", "design": "d", "tickets": "abc"}`)})
	if len(out.Ticket.Proposed) != 0 || out.Ticket.Proposed == nil {
		t.Errorf("a string of tickets proposes none: %+v", out.Ticket.Proposed)
	}
}

func TestRedirectedDesignPromptCarriesTheFeedback(t *testing.T) {
	// the prompt:
	// port: TF IntentTest.test_redirect_reruns_the_designer_with_the_feedback
	tk := designing(t)
	tk.Feedback = []ticket.Feedback{{At: "x", Design: "first design", Note: "keep the old API"}}
	got := For(model.StageDesign).Prompt(PromptIn{Ticket: tk})
	for _, want := range []string{"first design", "keep the old API", "The maintainer's feedback on it (follow this):"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}
