package gate

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func gateTicket(t *testing.T, reports string) *ticket.Ticket {
	t.Helper()
	return decodeText(t, `{"id": "T0001", "title": "t", "body": "", "door": "human", "project": "gem", "column": "gate",
		"branch": "b", "reports": `+reports+`, "history": [{"at": "x", "event": "start", "detail": "work t0001-w1 on b"}], "agent": null}`)
}

// port: TF TransitionTest.test_gate_blocks_auto_merge_without_stated_reasoning_and_audit_says_why
func TestGateBlocksAutoMergeWithoutStatedReasoningAndAuditSaysWhy(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	cfg, p := r.config("", "[gate]\nallowed_paths = [\"*.md\"]\n")
	tk := gateTicket(t, `{"work": {"status": "done", "summary": "", "category": "docs"},
		"review": {"verdict": "pass", "runtime_behaviour": "no", "summary": "fine"}}`)
	g, err := r.runner.Run(context.Background(), cfg, p, tk)
	if err != nil {
		t.Fatal(err)
	}
	if g.AutoEligible || !slices.Equal(g.Blockers, []string{"worker did not state its category and reasoning"}) {
		t.Errorf("gate %+v", g)
	}
	Record(tk, cfg, g, time.Unix(0, 0))
	audit, err := AuditText(tk)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"needs human review: worker did not state its category and reasoning", "Review: pass; runtime behaviour changes: no"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit lacks %q:\n%s", want, audit)
		}
	}
	// The facts Gate 1 recorded.
	if !g.ChecksPass || !slices.Equal(g.Files, []string{"README.md"}) || g.ChangedLines != 2 || !g.WithinSize ||
		len(g.OutsideAllowed) != 0 || g.ReasoningStated {
		t.Errorf("gate facts %+v", g)
	}
	if g.Head != r.git(r.repo.Dir, "rev-parse", "b") || g.Base != r.git(r.repo.Dir, "rev-parse", "main") {
		t.Errorf("heads %q %q", g.Head, g.Base)
	}
	out, err := os.ReadFile(filepath.Join(r.paths.TicketLogs("T0001"), "gate-output.txt"))
	if err != nil || string(out) != "ok" {
		t.Errorf("gate-output.txt: %q %v", out, err)
	}
}

// Review §4.9: the gate takes the branch's head first and checks exactly
// that commit: its files, its size and the checks on a fresh clone. A
// commit that lands on the branch while the checks run is not what was
// gated: the gate records the commit it checked, says in the log that the
// branch moved, and the merge path merges only that commit.
func TestTheGateChecksTheHeadItTookFirst(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	gated := r.git(r.repo.Dir, "rev-parse", "b")
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config("", "[gate]\nallowed_paths = [\"*.md\"]\n")
	tk := gateTicket(t, `{"work": {"status": "done", "summary": "s", "category": "docs"},
		"review": {"verdict": "pass", "runtime_behaviour": "no", "summary": "fine"}}`)
	wt := filepath.Join(r.paths.Gate, "T0001")
	var checked string
	r.podman.inspect = func(podman.Gate) {
		checked = r.git(wt, "rev-parse", "HEAD")
		// an agent's late commit, and main moving, while the checks run
		r.git(r.repo.Dir, "checkout", "-q", "b")
		r.repo.Write("lib/late.rb", strings.Repeat("late\n", 50))
		r.repo.Commit("late")
		r.git(r.repo.Dir, "checkout", "-q", "main")
		r.repo.Write("CHANGELOG.md", "moved\n")
		r.repo.Commit("main moves")
	}
	g, err := r.runner.Run(context.Background(), cfg, p, tk)
	if err != nil {
		t.Fatal(err)
	}
	if checked != gated {
		t.Errorf("the checks ran on %s, not the head taken first %s", checked, gated)
	}
	if g.Head != gated || g.Base != base {
		t.Errorf("gated head %s base %s, want %s %s", g.Head, g.Base, gated, base)
	}
	if !slices.Equal(g.Files, []string{"README.md"}) || g.ChangedLines != 2 || !g.AutoEligible {
		t.Errorf("gate %+v", g)
	}
	if !slices.ContainsFunc(r.log.lines, func(l string) bool { return strings.Contains(l, "T0001") && strings.Contains(l, gated[:7]) }) {
		t.Errorf("log %q", r.log.lines)
	}
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name     string
		gate     string
		reports  string
		files    []string
		size     int
		checks   bool
		blockers []string
		reason   bool
		behave   string // the runtime_behaviour recorded, as JSON
	}{
		{name: "eligible", reports: `{"work": {"category": "fix", "summary": "s"}, "review": {"runtime_behaviour": "no"}}`,
			files: []string{"README.md"}, size: 3, checks: true, reason: true, behave: `"no"`,
			gate: "[gate]\nallowed_paths = [\"*.md\"]\n"},
		{name: "every blocker, in Python's order", reports: `{"work": {}, "review": {"runtime_behaviour": "yes"}}`,
			files: []string{"lib/a.rb", "README.md", "bin/x"}, size: 11, behave: `"yes"`,
			gate: "[gate]\nallowed_paths = [\"*.md\"]\n",
			blockers: []string{"tests or lint failed", "files outside the allowed paths: lib/a.rb, bin/x",
				"11 changed lines (limit 10)", "reviewer says runtime behaviour changes: yes",
				"worker did not state its category and reasoning"}},
		{name: "no allowed paths: every file is outside", reports: `{"work": {"category": "fix", "summary": "s"}, "review": {"runtime_behaviour": "no"}}`,
			files: []string{"README.md"}, size: 1, checks: true, reason: true, behave: `"no"`,
			blockers: []string{"files outside the allowed paths: README.md"}},
		{name: "no review report: unknown", reports: `{"work": {"category": "fix", "summary": "s"}}`,
			files: []string{}, size: 0, checks: true, reason: true, behave: `"unknown"`,
			blockers: []string{"reviewer says runtime behaviour changes: unknown"}},
		{name: "a behaviour that is not a string, printed as Python", reports: `{"work": {"category": "fix", "summary": "s"}, "review": {"runtime_behaviour": false}}`,
			files: []string{}, checks: true, reason: true, behave: `false`,
			blockers: []string{"reviewer says runtime behaviour changes: False"}},
		{name: "a null category is str(None): stated (a Python quirk kept)", reports: `{"work": {"category": null, "summary": " ok "}, "review": {"runtime_behaviour": "no"}}`,
			files: []string{}, checks: true, reason: true, behave: `"no"`},
		{name: "blank reasoning", reports: `{"work": {"category": " \t", "summary": "s"}, "review": {"runtime_behaviour": "no"}}`,
			files: []string{}, checks: true, behave: `"no"`,
			blockers: []string{"worker did not state its category and reasoning"}},
		{name: "at the size limit", reports: `{"work": {"category": "fix", "summary": "s"}, "review": {"runtime_behaviour": "no"}}`,
			files: []string{}, size: 25, checks: true, reason: true, behave: `"no"`, gate: "[gate]\nmax_changed_lines = 25\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := mustConfig(t, c.gate)
			tk := gateTicket(t, c.reports)
			g, err := Evaluate(GateIn{Config: cfg, Ticket: tk, Files: c.files, Size: c.size, ChecksPass: c.checks, Head: "h", Base: "b"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(g.Blockers, c.blockers) && (len(g.Blockers) != 0 || len(c.blockers) != 0) {
				t.Errorf("blockers %q\nwant %q", g.Blockers, c.blockers)
			}
			if g.Blockers == nil || g.Files == nil || g.OutsideAllowed == nil {
				t.Error("a list is nil: Python writes []")
			}
			if g.AutoEligible != (len(c.blockers) == 0) || g.ReasoningStated != c.reason || g.ChecksPass != c.checks {
				t.Errorf("gate %+v", g)
			}
			// The value is recorded as the reviewer wrote it.
			tk.Gate = ticket.Some(g)
			if got := string(ticket.Encode(tk)); !strings.Contains(got, `"runtime_behaviour": `+c.behave) {
				t.Errorf("runtime_behaviour is not %s in\n%s", c.behave, got)
			}
		})
	}
	// Python's gate() reads the reports as dicts, and fails on anything else.
	for _, reports := range []string{`{"work": "done"}`, `{"review": null}`} {
		if _, err := Evaluate(GateIn{Config: mustConfig(t, ""), Ticket: gateTicket(t, reports)}); err == nil {
			t.Errorf("reports %s: no error", reports)
		}
	}
}

// The ticket's gate keys, in the order Python's gate() builds the dict.
func TestGateKeyOrder(t *testing.T) {
	tk := gateTicket(t, `{"work": {"category": "fix", "summary": "s"}, "review": {"runtime_behaviour": "no"}}`)
	g, err := Evaluate(GateIn{Config: mustConfig(t, ""), Ticket: tk, Files: []string{"a"}, Size: 1, ChecksPass: true, Head: "h", Base: "b"})
	if err != nil {
		t.Fatal(err)
	}
	tk.Gate = ticket.Some(g)
	got := string(ticket.Encode(tk))
	got = got[strings.Index(got, `"gate": {`):]
	keys := []string{"checks_pass", "files", "changed_lines", "within_size", "outside_allowed_paths", "runtime_behaviour",
		"reasoning_stated", "head", "base", "auto_eligible", "blockers"}
	last := -1
	for _, k := range keys {
		i := strings.Index(got, `"`+k+`"`)
		if i < last {
			t.Fatalf("key %q out of order in\n%s", k, got)
		}
		last = i
	}
}

// port: TF AskTest.test_gate_records_its_rule
func TestGateRecordsItsRule(t *testing.T) {
	cfg := mustConfig(t, "[gate]\nmax_changed_lines = 10\n")
	g := ticket.Gate{Blockers: []string{}, ChangedLines: 7}
	if got := Rule(cfg, g); got != "gate: auto-merge eligible: tests and lint pass, only allowed paths, 7 changed lines (limit 10), reviewer says no runtime change" {
		t.Errorf("eligible rule %q", got)
	}
	g = ticket.Gate{Blockers: []string{"tests or lint failed", "12 changed lines (limit 10)"}, ChangedLines: 12}
	if got := Rule(cfg, g); got != "gate: needs a maintainer: tests or lint failed; 12 changed lines (limit 10)" {
		t.Errorf("blocked rule %q", got)
	}
}

// port: TF AskTest.test_gate_in_the_tick_moves_with_its_rule
//
// The tick's move after Gate 1 (F:2262): a failed check needs a person and
// asks to unstick back to the gate; a pass goes to ready_for_pr, with the
// blockers (cut at 200) as the detail when it needs a person's review.
func TestRecordMovesWithItsRule(t *testing.T) {
	cfg := mustConfig(t, "")
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	tk := gateTicket(t, `{}`)
	failed := ticket.Gate{Blockers: []string{"tests or lint failed"}, ChangedLines: 3, Files: []string{"README.md"}}
	Record(tk, cfg, failed, now)
	last := tk.History[len(tk.History)-1]
	if tk.Column != "needs_human" || last.Rule != "gate: needs a maintainer: tests or lint failed" ||
		last.Detail != "gate -> needs_human: gate: tests or lint failed on a fresh checkout" {
		t.Errorf("column %q, entry %+v", tk.Column, last)
	}
	if m, ok := last.Data.(ticket.Moved); !ok || m.Reason != model.ReasonGateFailed {
		t.Errorf("payload %+v", last.Data)
	}
	ask, ok := tk.Ask.Get()
	if !ok || ask.Kind != "unstick" || ask.RetryColumn.Or("") != "gate" {
		t.Errorf("ask %+v", tk.Ask)
	}
	if g, ok := tk.Gate.Get(); !ok || !slices.Equal(g.Blockers, failed.Blockers) {
		t.Errorf("gate not recorded: %+v", tk.Gate)
	}

	tk = gateTicket(t, `{}`)
	Record(tk, cfg, ticket.Gate{ChecksPass: true, AutoEligible: true, Blockers: []string{}, ChangedLines: 4}, now)
	last = tk.History[len(tk.History)-1]
	if tk.Column != "ready_for_pr" || last.Detail != "gate -> ready_for_pr: auto-merge eligible" ||
		!strings.HasPrefix(last.Rule, "gate: auto-merge eligible: ") {
		t.Errorf("column %q, entry %+v", tk.Column, last)
	}
	if m, ok := last.Data.(ticket.Moved); !ok || m.Reason != model.ReasonGatePassed {
		t.Errorf("payload %+v", last.Data)
	}

	tk = gateTicket(t, `{}`)
	long := strings.Repeat("é", 150)
	Record(tk, cfg, ticket.Gate{ChecksPass: true, Blockers: []string{long, long}}, now)
	last = tk.History[len(tk.History)-1]
	// Python: "needs human review: " + "; ".join(blockers)[:200]: the cut is on the blockers.
	want := "gate -> ready_for_pr: needs human review: " + long + "; " + strings.Repeat("é", 48)
	if last.Detail != want {
		t.Errorf("detail %q\nwant %q", last.Detail, want)
	}
}
