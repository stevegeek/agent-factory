package prflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// port: TF GithubOutTest.test_sending_a_pr_back_turns_it_back_to_draft
func TestSendingAPRBackTurnsItBackToDraft(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, true)
	h.routes(fakeRepoPR(openPR5(false))...)
	ticket.Move(tk, "ready", "", "", "", "", startTime)
	if err := h.flow.HoldPR(context.Background(), cfg, tk); err != nil {
		t.Fatal(err)
	}
	if q, _ := h.gh.body(t, "POST", "/graphql").Get("query").Str(); !strings.Contains(q, "convertPullRequestToDraft") {
		t.Errorf("graphql = %s", q)
	}
	if !tk.PR.V.Draft.V || lastEntry(tk).Detail != "PR #5 back to draft while it is reworked" {
		t.Fatalf("pr %+v, last %+v", tk.PR.V, lastEntry(tk))
	}
}

// A hold that fails is recorded on the ticket and reported, so the CLI can
// warn that the PR is still mergeable.
func TestAFailedHoldIsRecordedAndReported(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, true)
	h.routes(at("GET", "/repos/o/gem/pulls/5", ok(openPR5(false))), at("POST", "/graphql", ok(map[string]any{"errors": []any{map[string]any{"message": "no"}}})))
	err := h.flow.HoldPR(context.Background(), cfg, tk)
	var nh *NotHeldError
	if !errors.As(err, &nh) || err.Error() != "PR #5 is still open for merging; it could not be turned back to draft" {
		t.Fatalf("err = %v", err)
	}
	if d := lastEntry(tk).Detail; d != "github: could not turn PR #5 back to draft: github: convertPullRequestToDraft: [{'message': 'no'}]" || lastEntry(tk).Event != "error" {
		t.Errorf("last = %+v", lastEntry(tk))
	}
	if tk.PR.V.Draft.V {
		t.Error("draft recorded although the PR is not a draft")
	}
}

func syncFor(h *harness) Sync {
	return Sync{Flow: h.flow, Board: h.board, Config: configSource{cfg: parseConfig(h.t, testConfig)}}
}

// `factory pr T` opens the PR at once and gives its URL.
func TestSyncOpenOpensThePRAndGivesItsURL(t *testing.T) {
	h := newHarness(t)
	tk := readyTicket(t, true, sha('a'))
	h.board.put(t, tk)
	h.setHead("bot/x", sha('a'))
	h.routes(at("POST", "/repos/o/gem/pulls/5/requested_reviewers", ok(map[string]any{})),
		at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 5, "html_url": "https://github.com/o/gem/pull/5", "draft": false})))
	url, err := syncFor(h).Open(context.Background(), "T0042")
	if err != nil || url != "https://github.com/o/gem/pull/5" {
		t.Fatalf("url %q, err %v", url, err)
	}
	if got := h.board.load(t, "T0042"); got.Column != "pr_open" || got.Updated != "2026-10-17T09:00:00+00:00" {
		t.Errorf("column %s, updated %s", got.Column, got.Updated)
	}
	if strings.Join(h.board.logs, "\n") != "T0042 move ready_for_pr -> pr_open: PR https://github.com/o/gem/pull/5\nT0042 review requested maintainer-login" {
		t.Errorf("log = %q", h.board.logs)
	}
}

// A PR that cannot be opened leaves the ticket as it was.
func TestSyncOpenRefusesAndSavesNothing(t *testing.T) {
	h := newHarness(t)
	tk := readyTicket(t, true, sha('a'))
	h.board.put(t, tk)
	h.setHead("bot/x", sha('b'))
	before := string(h.board.files["T0042"])
	_, err := syncFor(h).Open(context.Background(), "t0042")
	var r *Refusal
	if !errors.As(err, &r) || len(h.board.saves) != 0 || string(h.board.files["T0042"]) != before {
		t.Fatalf("err %v, saves %v", err, h.board.saves)
	}

	board := decode(t, `{"id": "T0043", "schema": 1, "mode": "board", "project": "gem", "title": "x", "column": "open", "history": []}`)
	h.board.put(t, board)
	_, err = syncFor(h).Open(context.Background(), "T0043")
	if !errors.As(err, &r) || r.Code != "factory_ticket" || err.Error() != "T0043 is on a board project; use `factory ticket …`" {
		t.Fatalf("err = %v", err)
	}

	gone := readyTicket(t, true, sha('a'))
	gone.ID, gone.Project = "T0044", "elsewhere"
	h.board.put(t, gone)
	if _, err = syncFor(h).Open(context.Background(), "T0044"); !errors.As(err, &r) || err.Error() != "elsewhere has no GitHub repository" {
		t.Fatalf("err = %v", err)
	}
}

// `factory move T ready` out of pr_open drafts the PR before it returns:
// Sync.Hold holds the ticket's PR and saves; a failure is saved
// on the ticket and comes back as the warning.
func TestSyncHoldDraftsThePRAndWarnsWhenItCannot(t *testing.T) {
	h := newHarness(t)
	tk := prTicket(t, true)
	ticket.Move(tk, "ready", "", "by human", "", "maintainer", startTime)
	h.board.put(t, tk)
	h.routes(fakeRepoPR(openPR5(false))...)
	if err := syncFor(h).Hold(context.Background(), "T0042"); err != nil {
		t.Fatal(err)
	}
	if got := h.board.load(t, "T0042"); !got.PR.V.Draft.V {
		t.Errorf("pr = %+v", got.PR.V)
	}

	h = newHarness(t)
	h.board.put(t, prTicket(t, true))
	h.routes(at("GET", "/repos/o/gem/pulls/5", refuse(502, "Bad Gateway")))
	err := syncFor(h).Hold(context.Background(), "T0042")
	var nh *NotHeldError
	if !errors.As(err, &nh) || nh.Number != 5 {
		t.Fatalf("err = %v", err)
	}
	if got := h.board.load(t, "T0042"); lastEntry(got).Event != "error" || len(h.board.saves) != 1 {
		t.Errorf("last %+v, saves %v", lastEntry(got), h.board.saves)
	}
}

// port: TF BranchTest.test_branch_name_has_no_ticket_id_and_is_unique
func TestBranchNameHasNoTicketIDAndIsUnique(t *testing.T) {
	g := &fakeGit{heads: map[string]string{}}
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	first, err := PickBranch(context.Background(), g, p, "Ignore generated test/tmp files!", nil)
	if err != nil || first != "bot/ignore-generated-test-tmp-files" {
		t.Fatalf("branch %q, err %v", first, err)
	}
	if strings.Contains(strings.ToLower(first), "t0007") {
		t.Error("the branch names the ticket")
	}
	g.heads["refs/heads/"+first] = sha('a')
	if got, _ := PickBranch(context.Background(), g, p, "Ignore generated test/tmp files!", nil); got != first+"-2" {
		t.Errorf("second = %q", got)
	}
	g.heads["refs/heads/"+first+"-2"] = sha('b')
	if got, _ := PickBranch(context.Background(), g, p, "Ignore generated test/tmp files!", nil); got != first+"-3" {
		t.Errorf("third = %q", got)
	}
	if got, _ := PickBranch(context.Background(), g, p, "!!!", nil); got != "bot/change" {
		t.Errorf("empty slug = %q", got)
	}
}

// On a hygiene project a title that names a denied term gives the neutral
// branch bot/change, numbered like any other; a clean title, or an
// ordinary project, keeps its slug.
func TestPickBranchNeutralOnHit(t *testing.T) {
	g := &fakeGit{heads: map[string]string{}}
	cfg := parseConfig(t, hygieneConfig())
	p := project(t, cfg, "gem")
	deny := cfg.Hygiene.Deny
	first, err := PickBranch(context.Background(), g, p, "Fix the HOSTX mounts", deny)
	if err != nil || first != "bot/change" {
		t.Fatalf("branch %q, err %v", first, err)
	}
	g.heads["refs/heads/"+first] = sha('a')
	if got, _ := PickBranch(context.Background(), g, p, "Fix the hostx mounts", deny); got != "bot/change-2" {
		t.Errorf("second = %q", got)
	}
	if got, _ := PickBranch(context.Background(), g, p, "Fix the mounts", deny); got != "bot/fix-the-mounts" {
		t.Errorf("clean = %q", got)
	}
	if got, _ := PickBranch(context.Background(), g, project(t, cfg, "local"), "Fix the hostx mounts", deny); got != "bot/fix-the-hostx-mounts" {
		t.Errorf("ordinary project = %q", got)
	}
}
