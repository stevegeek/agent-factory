package stage

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestAnAgentAsksAndTheQuestionWaitsForTheMaintainer(t *testing.T) {
	// the finish:
	// port: TF AgentQuestionTest.test_an_agent_asks_the_reply_carries_the_stage_on
	tk := running(t, "T0001", "task", "working", "")
	tk.Kind = ticket.Some("task")
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "question", "question": "Should the minimum be 8 or 16?",
		"summary": "Stopped before changing the default."}`)})
	got := out.Ticket
	a, _ := got.Ask.Get()
	if got.Column != "needs_human" || a.Kind != "question" || a.Question != "Should the minimum be 8 or 16?" ||
		a.Why != "Stopped before changing the default." || reason(got) != model.ReasonAgentQuestion {
		t.Errorf("%s %+v", got.Column, a)
	}
	q, _ := got.Question.Get()
	if q != (ticket.Question{Text: "Should the minimum be 8 or 16?", Stage: "task", Agent: "t0001-k1", At: "2026-10-17T09:30:00+00:00"}) {
		t.Errorf("question %+v", q)
	}
	if last(got).Detail != "working -> needs_human: task asks: Should the minimum be 8 or 16?" {
		t.Errorf("%q", last(got).Detail)
	}
	// the question note is in every stage that may ask
	for _, info := range model.Stages() {
		p := For(info.Name).Prompt(PromptIn{Ticket: got})
		if strings.Contains(p, "If you need a decision") != info.MayAsk {
			t.Errorf("%s: question note %v", info.Name, !info.MayAsk)
		}
	}
}

func TestAQuestionWithoutAQuestionIsJustBlocked(t *testing.T) {
	// port: TF AgentQuestionTest.test_a_question_without_a_question_is_just_blocked
	tk := running(t, "T0001", "task", "working", "")
	tk.Kind = ticket.Some("task")
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "question", "question": "  ", "summary": "hm"}`)})
	if a, _ := out.Ticket.Ask.Get(); a.Kind == "question" || reason(out.Ticket) != model.ReasonTaskBlocked {
		t.Errorf("%+v %s", a, reason(out.Ticket))
	}
}

func TestReviewersCannotAsk(t *testing.T) {
	tk := running(t, "T0001", "review", "review", `"head_before_review": "`+head+`"`)
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "question", "question": "May I?", "verdict": "pass"}`), Facts: Facts{Head: head}})
	if out.Ticket.Column != "security" || out.Ticket.Question.Set {
		t.Errorf("%s %+v", out.Ticket.Column, out.Ticket.Question)
	}
}

func TestQuestionIsCut(t *testing.T) {
	long := strings.Repeat("q ", 1500)
	tk := running(t, "T0001", "work", "working", "")
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"status": "question", "question": "` + long + `"}`)})
	q, _ := out.Ticket.Question.Get()
	if n := len([]rune(q.Text)); n != 2000 {
		t.Errorf("question of %d", n)
	}
	detail := strings.TrimPrefix(last(out.Ticket).Detail, "working -> needs_human: work asks: ")
	if n := len([]rune(detail)); n != 200 {
		t.Errorf("detail of %d", n)
	}
	out = finish(t, FinishIn{Ticket: running(t, "T0001", "work", "working", ""), Report: report(`{"status": "question", "question": 42}`)})
	if q, _ := out.Ticket.Question.Get(); q.Text != "42" {
		t.Errorf("a question is str()'d: %q", q.Text)
	}
}
