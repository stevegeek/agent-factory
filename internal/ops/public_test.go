package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const readHead = "0123456789abcdef0123456789abcdef01234567"

// blocked files a change public review blocked at readHead.
func blocked(w *world) ticket.ID {
	t := w.new(ticket.NewIn{Project: "widgets", Title: "Tidy", Body: "b", Door: "human", Column: "public"}, func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/tidy")
		t.HeadBeforePublic = ticket.Some(readHead)
		t.Reports.Set("public", jsonx.ObjectOf(jsonx.Member{Key: "verdict", Value: jsonx.String("block")},
			jsonx.Member{Key: "findings", Value: jsonx.ArrayOf(jsonx.ObjectOf(jsonx.Member{Key: "where", Value: jsonx.String("branch")},
				jsonx.Member{Key: "why", Value: jsonx.String("names a host")}))},
			jsonx.Member{Key: "summary", Value: jsonx.String("one finding")}))
		ticket.Move(t, "needs_human", model.ReasonPublicBlocked, "public review blocked it: 1 finding(s), 1 in text the worker cannot change", "", "", now)
	})
	return t.ID
}

// A change public review blocked asks a person for one of three answers:
// back to work with a note, safe to publish at the head it read, or drop.
func TestPublicBlockedAsksThreeAnswers(t *testing.T) {
	w := newWorld(t, deskConfig)
	tk := w.load(blocked(w))
	a, _ := tk.Ask.Get()
	if a.Kind != "unstick" || !strings.Contains(a.Question, "safe to publish") || a.RetryColumn.Or("") != "" ||
		!strings.Contains(a.Why, "one finding") {
		t.Fatalf("ask %+v", a)
	}
	as := ops.Answers(tk, &a)
	if values(as) != "send_back,publish,drop" || as[1].Label != "Safe to publish at 0123456789ab" || !as[0].NeedsNote {
		t.Errorf("answers %+v", as)
	}
	// an ordinary unstick offers none of them
	plain := &ticket.Ticket{Column: "needs_human"}
	if got := values(ops.Answers(plain, &ticket.Ask{Kind: "unstick", RetryColumn: ticket.Some("gate")})); got != "retry,retry_note,drop" {
		t.Errorf("plain unstick: %s", got)
	}
}

func TestPublicBlockedAnswersLeaveTheBoard(t *testing.T) {
	ctx := context.Background()
	answer := func(w *world, id ticket.ID, value, note, as string) error {
		kind, since := askOf(w.load(id))
		_, err := w.svc.Answer(ctx, ops.AnswerIn{ID: string(id), Value: value, Note: note, Kind: kind, Since: since, By: as})
		return err
	}
	// the maintainer's pass is public review's pass of the head it read,
	// with who said so
	w := newWorld(t, deskConfig)
	id := blocked(w)
	if err := answer(w, id, "publish", "", ""); err != nil {
		t.Fatal(err)
	}
	got := w.load(id)
	m, _ := got.History[len(got.History)-1].Data.(ticket.Moved)
	if got.Column != "gate" || got.PublicHead.Or("") != readHead || m.Reason != model.ReasonPublicPassed || m.By != "sam" ||
		!strings.Contains(got.History[len(got.History)-1].Detail, "said by sam") {
		t.Errorf("publish: %s %v %+v", got.Column, got.PublicHead, m)
	}
	// any Foreman user may say so, recorded as theirs
	w = newWorld(t, deskConfig)
	id = blocked(w)
	if err := answer(w, id, "publish", "", "alex"); err != nil {
		t.Errorf("a Foreman user's publish: %v", err)
	}
	if got := w.load(id); got.Column != "gate" || !strings.Contains(got.History[len(got.History)-1].Detail, "said by alex") {
		t.Errorf("after alex's publish: %s %+v", got.Column, got.History[len(got.History)-1])
	}
	// back to work: the note and the findings both reach the worker, and
	// it counts as public review's one rework
	w = newWorld(t, deskConfig)
	id = blocked(w)
	if err := answer(w, id, "send_back", "", ""); err == nil || !strings.Contains(err.Error(), "needs a note") {
		t.Fatalf("send back without a note: %v", err)
	}
	if err := answer(w, id, "send_back", "rename the branch", ""); err != nil {
		t.Fatal(err)
	}
	got = w.load(id)
	if got.Column != "ready" || len(got.Notes) != 1 || ticket.PublicRounds(got) != 1 || got.PublicHead.Set {
		t.Errorf("send back: %s %+v %d", got.Column, got.Notes, ticket.PublicRounds(got))
	}
	w = newWorld(t, deskConfig)
	id = blocked(w)
	if err := answer(w, id, "drop", "", ""); err != nil || w.load(id).Column != "rejected" {
		t.Errorf("drop: %v %s", err, w.load(id).Column)
	}
	// the answers belong to a public block only
	w = newWorld(t, deskConfig)
	desk(w)
	if err := answer(w, "T0006", "publish", "", ""); err == nil || !strings.Contains(err.Error(), "not an answer") {
		t.Errorf("publish on another unstick: %v", err)
	}
}
