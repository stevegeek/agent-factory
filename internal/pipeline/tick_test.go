package pipeline

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// port: TF WakeTest.test_a_wake_during_a_tick_makes_it_go_round_once_more
func TestAWakeDuringATickMakesItGoRoundOnceMore(t *testing.T) {
	h := newHarness(t)
	held, err := h.board.TickLock(context.Background(), store.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	h.tick() // another tick holds the lock: this one only leaves the flag
	if !h.board.AgainSet() {
		t.Fatal("a tick that found the lock taken left no .tick.again")
	}
	if n := len(h.calls.names()); n != 0 {
		t.Fatalf("a tick that did not get the lock still ran: %v", h.calls.names())
	}
	if !h.log.has("tick already running; it goes round once more") {
		t.Fatalf("log: %v", h.log.lines)
	}
	_ = held.Unlock()

	// a reply arrives during the first pass: one more pass, then it stops
	h.local.onList = func(n int) {
		if n == 1 {
			if err := h.board.Again(); err != nil {
				t.Error(err)
			}
		}
	}
	h.tick()
	if got := h.local.lists; got != 4 { // two scion lists per pass
		t.Fatalf("passes: %d scion lists, want 4 (two passes)", got)
	}
	if h.board.AgainSet() {
		t.Fatal(".tick.again left behind")
	}
	if h.doors.watches != 1 {
		t.Fatalf("watches ran %d times, want once per tick", h.doors.watches)
	}
}

// the 3-pass cap.
func TestATickGoesRoundAtMostThreeTimes(t *testing.T) {
	h := newHarness(t)
	h.local.onList = func(int) { _ = h.board.Again() } // wakes keep arriving
	h.tick()
	if got := h.local.lists; got != 6 {
		t.Fatalf("%d scion lists, want 6 (three passes)", got)
	}
}

// the config is read again between passes (a deploy may land
// while a pass runs).
func TestTheConfigIsReadAgainBetweenPasses(t *testing.T) {
	h := newHarness(t)
	paused := defaultConfig + "\n[projects.q]\npath = \"{repo}\"\nbase = \"main\"\nenabled = false\n"
	h.config(paused)
	id := h.file("q", "Fix it", "ready", nil)
	h.local.onList = func(n int) {
		if n == 1 {
			h.config(defaultConfig + "\n[projects.q]\npath = \"{repo}\"\nbase = \"main\"\n")
			_ = h.board.Again()
		}
	}
	h.tick()
	if len(h.local.started) != 1 || h.local.started[0].Ticket.ID != id {
		t.Fatalf("started %v, want %s on the second pass", h.local.started, id)
	}

	// a config that no longer parses keeps the last good one
	h2 := newHarness(t)
	h2.local.onList = func(n int) {
		if n == 1 {
			h2.config("max_agents = [\n")
			_ = h2.board.Again()
		}
	}
	h2.tick()
	if h2.local.lists != 4 {
		t.Fatalf("second pass did not run on the old config: %d lists", h2.local.lists)
	}
}

func TestABrokenConfigStopsTheTick(t *testing.T) {
	h := newHarness(t)
	h.config("max_agents = [\n")
	if err := Tick(context.Background(), h.deps()); err == nil {
		t.Fatal("a tick ran without a config")
	}
}

func TestAFailingWatchIsLoggedAndTheTickGoesOn(t *testing.T) {
	h := newHarness(t)
	h.doors.watchErr = errors.New("output is not text")
	h.tick()
	if !h.log.has("watches: output is not text") {
		t.Fatalf("log: %v", h.log.lines)
	}
	if h.local.lists != 2 {
		t.Fatal("the pass did not run after a failed watch")
	}
}

// the tick runs under its own deadline, below the unit's 45 minutes,
// and the GitHub pass sees it (prflow defers a merge that would not fit).
func TestTheTickRunsUnderItsDeadline(t *testing.T) {
	h := newHarness(t)
	h.config(defaultConfig + "\n[github]\ninstallation_id = \"1\"\n")
	h.tick()
	if len(h.github.deadlines) != 1 {
		t.Fatalf("github pass ran %d times", len(h.github.deadlines))
	}
	if d := h.github.deadlines[0]; d > DefaultDeadline || d < DefaultDeadline-time.Minute {
		t.Fatalf("the GitHub pass's deadline is %v away, want about %v", d, DefaultDeadline)
	}
	if DefaultDeadline >= 45*time.Minute || DefaultReserve != 32*time.Minute {
		t.Fatalf("deadline %v, reserve %v", DefaultDeadline, DefaultReserve)
	}
}

// a ticket file that cannot be read is passed over, logged
// and noticed, instead of crashing the tick.
func TestACorruptTicketIsPassedOverAndNoticed(t *testing.T) {
	h := newHarness(t)
	good := h.file("p", "Fix it", "ready", nil)
	if err := os.WriteFile(h.paths.TicketFile("T0099"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if len(h.local.started) != 1 || h.local.started[0].Ticket.ID != good {
		t.Fatalf("the good ticket did not start: %v", h.local.started)
	}
	if len(h.doors.notices) != 1 || !strings.Contains(h.doors.notices[0], "T0099") || !strings.Contains(h.doors.notices[0], "cannot be read") {
		t.Fatalf("notices: %v", h.doors.notices)
	}
	h.tick()
	if len(h.doors.notices) != 1 {
		t.Fatalf("noticed twice: %v", h.doors.notices)
	}
}

// port: TF RobustnessTest.test_a_ticket_left_working_without_an_agent_comes_to_your_desk
func TestATicketLeftWorkingWithoutAnAgentComesToYourDesk(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "x", "working", nil)
	h.tick()
	got := h.load(id)
	if got.Column != string(model.ColNeedsHuman) || got.Ask.V.Kind != "unstick" || got.Ask.V.RetryColumn.V != "ready" {
		t.Fatalf("column %s ask %+v", got.Column, got.Ask.V)
	}
	if moveReason(lastMove(got)) != model.ReasonAgentLost {
		t.Fatalf("reason %q", moveReason(lastMove(got)))
	}
	if !strings.Contains(lastMove(got).Detail, "left in Working with no agent") {
		t.Fatalf("detail %q", lastMove(got).Detail)
	}
}

// helper for tests that need a ticket's tree entry.
func histKeys(t *ticket.Ticket, i int) map[string]string {
	arr, _ := t.Tree().Get("history").Array()
	if i < 0 {
		i += len(arr)
	}
	out := map[string]string{}
	obj, _ := arr[i].Object()
	for _, m := range obj {
		if s, ok := m.Value.Str(); ok {
			out[m.Key] = s
		} else if m.Value.IsNull() {
			out[m.Key] = "null"
		}
	}
	return out
}

// fakeDeployer records each Drain and the tickets it saw.
type fakeDeployer struct {
	drains int
	seen   []ticket.ID
	err    error
}

func (f *fakeDeployer) Drain(_ context.Context, _ *config.Config, ts []*ticket.Ticket) error {
	f.drains++
	for _, t := range ts {
		f.seen = append(f.seen, t.ID)
	}
	return f.err
}

func TestPassDrainsDeploys(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "Fix it", "backlog", nil)
	dep := &fakeDeployer{}
	d := h.deps()
	d.Deploys = dep
	if err := Tick(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if dep.drains != 1 || len(dep.seen) != 1 || dep.seen[0] != id {
		t.Fatalf("drains %d, saw %v", dep.drains, dep.seen)
	}
}

// A failed GitHub pass (an outage) still drains: the maintainer's Deploy
// it and an interrupted deploy's notice do not wait for GitHub.
func TestPassDrainsWhenTheGitHubPassFails(t *testing.T) {
	h := newHarness(t)
	h.github.passErr = errors.New("github is down")
	dep := &fakeDeployer{}
	d := h.deps()
	d.Deploys = dep
	if err := Tick(context.Background(), d); err == nil || !strings.Contains(err.Error(), "github is down") {
		t.Fatalf("err %v", err)
	}
	if dep.drains != 1 {
		t.Fatalf("drains %d", dep.drains)
	}
}

func TestPassGoesOnWhenDrainFails(t *testing.T) {
	h := newHarness(t)
	dep := &fakeDeployer{err: errors.New("no user bus: run factory deploy run")}
	d := h.deps()
	d.Deploys = dep
	if err := Tick(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !h.log.has("deploys: no user bus: run factory deploy run") {
		t.Fatalf("log: %v", h.log.lines)
	}
	if h.local.lists != 2 {
		t.Fatal("the pass stopped at the failed drain")
	}
}

// fakeOperator records each Settle, and the drains before it.
type fakeOperator struct {
	settles      int
	drainsBefore int
	dep          *fakeDeployer
	seen         []ticket.ID
	err          error
}

func (f *fakeOperator) Settle(_ context.Context, _ *config.Config, ts []*ticket.Ticket) error {
	f.settles++
	if f.dep != nil {
		f.drainsBefore = f.dep.drains
	}
	for _, t := range ts {
		f.seen = append(f.seen, t.ID)
	}
	return f.err
}

func TestPassSettlesOperatorAfterDrain(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "Fix it", "backlog", nil)
	dep := &fakeDeployer{}
	op := &fakeOperator{dep: dep}
	d := h.deps()
	d.Deploys, d.Operator = dep, op
	if err := Tick(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if op.settles != 1 || op.drainsBefore != 1 || len(op.seen) != 1 || op.seen[0] != id {
		t.Fatalf("settles %d after %d drains, saw %v", op.settles, op.drainsBefore, op.seen)
	}
	// a failed GitHub pass still settles: the items record their own errors
	h.github.passErr = errors.New("github is down")
	if err := Tick(context.Background(), d); err == nil {
		t.Fatal("no error")
	}
	if op.settles != 2 {
		t.Fatalf("settles %d", op.settles)
	}
}

func TestPassGoesOnWhenSettleFails(t *testing.T) {
	h := newHarness(t)
	d := h.deps()
	d.Operator = &fakeOperator{err: errors.New("board: disk full")}
	if err := Tick(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !h.log.has("operator: board: disk full") {
		t.Fatalf("log: %v", h.log.lines)
	}
	if h.local.lists != 2 {
		t.Fatal("the pass stopped at the failed settle")
	}
}
