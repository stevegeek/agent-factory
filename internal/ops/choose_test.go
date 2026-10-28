package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// asking files T0001, a change whose worker asks with options, through door.
func asking(w *world, door string, change func(t *ticket.Ticket)) *ticket.Ticket {
	return w.new(ticket.NewIn{Project: "widgets", Title: "Rename the parser", Body: "b", Door: door, Column: "working"}, func(t *ticket.Ticket) {
		t.Question = ticket.Some(ticket.Question{Text: "Which name?", Stage: "work", Agent: "t0001-w1", At: "2026-10-18T09:00:00+00:00",
			Options: []ticket.QuestionOption{
				{Key: "keep", Label: "Keep the old name", Detail: "The API stays.", Recommended: true},
				{Key: "done", Label: "It is fine as it is", Effect: ticket.EffectClose},
				{Key: "no", Label: "Not worth it", Effect: ticket.EffectDrop},
				{Key: "split", Label: "Rename it in its own change", Detail: "Callers move too.", Effect: ticket.EffectFile, Then: ticket.EffectContinue},
				{Key: "idea", Label: "Design a new parser", Effect: ticket.EffectFile, Kind: "intent", Then: ticket.EffectClose},
			}})
		ticket.Move(t, "needs_human", model.ReasonAgentQuestion, "work asks: Which name?", "", "", now)
		if change != nil {
			change(t)
		}
	})
}

func lastChoice(t *testing.T, tk *ticket.Ticket) ticket.OptionChosen {
	t.Helper()
	for i := len(tk.History) - 1; i >= 0; i-- {
		if c, ok := tk.History[i].Data.(ticket.OptionChosen); ok {
			return c
		}
	}
	t.Fatal("no option_chosen event")
	return ticket.OptionChosen{}
}

func reasonOf(tk *ticket.Ticket) model.Reason {
	for i := len(tk.History) - 1; i >= 0; i-- {
		if m, ok := tk.History[i].Data.(ticket.Moved); ok {
			return m.Reason
		}
	}
	return ""
}

func TestEachEffectSettlesTheTicket(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		key, note, column string
		reason            model.Reason
		out               string
	}{
		{"keep", "", "ready", model.ReasonQuestionAnswered, "T0001: answered; a new work run carries on"},
		{"done", "thanks", "done", model.ReasonQuestionClosed, "T0001: closed: It is fine as it is"},
		{"no", "", "rejected", model.ReasonQuestionDropped, "T0001: dropped"},
		{"split", "", "ready", model.ReasonQuestionAnswered, "T0001: filed T0002; a new work run carries on"},
		{"idea", "", "done", model.ReasonQuestionClosed, "T0001: closed: Design a new parser"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			w := newWorld(t, deskConfig)
			asking(w, "human", nil)
			out, err := w.svc.Reply(ctx, "T0001", tc.note, tc.key)
			if err != nil || out != tc.out {
				t.Fatalf("%q %v", out, err)
			}
			tk := w.load("T0001")
			if tk.Column != tc.column || reasonOf(tk) != tc.reason {
				t.Errorf("%s %s", tk.Column, reasonOf(tk))
			}
			// only continue lands where a stage starts
			if _, runs := model.StageForColumn(model.Column(tk.Column)); runs != (tc.reason == model.ReasonQuestionAnswered) {
				t.Errorf("%s: a stage starts from %s: %v", tc.key, tk.Column, runs)
			}
			q, _ := tk.Question.Get()
			c := lastChoice(t, tk)
			effect, _ := q.Option(tc.key)
			if q.Answered == "" || len(q.Chosen) != 1 || q.Chosen[0] != tc.key || q.Effect != effect.EffectOf() ||
				c.Keys[0] != tc.key || c.Effect != q.Effect || c.Filed != q.Filed || c.By != "sam" {
				t.Errorf("question %+v, event %+v", q, c)
			}
			n := tk.Notes[len(tk.Notes)-1]
			if !strings.Contains(n.Text, "“"+effect.Label+"”") || n.Question.Or("") != "Which name?" {
				t.Errorf("the note does not name the choice: %+v", n)
			}
			if w.wakes.count() != 1 {
				t.Errorf("%d wakes", w.wakes.count())
			}
		})
	}
}

func TestContinueNamesTheChoiceForTheNextRun(t *testing.T) {
	w := newWorld(t, deskConfig)
	asking(w, "human", nil)
	if _, err := w.svc.Reply(context.Background(), "T0001", "and mind the docs", "keep"); err != nil {
		t.Fatal(err)
	}
	n := w.load("T0001").Notes[0]
	want := "Your question: “Which name?” The maintainer's answer: chose “Keep the old name” (The API stays.). and mind the docs"
	if n.Text != want {
		t.Errorf("%q", n.Text)
	}
}

func TestCloseRecordsTheResultAndDropTheReason(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	asking(w, "human", nil)
	if _, err := w.svc.Reply(ctx, "T0001", "thanks", "done"); err != nil {
		t.Fatal(err)
	}
	if r := w.load("T0001").Result.Or(""); r != "It is fine as it is\n\nthanks" {
		t.Errorf("result %q", r)
	}
	w = newWorld(t, deskConfig)
	asking(w, "human", nil)
	if _, err := w.svc.Reply(ctx, "T0001", "", "no"); err != nil {
		t.Fatal(err)
	}
	h := w.load("T0001").History
	if d := h[len(h)-1].Detail; d != "needs_human -> rejected: dropped by the answer: Not worth it" {
		t.Errorf("%q", d)
	}
}

func TestFileFilesALinkedTicket(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	asking(w, "human", nil)
	if _, err := w.svc.Reply(ctx, "T0001", "soon", "split"); err != nil {
		t.Fatal(err)
	}
	parent, child := w.load("T0001"), w.load("T0002")
	if child.Column != "triage" || child.KindName() != "" || child.Parent.Or("") != "T0001" || child.Door != "human" ||
		child.Title != "Rename it in its own change" || child.Project != "widgets" {
		t.Errorf("child %s %q %q %s %q", child.Column, child.KindName(), child.Parent.Or(""), child.Door, child.Title)
	}
	for _, s := range []string{"Callers move too.", "Which name?", "soon"} {
		if !strings.Contains(child.Body, s) {
			t.Errorf("the body has no %q:\n%s", s, child.Body)
		}
	}
	if len(parent.Children) != 1 || parent.Children[0] != "T0002" {
		t.Errorf("children %v", parent.Children)
	}
	filed := false
	for _, e := range parent.History {
		if f, ok := e.Data.(ticket.Filed); ok && f.Ticket == "T0002" {
			filed = true
		}
	}
	if q, _ := parent.Question.Get(); !filed || q.Filed != "T0002" {
		t.Errorf("filed %v, question %+v", filed, q)
	}

	w = newWorld(t, deskConfig)
	asking(w, "human", nil)
	if _, err := w.svc.Reply(ctx, "T0001", "", "idea"); err != nil {
		t.Fatal(err)
	}
	if child := w.load("T0002"); child.Column != "ready" || child.KindName() != "intent" {
		t.Errorf("an intent from a trusted door: %s %q", child.Column, child.KindName())
	}
	if r := w.load("T0001").Result.Or(""); r != "Filed as T0002: Design a new parser" {
		t.Errorf("result %q", r)
	}

	w = newWorld(t, deskConfig)
	asking(w, "github", nil)
	if _, err := w.svc.Reply(ctx, "T0001", "", "idea"); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Errorf("an intent from an untrusted door: %v", err)
	}
	if _, err := w.board.Reader.Load("T0002"); err == nil {
		t.Error("a refused file filed a ticket")
	}
}

func TestChoicesAreRefused(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	asking(w, "human", func(t *ticket.Ticket) {
		t.PR = ticket.Some(ticket.PR{Number: 23, URL: "https://github.com/example/widgets/pull/23"})
	})
	for _, key := range []string{"done", "no", "idea"} {
		if _, err := w.svc.Reply(ctx, "T0001", "", key); err == nil || !strings.Contains(err.Error(), "close the PR on GitHub first") {
			t.Errorf("%s with an open PR: %v", key, err)
		}
	}
	for _, tc := range []struct {
		keys []string
		want string
	}{
		{[]string{"nope"}, "not an option"},
		{[]string{"keep", "split"}, "takes one option"},
	} {
		if _, err := w.svc.Reply(ctx, "T0001", "", tc.keys...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v", tc.keys, err)
		}
	}
	if _, err := w.svc.Reply(ctx, "T0001", "", " ", ""); err == nil || !strings.Contains(err.Error(), "reply what?") {
		t.Errorf("no option and no text: %v", err)
	}
	if tk := w.load("T0001"); tk.Column != "needs_human" || len(tk.Notes) != 0 {
		t.Errorf("a refusal changed the ticket: %s", tk.Column)
	}
	// with the open PR, continue still works
	if out, err := w.svc.Reply(ctx, "T0001", "", "keep"); err != nil || !strings.Contains(out, "carries on") {
		t.Errorf("continue with an open PR: %q %v", out, err)
	}
	// an answered question takes no option
	if _, err := w.svc.Reply(ctx, "T0001", "", "keep"); err == nil || !strings.Contains(err.Error(), "no open question") {
		t.Errorf("a second choice: %v", err)
	}
}

func TestANoteAloneAnswersAQuestionWithOptions(t *testing.T) {
	w := newWorld(t, deskConfig)
	asking(w, "human", nil)
	out, err := w.svc.Reply(context.Background(), "T0001", "neither: call it reader")
	if err != nil || out != "T0001: answered; a new work run carries on" {
		t.Fatalf("%q %v", out, err)
	}
	tk := w.load("T0001")
	if q, _ := tk.Question.Get(); tk.Column != "ready" || q.Effect != "" || q.Chosen != nil {
		t.Errorf("%s %+v", tk.Column, q)
	}
}

func TestDeskAnswersWithOptions(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	tk := asking(w, "human", nil)
	if a := ops.Answers(tk, &ticket.Ask{Kind: string(model.AskQuestion)}); len(a) != 1 || a[0].NeedsNote {
		t.Errorf("a question with options needs no note: %+v", a)
	}
	kind, since := askOf(tk)
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "answer", Kind: kind, Since: since}); err != ops.ErrNoteNeeded {
		t.Errorf("no option and no note: %v", err)
	}
	out, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "answer", Options: []string{"no"}, Kind: kind, Since: since, By: "robin"})
	if err != nil || out != "T0001: dropped" {
		t.Fatalf("%q %v", out, err)
	}
	if c := lastChoice(t, w.load("T0001")); c.By != "robin" || c.Effect != "drop" {
		t.Errorf("%+v", c)
	}
	choices, many := ops.Choices(tk)
	if len(choices) != 5 || many || !choices[0].Recommended || choices[3].Effect != "files a change, then carries on" ||
		choices[1].Effect != "closes the ticket" {
		t.Errorf("%+v", choices)
	}
}
