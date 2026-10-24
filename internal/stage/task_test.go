package stage

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func tasking(t *testing.T, parent string) *ticket.Ticket {
	extra := ""
	if parent != "" {
		extra = `"parent": "` + parent + `"`
	}
	tk := running(t, "T0002", "task", "working", extra)
	tk.Project, tk.Kind, tk.Branch, tk.Title = "factory", ticket.Some("task"), ticket.Nil[string](), "Uptime"
	return tk
}

// asked is a parent in column that asked task T0002 a question; typed
// says whether the asked event is typed or an old one read by its detail.
func asked(t *testing.T, column string, typed bool) *ticket.Ticket {
	p := decode(t, `{"id": "T0001", "project": "gem", "title": "Tidy README", "body": "Make it tidy.", "door": "human", "by": "factory",
		"column": "`+column+`", "branch": null, "agent": null, "reports": {}, "created": "x", "history": [], "kind": null, "ask": null}`)
	if typed {
		ticket.Event(p, "asked", "T0002: why this way?", "", ticket.Asked{Ticket: "T0002", Text: "why this way?"}, now)
	} else {
		p.History = append(p.History, ticket.Entry{At: "x", Event: "asked", Detail: "T0002: why this way?"})
	}
	return p
}

func TestFinishedTaskBringsItsResultToAPerson(t *testing.T) {
	// the finish:
	// port: TF TaskTicketTest.test_finished_task_brings_its_result_to_a_person
	out := finish(t, FinishIn{Ticket: tasking(t, ""), Report: report(`{"status": "done", "summary": "up 100%", "result": "# Uptime\nall good"}`)})
	got := out.Ticket
	if got.Column != "needs_human" || got.Result.Or("") != "# Uptime\nall good" || !strings.Contains(last(got).Detail, "task done: up 100%") ||
		reason(got) != model.ReasonTaskDone || out.AnswerParent != nil || !out.DeleteBranch {
		t.Errorf("%s %v %q %s", got.Column, got.Result, last(got).Detail, reason(got))
	}
	// the task's notice:
	// port: TF KindAskTest.test_designs_tasks_and_notices_each_ask_their_own_question
	if a, _ := got.Ask.Get(); a.Kind != "notice" || a.Why != "# Uptime all good." {
		t.Errorf("ask %+v", a)
	}
	out = finish(t, FinishIn{Ticket: tasking(t, ""), Report: report(`{"status": "done", "summary": "s", "result": "all good"}`)})
	if a, _ := out.Ticket.Ask.Get(); a.Kind != "notice" || a.Why != "all good." {
		t.Errorf("ask %+v", a)
	}
}

func TestAnAnswerGoesBackToTheTicketItWasAskedOn(t *testing.T) {
	// the finish:
	// port: TF TaskTicketTest.test_an_answer_goes_back_to_the_ticket_it_was_asked_on
	done := report(`{"status": "done", "summary": "Because the lint rule says so.", "result": "## Why\nlint"}`)
	for _, typed := range []bool{true, false} {
		parent := asked(t, "pr_open", typed)
		out := finish(t, FinishIn{Ticket: tasking(t, "T0001"), Report: done, Parent: parent})
		if out.Ticket.Column != "done" || last(out.Ticket).Detail != "working -> done: answered on T0001" || reason(out.Ticket) != model.ReasonAnsweredOn {
			t.Errorf("typed %v: %s %q", typed, out.Ticket.Column, last(out.Ticket).Detail)
		}
		want := ParentAnswer{Parent: "T0001", Task: "T0002", Text: "Because the lint rule says so."}
		if out.AnswerParent == nil || *out.AnswerParent != want {
			t.Fatalf("answer %+v", out.AnswerParent)
		}
		if !Deliver(parent, *out.AnswerParent, now) {
			t.Fatal("not delivered")
		}
		e := last(parent)
		if e.Event != "answered" || e.Detail != "T0002: Because the lint rule says so." || e.Data != (ticket.Answered{Ticket: "T0002", Text: "Because the lint rule says so."}) {
			t.Errorf("parent event %+v", e)
		}
	}
	// asked on a ticket that has closed since: the answer stays on the desk by itself
	closed := asked(t, "done", true)
	if out := finish(t, FinishIn{Ticket: tasking(t, "T0001"), Report: done, Parent: closed}); out.Ticket.Column != "needs_human" || out.AnswerParent != nil {
		t.Errorf("closed parent: %s", out.Ticket.Column)
	}
	if Deliver(closed, ParentAnswer{Parent: "T0001", Task: "T0002", Text: "x"}, now) {
		t.Error("delivered to a closed ticket")
	}
	// a blocked answer is something to look at on its own
	parent := asked(t, "pr_open", true)
	out := finish(t, FinishIn{Ticket: tasking(t, "T0001"), Report: report(`{"status": "blocked", "summary": "no access"}`), Parent: parent})
	if out.Ticket.Column != "needs_human" || out.AnswerParent != nil || reason(out.Ticket) != model.ReasonTaskBlocked {
		t.Errorf("blocked: %s", out.Ticket.Column)
	}
	// a parent that never asked, or another ticket than the parent
	if out := finish(t, FinishIn{Ticket: tasking(t, "T0001"), Report: done, Parent: silent(asked(t, "pr_open", false))}); out.AnswerParent != nil {
		t.Error("answered a ticket that never asked")
	}
	if out := finish(t, FinishIn{Ticket: tasking(t, "T0009"), Report: done, Parent: asked(t, "pr_open", true)}); out.AnswerParent != nil {
		t.Error("answered the wrong ticket")
	}
	long := strings.Repeat("word ", 100)
	out = finish(t, FinishIn{Ticket: tasking(t, "T0001"), Report: report(`{"status": "done", "summary": "` + long + `"}`), Parent: asked(t, "review", true)})
	if n := len([]rune(out.AnswerParent.Text)); n != 300 {
		t.Errorf("answer of %d", n)
	}
}

func silent(p *ticket.Ticket) *ticket.Ticket {
	p.History = nil
	return p
}
