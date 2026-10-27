package pipeline

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// goConfig is defaultConfig with a project whose checks start go.
var goConfig = strings.Replace(strings.Replace(defaultConfig, "install = \"true\"\n", "install = \"go version\"\n", 1),
	"test = \"true\"\n", "test = \"go test ./...\"\n", 1)

// pullHost is a pull host called name with "gate" in prefer, and extra
// lines.
func pullHost(name, extra string) string {
	return "\n[hosts." + name + "]\ntransport = \"pull\"\nslots = 1\nprefer = [\"gate\"]\n" + extra
}

// A gate job whose host lacks a command (exit 127) is no gate result: an
// error event names the host and what it lacks, no ask opens, and the gate
// goes to another host that takes gates.
func TestAGateTheHostCannotRunGoesToAnotherHost(t *testing.T) {
	h, desk, id, _ := gateHarness(t, goConfig+pullHost("desk", "")+pullHost("hostx", ""))
	other := &fakeHelper{c: h.calls, name: "hostx", state: jobproto.StateRunning}
	h.helpers["hostx"] = other
	h.tick()
	if len(desk.started) != 1 {
		t.Fatalf("started on desk %v", desk.started)
	}
	doneGateJob(desk, `{"status": "done", "gate_exit": 127}`, "checks.sh: 1: go: not found\n")
	h.tick()
	got := h.load(id)
	if got.Column != "gate" || got.Ask.Present() || got.Gate.Present() || len(h.gate.decided) != 0 {
		t.Fatalf("column %s ask %+v gate %+v decided %v", got.Column, got.Ask, got.Gate, h.gate.decided)
	}
	errs := events(got, "error")
	if len(errs) != 1 {
		t.Fatalf("errors %+v", errs)
	}
	if x := errs[0].Data.(ticket.Errored); x.Host != "desk" || x.Missing != "go: not found" || errs[0].Detail != "desk cannot run the checks: go: not found" {
		t.Fatalf("error %+v %+v", errs[0], x)
	}
	if len(other.started) != 1 || len(h.gate.ran) != 0 {
		t.Fatalf("started on hostx %v, ran here %v", other.started, h.gate.ran)
	}
	if got := h.load(id); got.Agent.V.Host != "hostx" {
		t.Fatalf("agent %+v", got.Agent.V)
	}
	if len(desk.started) != 1 {
		t.Fatalf("desk was sent the gate again: %v", desk.started)
	}
}

// With no host left that can run the checks, the ask names each host and
// what it lacks, not failed tests or lint.
func TestNoHostLeftAsksWhichHostLacksWhat(t *testing.T) {
	h, desk, id, _ := gateHarness(t, goConfig+pullHost("desk", ""))
	h.gate.result = failedGate()
	h.gate.result.Output = "sh: 1: go: not found"
	h.tick()
	doneGateJob(desk, `{"status": "done", "gate_exit": 127}`, "checks.sh: 1: go: not found\n")
	h.tick() // the job is collected, and the gate runs on the master, which lacks go too
	got := h.load(id)
	if len(h.gate.ran) != 1 || got.Column != "needs_human" || got.Gate.Present() {
		t.Fatalf("ran here %v column %s gate %+v", h.gate.ran, got.Column, got.Gate)
	}
	mv := lastMove(got)
	want := "no host could run p's checks: desk: go: not found; the master: go: not found"
	if moveReason(mv) != model.ReasonGateFailed || !strings.Contains(mv.Detail, want) {
		t.Fatalf("move %+v", mv)
	}
	a := got.Ask.V
	if a.Kind != string(model.AskUnstick) || !strings.Contains(a.Why, want) || strings.Contains(a.Why, "tests or lint failed") {
		t.Fatalf("ask %+v", a)
	}
}

// Checks that fail on a host that has its tools still fail the gate, and
// the ask says where they ran, their exit and their last lines.
func TestFailedChecksShowTheirOutputInTheAsk(t *testing.T) {
	h, desk, id, _ := gateHarness(t, goConfig+pullHost("desk", ""))
	h.tick()
	doneGateJob(desk, `{"status": "done", "gate_exit": 1}`, "ok  \tp/a\t0.1s\n--- FAIL: TestThing (0.00s)\nFAIL\tp/b\t0.2s\n")
	h.tick()
	got := h.load(id)
	if got.Column != "needs_human" || !strings.Contains(lastMove(got).Detail, "tests or lint failed") {
		t.Fatalf("column %s move %+v", got.Column, lastMove(got))
	}
	if errs := events(got, "error"); len(errs) != 0 {
		t.Fatalf("errors %+v", errs)
	}
	g := got.Gate.V
	if e, _ := g.Exit.Get(); e != 1 || g.Output != "ok  \tp/a\t0.1s\n--- FAIL: TestThing (0.00s)\nFAIL\tp/b\t0.2s" {
		t.Fatalf("gate %+v", g)
	}
	why := got.Ask.V.Why
	if !strings.Contains(why, "Gate on desk: exit 1: ") || !strings.Contains(why, "`--- FAIL: TestThing (0.00s)`") {
		t.Fatalf("why %q", why)
	}
}

// A pull host whose not_kinds names the project's kind never takes its
// gate; one whose kinds names it does.
func TestAGateHostTakesOnlyTheKindsItAllows(t *testing.T) {
	h, desk, id, _ := gateHarness(t, defaultConfig+pullHost("desk", "not_kinds = [\"ruby-gem\"]\n"))
	h.tick()
	if len(desk.started) != 0 || len(h.gate.ran) != 1 || h.load(id).Column != "ready_for_pr" {
		t.Fatalf("not_kinds: started there %v, ran here %v", desk.started, h.gate.ran)
	}
	h, desk, _, _ = gateHarness(t, defaultConfig+pullHost("desk", "kinds = [\"ruby-gem\"]\n"))
	h.tick()
	if len(desk.started) != 1 || len(h.gate.ran) != 0 {
		t.Fatalf("kinds: started there %v, ran here %v", desk.started, h.gate.ran)
	}
}

// A ticket pinned to a host that does not take its project's kind is
// refused, with the reason, and never sent there.
func TestAPinToAHostThatExcludesTheKindIsRefused(t *testing.T) {
	h := newHarness(t)
	pi := &fakeHelper{c: h.calls, name: "pi", state: jobproto.StateRunning}
	h.helpers["pi"] = pi
	h.config(defaultConfig + "not_kinds = [\"ruby-gem\"]\n")
	id := h.file("p", "t", "ready", func(t *ticket.Ticket) { t.Host = ticket.Some("pi") })
	h.tick()
	got := h.load(id)
	if len(pi.started) != 0 || got.Column != "needs_human" || moveReason(lastMove(got)) != model.ReasonDispatcherError ||
		!strings.Contains(lastMove(got).Detail, "pinned to pi does not take ruby-gem projects") {
		t.Fatalf("started %v column %s move %+v", pi.started, got.Column, lastMove(got))
	}
}
