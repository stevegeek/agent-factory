package pipeline

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/prflow"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// a public project, a second project, a host and a deny term: the names
// public review may be told are private
const publicConfig = `max_agents = 2
agent_timeout_minutes = 30

[identity]
pr_footer = "Opened by a bot."

[hygiene]
deny = ["hosty", "readme-term"]
allow = ["Widgets"]

[projects.p]
path = "{repo}"
base = "main"
install = "true"
test = "true"
lint = "true"
hygiene = true

[projects.gadgets]
path = "/nowhere"

[projects.widgets]
path = "/nowhere"

[hosts.hostz]
ssh = "factory@hostz"
stages = ["triage"]
`

// Public review starts from the branch's head with every text the PR
// would carry and the private values: never the ticket's own project, a
// name the repository already has at its base, or one [hygiene] allow
// names.
func TestPublicReviewIsToldTheTextsAndThePrivateValues(t *testing.T) {
	h := newHarness(t)
	h.config(publicConfig)
	h.repo.Write("README.md", "hello readme-term\n")
	h.repo.Commit("readme")
	h.repo.BranchWith("bot/x", map[string]string{"lib/thing.rb": "def thing; 2; end\n"})
	head := h.repo.Git("rev-parse", "bot/x")
	id := h.file("p", "Fix the thing", "public", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		t.Source = ticket.Some(ticket.Source{Issue: 7})
		t.Reports.Set("work", jsonx.ObjectOf(jsonx.Member{Key: "summary", Value: jsonx.String("Fixed the thing.")},
			jsonx.Member{Key: "tests", Value: jsonx.String("rake")}))
		t.Reports.Set("review", jsonx.ObjectOf(jsonx.Member{Key: "verdict", Value: jsonx.String("pass")},
			jsonx.Member{Key: "runtime_behaviour", Value: jsonx.String("no")}))
	})
	h.tick()
	got := h.load(id)
	if got.HeadBeforePublic.Or("") != head || got.Agent.V.Stage != "public" || len(h.local.started) != 1 {
		t.Fatalf("head %v agent %+v", got.HeadBeforePublic, got.Agent)
	}
	p := h.local.started[0].Prompt
	for _, want := range []string{"### branch\n```\nbot/x\n```", "### pr-title\n```\nFix the thing\n```", "### ticket\n```\nFixes #7\n```",
		"### summary\n```\nFixed the thing.\n```", "### tests\n```\nrake\n```", "### review\n```\nverdict: pass\nchanges runtime behaviour for users: no\n```",
		"### footer\n```\nOpened by a bot.\n```", "- hosty\n- gadgets\n- hostz\n", "main..HEAD"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	for _, private := range []string{"- p\n", "readme-term", "widgets", "Widgets"} {
		if strings.Contains(p, private) {
			t.Errorf("prompt names %q as private:\n%s", private, p)
		}
	}
	if strings.Contains(p, "### lint") {
		t.Error("a text the PR does not carry is shown")
	}
}

// A public project's ticket in the gate column is read before it is
// gated, unless public review passed the head its branch is at now: a
// ticket moved there by hand, or a pass of an older head, is read again.
func TestTheGateRefusesAHygieneTicketPublicReviewHasNotRead(t *testing.T) {
	h := newHarness(t)
	h.config(publicConfig)
	h.gate.result = passedGate()
	h.repo.BranchWith("bot/x", map[string]string{"lib/thing.rb": "def thing; 2; end\n"})
	head := h.repo.Git("rev-parse", "bot/x")
	unread := h.file("p", "Fix", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	stale := h.file("p", "Fix", "gate", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		t.PublicHead = ticket.Some("0123456789abcdef0123456789abcdef01234567")
	})
	h.tick()
	for _, id := range []ticket.ID{unread, stale} {
		got := h.load(id)
		if got.Column != "public" || got.Agent.V.Stage != "public" || got.HeadBeforePublic.Or("") != head {
			t.Errorf("%s: column %s agent %+v", id, got.Column, got.Agent)
		}
		if moveReason(lastMove(got)) != model.ReasonStarted {
			t.Errorf("%s: move %+v", id, lastMove(got))
		}
	}
	if len(h.gate.ran) != 0 {
		t.Fatalf("gated unread: %v", h.gate.ran)
	}
	h2 := newHarness(t)
	h2.config(publicConfig)
	h2.gate.result = passedGate()
	h2.repo.BranchWith("bot/x", map[string]string{"lib/thing.rb": "def thing; 2; end\n"})
	read := h2.file("p", "Fix", "gate", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		h2.passPublic(t, h2.repo.Git("rev-parse", "bot/x"))
	})
	// the same head, but a summary that changed after the read: a rerun
	// with no new commit
	retold := h2.file("p", "Fix", "gate", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		h2.passPublic(t, h2.repo.Git("rev-parse", "bot/x"))
		t.Reports.Set("work", jsonx.ObjectOf(jsonx.Member{Key: "summary", Value: jsonx.String("Told again.")}))
	})
	h2.tick()
	if len(h2.gate.ran) != 1 || h2.load(read).Column != "ready_for_pr" {
		t.Fatalf("a read head was not gated: %v %s", h2.gate.ran, h2.load(read).Column)
	}
	if got := h2.load(retold); got.Column != "public" || got.Agent.V.Stage != "public" {
		t.Errorf("changed texts: %s %+v", got.Column, got.Agent)
	}
}

// passPublic records public review's pass of head, with the texts the PR
// would carry now.
func (h *harness) passPublic(t *ticket.Ticket, head string) {
	h.t.Helper()
	cfg, err := h.deps().Config.Current()
	if err != nil {
		h.t.Fatal(err)
	}
	p, _ := cfg.Project(t.Project)
	t.PublicHead = ticket.Some(head)
	t.TextsBeforePublic = ticket.Some(prflow.PublicDigest(prflow.PublicTexts(cfg, *p, t)))
}

// Through the pipeline: security passes a public project's change to
// public review, whose pass at the head it read goes to the gate.
func TestSecurityPassesToPublicReviewAndItsPassToTheGate(t *testing.T) {
	h := newHarness(t)
	h.config(publicConfig + "\n[memory]\nenabled = false\n")
	h.gate.result = passedGate()
	h.repo.BranchWith("bot/x", map[string]string{"README.md": "hello again\n"})
	id := h.file("p", "Fix", "security", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.tick()
	h.local.reports["t0001-s1"] = []byte(`{"verdict": "pass", "answers": [{"answer": "fine", "concern": "none"}]}`)
	h.tick()
	got := h.load(id)
	if got.Column != "public" || got.Agent.V.Name != "t0001-o1" {
		t.Fatalf("after security: %s %+v", got.Column, got.Agent)
	}
	h.local.reports["t0001-o1"] = []byte(`{"verdict": "pass", "findings": [], "summary": "nothing private"}`)
	h.tick()
	got = h.load(id)
	if got.PublicHead.Or("") != h.repo.Git("rev-parse", "bot/x") || len(h.gate.ran) != 1 || got.Column != "ready_for_pr" {
		t.Fatalf("after public review: %s %v gated %v", got.Column, got.PublicHead, h.gate.ran)
	}
}

// Through the pipeline: a report public review cannot read leaves the
// ticket in the public column, where the tick reads it again; a second one
// stops for a person, who may have it read again.
func TestAnUnreadablePublicReportIsReadAgainThenStops(t *testing.T) {
	h := newHarness(t)
	h.config(publicConfig + "\n[memory]\nenabled = false\n")
	h.repo.BranchWith("bot/x", map[string]string{"README.md": "hello again\n"})
	id := h.file("p", "Fix", "public", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.tick()
	h.local.reports["t0001-o1"] = []byte(`{"verdict": "maybe"}`)
	h.tick()
	got := h.load(id)
	if got.Column != "public" || got.Agent.V.Name != "t0001-o2" {
		t.Fatalf("after the first: %s %+v", got.Column, got.Agent)
	}
	h.local.reports["t0001-o2"] = []byte(`not json`)
	h.tick()
	got = h.load(id)
	a, _ := got.Ask.Get()
	if got.Column != "needs_human" || got.Agent.Present() || a.RetryColumn.Or("") != "public" || got.PublicHead.Or("") != "" {
		t.Fatalf("after the second: %s %+v %+v", got.Column, got.Agent, a)
	}
}

// The finish routes a finding by the texts the review was shown: one in
// the worker's summary goes back to work, one in the branch to a person.
func TestPublicFindingsAreRoutedByTheTextsShown(t *testing.T) {
	for where, back := range map[string]bool{"summary": true, "branch": false, "updates": false} {
		h := newHarness(t)
		h.config(publicConfig + "\n[memory]\nenabled = false\n")
		h.repo.BranchWith("bot/x", map[string]string{"README.md": "hello again\n"})
		id := h.file("p", "Fix", "public", func(t *ticket.Ticket) {
			t.Branch = ticket.Some("bot/x")
			t.Reports.Set("work", jsonx.ObjectOf(jsonx.Member{Key: "summary", Value: jsonx.String("Fixed the thing.")}))
			t.Updates = []ticket.Update{{At: "2026-01-02T03:04:05Z", Why: "review", Text: "Added a test."}}
		})
		h.tick()
		h.local.reports["t0001-o1"] = []byte(`{"verdict": "block", "findings": [{"where": "` + where + `", "why": "names a host"}]}`)
		h.tick()
		// back to work is a worker started on the same tick
		if got := h.load(id); (ticket.PublicRounds(got) == 1) != back || (got.Column == "needs_human") == back {
			t.Errorf("%s: %s, %d rounds", where, got.Column, ticket.PublicRounds(got))
		}
	}
}

// A person's send-back reaches the worker with both the note and public
// review's findings.
func TestAPersonsSendBackGivesTheWorkerTheNoteAndTheFindings(t *testing.T) {
	h := newHarness(t)
	h.config(publicConfig + "\n[memory]\nenabled = false\n")
	h.repo.BranchWith("bot/x", map[string]string{"README.md": "hello again\n"})
	id := h.file("p", "Fix", "needs_human", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		t.Reports.Set("public", jsonx.ObjectOf(jsonx.Member{Key: "verdict", Value: jsonx.String("block")},
			jsonx.Member{Key: "findings", Value: jsonx.ArrayOf(jsonx.ObjectOf(jsonx.Member{Key: "where", Value: jsonx.String("summary")},
				jsonx.Member{Key: "why", Value: jsonx.String("names a host")}))}))
		ticket.AddNote(t, "say it without the host", "alice", nil, h.clock.Now())
		ticket.Move(t, "ready", model.ReasonPublicChanges, "sent back to work after public review by alice", "", "alice", h.clock.Now())
	})
	h.tick()
	if got := h.load(id); got.Agent.V.Stage != "work" || len(h.local.started) != 1 {
		t.Fatalf("%s %+v", got.Column, got.Agent)
	}
	p := h.local.started[0].Prompt
	for _, want := range []string{"say it without the host", "summary: names a host", "amend or squash"} {
		if !strings.Contains(p, want) {
			t.Errorf("work prompt lacks %q:\n%s", want, p)
		}
	}
}

// On a public project whose ticket title has a deny term, the PR title
// public review reads is the one OpenPR sends: made from the summary.
func TestPublicReviewReadsTheTitleThePRWouldCarry(t *testing.T) {
	h := newHarness(t)
	h.config(publicConfig)
	h.repo.BranchWith("bot/x", map[string]string{"lib/thing.rb": "def thing; 2; end\n"})
	h.file("p", "Fix the thing on hosty", "public", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		t.Reports.Set("work", jsonx.ObjectOf(jsonx.Member{Key: "summary", Value: jsonx.String("Fixed the thing. And more.")}))
	})
	h.tick()
	if len(h.local.started) != 1 {
		t.Fatal("public review did not start")
	}
	if p := h.local.started[0].Prompt; !strings.Contains(p, "### pr-title\n```\nFixed the thing.\n```") {
		t.Errorf("prompt:\n%s", p)
	}
}
