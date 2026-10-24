package stage

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func securing(t *testing.T, extra string) *ticket.Ticket {
	if extra != "" {
		extra = ", " + extra
	}
	return running(t, "T0001", "security", "security", `"head_before_security": "`+head+`"`+extra)
}

func TestSecurityFailNeedsHuman(t *testing.T) {
	// port: TF TransitionTest.test_security_fail_needs_human
	out := finish(t, FinishIn{Ticket: securing(t, ""), Report: report(`{"verdict": "fail"}`), Facts: Facts{Head: head}})
	if out.Ticket.Column != "needs_human" || reason(out.Ticket) != model.ReasonSecurityFailed {
		t.Errorf("%s %s", out.Ticket.Column, reason(out.Ticket))
	}
	// no tier on the ticket: judged as tier 4, all seven questions
	if e := last(out.Ticket); e.Detail != "security -> needs_human: security: 0 answers for 7 questions" ||
		e.Rule != "security: tier 4 (input reaches a sink, and build or dependencies); 7 questions, 0 answers for 7 questions" {
		t.Errorf("%q %q", e.Detail, e.Rule)
	}
}

func TestSecurityIsDecidedFromEveryAnswerNotTheVerdict(t *testing.T) {
	// port: TF TransitionTest.test_security_is_decided_from_every_answer_not_the_verdict
	answer := func(c string) string { return `{"question": "q", "answer": "evidence", "concern": "` + c + `"}` }
	run := func(r string) *ticket.Ticket {
		tk := securing(t, `"security_questions": ["q1", "q2"]`)
		return finish(t, FinishIn{Ticket: tk, Report: report(r), Facts: Facts{Head: head}}).Ticket
	}
	got := run(`{"verdict": "pass", "answers": [` + answer("none") + `, ` + answer("low") + `]}`)
	if got.Column != "gate" || reason(got) != model.ReasonSecurityPassed || last(got).Detail != "security -> gate" ||
		last(got).Rule != "security: tier 4 (input reaches a sink, and build or dependencies); 2 questions, none above low" {
		t.Errorf("pass: %s %q %q", got.Column, last(got).Detail, last(got).Rule)
	}
	for _, c := range []struct{ report, want string }{
		{`{"verdict": "pass", "answers": [` + answer("none") + `, ` + answer("medium") + `]}`, "q2 medium"},
		{`{"verdict": "pass", "answers": [` + answer("none") + `]}`, "1 answers for 2 questions"},
		{`{"verdict": "pass", "answers": [` + answer("none") + `, ` + answer("fine") + `]}`, "question 2 has no usable answer"},
		{`{"verdict": "fail", "answers": [` + answer("none") + `, ` + answer("low") + `]}`, "reviewer did not pass it"},
	} {
		got := run(c.report)
		if got.Column != "needs_human" || !strings.Contains(last(got).Detail, c.want) {
			t.Errorf("%s: %s %q", c.want, got.Column, last(got).Detail)
		}
	}
}

func TestSecurityPassRecordsTheTierAsked(t *testing.T) {
	tk := securing(t, `"security_tier": 0, "security_questions": ["only one"]`)
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"verdict": "pass", "answers": [{"answer": "fine", "concern": "None"}]}`),
		Facts: Facts{Head: head}})
	if out.Ticket.Column != "gate" || last(out.Ticket).Rule != "security: tier 0 (text only); 1 questions, none above none" {
		t.Errorf("%s %q", out.Ticket.Column, last(out.Ticket).Rule)
	}
	if !out.Ticket.Agent.Set || out.Ticket.Agent.Present() {
		t.Errorf("agent cleared to null: %+v", out.Ticket.Agent)
	}
}

func TestSecurityPromptAsksTheFrozenQuestions(t *testing.T) {
	tk := securing(t, "")
	Begin(tk, model.StageSecurity, BeginIn{Head: head, Tier: 1})
	got := For(model.StageSecurity).Prompt(PromptIn{Ticket: tk, Project: gemProject(t)})
	if !strings.Contains(got, "Review tier: 1.") || !strings.Contains(got, "2. Does the diff do anything the task did not ask for") ||
		strings.Contains(got, "3. ") {
		t.Errorf("prompt:\n%s", got)
	}
}
