package pipeline

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// localAgent is a ticket whose agent runs here.
func localAgent(h *harness, stage, column string, change func(*ticket.Ticket)) ticket.ID {
	id := h.file("p", "t", column, func(t *ticket.Ticket) {
		agentOn(t, "t0001-"+string(stageLetter(stage))+"1", stage, "local", stamp(h.clock.Now()))
		if change != nil {
			change(t)
		}
	})
	return id
}

func stageLetter(stage string) byte {
	info, _ := model.StageInfoOf(model.Stage(stage))
	return info.Suffix
}

// port: TF LimitTest.test_a_helper_job_out_of_credit_goes_back_and_pauses_its_host
func TestAHelperJobOutOfCreditGoesBackAndPausesItsHost(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "x", "working", func(t *ticket.Ticket) {
		t.Kind = ticket.Some("task")
		agentOn(t, "t0001-k1", "task", "pi", stamp(h.clock.Now()))
	})
	got := collected(t, h, id, map[string][]byte{"agent.log": []byte(
		"pi: 400 invalid_request_error: Your credit balance is too low to access the Anthropic API.\npi exited 1\n")})
	var requeue ticket.Entry
	for _, e := range got.History {
		if moveReason(e) == model.ReasonUsageLimit {
			requeue = e
		}
	}
	if !strings.HasPrefix(requeue.Detail, "working -> ready: pi hit a usage limit (credit balance is too low); retried after ") {
		t.Fatalf("requeue %q", requeue.Detail)
	}
	if !h.board.Paused("pi") || h.board.Paused("local") {
		t.Fatal("pi is not paused, or this host is")
	}
	// a paused helper gets no new work; this host still does
	if len(h.helpers["pi"].started) != 0 || len(h.local.started) != 1 {
		t.Fatalf("pi %d, local %d", len(h.helpers["pi"].started), len(h.local.started))
	}
}

// The local limit path in the tick: a quiet agent whose screen ends in a
// limit is stopped, put back and this host paused.
func TestAQuietLocalAgentAtItsLimitIsRequeued(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "work", "working", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.local.running["t0001-w1"] = true
	h.local.screens["t0001-w1"] = "working…\n⎿  Claude usage limit reached. Your limit will reset at 5pm"
	h.tick()
	if got := h.load(id); !got.Agent.Present() {
		t.Fatal("an agent that is not quiet was stopped")
	}
	h = newHarness(t)
	id = localAgent(h, "work", "working", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.local.running["t0001-w1"] = true
	h.local.quiet["t0001-w1"] = true
	h.local.screens["t0001-w1"] = "working…\n⎿  Claude usage limit reached. Your limit will reset at 5pm"
	h.tick()
	got := h.load(id)
	if got.Agent.Present() || got.Column != "ready" || moveReason(lastMove(got)) != model.ReasonUsageLimit {
		t.Fatalf("agent %+v column %s move %+v", got.Agent, got.Column, lastMove(got))
	}
	if len(h.local.removed) != 1 || !h.local.removed[0].KeepBranch {
		t.Fatalf("removed %+v", h.local.removed)
	}
	if !h.board.Paused("local") || len(h.local.started) != 0 {
		t.Fatal("this host was not paused, or got new work")
	}
}

func TestAQuietAgentWithoutALimitKeepsRunning(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "work", "working", nil)
	h.local.running["t0001-w1"] = true
	h.local.quiet["t0001-w1"] = true
	h.local.screens["t0001-w1"] = "● Edit(lib/rate_limit.rb)\n  def rate_limit_exceeded?; end"
	h.tick()
	if got := h.load(id); !got.Agent.Present() || got.Column != "working" {
		t.Fatalf("agent %+v column %s", got.Agent, got.Column)
	}
}

// the local agent_lost path.
func TestALocalAgentThatDisappearedIsLost(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "triage", "triage", nil)
	h.tick()
	got := h.load(id)
	if got.Agent.Present() || got.Column != "needs_human" || moveReason(lastMove(got)) != model.ReasonAgentLost {
		t.Fatalf("agent %+v column %s move %+v", got.Agent, got.Column, lastMove(got))
	}
	want := "triage -> needs_human: agent t0001-t1 stopped: container ended without a report (seen " + stamp(h.clock.Now()) + ")"
	if lastMove(got).Detail != want {
		t.Fatalf("detail %q, want %q", lastMove(got).Detail, want)
	}
	if len(h.local.dropped) != 1 || len(h.local.removed) != 1 || !h.local.removed[0].KeepBranch {
		t.Fatalf("dropped %v removed %+v", h.local.dropped, h.local.removed)
	}
}

func TestAnAgentStoppedByADeployIsRetried(t *testing.T) {
	for name, finished := range map[string]time.Duration{"deploy still running": -1, "deploy ended after the start": 2 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			id := localAgent(h, "work", "working", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
			st := deployrec.State{Phase: "done", Commit: "0123456789ab", Started: stamp(h.clock.Now().Add(-30 * time.Second))}
			if finished >= 0 {
				st.Finished = stamp(h.clock.Now().Add(finished))
			}
			if err := deployrec.WriteState(h.paths.DeployState(), st); err != nil {
				t.Fatal(err)
			}
			h.clock.Add(3 * time.Minute)
			h.tick()
			got := h.load(id)
			if len(h.local.removed) != 1 {
				t.Fatalf("removed %+v", h.local.removed)
			}
			m, ok := moveFrom(got, "working")
			if !ok || m.To != "ready" || m.Reason != model.ReasonAgentLost {
				t.Fatalf("column %s moves %+v", got.Column, got.History)
			}
			if d := m.Text; !strings.Contains(d, "during the deploy of 0123456789ab; it is retried") {
				t.Fatalf("detail %q", d)
			}
		})
	}
}

// moveFrom is the ticket's first move out of column, with its detail.
func moveFrom(t *ticket.Ticket, column string) (ticket.Moved, bool) {
	for _, e := range t.History {
		var m ticket.Moved
		switch v := e.Data.(type) {
		case ticket.Moved:
			m = v
		case *ticket.Moved:
			m = *v
		default:
			continue
		}
		if m.From == column {
			m.Text = e.Detail
			return m, true
		}
	}
	return ticket.Moved{}, false
}

func TestAnAgentStoppedLongAfterADeployNeedsAPerson(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "triage", "triage", nil)
	st := deployrec.State{Phase: "done", Commit: "0123456789ab",
		Started: stamp(h.clock.Now().Add(-10 * time.Minute)), Finished: stamp(h.clock.Now().Add(-9 * time.Minute))}
	if err := deployrec.WriteState(h.paths.DeployState(), st); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := h.load(id); got.Column != "needs_human" {
		t.Fatalf("column %s", got.Column)
	}
}

func TestAnAgentThatOutlivedADeployAndStoppedLaterNeedsAPerson(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "triage", "triage", nil)
	st := deployrec.State{Phase: "done", Commit: "0123456789ab",
		Started: stamp(h.clock.Now().Add(10 * time.Minute)), Finished: stamp(h.clock.Now().Add(11 * time.Minute))}
	if err := deployrec.WriteState(h.paths.DeployState(), st); err != nil {
		t.Fatal(err)
	}
	h.clock.Add(40 * time.Minute)
	h.tick()
	got := h.load(id)
	m, ok := moveFrom(got, "triage")
	if !ok || m.To != "needs_human" || m.Reason != model.ReasonAgentLost {
		t.Fatalf("column %s moves %+v", got.Column, got.History)
	}
	if strings.Contains(m.Text, "deploy") {
		t.Fatalf("detail %q", m.Text)
	}
}

// A failed scion list is not an empty one: nothing is lost and nothing new
// starts here.
func TestAFailedScionListStrandsNothing(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "triage", "triage", nil)
	waiting := h.file("p", "t", "ready", nil)
	h.local.runErr = errors.New("could not tell")
	h.tick()
	if got := h.load(id); !got.Agent.Present() || got.Column != "triage" {
		t.Fatalf("agent %+v column %s", got.Agent, got.Column)
	}
	if h.load(waiting).Agent.Present() || len(h.local.started) != 0 {
		t.Fatal("started here although this host could not say what runs")
	}
}

// the local timed_out path, which keeps the branch even for a
// stage without one.
func TestALocalAgentPastItsTimeoutIsStopped(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "triage", "triage", nil)
	h.local.running["t0001-t1"] = true
	h.clock.Add(31 * time.Minute)
	h.tick()
	got := h.load(id)
	if got.Agent.Present() || got.Column != "needs_human" || moveReason(lastMove(got)) != model.ReasonTimedOut {
		t.Fatalf("agent %+v column %s move %+v", got.Agent, got.Column, lastMove(got))
	}
	if lastMove(got).Detail != "triage -> needs_human: agent t0001-t1 timed out" {
		t.Fatalf("detail %q", lastMove(got).Detail)
	}
	if len(h.local.removed) != 1 || !h.local.removed[0].KeepBranch {
		t.Fatalf("removed %+v", h.local.removed)
	}
	// its own timeout_minutes wins
	h = newHarness(t)
	id = localAgent(h, "triage", "triage", func(t *ticket.Ticket) { t.TimeoutMinutes = ticket.Some(60) })
	h.local.running["t0001-t1"] = true
	h.clock.Add(31 * time.Minute)
	h.tick()
	if got := h.load(id); !got.Agent.Present() {
		t.Fatal("a ticket with a longer timeout was stopped")
	}
}

// port: TF TriageTest.test_triage_agent_branch_is_deleted
func TestTriageAgentBranchIsDeleted(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "triage", "triage", nil)
	h.local.running["t0001-t1"] = true
	h.local.reports["t0001-t1"] = []byte(`{"decision": "reject", "reason": "dup"}`)
	h.tick()
	if len(h.local.removed) != 1 || h.local.removed[0].KeepBranch {
		t.Fatalf("removed %+v", h.local.removed)
	}
	if got := h.load(id); got.Column != "rejected" {
		t.Fatalf("column %s", got.Column)
	}
}

// A review files the pre-existing bugs it found as their own tickets.
func TestAReviewFilesPreExistingBugs(t *testing.T) {
	h := newHarness(t)
	h.repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
	head := h.repo.Git("rev-parse", "bot/x")
	id := localAgent(h, "review", "review", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		t.HeadBeforeReview = ticket.Some(head)
	})
	h.local.running["t0001-r1"] = true
	h.local.reports["t0001-r1"] = []byte(`{"verdict": "pass", "pre_existing": [{"title": "Old bug", "detail": "d"}]}`)
	h.local.alive = false
	h.tick()
	got := h.load(id)
	if got.Column != "security" || got.ReviewedHead.V != head {
		t.Fatalf("column %s reviewed %q", got.Column, got.ReviewedHead.V)
	}
	bug := h.load("T0002")
	if bug.Column != "triage" || bug.Door != "peer" || bug.Parent.V != "T0001" || bug.Title != "Old bug" {
		t.Fatalf("bug %+v", bug)
	}
	if filed := events(got, "filed"); len(filed) != 1 || !strings.HasPrefix(filed[0].Detail, "T0002: pre-existing") {
		t.Fatalf("filed %+v", filed)
	}
}

// port: TF AgentQuestionTest.test_an_agent_asks_the_reply_carries_the_stage_on
func TestAnAgentAsksTheMaintainer(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "Tune the minimum", "working", func(t *ticket.Ticket) {
		t.Kind = ticket.Some("task")
		agentOn(t, "t0001-k1", "task", "pi", stamp(h.clock.Now()))
	})
	got := collected(t, h, id, map[string][]byte{"report.json": []byte(
		`{"status": "question", "question": "Should the minimum be 8 or 16?", "summary": "Stopped before changing the default."}`)})
	if got.Column != "needs_human" || got.Ask.V.Kind != "question" || got.Ask.V.Question != "Should the minimum be 8 or 16?" {
		t.Fatalf("column %s ask %+v", got.Column, got.Ask.V)
	}
	if got.Ask.V.Why != "Stopped before changing the default." || got.Question.V.Stage != "task" {
		t.Fatalf("ask %+v question %+v", got.Ask.V, got.Question.V)
	}
}

// port: TF AgentQuestionTest.test_a_question_without_a_question_is_just_blocked
func TestAQuestionWithoutAQuestionIsJustBlocked(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "x", "working", func(t *ticket.Ticket) {
		t.Kind = ticket.Some("task")
		agentOn(t, "t0001-k1", "task", "pi", stamp(h.clock.Now()))
	})
	got := collected(t, h, id, map[string][]byte{"report.json": []byte(`{"status": "question", "summary": "hm"}`)})
	if got.Ask.V.Kind == "question" {
		t.Fatalf("ask %+v", got.Ask.V)
	}
}

// The answer of an Ask goes back to the ticket it was asked on, under that
// ticket's lock; one asked on a ticket that has closed stays on the desk.
func TestAnAnswerGoesBackToTheTicketItWasAskedOn(t *testing.T) {
	for _, c := range []struct {
		parentColumn, report, childColumn string
		delivered                         bool
	}{
		{"pr_open", `{"status": "done", "summary": "Because the lint rule says so.", "result": "## Why\nlint"}`, "done", true},
		{"done", `{"status": "done", "summary": "Because the lint rule says so."}`, "needs_human", false},
		{"pr_open", `{"status": "blocked", "summary": "no access"}`, "needs_human", false},
	} {
		h := newHarness(t)
		parent := h.file("p", "Tidy README", c.parentColumn, func(t *ticket.Ticket) {
			ticket.Event(t, "asked", "T0002: why this way?", "", nil, h.clock.Now())
		})
		child := h.file("p", "Question on it", "working", func(t *ticket.Ticket) {
			t.Kind = ticket.Some("task")
			t.Parent = ticket.Some(string(parent))
			agentOn(t, "t0002-k1", "task", "pi", stamp(h.clock.Now()))
		})
		got := collected(t, h, child, map[string][]byte{"report.json": []byte(c.report)})
		if got.Column != c.childColumn {
			t.Errorf("parent %s: child in %s, want %s", c.parentColumn, got.Column, c.childColumn)
		}
		p := last(h.load(parent))
		if c.delivered != (p.Event == "answered") {
			t.Errorf("parent %s: last event %+v", c.parentColumn, p)
		}
		if c.delivered && p.Detail != "T0002: Because the lint rule says so." {
			t.Errorf("answered %q", p.Detail)
		}
		if c.delivered && !strings.HasSuffix(lastMove(got).Detail, "answered on T0001") {
			t.Errorf("move %q", lastMove(got).Detail)
		}
	}
}

// port: TF TaskTicketTest.test_finished_task_brings_its_result_to_a_person
func TestFinishedTaskBringsItsResultToAPerson(t *testing.T) {
	h := newHarness(t)
	id := h.file("factory", "Uptime", "working", func(t *ticket.Ticket) {
		t.Kind = ticket.Some("task")
		agentOn(t, "t0001-k1", "task", "pi", stamp(h.clock.Now()))
	})
	got := collected(t, h, id, map[string][]byte{
		"report.json": []byte(`{"status": "done", "summary": "up 100%", "result": "# Uptime\nall good"}`), "head": []byte("abc\n")})
	if got.Column != "needs_human" || got.Result.V != "# Uptime\nall good" {
		t.Fatalf("column %s result %q", got.Column, got.Result.V)
	}
	if !strings.Contains(lastMove(got).Detail, "task done: up 100%") {
		t.Fatalf("detail %q", lastMove(got).Detail)
	}
}
