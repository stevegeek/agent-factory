package pipeline

import (
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// port: TF RobustnessTest.test_an_agent_is_on_its_ticket_before_it_starts
func TestAnAgentIsOnItsTicketBeforeItStarts(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "x", "triage", nil)
	var seen ticket.Agent
	h.local.onStart = func(j hostrun.Job) {
		seen = h.load(id).Agent.V // what the file says while the launch runs
	}
	h.local.startErr = errors.New("podman fell over")
	h.tick()
	if seen.Name != "t0001-t1" || !seen.Starting.Or(false) {
		t.Fatalf("on disk during the launch: %+v", seen)
	}
	got := h.load(id)
	if !got.Agent.Set || !got.Agent.Null {
		t.Fatalf("agent after a failed launch: %+v", got.Agent)
	}
	if got.Column != string(model.ColNeedsHuman) || moveReason(lastMove(got)) != model.ReasonDispatcherError {
		t.Fatalf("column %s, move %+v", got.Column, lastMove(got))
	}
	if !strings.Contains(got.Ask.V.Why, "podman fell over") {
		t.Fatalf("ask why %q", got.Ask.V.Why)
	}
}

// port: TF ConfigTest.test_stage_uses_configured_template_name
func TestStageUsesConfiguredTemplateName(t *testing.T) {
	h := newHarness(t)
	h.config(defaultConfig + "\n[templates]\ntriage = \"gem-triage\"\n")
	h.file("p", "t", "triage", nil)
	h.tick()
	if len(h.local.started) != 1 || h.local.started[0].Template != "gem-triage" {
		t.Fatalf("started %+v", h.local.started)
	}
}

// port: TT TypedEventTest.test_a_start_and_finish_say_stage_agent_host_model_and_branch
func TestAStartAndFinishSayStageAgentHostModelAndBranch(t *testing.T) {
	h := newHarness(t)
	h.config(defaultConfig + "\n[models]\nwork = \"claude-opus-5-5\"\n\n[memory]\nenabled = false\n")
	id := h.file("p", "Fix it", "ready", nil)
	h.local.alive = true
	h.tick()
	got := h.load(id)
	start := histKeys(got, -1)
	if start["detail"] != "work t0001-w1 on bot/fix-it (claude-opus-5-5)" {
		t.Fatalf("start detail %q", start["detail"])
	}
	want := map[string]string{"type": "started", "stage": "work", "agent": "t0001-w1", "host": "local",
		"model": "claude-opus-5-5", "branch": "bot/fix-it"}
	for k, v := range want {
		if start[k] != v {
			t.Errorf("start %s = %q, want %q", k, start[k], v)
		}
	}
	if moveReason(got.History[len(got.History)-2]) != model.ReasonStarted {
		t.Fatalf("the move before the start: %+v", got.History[len(got.History)-2])
	}
	if ticket.Starts(got, model.StageWork) != 1 || got.Column != "working" {
		t.Fatalf("starts %d column %s", ticket.Starts(got, model.StageWork), got.Column)
	}
	a := got.Agent.V
	if a.Starting.Set || a.Name != "t0001-w1" || a.Host != "local" || a.Stage != "work" {
		t.Fatalf("agent %+v", a)
	}
	if job := h.local.started[0]; job.Model != "claude-opus-5-5" || job.Project == nil || job.Project.Path != h.repo.Dir {
		t.Fatalf("job %+v", job)
	}

	h.repo.BranchWith("bot/fix-it", map[string]string{"lib/thing.rb": "def thing; 2; end\n"})
	h.local.reports["t0001-w1"] = []byte(`{"status": "done", "summary": "did it"}`)
	h.tick()
	got = h.load(id)
	// the review starts in the same tick, after the finish and its move
	if rs := histKeys(got, -1); rs["detail"] != "review t0001-r1 on bot/fix-it" {
		t.Fatalf("review start %v", rs)
	}
	fin, mv := histKeys(got, -3), histKeys(got, -2)
	if fin["detail"] != "work t0001-w1" || fin["type"] != "finished" || fin["stage"] != "work" || fin["agent"] != "t0001-w1" {
		t.Fatalf("finished %v", fin)
	}
	if mv["detail"] != "working -> review" || mv["reason"] != "work_done" {
		t.Fatalf("move %v", mv)
	}
	if len(h.local.removed) != 1 || !h.local.removed[0].KeepBranch {
		t.Fatalf("a worker's agent is removed keeping its branch: %+v", h.local.removed)
	}
	if got.AgentLastHost.V != "local" || got.Agent.V.Name != "t0001-r1" {
		t.Fatalf("agent_last_host %+v agent %+v", got.AgentLastHost, got.Agent)
	}
}

// port: TF TickRaceTest.test_a_dispatcher_error_says_what_went_wrong
func TestADispatcherErrorSaysWhatWentWrong(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "Fix", "ready", nil)
	h.local.startErr = errBoom
	h.tick()
	got := h.load(id)
	if got.Ask.V.Kind != "unstick" || got.Ask.V.RetryColumn.V != "ready" {
		t.Fatalf("ask %+v", got.Ask.V)
	}
	if !strings.Contains(got.Ask.V.Why, "no space left on device") {
		t.Fatalf("why %q", got.Ask.V.Why)
	}
	errs := events(got, "error")
	if len(errs) != 1 || errs[0].Detail != "no space left on device" {
		t.Fatalf("error events %+v", errs)
	}
}

func TestATicketOnAnUnknownProjectIsADispatcherError(t *testing.T) {
	h := newHarness(t)
	id := h.file("gone", "Fix", "ready", nil)
	h.tick()
	got := h.load(id)
	if got.Column != "needs_human" || !strings.Contains(lastMove(got).Detail, "dispatcher error: unknown project gone") {
		t.Fatalf("column %s move %q", got.Column, lastMove(got).Detail)
	}
}

// A review starts from where the branch is, which the reviewer must leave;
// security freezes its tier and questions on the ticket.
func TestReviewAndSecurityRecordTheHeadTheyStartFrom(t *testing.T) {
	h := newHarness(t)
	h.repo.BranchWith("bot/x", map[string]string{"lib/thing.rb": "def thing; eval(x); end\n"})
	head := h.repo.Git("rev-parse", "bot/x")
	rev := h.file("p", "Fix", "review", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	sec := h.file("p", "Fix", "security", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.tick()
	r, s := h.load(rev), h.load(sec)
	if r.HeadBeforeReview.V != head || s.HeadBeforeSecurity.V != head {
		t.Fatalf("heads %q %q, want %q", r.HeadBeforeReview.V, s.HeadBeforeSecurity.V, head)
	}
	if s.SecurityTier.V != 2 || len(s.SecurityQuestions) != 5 {
		t.Fatalf("tier %d, %d questions", s.SecurityTier.V, len(s.SecurityQuestions))
	}
	if r.Agent.V.Host != "local" || s.Agent.V.Host != "local" {
		t.Fatalf("hosts %q %q", r.Agent.V.Host, s.Agent.V.Host)
	}
}

// A review reads REVIEW.md from the base commit only.
func TestTheReviewPromptReadsREVIEWmdFromTheBase(t *testing.T) {
	h := newHarness(t)
	h.repo.BranchWith("bot/x", map[string]string{"REVIEW.md": "planted by the agent\n"})
	h.file("p", "Fix", "review", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.tick()
	if strings.Contains(h.local.started[0].Prompt, "REVIEW.md") {
		t.Fatal("a REVIEW.md on the branch was followed")
	}
	h2 := newHarness(t)
	h2.repo.Write("REVIEW.md", "the maintainer's\n")
	h2.repo.Commit("review rules")
	h2.repo.BranchWith("bot/x", map[string]string{"lib/thing.rb": "x\n"})
	h2.file("p", "Fix", "review", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h2.tick()
	if !strings.Contains(h2.local.started[0].Prompt, "REVIEW.md") {
		t.Fatal("the base's REVIEW.md was not named")
	}
}
