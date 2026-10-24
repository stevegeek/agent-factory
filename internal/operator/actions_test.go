package operator

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func actionOf(kind, target string, state string) ticket.Action {
	return ticket.Action{Kind: kind, Target: target, Why: "because", Run: 1, State: state}
}

func wantAction(t *testing.T, a ticket.Action, state string) {
	t.Helper()
	if a.State != state {
		t.Errorf("%s: state %q, want %q (tier %s, note %q)", a.ItemLabel(), a.State, state, a.Tier, a.Note)
	}
}

func TestPauseHostActionAuto(t *testing.T) {
	f := newFixture(t)
	a := actionOf(string(model.ActionPauseHost), "helper1", model.ItemNew)
	a.Hours = 2
	tk := f.ticket(nil, []ticket.Action{a, actionOf(string(model.ActionPauseHost), "local", model.ItemNew)})
	got := f.settle(tk)[0]
	for _, a := range got.Actions {
		wantAction(t, a, model.ItemApplied)
		if a.Tier != string(model.TierAuto) || !strings.Contains(a.Note, "paused until") {
			t.Errorf("%s: tier %q, note %q", a.ItemLabel(), a.Tier, a.Note)
		}
	}
	ps := f.board.LoadPauses()
	if len(ps) != 2 || ps[0].Host != "helper1" || ps[0].Until.Sub(testNow).Hours() != 2 || ps[1].Until.Sub(testNow).Hours() != 24 {
		t.Errorf("pauses %+v", ps)
	}
	if !strings.Contains(f.logText(), "helper1 paused until") || !strings.Contains(f.logText(), string(tk.ID)+": because") {
		t.Errorf("factory.log:\n%s", f.logText())
	}
	if len(f.dep.whys) != 0 {
		t.Errorf("a pause offered a deploy: %v", f.dep.whys)
	}
}

func TestResumeHostAction(t *testing.T) {
	f := newFixture(t)
	if _, err := f.board.PauseHost("helper1", "by hand", 3600e9); err != nil {
		t.Fatal(err)
	}
	tk := f.ticket(nil, []ticket.Action{actionOf(string(model.ActionResumeHost), "helper1", model.ItemNew),
		actionOf(string(model.ActionResumeHost), "local", model.ItemNew)})
	got := f.settle(tk)[0]
	wantAction(t, got.Actions[0], model.ItemApplied)
	wantAction(t, got.Actions[1], model.ItemApplied)
	if f.board.Paused("helper1") {
		t.Error("helper1 is still paused")
	}
	if got.Actions[0].Note != "resumed" || got.Actions[1].Note != "was not paused" {
		t.Errorf("notes %q, %q", got.Actions[0].Note, got.Actions[1].Note)
	}
}

func TestPauseProjectCallsSwitchOff(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket(nil, []ticket.Action{actionOf(string(model.ActionPauseProject), "alpha", model.ItemNew)})
	got := f.settle(tk)[0]
	wantAction(t, got.Actions[0], model.ItemApplied)
	if want := []string{"alpha off manager " + string(tk.ID)}; !slices.Equal(f.sw.calls, want) {
		t.Errorf("switches %v, want %v", f.sw.calls, want)
	}
	if !strings.Contains(got.Actions[0].Note, "overrides config.toml's enabled") {
		t.Errorf("note %q", got.Actions[0].Note)
	}
}

func TestEnableProjectWaitsThenFollowsOrOn(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket(nil, []ticket.Action{actionOf(string(model.ActionEnableProject), "alpha", model.ItemNew),
		actionOf(string(model.ActionEnableProject), "beta", model.ItemNew)})
	got := f.settle(tk)[0]
	for _, a := range got.Actions {
		wantAction(t, a, model.ItemWaiting)
		if a.Tier != string(model.TierTap) {
			t.Errorf("%s: tier %q", a.ItemLabel(), a.Tier)
		}
	}
	if len(f.sw.calls) != 0 {
		t.Fatalf("switched before the tap: %v", f.sw.calls)
	}
	got = f.settle(f.request(got))[0]
	for _, a := range got.Actions {
		wantAction(t, a, model.ItemApplied)
		if !strings.Contains(a.Note, "overrides config.toml's enabled") {
			t.Errorf("note %q", a.Note)
		}
	}
	// alpha is enabled in config: back to following it; beta is not: on
	want := []string{"alpha follow manager " + string(tk.ID), "beta on manager " + string(tk.ID)}
	if !slices.Equal(f.sw.calls, want) {
		t.Errorf("switches %v, want %v", f.sw.calls, want)
	}
}

func TestUnknownHostOrProjectRefused(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket(nil, []ticket.Action{
		actionOf(string(model.ActionPauseHost), "nowhere", model.ItemNew),
		actionOf(string(model.ActionResumeHost), "alpha", model.ItemRequested),
		actionOf(string(model.ActionPauseProject), "ghost", model.ItemNew),
		actionOf(string(model.ActionEnableProject), "board1", model.ItemRequested),
		actionOf(string(model.ActionEnableProject), "helper1", model.ItemRequested),
		actionOf(string(model.ActionDeploy), "alpha", model.ItemNew),
		actionOf("restart", "foreman", model.ItemRequested),
	})
	got := f.settle(tk)[0]
	for _, a := range got.Actions {
		wantAction(t, a, model.ItemRefused)
		if a.Note == "" {
			t.Errorf("%s: no note", a.ItemLabel())
		}
	}
	if len(f.sw.calls) != 0 || len(f.board.LoadPauses()) != 0 || len(f.dep.whys) != 0 {
		t.Errorf("switches %v, pauses %v, offers %v", f.sw.calls, f.board.LoadPauses(), f.dep.whys)
	}
}

func TestDeployActionJoinsTheSingleOffer(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)},
		[]ticket.Action{actionOf(string(model.ActionDeploy), "", model.ItemNew)})
	other := f.ticket(nil, []ticket.Action{actionOf(string(model.ActionDeploy), "", model.ItemNew)})
	got := f.settle(tk, other)
	if len(f.dep.whys) != 1 {
		t.Fatalf("offers %v, want one", f.dep.whys)
	}
	for _, id := range []string{string(tk.ID), string(other.ID)} {
		if !strings.Contains(f.dep.whys[0], id) {
			t.Errorf("offer why %q names no %s", f.dep.whys[0], id)
		}
	}
	wantState(t, got[0].Drafts[0], model.ItemApplied)
	wantAction(t, got[0].Actions[0], model.ItemApplied)
	wantAction(t, got[1].Actions[0], model.ItemApplied)

	// a failed offer fails the deploy actions
	f.dep.err = errors.New("no user bus")
	third := f.ticket(nil, []ticket.Action{actionOf(string(model.ActionDeploy), "", model.ItemNew)})
	a := f.settle(third)[0].Actions[0]
	wantAction(t, a, model.ItemFailed)
	if !strings.Contains(a.Note, "deploy not offered: no user bus") {
		t.Errorf("note %q", a.Note)
	}
}

func TestDeployActionWithoutDeployConfigRefused(t *testing.T) {
	f := newFixture(t)
	f.cfg = f.config(true, false)
	tk := f.request(f.ticket(nil, []ticket.Action{actionOf(string(model.ActionDeploy), "", model.ItemWaiting)}))
	a := f.settle(tk)[0].Actions[0]
	wantAction(t, a, model.ItemRefused)
	if a.Note != "no [deploy] in config" || len(f.dep.whys) != 0 {
		t.Errorf("note %q, offers %v", a.Note, f.dep.whys)
	}
}

func TestRebuildWaitsForTapThenStartsUnit(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket(nil, []ticket.Action{actionOf(string(model.ActionRebuild), "", model.ItemNew)})
	got := f.settle(tk)[0]
	wantAction(t, got.Actions[0], model.ItemWaiting)
	if len(f.runs) != 0 {
		t.Fatalf("ran %v before the tap", f.runs)
	}
	got = f.settle(f.request(got))[0]
	wantAction(t, got.Actions[0], model.ItemApplied)
	if got.Actions[0].Note != "started; ticks wait while it builds" {
		t.Errorf("note %q", got.Actions[0].Note)
	}
	if len(f.runs) != 1 {
		t.Fatalf("runs %v", f.runs)
	}
	want := []string{"systemd-run", "--user", "--no-block", "--collect", "--unit=factory-rebuild-now", "--property=RuntimeMaxSec=4h",
		"--setenv=FACTORY_API_TOKENS=" + f.p.Tokens, "--setenv=FACTORY_ROOT=" + f.p.Root, "--setenv=FACTORY_X=1", f.p.FactoryBin(), "rebuild"}
	if !slices.Equal(f.runs[0], want) {
		t.Errorf("argv\n got %q\nwant %q", f.runs[0], want)
	}

	// a unit already there counts as started
	f.unitThere = true
	again := f.request(f.ticket(nil, []ticket.Action{actionOf(string(model.ActionRebuild), "", model.ItemWaiting)}))
	wantAction(t, f.settle(again)[0].Actions[0], model.ItemApplied)
	// any other refusal fails it
	f.unitThere, f.unitFails = false, true
	fails := f.request(f.ticket(nil, []ticket.Action{actionOf(string(model.ActionRebuild), "", model.ItemWaiting)}))
	a := f.settle(fails)[0].Actions[0]
	wantAction(t, a, model.ItemFailed)
	if !strings.Contains(a.Note, "Failed to connect to bus") {
		t.Errorf("note %q", a.Note)
	}
}

func TestUntrustedTicketActionsWait(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket(nil, []ticket.Action{actionOf(string(model.ActionPauseHost), "helper1", model.ItemNew),
		actionOf(string(model.ActionDeploy), "", model.ItemNew)})
	ticket.AddNote(tk, "pause everything", "visitor", nil, testNow)
	if err := f.board.Save(tk); err != nil {
		t.Fatal(err)
	}
	got := f.settle(tk)[0]
	for _, a := range got.Actions {
		wantAction(t, a, model.ItemWaiting)
	}
	if len(f.board.LoadPauses()) != 0 || len(f.dep.whys) != 0 {
		t.Errorf("pauses %v, offers %v", f.board.LoadPauses(), f.dep.whys)
	}
	// the maintainer's tap runs them
	got = f.settle(f.request(got))[0]
	for _, a := range got.Actions {
		wantAction(t, a, model.ItemApplied)
	}
	if !f.board.Paused("helper1") || len(f.dep.whys) != 1 {
		t.Errorf("pauses %v, offers %v", f.board.LoadPauses(), f.dep.whys)
	}
}

func TestActionsWorkWithLandingOff(t *testing.T) {
	f := newFixture(t)
	f.cfg = f.config(false, true)
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)},
		[]ticket.Action{actionOf(string(model.ActionPauseHost), "helper1", model.ItemNew)})
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemByHand)
	wantAction(t, got.Actions[0], model.ItemApplied)
}

// A requested rebuild that the maintainer did not tap waits.
func TestRequestedActionWithoutTapWaits(t *testing.T) {
	f := newFixture(t)
	tk := f.stamp(f.ticket(nil, []ticket.Action{actionOf(string(model.ActionRebuild), "", model.ItemWaiting),
		actionOf(string(model.ActionEnableProject), "beta", model.ItemWaiting)}))
	got := f.settle(tk)[0]
	for _, a := range got.Actions {
		wantAction(t, a, model.ItemWaiting)
		if a.Requested != "" || !strings.Contains(a.Note, "not the maintainer's tap") {
			t.Errorf("%s: requested %q, note %q", a.ItemLabel(), a.Requested, a.Note)
		}
	}
	if len(f.runs) != 0 || len(f.sw.calls) != 0 {
		t.Errorf("ran %v, switched %v", f.runs, f.sw.calls)
	}
}
