package ops_test

import (
	"context"
	"strings"
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

// Deploying the factory is [board] people's; not now is anyone's.
func TestAnswerDeployIsBoardPeoples(t *testing.T) {
	w := newWorld(t, deskConfig)
	other := heldDeploy(w).ID
	if _, err := answerDeploy(w, other, "deploy", "alex"); err != nil {
		t.Errorf("alex, the second of [board] people, deploys: %v", err)
	}
	id := heldDeploy(w).ID
	before, wakes := w.files(), w.wakes.count()
	if _, err := answerDeploy(w, id, "deploy", "robin"); err == nil || !strings.Contains(err.Error(), "only [board] people") {
		t.Errorf("a Foreman user outside [board] people deployed: %v", err)
	}
	if after := w.files(); after[string(id)+".json"] != before[string(id)+".json"] {
		t.Error("a refused deploy saved something")
	}
	if n := w.wakes.count() - wakes; n != 0 {
		t.Errorf("%d wakes", n)
	}
	if _, err := answerDeploy(w, id, "drop", "robin"); err != nil {
		t.Errorf("robin's drop: %v", err)
	}
	got := w.load(id)
	m, _ := got.History[len(got.History)-1].Data.(ticket.Moved)
	if got.Column != "rejected" || m.By != "robin" {
		t.Errorf("after robin's drop: %s by %q", got.Column, m.By)
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
	if _, err := w.svc.HoldDeploys(ctx, true, "", "robin"); err != nil {
		t.Fatalf("robin's hold: %v", err)
	}
	if h, held, _ := deployrec.ReadHold(path); !held || h.By != "robin" {
		t.Errorf("robin's hold: %+v", h)
	}
	if _, err := w.svc.HoldDeploys(ctx, true, "bad\x00note", ""); err == nil {
		t.Error("a note with control characters")
	}
}
