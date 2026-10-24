package pipeline

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const oneSlot = `max_agents = 1
agent_timeout_minutes = 30

[projects.p]
path = "{repo}"
base = "main"

[hosts.pi]
ssh = "factory@pi"
slots = 1
stages = ["triage", "review"]
`

// port: TF RemoteStageTest.test_helper_takes_work_only_when_this_host_is_full
func TestHelperTakesWorkOnlyWhenThisHostIsFull(t *testing.T) {
	h := newHarness(t)
	h.config(oneSlot)
	first := h.file("p", "t", "triage", nil)
	second := h.file("p", "t", "triage", nil)
	h.tick()
	if a, b := h.load(first).Agent.V.Host, h.load(second).Agent.V.Host; a != "local" || b != "pi" {
		t.Fatalf("hosts %q %q", a, b)
	}
	if len(h.local.started) != 1 || len(h.helpers["pi"].started) != 1 {
		t.Fatalf("local %d, pi %d", len(h.local.started), len(h.helpers["pi"].started))
	}
	job := h.helpers["pi"].started[0]
	if job.TimeoutMinutes != 30 || job.Repo == nil || job.Project == nil || job.Template != "triage" {
		t.Fatalf("job %+v", job)
	}
	if h.load(second).Column != "triage" {
		t.Fatal("triage runs in its own column")
	}
}

// port: TF RemoteStageTest.test_unreachable_helper_leaves_overflow_waiting
func TestUnreachableHelperLeavesOverflowWaiting(t *testing.T) {
	h := newHarness(t)
	h.config(oneSlot)
	h.local.running["someone-else"] = true
	h.helpers["pi"].startErr = remoteDown("pi")
	id := h.file("p", "t", "triage", nil)
	h.tick()
	got := h.load(id)
	if got.Agent.Present() || got.Column != "triage" {
		t.Fatalf("agent %+v column %s", got.Agent, got.Column)
	}
	if len(h.local.started) != 0 {
		t.Fatal("started here although this host is full")
	}
	errs := events(got, "error")
	if len(errs) != 1 || errs[0].Detail != "pi: factory@pi start failed (255): down" {
		t.Fatalf("errors %+v", errs)
	}
}

// port: TF RemoteStageTest.test_pinned_ticket_waits_for_its_host
func TestPinnedTicketWaitsForItsHost(t *testing.T) {
	h := newHarness(t)
	h.config(oneSlot)
	h.helpers["pi"].startErr = remoteDown("pi")
	id := h.file("p", "t", "triage", func(t *ticket.Ticket) { t.Host = ticket.Some("pi") })
	h.tick()
	got := h.load(id)
	if got.Agent.Present() || got.Column != "triage" || len(h.local.started) != 0 {
		t.Fatalf("agent %+v column %s local %d", got.Agent, got.Column, len(h.local.started))
	}
}

// A remote agent holds its helper's slot.
func TestARemoteAgentHoldsItsHelpersSlot(t *testing.T) {
	h := newHarness(t)
	h.config(oneSlot)
	h.local.running["someone-else"] = true
	h.file("p", "busy", "triage", func(t *ticket.Ticket) {
		agentOn(t, "t0001-t1", "triage", "pi", stamp(h.clock.Now()))
	})
	waiting := h.file("p", "t", "triage", nil)
	h.tick()
	if h.load(waiting).Agent.Present() || len(h.helpers["pi"].started) != 0 {
		t.Fatal("a full helper took more work")
	}
}

// collected runs one tick over a ticket whose helper job is done with files.
func collected(t *testing.T, h *harness, id ticket.ID, files map[string][]byte) *ticket.Ticket {
	t.Helper()
	pi := h.helpers["pi"]
	pi.state, pi.files = jobproto.StateDone, files
	h.tick()
	if len(pi.removed) == 0 {
		t.Fatal("the job was not removed from the helper")
	}
	return h.load(id)
}

func remoteAgent(h *harness, stage, column, branch string) ticket.ID {
	return h.file("p", "t", column, func(t *ticket.Ticket) {
		if branch != "" {
			t.Branch = ticket.Some(branch)
		}
		agentOn(t, "t0001-x1", stage, "pi", stamp(h.clock.Now()))
	})
}

// port: TF RemoteStageTest.test_remote_worker_commits_come_back_through_the_bundle
func TestRemoteWorkerCommitsComeBackThroughTheBundle(t *testing.T) {
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
	bundle := readFile(t, filepath.Join(other, "out.bundle"))
	id := remoteAgent(h, "work", "working", "bot/x")
	got := collected(t, h, id, map[string][]byte{"report.json": []byte(`{"status": "done"}`), "out.bundle": bundle, "agent.log": []byte("log")})
	if got.Column != "review" {
		t.Fatalf("column %s, %+v", got.Column, lastMove(got))
	}
	if n := h.repo.Git("rev-list", "--count", "main..bot/x"); n != "1" {
		t.Fatalf("commits ahead %s", n)
	}
}

// port: TF RemoteStageTest.test_remote_reviewer_must_leave_the_head_where_it_was
func TestRemoteReviewerMustLeaveTheHeadWhereItWas(t *testing.T) {
	for _, c := range []struct {
		reported string // "" sends no head file
		column   string
	}{{"head", "security"}, {strings.Repeat("0", 40), "needs_human"}, {"", "needs_human"}} {
		h := newHarness(t)
		h.repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
		head := h.repo.Git("rev-parse", "bot/x")
		id := remoteAgent(h, "review", "review", "bot/x")
		h.change(id, func(t *ticket.Ticket) { t.HeadBeforeReview = ticket.Some(head) })
		files := map[string][]byte{"report.json": []byte(`{"verdict": "pass"}`)}
		switch c.reported {
		case "head":
			files["head"] = []byte(head + "\n")
		case "":
		default:
			files["head"] = []byte(c.reported + "\n")
		}
		if got := collected(t, h, id, files); got.Column != c.column {
			t.Errorf("head %q: column %s, %+v", c.reported, got.Column, lastMove(got))
		}
	}
}

// port: TF RemoteStageTest.test_remote_job_without_report_needs_human
func TestRemoteJobWithoutReportNeedsHuman(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	got := collected(t, h, id, map[string][]byte{"agent.log": []byte("crashed")})
	if got.Column != "needs_human" || got.Agent.Present() || moveReason(lastMove(got)) != model.ReasonNoReport {
		t.Fatalf("column %s agent %+v move %+v", got.Column, got.Agent, lastMove(got))
	}
	if lastMove(got).Detail != "triage -> needs_human: agent t0001-x1 on pi finished without a report" {
		t.Fatalf("detail %q", lastMove(got).Detail)
	}
}

func TestARemoteAgentThatIsGoneIsLost(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	h.helpers["pi"].state = "missing"
	h.tick()
	got := h.load(id)
	if got.Column != "needs_human" || moveReason(lastMove(got)) != model.ReasonAgentLost || got.Agent.Present() {
		t.Fatalf("column %s move %+v", got.Column, lastMove(got))
	}
	if !strings.HasSuffix(lastMove(got).Detail, "agent t0001-x1 on pi disappeared without a report") {
		t.Fatalf("detail %q", lastMove(got).Detail)
	}
}

func TestARemoteAgentPastItsTimeoutIsStopped(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	h.clock.Add(31 * time.Minute)
	h.tick()
	got := h.load(id)
	if got.Column != "needs_human" || moveReason(lastMove(got)) != model.ReasonTimedOut {
		t.Fatalf("column %s move %+v", got.Column, lastMove(got))
	}
	if len(h.helpers["pi"].removed) != 1 {
		t.Fatal("the job was not removed")
	}
}

// a remote exception past the timeout.
func TestARemoteErrorPastTheTimeoutLosesTheAgent(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	h.helpers["pi"].pollErr = errors.New("factory@pi status failed (255): no route to host")
	h.tick()
	got := h.load(id)
	if !got.Agent.Present() || got.Column != "triage" || len(events(got, "error")) != 1 {
		t.Fatalf("within its time an error only records: agent %+v column %s", got.Agent, got.Column)
	}
	h.clock.Add(31 * time.Minute)
	h.tick()
	got = h.load(id)
	if got.Agent.Present() || got.Column != "needs_human" || moveReason(lastMove(got)) != model.ReasonAgentLost {
		t.Fatalf("agent %+v column %s move %+v", got.Agent, got.Column, lastMove(got))
	}
	if !strings.HasSuffix(lastMove(got).Detail, "agent t0001-x1 on pi: factory@pi status failed (255): no route to host") {
		t.Fatalf("detail %q", lastMove(got).Detail)
	}
	if len(h.helpers["pi"].removed) != 1 {
		t.Fatalf("removed %v", h.helpers["pi"].removed)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
