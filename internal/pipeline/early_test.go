package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const doneWork = `{"status": "done", "summary": "did it", "category": "fix"}`

// localWorkDone is a ticket whose local worker has just left report on
// bot/x, one commit ahead of main; head is that commit.
func localWorkDone(h *harness, report string) (id ticket.ID, head string) {
	h.t.Helper()
	h.repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
	id = localAgent(h, "work", "working", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.local.reports["t0001-w1"] = []byte(report)
	return id, h.repo.Git("rev-parse", "bot/x")
}

// A worker that leaves a head for review starts Gate 1's checks of that
// head at once, beside the review; the ticket itself goes on as before.
func TestWorkThatLeavesAHeadStartsTheEarlyChecks(t *testing.T) {
	h := newHarness(t)
	id, head := localWorkDone(h, doneWork)
	h.tick()
	got := h.load(id)
	if moveReason(firstMoveFrom(got, "working")) != model.ReasonWorkDone {
		t.Fatalf("column %s, %+v", got.Column, lastMove(got))
	}
	if len(h.early.started) != 1 || h.early.started[0] != (earlyStart{id, head}) {
		t.Fatalf("early checks started %+v, want %s at %s", h.early.started, id, head)
	}
	if h.calls.count("gate.run") != 0 {
		t.Error("the tick ran Gate 1 itself")
	}
}

// A helper's worker: its branch comes back in the bundle, and the early
// checks are of the head it brought.
func TestARemoteWorkersHeadStartsTheEarlyChecks(t *testing.T) {
	h := newHarness(t)
	other := filepath.Join(t.TempDir(), "clone")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, h.env.Environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(t.TempDir(), "clone", "-q", h.repo.Dir, other)
	run(other, "checkout", "-q", "-b", "bot/x")
	run(other, "-c", "user.name=t", "-c", "user.email=t@e", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "fix")
	run(other, "bundle", "create", "-q", "out.bundle", "refs/heads/bot/x")
	id := remoteAgent(h, "work", "working", "bot/x")
	got := collected(t, h, id, map[string][]byte{"report.json": []byte(doneWork),
		"out.bundle": readFile(t, filepath.Join(other, "out.bundle")), "agent.log": []byte("log")})
	if got.Column == "needs_human" {
		t.Fatalf("column %s, %+v", got.Column, lastMove(got))
	}
	head := h.repo.Git("rev-parse", "bot/x")
	if len(h.early.started) != 1 || h.early.started[0] != (earlyStart{id, head}) {
		t.Fatalf("early checks started %+v, want %s at %s", h.early.started, id, head)
	}
}

// No early checks for a run that leaves nothing to gate, for another
// stage, for a paused project (nothing of it is gated until it is on
// again), for a board project (never gated), or for a heavy gate, which
// waits for an idle host and would not fit beside the review.
func TestNoEarlyChecksWithoutAHeadToGate(t *testing.T) {
	for _, c := range []struct {
		name   string
		config string
		report string
		stage  string
	}{
		{name: "blocked", report: `{"status": "blocked", "summary": "cannot"}`},
		{name: "review", report: `{"verdict": "pass", "runtime_behaviour": "no", "summary": "fine"}`, stage: "review"},
		{name: "paused", report: doneWork, config: strings.Replace(defaultConfig, "[projects.p]\n", "[projects.p]\nenabled = false\n", 1)},
		{name: "heavy", report: doneWork, config: strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\ngate_memory = \"1800m\"\n", 1)},
		{name: "board", report: doneWork, config: strings.Replace(defaultConfig, "[projects.p]\n", "[projects.p]\nmode = \"board\"\n", 1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			if c.config != "" {
				h.config(c.config)
			}
			if c.stage == "" {
				localWorkDone(h, c.report)
			} else {
				h.repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
				head := h.repo.Git("rev-parse", "bot/x")
				localAgent(h, c.stage, c.stage, func(t *ticket.Ticket) {
					t.Branch = ticket.Some("bot/x")
					t.HeadBeforeReview = ticket.Some(head)
				})
				h.local.reports["t0001-r1"] = []byte(c.report)
			}
			h.tick()
			if len(h.early.started) != 0 {
				t.Errorf("early checks started: %+v", h.early.started)
			}
		})
	}
}

// Early checks take one of this host's slots, as a gate does: they do not
// start while every slot is taken, nor when a pull host prefers gates
// (the gate would run there, and the checks here for nothing).
func TestEarlyChecksNeedAFreeSlotAndALocalGate(t *testing.T) {
	h := newHarness(t)
	h.local.running["a"], h.local.running["b"] = true, true
	id, _ := localWorkDone(h, doneWork)
	h.tick()
	if len(h.early.started) != 0 {
		t.Errorf("started with every slot taken: %+v", h.early.started)
	}
	if !h.log.has(string(id) + ": not checked early: no free slot here") {
		t.Errorf("log %q", h.log.lines)
	}

	h = newHarness(t)
	h.config(defaultConfig + "\n[hosts.desk]\ntransport = \"pull\"\nslots = 1\nprefer = [\"gate\"]\n")
	localWorkDone(h, doneWork)
	h.tick()
	if len(h.early.started) != 0 {
		t.Errorf("started for a gate a pull host takes: %+v", h.early.started)
	}
}

// Early checks that run count as one of this host's slots: with one agent
// running and max_agents 2, no new agent starts here beside them.
func TestRunningEarlyChecksTakeASlot(t *testing.T) {
	h := newHarness(t)
	h.local.running["a"] = true
	h.early.running = &earlyStart{"T0099", strings.Repeat("a", 40)}
	work := h.file("p", "w", "ready", nil)
	h.tick()
	if len(h.local.started) != 0 || h.load(work).Agent.Present() {
		t.Fatalf("an agent started beside the early checks: %+v", h.local.started)
	}
	h.early.running = nil
	h.tick()
	if len(h.local.started) != 1 {
		t.Fatalf("started %+v once the early checks were done", h.local.started)
	}
}

// Early checks started in this tick take their slot in the same tick, so
// nothing starts here beside them: with one other agent running and
// max_agents 2, the review of the ticket they check goes to the helper.
func TestEarlyChecksStartedThisTickTakeASlot(t *testing.T) {
	h := newHarness(t)
	h.early.marks = true
	h.local.running["a"] = true
	id, _ := localWorkDone(h, doneWork)
	h.tick()
	if len(h.early.started) != 1 {
		t.Fatalf("early checks started %+v", h.early.started)
	}
	if a := h.load(id).Agent; len(h.local.started) != 0 || a.V.Host == "local" {
		t.Errorf("started here beside the early checks: %+v, the review %+v", h.local.started, a)
	}
}

// The gate of a head whose early checks run now waits for their result
// (the ticket is not even saved) rather than run the same checks beside
// them; the gate of another head, or of another ticket, does not wait.
func TestTheGateWaitsForEarlyChecksOfItsHead(t *testing.T) {
	for _, c := range []struct {
		name   string
		ticket ticket.ID
		head   func(head string) string
		waits  bool
	}{
		{name: "this head", ticket: "T0001", head: func(h string) string { return h }, waits: true},
		{name: "an older head", ticket: "T0001", head: func(string) string { return strings.Repeat("a", 40) }},
		{name: "another ticket", ticket: "T0099", head: func(h string) string { return h }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.gate.result = passedGate()
			h.repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
			head := h.repo.Git("rev-parse", "bot/x")
			id := h.file("p", "t", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
			h.early.running = &earlyStart{c.ticket, c.head(head)}
			before := h.raw(id)
			h.tick()
			if c.waits {
				if len(h.gate.ran) != 0 || h.calls.count("gate.kept") != 0 || h.raw(id) != before {
					t.Fatalf("the gate did not wait: ran %v", h.gate.ran)
				}
				if !h.log.has(string(id) + ": the gate waits for the early checks of " + head[:12]) {
					t.Errorf("log %q", h.log.lines)
				}
				h.early.running = nil
				h.tick()
			}
			if len(h.gate.ran) != 1 || h.load(id).Column != "ready_for_pr" {
				t.Fatalf("ran %v, column %s", h.gate.ran, h.load(id).Column)
			}
		})
	}
}

// Early checks that cannot start (no user bus) are only a line in the
// log: the ticket goes to review as before, and its checks run at the gate.
func TestEarlyChecksThatCannotStartAreLogged(t *testing.T) {
	h := newHarness(t)
	h.early.err = errBoom
	id, _ := localWorkDone(h, doneWork)
	h.tick()
	if got := h.load(id); got.Column == "needs_human" || len(events(got, "error")) != 0 {
		t.Fatalf("column %s, errors %+v", got.Column, events(got, "error"))
	}
	if !h.log.has(string(id) + ": early checks: " + errBoom.Error()) {
		t.Errorf("log %q", h.log.lines)
	}
}

// At the gate a kept result whose whole key matches is used: Gate 1
// decides from it (files, walls, hygiene, the move, the audit) and runs no
// checks, so it needs no free slot. With none, Gate 1 runs as before.
func TestTheGateUsesTheEarlyChecks(t *testing.T) {
	h := newHarness(t)
	h.gate.result = passedGate()
	h.gate.kept = &gate.Remote{Head: "abc", Base: "def", ChecksPass: true, Output: "early output"}
	h.local.running["a"], h.local.running["b"] = true, true // every slot taken
	id := h.file("p", "t", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.tick()
	got := h.load(id)
	if got.Column != "ready_for_pr" || len(h.gate.ran) != 0 || len(h.gate.decided) != 1 || h.gate.decided[0].Head != "abc" {
		t.Fatalf("column %s, ran %v, decided %+v", got.Column, h.gate.ran, h.gate.decided)
	}
	if got.Gate.V.Host != "" || strings.Contains(lastMove(got).Detail, "checks on") {
		t.Errorf("the early checks named a host: %+v", lastMove(got))
	}
	if _, err := os.Stat(filepath.Join(h.paths.Logs, string(id), "audit.md")); err != nil {
		t.Errorf("no audit: %v", err)
	}

	h = newHarness(t)
	h.gate.result = passedGate()
	id = h.file("p", "t", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.tick()
	if len(h.gate.ran) != 1 || len(h.gate.decided) != 0 || h.load(id).Column != "ready_for_pr" {
		t.Fatalf("no kept result: ran %v, decided %+v", h.gate.ran, h.gate.decided)
	}
}

// A kept result that cannot be checked (git fails) is not used, and the
// gate runs as it would have.
func TestAKeptResultThatCannotBeCheckedIsNotUsed(t *testing.T) {
	h := newHarness(t)
	h.gate.result = passedGate()
	h.gate.keptErr = errBoom
	id := h.file("p", "t", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.tick()
	if len(h.gate.ran) != 1 || len(h.gate.decided) != 0 || h.load(id).Column != "ready_for_pr" {
		t.Fatalf("ran %v, decided %+v", h.gate.ran, h.gate.decided)
	}
	if !h.log.has(string(id) + ": early checks: " + errBoom.Error()) {
		t.Errorf("log %q", h.log.lines)
	}
}

// firstMoveFrom is the ticket's first move out of column.
func firstMoveFrom(t *ticket.Ticket, column string) ticket.Entry {
	for _, e := range t.History {
		if e.Event == "move" && strings.HasPrefix(e.Detail, column+" -> ") {
			return e
		}
	}
	return ticket.Entry{}
}
