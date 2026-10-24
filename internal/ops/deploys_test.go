package ops_test

import (
	"context"
	"os"
	"testing"

	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// heldDeploy files a Deploy? ticket as a held deploy does.
func heldDeploy(w *world) *ticket.Ticket {
	w.t.Helper()
	return w.new(ticket.NewIn{Project: model.NoticeProject, Title: "Deploy the factory?", Door: "clock", Column: "needs_human"},
		func(t *ticket.Ticket) {
			t.Deploy = ticket.Some(ticket.Deploy{Rev: "0123456789ab", Why: []string{"merged #12"}})
			ticket.Move(t, "needs_human", model.ReasonDeployHeld, "deploys are held", "", "", now)
		})
}

func answerDeploy(w *world, id ticket.ID, value, by string) (string, error) {
	kind, since := askOf(w.load(id))
	return w.svc.Answer(context.Background(), ops.AnswerIn{ID: string(id), Value: value, Kind: kind, Since: since, By: by})
}

func TestDeployAnswers(t *testing.T) {
	as := ops.Answers(&ticket.Ticket{Column: "needs_human"}, &ticket.Ask{Kind: string(model.AskDeploy)})
	if values(as) != "deploy,drop" || !as[0].Primary || as[0].Label != "Deploy it" || !as[1].Danger || as[1].Label != "Not now" {
		t.Errorf("answers %+v", as)
	}
}

func TestAnswerDeployRecordsTheIntent(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := heldDeploy(w).ID
	if kind, _ := askOf(w.load(id)); kind != string(model.AskDeploy) {
		t.Fatalf("ask %q", kind)
	}
	out, err := answerDeploy(w, id, "deploy", "")
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Error("nothing printed")
	}
	got := w.load(id)
	d, _ := got.Deploy.Get()
	if got.Column != "done" || d.Requested.Or("") != "2026-10-18T10:00:00+00:00" || d.Started.Set {
		t.Errorf("column %s deploy %+v", got.Column, d)
	}
	last := got.History[len(got.History)-1]
	if m, _ := last.Data.(ticket.Moved); m.Reason != model.ReasonDeployRequested || m.By != "sam" {
		t.Errorf("last %+v", last)
	}
	if got.Ask.Present() {
		t.Errorf("a done deploy still asks: %+v", got.Ask)
	}
	if w.wakes.count() != 1 {
		t.Errorf("%d wakes", w.wakes.count())
	}
}

func TestAnswerDeployIsMaintainerOnly(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := heldDeploy(w).ID
	before := w.files()
	if _, err := answerDeploy(w, id, "deploy", "robin"); err == nil {
		t.Error("a Foreman user deployed")
	}
	if after := w.files(); after[string(id)+".json"] != before[string(id)+".json"] {
		t.Error("a refused deploy saved something")
	}
	if w.wakes.count() != 0 {
		t.Errorf("%d wakes", w.wakes.count())
	}
	// nor drop it: not now is the maintainer's too
	if _, err := answerDeploy(w, id, "drop", "robin"); err == nil {
		t.Error("a Foreman user dropped the deploy")
	}
	if after := w.files(); after[string(id)+".json"] != before[string(id)+".json"] {
		t.Error("a refused drop saved something")
	}
	// a note is anyone's, as on any ticket
	if _, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: string(id), Value: ops.AnswerNote, Note: "looks fine", By: "robin"}); err != nil {
		t.Errorf("note: %v", err)
	}
}

func TestAnswerDropClosesTheDeploy(t *testing.T) {
	w := newWorld(t, deskConfig)
	id := heldDeploy(w).ID
	if _, err := answerDeploy(w, id, "drop", ""); err != nil {
		t.Fatal(err)
	}
	got := w.load(id)
	if d, _ := got.Deploy.Get(); got.Column != "rejected" || d.Requested.Set {
		t.Errorf("column %s deploy %+v", got.Column, d)
	}
}

func TestHoldDeploys(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	w.svc.Log = w.log
	path := w.paths.DeployHold()
	line, err := w.svc.HoldDeploys(ctx, true, "a week away", "")
	if err != nil || line == "" {
		t.Fatalf("hold: %q, %v", line, err)
	}
	h, held, err := deployrec.ReadHold(path)
	if err != nil || !held || h != (deployrec.Hold{By: "sam", At: "2026-10-18T10:00:00+00:00", Note: "a week away"}) {
		t.Errorf("hold = %+v, %v, %v", h, held, err)
	}
	if w.wakes.count() != 1 {
		t.Errorf("%d wakes", w.wakes.count())
	}
	if logs := w.log.take(); len(logs) != 1 || logs[0] != "deploys held by sam" {
		t.Errorf("log %q", logs)
	}
	if _, err := w.svc.HoldDeploys(ctx, false, "", "sam"); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := deployrec.ReadHold(path); held {
		t.Error("still held")
	}
	if _, err := w.svc.HoldDeploys(ctx, true, "", "robin"); err == nil {
		t.Error("a Foreman user held deploys")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("a refused hold wrote %s: %v", path, err)
	}
	if _, err := w.svc.HoldDeploys(ctx, true, "bad\x00note", ""); err == nil {
		t.Error("a note with control characters")
	}
}
