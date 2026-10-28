package ops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestAQuestionOffersCloseItAndDrop(t *testing.T) {
	tk := &ticket.Ticket{ID: "T0001", Column: "needs_human"}
	as := ops.Answers(tk, &ticket.Ask{Kind: "question"})
	if values(as) != "answer,close,drop" {
		t.Fatalf("answers %s", values(as))
	}
	for _, x := range as[1:] {
		if !x.NeedsNote || !x.MaintainerOnly {
			t.Errorf("%s: %+v", x.Value, x)
		}
	}
	if as[1].Label != "Close it" || as[1].Danger || as[2].Label != "Drop" || !as[2].Danger {
		t.Errorf("labels %+v", as[1:])
	}
	if got := values(ops.Answers(tk, &ticket.Ask{Kind: "design"})); got != "approve,redirect,close,drop" {
		t.Errorf("design answers %s", got)
	}
	if got := values(ops.Answers(tk, &ticket.Ask{Kind: "open"})); got != "open_pr,send_back,close,drop" {
		t.Errorf("open answers with no PR %s", got)
	}
	tk.PR = ticket.Some(ticket.PR{Number: 23})
	if got := values(ops.Answers(tk, &ticket.Ask{Kind: "open"})); got != "open_pr,send_back" {
		t.Errorf("open answers with a PR %s", got)
	}
}

// closeWorld is the desk with T0009's agent question on bot/asks, and two
// questions asked about it: T0013 open, T0014 with an agent answering it.
func closeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, deskConfig)
	desk(w)
	ctx := context.Background()
	if err := w.board.Changing(ctx, "T0009", store.NoWait, func(t *ticket.Ticket) error {
		t.Branch = ticket.Some("bot/asks")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"is it needed?", "where is it?"} {
		if _, err := w.svc.Ask(ctx, "T0009", q); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.board.Changing(ctx, "T0014", store.NoWait, func(t *ticket.Ticket) error {
		t.Agent = ticket.Some(ticket.Agent{Name: "t0014-w1", Stage: "task", Started: "2026-10-18T09:30:00+00:00", Host: "local"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func answerAsk(w *world, id, value, note, by string) (string, error) {
	kind, since := askOf(w.load(ticket.ID(id)))
	return w.svc.Answer(context.Background(), ops.AnswerIn{ID: id, Value: value, Note: note, Kind: kind, Since: since, By: by})
}

func TestCloseAndDropEndAQuestionWithNoAgentRun(t *testing.T) {
	for _, c := range []struct{ value, column string }{{"close", "done"}, {"drop", "rejected"}} {
		t.Run(c.value, func(t *testing.T) {
			w := closeWorld(t)
			out, err := answerAsk(w, "T0009", c.value, "  it already exists  ", "")
			if err != nil {
				t.Fatal(err)
			}
			got := w.load("T0009")
			if got.Column != c.column || got.Agent.Present() {
				t.Errorf("column %s, agent %v", got.Column, got.Agent.Present())
			}
			if q := got.Question.V; q.Answered == "" {
				t.Errorf("the agent's question is still open")
			}
			last := got.History[len(got.History)-1]
			if m, ok := last.Data.(ticket.Moved); !ok || m.By != "sam" || !strings.Contains(last.Detail, "it already exists") {
				t.Errorf("the move %+v", last)
			}
			if c.value == "close" && got.Result.Or("") != "it already exists" {
				t.Errorf("result %q", got.Result.Or(""))
			}
			if c.value == "drop" && got.Result.Present() {
				t.Errorf("a drop records a result")
			}
			if strings.Join(w.pr.prunes, ",") != "bot/asks" {
				t.Errorf("prunes %v", w.pr.prunes)
			}
			q := w.load("T0013")
			if q.Column != "done" || !strings.Contains(q.History[len(q.History)-1].Detail, "T0009") {
				t.Errorf("the open question task: %s %+v", q.Column, q.History[len(q.History)-1])
			}
			if running := w.load("T0014"); running.Column != "ready" {
				t.Errorf("the question task with an agent: %s", running.Column)
			}
			if !strings.Contains(out, "T0013 closed") || !strings.Contains(out, "T0014 stays open") {
				t.Errorf("out %q", out)
			}
		})
	}
}

func TestCloseKeepsTheBranchOfATicketWithAPR(t *testing.T) {
	w := closeWorld(t)
	if err := w.board.Changing(context.Background(), "T0009", store.NoWait, func(t *ticket.Ticket) error {
		t.PR = ticket.Some(ticket.PR{Number: 5, URL: "https://github.com/example/widgets/pull/5"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := answerAsk(w, "T0009", "close", "done elsewhere", ""); err != nil {
		t.Fatal(err)
	}
	if w.load("T0009").Column != "done" || len(w.pr.prunes) != 0 {
		t.Errorf("column %s, prunes %v", w.load("T0009").Column, w.pr.prunes)
	}
}

func TestCloseAndDropAreTheMaintainersAndNeedANote(t *testing.T) {
	w := closeWorld(t)
	before := w.files()
	for _, value := range []string{"close", "drop"} {
		if _, err := answerAsk(w, "T0009", value, "not needed", "robin"); err == nil || !strings.Contains(err.Error(), "only the maintainer") {
			t.Errorf("%s by another user: %v", value, err)
		}
		if _, err := answerAsk(w, "T0009", value, "", ""); !errors.Is(err, ops.ErrNoteNeeded) {
			t.Errorf("%s with no note: %v", value, err)
		}
	}
	if _, err := answerAsk(w, "T0008", "close", "x", ""); err == nil || !strings.Contains(err.Error(), "not an answer") {
		t.Errorf("close on a notice: %v", err)
	}
	after := w.files()
	for name, b := range before {
		if after[name] != b {
			t.Errorf("a refused answer changed %s", name)
		}
	}
	if len(w.pr.prunes) != 0 {
		t.Errorf("prunes %v", w.pr.prunes)
	}
}

func TestIntentsRecordAPruneOfABotBranchOnly(t *testing.T) {
	in := ops.Intents{Clock: fixedClock{}}
	tk := &ticket.Ticket{Branch: ticket.Some("bot/x")}
	if err := in.Prune(context.Background(), &config.Config{}, tk); err != nil || tk.PruneRequested.Or("") != "2026-10-18T10:00:00+00:00" {
		t.Errorf("bot branch: %v %+v", err, tk.PruneRequested)
	}
	tk = &ticket.Ticket{Branch: ticket.Some("main")}
	if err := in.Prune(context.Background(), &config.Config{}, tk); err != nil || tk.PruneRequested.Set {
		t.Errorf("a branch that is not the factory's is never pruned")
	}
}
