package stage

import (
	"reflect"
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
	if !reflect.DeepEqual(q, ticket.Question{Text: "Should the minimum be 8 or 16?", Stage: "task", Agent: "t0001-k1", At: "2026-10-17T09:30:00+00:00"}) {
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

const optionsReport = `{"status": "question", "question": "Which name?", "summary": "Stopped before the rename.", "options": [
	{"key": "keep", "label": "Keep the  old\nname", "detail": " The API stays. ", "recommended": true},
	{"key": "rename", "label": "Rename it", "effect": "close"},
	{"key": "later", "label": "Rename it in a follow-up", "effect": "file", "kind": "intent", "then": "continue"},
	{"key": "no", "label": "Neither", "effect": "drop"}]}`

func TestAQuestionKeepsGoodOptions(t *testing.T) {
	out := finish(t, FinishIn{Ticket: running(t, "T0001", "work", "working", ""), Report: report(optionsReport)})
	q, _ := out.Ticket.Question.Get()
	want := []ticket.QuestionOption{
		{Key: "keep", Label: "Keep the old name", Detail: "The API stays.", Recommended: true},
		{Key: "rename", Label: "Rename it", Effect: "close"},
		{Key: "later", Label: "Rename it in a follow-up", Effect: "file", Kind: "intent", Then: "continue"},
		{Key: "no", Label: "Neither", Effect: "drop"},
	}
	if out.Ticket.Column != "needs_human" || q.Text != "Which name?" || q.Select != "" || !reflect.DeepEqual(q.Options, want) {
		t.Errorf("%s %+v", out.Ticket.Column, q)
	}
	if d := last(out.Ticket).Detail; d != "working -> needs_human: work asks: Which name?" {
		t.Errorf("%q", d)
	}
	many := `{"status": "question", "question": "Which?", "select": "many", "options": [{"key": "a", "label": "A"}, {"key": "b", "label": "B", "effect": "continue"}]}`
	out = finish(t, FinishIn{Ticket: running(t, "T0001", "work", "working", ""), Report: report(many)})
	if q, _ := out.Ticket.Question.Get(); q.Select != "many" || len(q.Options) != 2 {
		t.Errorf("many: %+v", q)
	}
}

func TestBadOptionsAreDroppedAndTheQuestionStays(t *testing.T) {
	opt := func(key, extra string) string { return `{"key": "` + key + `", "label": "` + key + `"` + extra + `}` }
	two := opt("a", "") + ", " + opt("b", "")
	for name, tc := range map[string]struct{ sel, opts string }{
		"duplicate keys":    {"", opt("a", "") + ", " + opt("a", "")},
		"unknown effect":    {"", opt("a", "") + ", " + opt("b", `, "effect": "merge"`)},
		"six options":       {"", two + ", " + opt("c", "") + ", " + opt("d", "") + ", " + opt("e", "") + ", " + opt("f", "")},
		"one option":        {"", opt("a", "")},
		"two recommended":   {"", opt("a", `, "recommended": true`) + ", " + opt("b", `, "recommended": true`)},
		"file with no then": {"", opt("a", "") + ", " + opt("b", `, "effect": "file"`)},
		"many and a close":  {`"select": "many", `, opt("a", "") + ", " + opt("b", `, "effect": "close"`)},
		"not a list":        {"", `"a"`},
		"wrong type":        {"", opt("a", "") + ", " + opt("b", `, "recommended": "yes"`)},
	} {
		rep := `{"status": "question", "question": "Which?", ` + tc.sel + `"options": [` + tc.opts + `]}`
		if name == "not a list" {
			rep = `{"status": "question", "question": "Which?", "options": "a"}`
		}
		out := finish(t, FinishIn{Ticket: running(t, "T0001", "work", "working", ""), Report: report(rep)})
		q, ok := out.Ticket.Question.Get()
		if !ok || q.Text != "Which?" || q.Options != nil || q.Select != "" || out.Ticket.Column != "needs_human" {
			t.Errorf("%s: %s %+v", name, out.Ticket.Column, q)
		}
		if d := last(out.Ticket).Detail; !strings.HasPrefix(d, "working -> needs_human: work asks: Which?; its options were dropped: ") {
			t.Errorf("%s: %q", name, d)
		}
	}
}
