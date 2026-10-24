package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func failedGate() ticket.Gate {
	return ticket.Gate{ChecksPass: false, Files: []string{"README.md"}, ChangedLines: 3, OutsideAllowed: []string{"README.md"},
		RuntimeBehaviour: jsonx.Null(), Blockers: []string{"tests or lint failed"}}
}

func passedGate() ticket.Gate {
	return ticket.Gate{ChecksPass: true, Files: []string{"README.md"}, ChangedLines: 3, WithinSize: true,
		OutsideAllowed: []string{}, RuntimeBehaviour: jsonx.String("no"), ReasoningStated: true, AutoEligible: true,
		Blockers: []string{}, Head: "abc", Base: "def"}
}

// port: TF AskTest.test_gate_in_the_tick_moves_with_its_rule
func TestGateInTheTickMovesWithItsRule(t *testing.T) {
	h := newHarness(t)
	h.gate.result = failedGate()
	id := h.file("p", "t", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.tick()
	got := h.load(id)
	mv := lastMove(got)
	if got.Column != "needs_human" || mv.Rule != "gate: needs a maintainer: tests or lint failed" || moveReason(mv) != model.ReasonGateFailed {
		t.Fatalf("column %s move %+v", got.Column, mv)
	}
	if got.Ask.V.Kind != "unstick" || got.Ask.V.RetryColumn.V != "gate" {
		t.Fatalf("ask %+v", got.Ask.V)
	}
	if !got.Gate.Present() || got.Gate.V.ChecksPass {
		t.Fatalf("gate %+v", got.Gate)
	}
	audit, err := os.ReadFile(filepath.Join(h.paths.Logs, string(id), "audit.md"))
	if err != nil || !strings.Contains(string(audit), "Gate 1") {
		t.Fatalf("audit %q %v", audit, err)
	}
}

// a gate waits for a free slot here (it runs on this host).
func TestAGateWaitsForAFreeSlot(t *testing.T) {
	h := newHarness(t)
	h.gate.result = passedGate()
	h.local.running["a"], h.local.running["b"] = true, true
	id := h.file("p", "t", "gate", nil)
	before := h.raw(id)
	h.tick()
	if len(h.gate.ran) != 0 || h.raw(id) != before {
		t.Fatal("a gate ran with no free slot, or its ticket was saved")
	}
	delete(h.local.running, "b")
	h.tick()
	if len(h.gate.ran) != 1 || h.load(id).Column != "ready_for_pr" {
		t.Fatalf("ran %v column %s", h.gate.ran, h.load(id).Column)
	}
}

// A gate heavier than [gate] heavy_memory needs this host to itself. While
// an agent runs here it waits, and no new agent starts here meanwhile, so
// the host drains for it; on an idle host it runs, and what waits starts
// after it.
func TestAHeavyGateWaitsForAnIdleHost(t *testing.T) {
	h := newHarness(t)
	h.config(strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\ngate_memory = \"1800m\"\n", 1) + "\n[memory]\nenabled = false\n")
	h.gate.result = passedGate()
	h.local.running["a"] = true
	id := h.file("p", "t", "gate", nil)
	work := h.file("p", "w", "ready", nil)
	before := h.raw(id)
	h.tick()
	if len(h.gate.ran) != 0 || h.raw(id) != before {
		t.Fatal("a heavy gate ran beside an agent, or its ticket was saved")
	}
	if len(h.local.started) != 0 {
		t.Fatalf("an agent started here while a heavy gate waited: %+v", h.local.started)
	}
	if !h.log.has(string(id) + ": the gate waits for this host to be idle") {
		t.Fatalf("log %v", h.log.lines)
	}
	delete(h.local.running, "a")
	h.tick()
	if len(h.gate.ran) != 1 || h.load(id).Column != "ready_for_pr" {
		t.Fatalf("ran %v column %s", h.gate.ran, h.load(id).Column)
	}
	if len(h.local.started) != 1 || !h.load(work).Agent.Present() {
		t.Fatalf("started %+v", h.local.started)
	}

	// a gate under heavy_memory runs beside an agent, in a free slot
	h = newHarness(t)
	h.gate.result = passedGate()
	h.local.running["a"] = true
	h.file("p", "t", "gate", nil)
	h.tick()
	if len(h.gate.ran) != 1 {
		t.Fatalf("ran %v", h.gate.ran)
	}
}

const withGitHub = `max_agents = 2

[github]
installation_id = "1"
app_client_id = "Iv1"

[projects.p]
path = "{repo}"
base = "main"
github = "o/gem"
`

// a gate pass on an open PR updates it at once, so GitHub never
// shows a stale commit that could be approved.
func TestAGatePassOnAnOpenPRUpdatesItAtOnce(t *testing.T) {
	h := newHarness(t)
	h.config(withGitHub)
	h.gate.result = passedGate()
	rework := h.file("p", "t", "gate", func(t *ticket.Ticket) { t.PR = ticket.Some(ticket.PR{Number: 5, URL: "u"}) })
	fresh := h.file("p", "t", "gate", nil)
	h.tick()
	if len(h.github.opened) != 1 || h.github.opened[0] != rework {
		t.Fatalf("opened %v", h.github.opened)
	}
	if h.load(rework).Column != "pr_open" || h.load(fresh).Column != "ready_for_pr" {
		t.Fatalf("columns %s %s", h.load(rework).Column, h.load(fresh).Column)
	}
	if h.load(rework).PR.V.OpenRequested.Set {
		t.Fatal("the commit point's intent is left after the PR was updated")
	}
	// a failure to update is an event only
	h = newHarness(t)
	h.config(withGitHub)
	h.gate.result = passedGate()
	h.github.openErr = errBoom
	rework = h.file("p", "t", "gate", func(t *ticket.Ticket) { t.PR = ticket.Some(ticket.PR{Number: 5}) })
	h.tick()
	got := h.load(rework)
	if got.Column != "ready_for_pr" || len(events(got, "error")) != 1 || events(got, "error")[0].Detail != "github: no space left on device" {
		t.Fatalf("column %s errors %+v", got.Column, events(got, "error"))
	}
}

// the tick's deadline during a rework's push loses only the push:
// the gate's outcome is saved first, with an open intent, so the gate is
// not run again and the next tick's GitHub pass updates the PR.
func TestADeadlineDuringThePushKeepsTheGate(t *testing.T) {
	h := newHarness(t)
	h.config(withGitHub)
	h.gate.result = passedGate()
	rework := h.file("p", "t", "gate", func(t *ticket.Ticket) { t.PR = ticket.Some(ticket.PR{Number: 5, URL: "u"}) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.github.cut = cancel
	if err := Tick(ctx, h.deps()); err == nil {
		t.Fatal("the tick went on after its context ended")
	}
	got := h.load(rework)
	if got.Column != "ready_for_pr" || !got.Gate.Present() || !got.PR.V.OpenRequested.Present() {
		t.Fatalf("column %s, gate %v, open_requested %+v", got.Column, got.Gate.Present(), got.PR.V.OpenRequested)
	}
	if len(h.gate.ran) != 1 {
		t.Fatalf("gates run %d", len(h.gate.ran))
	}
}

// a gate does not start with too little of the tick's time left;
// the ticket is left as it is for the next tick.
func TestAGateIsDeferredUnderTheDeadline(t *testing.T) {
	h := newHarness(t)
	h.gate.result = passedGate()
	id := h.file("p", "t", "gate", nil)
	before := h.raw(id)
	d := h.deps()
	d.Deadline = 10 * time.Minute
	if err := Tick(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(h.gate.ran) != 0 || h.raw(id) != before {
		t.Fatal("a gate ran with too little time left, or the ticket was saved")
	}
	if !h.log.has(string(id) + ": the gate waits for the next tick") {
		t.Fatalf("log %v", h.log.lines)
	}
}

// A gate that cannot run is a dispatcher error.
func TestAGateThatCannotRunIsADispatcherError(t *testing.T) {
	h := newHarness(t)
	h.gate.err = errBoom
	id := h.file("p", "t", "gate", nil)
	h.tick()
	got := h.load(id)
	if got.Column != "needs_human" || moveReason(lastMove(got)) != model.ReasonDispatcherError || got.Ask.V.RetryColumn.V != "gate" {
		t.Fatalf("column %s move %+v ask %+v", got.Column, lastMove(got), got.Ask.V)
	}
}
