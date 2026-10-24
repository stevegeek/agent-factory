package gate

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// A term is found in each place a PR publishes, whatever its case, and
// each hit names the place and the term.
func TestHygieneFindsEachPlace(t *testing.T) {
	in := HygieneIn{
		Files:    []string{"docs/HOSTX.md", "README.md"},
		Added:    []string{"clean line", "ssh to hostx.lan first"},
		Messages: []string{"Fix the deploy\n\nas seen on HostX"},
		Branch:   "bot/fix-hostx",
		Texts:    map[string]string{"the work summary": "Tested on hOsTx.", "the work lint": "ok"},
	}
	got := Hygiene([]string{"HostX", "10.9.8.7"}, in)
	want := []string{
		`a path: "HostX" in "docs/HOSTX.md"`,
		`an added line: "HostX" in "ssh to hostx.lan first"`,
		`a commit message: "HostX" in "Fix the deploy  as seen on HostX"`,
		`the branch name: "HostX" in "bot/fix-hostx"`,
		`the work summary: "HostX" in "Tested on hOsTx."`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Hygiene =\n%q\nwant\n%q", got, want)
	}
	if got := Hygiene(nil, in); len(got) != 0 {
		t.Errorf("no deny list: %q", got)
	}
	if got := Hygiene([]string{"", "  "}, in); len(got) != 0 {
		t.Errorf("blank terms match: %q", got)
	}
	// A long line is shown as 80 code points around the hit.
	long := strings.Repeat("é", 200) + "10.9.8.7" + strings.Repeat("ü", 200)
	hits := Hygiene([]string{"10.9.8.7"}, HygieneIn{Added: []string{long}})
	if len(hits) != 1 {
		t.Fatalf("hits %q", hits)
	}
	snippet := hits[0][strings.Index(hits[0], " in ")+5 : len(hits[0])-1]
	if utf8.RuneCountInString(snippet) != 80 || !strings.Contains(snippet, "10.9.8.7") {
		t.Errorf("snippet %q (%d code points)", snippet, utf8.RuneCountInString(snippet))
	}
}

// hygieneRig is a rig whose main already holds the term (public text),
// with a hygiene project and walls.
func hygieneRig(t *testing.T) *rig {
	r := newRig(t)
	r.repo.Write("docs/hostx.md", "about hostx\n")
	r.repo.Write("README.md", "hello\nsee hostx\n")
	r.repo.Commit("main names it")
	return r
}

func hygieneConfig() (cfgText, projText string) {
	return "[gate]\nallowed_paths = [\"*.md\"]\n[hygiene]\ndeny = [\"hostx\"]\n",
		"hygiene = true\nwalls = [\"docs/rules.md\"]\n"
}

// What a branch removes is public already: a deleted line or a deleted
// path is no hit.
func TestHygieneIgnoresRemovedText(t *testing.T) {
	r := hygieneRig(t)
	r.git(r.repo.Dir, "checkout", "-q", "-b", "b")
	r.git(r.repo.Dir, "rm", "-q", "docs/hostx.md")
	r.repo.Write("README.md", "hello\nsee the helper\n")
	r.repo.Commit("tidy the readme")
	r.git(r.repo.Dir, "checkout", "-q", "main")
	rest, proj := hygieneConfig()
	cfg, p := r.config(proj, rest)
	tk := gateTicket(t, `{"work": {"status": "done", "summary": "s", "category": "docs"},
		"review": {"verdict": "pass", "runtime_behaviour": "no"}}`)
	g, err := r.runner.Run(context.Background(), cfg, p, tk)
	if err != nil {
		t.Fatal(err)
	}
	if !g.AutoEligible || g.HygieneFindings != 0 {
		t.Errorf("gate %+v", g)
	}
	out, err := os.ReadFile(filepath.Join(r.paths.TicketLogs("T0001"), "gate-output.txt"))
	if err != nil || string(out) != "ok" {
		t.Errorf("gate-output.txt: %q %v", out, err)
	}
}

// Run scans everything the PR will publish: the kept paths, the added
// lines, the messages, the branch and the report texts. The hits go first
// in gate-output.txt; the blocker counts them and names no term.
func TestRunScansWhatThePRPublishes(t *testing.T) {
	r := hygieneRig(t)
	r.repo.BranchWith("bot/hostx-fix", map[string]string{"notes/HOSTX.md": "x\n", "docs/rules.md": "added hostx\n"})
	rest, proj := hygieneConfig()
	cfg, p := r.config(proj, rest)
	tk := decodeText(t, `{"id": "T0001", "title": "t", "body": "", "door": "human", "project": "gem", "column": "gate",
		"branch": "bot/hostx-fix", "reports": {"work": {"status": "done", "summary": "ran on hostx", "category": "docs"},
		"review": {"verdict": "pass", "runtime_behaviour": "no"}}, "history": [], "agent": null}`)
	g, err := r.runner.Run(context.Background(), cfg, p, tk)
	if err != nil {
		t.Fatal(err)
	}
	if g.HygieneFindings != 5 || g.AutoEligible || !slices.Equal(g.Walls, []string{"docs/rules.md"}) {
		t.Errorf("gate %+v", g)
	}
	if !slices.Contains(g.Blockers, "public-repo hygiene: 5 finding(s), see the gate output") ||
		!slices.Contains(g.Blockers, "touches the walls") {
		t.Errorf("blockers %q", g.Blockers)
	}
	out, err := os.ReadFile(filepath.Join(r.paths.TicketLogs("T0001"), "gate-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`a path: "hostx" in "notes/HOSTX.md"`,
		`an added line: "hostx" in "added hostx"`, `a commit message: "hostx" in "bot/hostx-fix"`,
		`the branch name: "hostx"`, `the work summary: "hostx" in "ran on hostx"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("gate-output.txt lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(string(out), "public-repo hygiene:\n") || !strings.HasSuffix(string(out), "\n\nok") {
		t.Errorf("gate-output.txt:\n%s", out)
	}

	// Decide (checks that ran on another host) scans the same commits.
	tk2 := ticket.Clone(tk)
	d, err := r.runner.Decide(context.Background(), cfg, p, tk2, Remote{Head: g.Head, Base: g.Base, ChecksPass: true, Output: "remote ok", Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if d.HygieneFindings != 5 || !slices.Equal(d.Walls, g.Walls) {
		t.Errorf("decided %+v", d)
	}

	// An ordinary project is not scanned, but its walls still count.
	cfg, p = r.config("walls = [\"docs/rules.md\"]\n", rest)
	g, err = r.runner.Run(context.Background(), cfg, p, ticket.Clone(tk))
	if err != nil {
		t.Fatal(err)
	}
	if g.HygieneFindings != 0 || !slices.Equal(g.Blockers, []string{"touches the walls"}) {
		t.Errorf("ordinary project %+v", g)
	}
}

// The report fields PRBody prints, under the names the hits give.
func TestPublicTexts(t *testing.T) {
	tk := gateTicket(t, `{"work": {"summary": "S", "tests": "T", "lint": "L", "category": "c", "files": ["x"]},
		"review": {"verdict": "V", "runtime_behaviour": false, "summary": "private"}}`)
	got := PublicTexts(tk)
	want := map[string]string{"the work summary": "S", "the work tests": "T", "the work lint": "L",
		"the review verdict": "V", "the review runtime_behaviour": "False"}
	if len(got) != len(want) {
		t.Errorf("PublicTexts = %q", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q; want %q", k, got[k], v)
		}
	}
	if got := PublicTexts(gateTicket(t, `{}`)); len(got) != 0 {
		t.Errorf("no reports: %q", got)
	}
}

func TestWallsMatchesPatterns(t *testing.T) {
	cfg := mustConfig(t, "[projects.p]\npath = \"/p\"\nwalls = [\"internal/gate/*\", \"deploy.sh\"]\n")
	p, _ := cfg.Project("p")
	got := Walls(*p, []string{"README.md", "internal/gate/x.go", "deploy.sh", "internal/gatex.go"})
	if want := []string{"internal/gate/x.go", "deploy.sh"}; !slices.Equal(got, want) {
		t.Errorf("Walls = %q; want %q", got, want)
	}
	if got := Walls(config.Project{}, []string{"a"}); len(got) != 0 {
		t.Errorf("no walls: %q", got)
	}
}

// A wall file blocks auto-merge even inside the allowed paths, and is
// recorded on the gate.
func TestEvaluateWallsBlock(t *testing.T) {
	cfg := mustConfig(t, "[gate]\nallowed_paths = [\"*.md\"]\n")
	tk := gateTicket(t, `{"work": {"category": "docs", "summary": "s"}, "review": {"runtime_behaviour": "no"}}`)
	g, err := Evaluate(GateIn{Config: cfg, Ticket: tk, Files: []string{"agent-rules.md"}, Size: 1, ChecksPass: true,
		Walls: []string{"agent-rules.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if g.AutoEligible || !slices.Equal(g.Blockers, []string{"touches the walls"}) || !slices.Equal(g.Walls, []string{"agent-rules.md"}) {
		t.Errorf("gate %+v", g)
	}
	tk.Gate = ticket.Some(g)
	if !strings.Contains(string(ticket.Encode(tk)), `"walls": [`) {
		t.Error("walls not saved")
	}
}

// The hygiene blocker reaches the public PR body: it counts and names no
// term.
func TestEvaluateHygieneBlocks(t *testing.T) {
	cfg := mustConfig(t, "[gate]\nallowed_paths = [\"*.md\"]\n")
	tk := gateTicket(t, `{"work": {"category": "docs", "summary": "s"}, "review": {"runtime_behaviour": "no"}}`)
	hits := []string{`an added line: "hostx" in "hostx"`, `the branch name: "10.9.8.7" in "bot/10.9.8.7"`}
	g, err := Evaluate(GateIn{Config: cfg, Ticket: tk, Files: []string{"a.md"}, Size: 1, ChecksPass: true, Hygiene: hits})
	if err != nil {
		t.Fatal(err)
	}
	if g.AutoEligible || !slices.Equal(g.Blockers, []string{"public-repo hygiene: 2 finding(s), see the gate output"}) || g.HygieneFindings != 2 {
		t.Errorf("gate %+v", g)
	}
	tk.Gate = ticket.Some(g)
	saved := string(ticket.Encode(tk))
	if strings.Contains(saved, "hostx") || strings.Contains(saved, "10.9.8.7") || strings.Contains(saved, `"walls"`) {
		t.Errorf("the saved gate carries a term or walls:\n%s", saved)
	}
}

// Checks that pass with hygiene hits still need a person, as a failed gate.
func TestRecordHygieneNeedsHuman(t *testing.T) {
	cfg := mustConfig(t, "")
	tk := gateTicket(t, `{}`)
	g := ticket.Gate{ChecksPass: true, HygieneFindings: 3, Blockers: []string{"public-repo hygiene: 3 finding(s), see the gate output"}, Host: "h"}
	Record(tk, cfg, g, time.Unix(0, 0))
	last := tk.History[len(tk.History)-1]
	if tk.Column != "needs_human" || last.Detail != "gate -> needs_human: gate: public-repo hygiene: 3 finding(s) (checks on h)" {
		t.Errorf("column %q, entry %+v", tk.Column, last)
	}
	if m, ok := last.Data.(ticket.Moved); !ok || m.Reason != model.ReasonGateFailed {
		t.Errorf("payload %+v", last.Data)
	}
}

// The push publishes every commit: a path and a line one commit adds and
// a later one takes out again are hits, though the net diff is clean.
func TestRunScansEveryCommit(t *testing.T) {
	r := hygieneRig(t)
	r.git(r.repo.Dir, "checkout", "-q", "-b", "b")
	r.repo.Write("notes/steps.md", "ssh hostx\n")
	r.repo.Write("notes/hostx.txt", "x\n")
	r.repo.Commit("add notes")
	r.git(r.repo.Dir, "rm", "-q", "notes/steps.md", "notes/hostx.txt")
	r.repo.Write("CHANGES.md", "tidied\n")
	r.repo.Commit("take them out")
	r.git(r.repo.Dir, "checkout", "-q", "main")
	rest, proj := hygieneConfig()
	cfg, p := r.config(proj, rest)
	tk := gateTicket(t, `{"work": {"status": "done", "summary": "s", "category": "docs"},
		"review": {"verdict": "pass", "runtime_behaviour": "no"}}`)
	g, err := r.runner.Run(context.Background(), cfg, p, tk)
	if err != nil {
		t.Fatal(err)
	}
	if g.HygieneFindings != 2 || g.AutoEligible {
		t.Errorf("gate %+v", g)
	}
	out, _ := os.ReadFile(filepath.Join(r.paths.TicketLogs("T0001"), "gate-output.txt"))
	for _, want := range []string{`a path: "hostx" in "notes/hostx.txt"`, `an added line: "hostx" in "ssh hostx"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("gate-output.txt lacks %q:\n%s", want, out)
		}
	}
}

// Moving a wall file out of the walls is a change to the walls, even to a
// path the allowed paths take.
func TestRunWallsSeeARenameOut(t *testing.T) {
	r := newRig(t)
	r.repo.Write("docs/rules.md", "the rules\n")
	r.repo.Commit("rules")
	r.git(r.repo.Dir, "checkout", "-q", "-b", "b")
	r.git(r.repo.Dir, "mv", "docs/rules.md", "x.md")
	r.repo.Commit("move them")
	r.git(r.repo.Dir, "checkout", "-q", "main")
	cfg, p := r.config("walls = [\"docs/rules.md\"]\n", "[gate]\nallowed_paths = [\"*.md\"]\n")
	tk := gateTicket(t, `{"work": {"status": "done", "summary": "s", "category": "docs"},
		"review": {"verdict": "pass", "runtime_behaviour": "no"}}`)
	g, err := r.runner.Run(context.Background(), cfg, p, tk)
	if err != nil {
		t.Fatal(err)
	}
	if g.AutoEligible || !slices.Equal(g.Walls, []string{"docs/rules.md"}) || !slices.Contains(g.Blockers, "touches the walls") {
		t.Errorf("gate %+v", g)
	}
}

// Failed checks with hygiene hits say both on the board.
func TestRecordFailedChecksStillReportHygiene(t *testing.T) {
	cfg := mustConfig(t, "")
	tk := gateTicket(t, `{}`)
	Record(tk, cfg, ticket.Gate{HygieneFindings: 2}, time.Unix(0, 0))
	last := tk.History[len(tk.History)-1]
	want := "gate -> needs_human: gate: tests or lint failed on a fresh checkout; public-repo hygiene: 2 finding(s)"
	if tk.Column != "needs_human" || last.Detail != want {
		t.Errorf("column %q, detail %q", tk.Column, last.Detail)
	}
}

// A letter whose lower case is longer in Python (U+0130) does not move the
// snippet off the hit: Go lowers rune for rune.
func TestHygieneSnippetHoldsTheHitPastWideLowerCase(t *testing.T) {
	text := strings.Repeat("\u0130", 100) + "hostx" + strings.Repeat("a", 100)
	hits := Hygiene([]string{"hostx"}, HygieneIn{Added: []string{text}})
	if len(hits) != 1 || !strings.Contains(hits[0], `in "`) || !strings.Contains(hits[0][strings.Index(hits[0], `in "`):], "hostx") {
		t.Errorf("hits %q", hits)
	}
}
