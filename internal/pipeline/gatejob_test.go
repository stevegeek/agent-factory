package pipeline

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// deskHost is a pull host taking gates: listed in prefer (every gate) or
// in stages (only a gate that would wait here).
func deskHost(list string) string {
	return "\n[hosts.desk]\ntransport = \"pull\"\nslots = 1\n" + list + " = [\"gate\"]\n"
}

// gateHarness is a harness with the pull host desk, and a ticket in the
// gate column on bot/x.
func gateHarness(t *testing.T, config string) (*harness, *fakeHelper, ticket.ID, string) {
	t.Helper()
	h := newHarness(t)
	desk := &fakeHelper{c: h.calls, name: "desk", state: jobproto.StateRunning}
	h.helpers["desk"] = desk
	h.config(config)
	h.gate.result = passedGate()
	h.repo.BranchWith("bot/x", map[string]string{"lib/thing.rb": "def thing; 2; end\n"})
	id := h.file("p", "t", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	return h, desk, id, h.repo.Git("rev-parse", "bot/x")
}

func doneGateJob(desk *fakeHelper, report, log string) {
	desk.mu.Lock()
	defer desk.mu.Unlock()
	desk.state = jobproto.StateDone
	desk.files = map[string][]byte{"report.json": []byte(report), "agent.log": []byte(log)}
}

// A gate goes to a pull host that prefers gates, as a job with the gated
// head, the checks and the heads recorded on the ticket; nothing runs here.
// Once done it is decided here from those heads, says where the checks ran,
// and the ticket moves on as a gate here would move it.
func TestAGateRunsOnAPullHostAndIsDecidedHere(t *testing.T) {
	h, desk, id, head := gateHarness(t, defaultConfig+deskHost("prefer"))
	h.tick()
	if len(h.gate.ran) != 0 || len(desk.started) != 1 {
		t.Fatalf("ran here %v, started there %v", h.gate.ran, desk.started)
	}
	j := desk.started[0]
	if j.Name != "t0001-g1" || j.Gate == nil || j.Gate.Head != head || j.Gate.Script != "set -e; true; true; true" {
		t.Fatalf("job %+v", j)
	}
	got := h.load(id)
	a := got.Agent.V
	main := h.repo.Git("rev-parse", "main")
	if got.Column != "gate" || a.Stage != "gate" || a.Host != "desk" || a.Starting.Present() || a.Gate.V.Head != head || a.Gate.V.Base != main {
		t.Fatalf("column %s agent %+v", got.Column, a)
	}
	if start := last(got); start.Event != "start" || start.Detail != "gate t0001-g1 on bot/x @desk" {
		t.Fatalf("start %+v", start)
	}

	h.tick() // still running: polled, nothing else
	if got := h.load(id); !got.Agent.Present() || len(desk.started) != 1 || len(h.gate.decided) != 0 {
		t.Fatalf("a running gate job moved on: %+v", got.Agent)
	}

	doneGateJob(desk, `{"status": "done", "gate_exit": 0}`, "installing\ntests ok\n")
	h.tick()
	got = h.load(id)
	if got.Column != "ready_for_pr" || got.Agent.Present() {
		t.Fatalf("column %s agent %+v", got.Column, got.Agent)
	}
	if len(h.gate.decided) != 1 {
		t.Fatalf("decided %+v", h.gate.decided)
	}
	in := h.gate.decided[0]
	if !in.ChecksPass || in.Head != head || in.Base != main || in.Host != "desk" || in.Output != "installing\ntests ok\n" {
		t.Fatalf("decided %+v", in)
	}
	if !strings.HasSuffix(lastMove(got).Detail, "auto-merge eligible (checks on desk)") || got.Gate.V.Host != "desk" {
		t.Fatalf("move %+v gate %+v", lastMove(got), got.Gate.V)
	}
	if len(events(got, "finished")) != 1 || len(desk.removed) != 1 || desk.removed[0] != "t0001-g1" {
		t.Fatalf("finished %v removed %v", events(got, "finished"), desk.removed)
	}
	if len(h.gate.ran) != 0 {
		t.Fatalf("the gate ran here too: %v", h.gate.ran)
	}
}

// Checks that fail on the pull host fail the gate, as here.
func TestAGateJobsFailedChecksFailTheGate(t *testing.T) {
	h, desk, id, _ := gateHarness(t, defaultConfig+deskHost("prefer"))
	h.tick()
	doneGateJob(desk, `{"status": "done", "gate_exit": 1}`, "1 failure\n")
	h.tick()
	got := h.load(id)
	mv := lastMove(got)
	if got.Column != "needs_human" || moveReason(mv) != model.ReasonGateFailed ||
		!strings.HasSuffix(mv.Detail, "tests or lint failed on a fresh checkout (checks on desk)") {
		t.Fatalf("column %s move %+v", got.Column, mv)
	}
}

// A gate job that is lost, or that reports it ran no checks, is never a
// failed gate: the ticket stays in the gate column and is gated again,
// here once maxGateJobs of its jobs were lost.
func TestALostGateJobGatesAgainThenHere(t *testing.T) {
	h, desk, id, _ := gateHarness(t, defaultConfig+deskHost("prefer"))
	h.tick()
	doneGateJob(desk, `{"status": "error", "summary": "gate.sh: could not clone the bundle"}`, "")
	h.tick()
	got := h.load(id)
	if got.Column != "gate" || len(h.gate.decided) != 0 {
		t.Fatalf("column %s decided %v", got.Column, h.gate.decided)
	}
	errs := events(got, "error")
	if len(errs) == 0 || !strings.Contains(errs[0].Detail, "gate job t0001-g1 on desk: it ran no checks: gate.sh: could not clone the bundle; the gate runs again") {
		t.Fatalf("errors %+v", errs)
	}
	desk.mu.Lock()
	desk.state = jobproto.StateMissing
	desk.mu.Unlock()
	for range 4 {
		h.tick()
	}
	if len(desk.started) != maxGateJobs || len(h.gate.ran) != 1 {
		t.Fatalf("started there %d, ran here %v", len(desk.started), h.gate.ran)
	}
	got = h.load(id)
	if got.Column != "ready_for_pr" || got.Gate.V.Host != "" {
		t.Fatalf("column %s gate %+v", got.Column, got.Gate.V)
	}
	if !strings.Contains(events(got, "error")[1].Detail, "gate job t0001-g2 on desk: it disappeared") {
		t.Fatalf("errors %+v", events(got, "error"))
	}
}

// A pull host with "gate" in stages takes only the gates that would wait
// here: a heavy one, not a light one on a free host.
func TestAPullHostTakesOnlyGatesThatWouldWaitHere(t *testing.T) {
	h, desk, id, _ := gateHarness(t, defaultConfig+deskHost("stages"))
	h.tick()
	if len(h.gate.ran) != 1 || len(desk.started) != 0 || h.load(id).Column != "ready_for_pr" {
		t.Fatalf("a light gate: ran here %v, started there %v", h.gate.ran, desk.started)
	}

	heavy := strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\ngate_memory = \"1800m\"\n", 1) + deskHost("stages")
	h, desk, id, _ = gateHarness(t, heavy)
	h.tick()
	if len(h.gate.ran) != 0 || len(desk.started) != 1 || h.load(id).Agent.V.Host != "desk" {
		t.Fatalf("a heavy gate: ran here %v, started there %v", h.gate.ran, desk.started)
	}
}

// A pull host that is away, or that refuses the job, leaves the gate here
// as before; a refusal is on the ticket.
func TestAGateStaysHereWhenThePullHostCannotTakeIt(t *testing.T) {
	h, desk, id, _ := gateHarness(t, defaultConfig+deskHost("prefer"))
	desk.away = true
	h.tick()
	if len(desk.started) != 0 || len(h.gate.ran) != 1 || h.load(id).Column != "ready_for_pr" {
		t.Fatalf("away: started there %v, ran here %v", desk.started, h.gate.ran)
	}

	h, desk, id, _ = gateHarness(t, defaultConfig+deskHost("prefer"))
	desk.startErr = remoteDown("desk")
	h.tick()
	got := h.load(id)
	if len(h.gate.ran) != 1 || got.Column != "ready_for_pr" || got.Agent.Present() {
		t.Fatalf("refused: ran here %v, column %s agent %+v", h.gate.ran, got.Column, got.Agent)
	}
	if errs := events(got, "error"); len(errs) != 1 || !strings.HasPrefix(errs[0].Detail, "desk: ") {
		t.Fatalf("errors %+v", errs)
	}
}

func TestGateExit(t *testing.T) {
	for _, c := range []struct {
		raw  string
		exit int
		ok   bool
		why  string
	}{
		{`{"status": "done", "gate_exit": 0}`, 0, true, ""},
		{`{"status": "done", "gate_exit": 2}`, 2, true, ""},
		{`{"status": "done"}`, 0, false, "no exit in the report"},
		{`{"status": "done", "gate_exit": "0"}`, 0, false, "no exit in the report"},
		{`{"status": "done", "gate_exit": 1.5}`, 0, false, "no exit in the report"},
		{`{"status": "error", "summary": "timed out on desk"}`, 0, false, "timed out on desk"},
		{`{"status": "blocked"}`, 0, false, `status "blocked"`},
		{`not json`, 0, false, "an unreadable report"},
	} {
		exit, ok, why := gateExit([]byte(c.raw))
		if exit != c.exit || ok != c.ok || why != c.why {
			t.Errorf("%s: %d %v %q", c.raw, exit, ok, why)
		}
	}
	if _, ok, why := gateExit(nil); ok || why != "no report" {
		t.Errorf("nil: %v %q", ok, why)
	}
}
