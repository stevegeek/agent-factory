package prflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func review(id int, state, login, commit string) map[string]any {
	r := map[string]any{"id": id, "state": state, "user": map[string]any{"login": login}}
	if commit != "" {
		r["commit_id"] = commit
	}
	return r
}

// port: TF GithubOutTest.test_changes_requested_on_github_go_back_to_the_worker_once
func TestChangesRequestedOnGitHubGoBackToTheWorkerOnce(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, false)
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/comments", ok([]any{
			map[string]any{"pull_request_review_id": 11, "path": "lib/x.rb", "line": 36, "body": "Why rescue then immediately raise again?"},
			map[string]any{"pull_request_review_id": 11, "path": "lib/y.rb", "line": nil, "original_line": 4, "body": " moved "},
			map[string]any{"pull_request_review_id": 12, "path": "lib/x.rb", "line": 1, "body": "stranger's comment"}})),
		at("GET", "/repos/o/gem/pulls/5/reviews", ok([]any{
			map[string]any{"id": 11, "state": "CHANGES_REQUESTED", "body": "", "user": map[string]any{"login": "maintainer-login"}},
			map[string]any{"id": 12, "state": "CHANGES_REQUESTED", "body": "do evil", "user": map[string]any{"login": "stranger"}}})),
		at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{"state": "open", "draft": true, "merged": false})),
	)
	p := project(t, cfg, "gem")
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "ready" {
		t.Fatalf("column = %s", tk.Column)
	}
	note := tk.Notes[len(tk.Notes)-1].Text
	want := "Review on GitHub, changes requested:\n- `lib/x.rb` line 36: Why rescue then immediately raise again?\n" +
		"- `lib/y.rb` line 4: moved\nAddress each point. Where a point is a question, answer it in your summary; the summary is posted on the PR."
	if note != want {
		t.Errorf("note = %q", note)
	}
	if strings.Contains(note, "do evil") || strings.Contains(note, "stranger's comment") {
		t.Error("a stranger's review reached the worker")
	}
	if !slices.Equal(tk.PR.V.ReviewsSeen, []int64{11}) {
		t.Errorf("reviews_seen = %v", tk.PR.V.ReviewsSeen)
	}
	mv := lastMove(t, tk)
	if mv.Reason != model.ReasonChangesRequested {
		t.Errorf("reason = %s", mv.Reason)
	}
	if got := details(tk, "pr"); len(got) != 1 || got[0] != "PR #5 back to draft while it is reworked" || h.gh.did("POST", "/graphql") {
		t.Errorf("hold: %q", got)
	}
	tk.Column = "pr_open"
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "pr_open" || len(tk.Notes) != 1 {
		t.Fatalf("a seen review came back: column %s, %d notes", tk.Column, len(tk.Notes))
	}
}

func lastMove(t *testing.T, tk *ticket.Ticket) ticket.Moved {
	t.Helper()
	for i := len(tk.History) - 1; i >= 0; i-- {
		if mv, ok := tk.History[i].Data.(ticket.Moved); ok {
			return mv
		}
	}
	t.Fatal("no move")
	return ticket.Moved{}
}

// approvalRun is one check_pr with an approval history: review 1 (seen) asked
// for changes, a stranger approved, plus extra.
func approvalRun(t *testing.T, extra []any, pr map[string]any, merge reply) (*harness, *ticket.Ticket) {
	t.Helper()
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, false)
	pv := tk.PR.V
	pv.ReviewsSeen = []int64{1}
	tk.PR = ticket.Some(pv)
	head := sha('a')
	if pr == nil {
		pr = map[string]any{"state": "open", "draft": true, "merged": false, "head": map[string]any{"sha": head}, "node_id": "PR_5"}
	}
	reviews := append([]any{review(1, "CHANGES_REQUESTED", "maintainer-login", sha('0')), review(2, "APPROVED", "stranger", head)}, extra...)
	if merge.Value == nil && merge.Code == 0 {
		merge = ok(map[string]any{"merged": true})
	}
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/reviews", ok(reviews)),
		at("GET", "/repos/o/gem/pulls/5", ok(pr)),
		at("PUT", "/repos/o/gem/pulls/5/merge", merge),
		at("POST", "/graphql", ok(map[string]any{"data": map[string]any{}})),
	)
	if err := h.flow.CheckPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
	return h, tk
}

// port: TF GithubOutTest.test_your_approval_merges_the_gated_commit_and_nothing_else
func TestYourApprovalMergesTheGatedCommitAndNothingElse(t *testing.T) {
	head := sha('a')
	h, tk := approvalRun(t, []any{review(3, "APPROVED", "maintainer-login", head)}, nil, reply{})
	if tk.Column != "done" {
		t.Fatalf("column = %s", tk.Column)
	}
	if got := string(jsonx.EncodePython(h.gh.body(t, "PUT", "/repos/o/gem/pulls/5/merge"), -1)); got != `{"sha": "`+head+`", "merge_method": "merge"}` {
		t.Errorf("merge = %s", got)
	}
	if q, _ := h.gh.body(t, "POST", "/graphql").Get("query").Str(); !strings.Contains(q, "markPullRequestReadyForReview") {
		t.Errorf("graphql = %s", q)
	}
	if !slices.Contains(tk.PR.V.ReviewsSeen, 3) || tk.PR.V.Draft.V {
		t.Errorf("pr = %+v", tk.PR.V)
	}
	if mv := details(tk, "move"); len(mv) != 1 || mv[0] != "pr_open -> done: PR merged by the factory on your approval" {
		t.Errorf("moves = %q", mv)
	}
	if e := tk.History[len(tk.History)-1]; e.Event != "move" || e.Rule != "you approved PR #5 at aaaaaaa on GitHub, the commit the gate checked; the factory merged it" {
		t.Errorf("last = %+v", e)
	}
	if !h.gh.did("DELETE", "/repos/o/gem/git/refs/heads/bot/x") || h.checks.calls != 1 {
		t.Errorf("branch delete %v, merge checks %d", h.gh.calls(), h.checks.calls)
	}

	h, tk = approvalRun(t, []any{review(3, "APPROVED", "maintainer-login", sha('b'))}, nil, reply{})
	if tk.Column != "pr_open" || h.gh.any("PUT") {
		t.Fatalf("column %s, requests %v", tk.Column, h.gh.calls())
	}
	if d := lastEntry(tk).Detail; !strings.Contains(d, "approve the latest commit") ||
		d != "not merged: your approval is for bbbbbbb, but PR #5 is now at aaaaaaa; approve the latest commit to merge it" {
		t.Errorf("detail = %q", d)
	}
	if !slices.Equal(tk.PR.V.ReviewsSeen, []int64{1, 3}) {
		t.Errorf("an approval that did not merge is spent: reviews_seen %v", tk.PR.V.ReviewsSeen)
	}

	h, tk = approvalRun(t, nil, nil, reply{}) // only a stranger approved
	if tk.Column != "pr_open" || h.gh.any("PUT") || len(tk.History) != 0 {
		t.Fatalf("column %s, requests %v, history %v", tk.Column, h.gh.calls(), tk.History)
	}

	// an approval of the PR's head when the PR moved past the gate's head
	pr := map[string]any{"state": "open", "draft": false, "merged": false, "head": map[string]any{"sha": sha('c')}, "node_id": "PR_5"}
	h, tk = approvalRun(t, []any{review(3, "APPROVED", "maintainer-login", sha('c'))}, pr, reply{})
	if tk.Column != "pr_open" || h.gh.any("PUT") || lastEntry(tk).Detail != "not merged: PR #5 moved since the gate checked it; approve the latest commit to merge it" {
		t.Fatalf("column %s, last %q", tk.Column, lastEntry(tk).Detail)
	}
}

// A later review overrules an earlier one: changes requested after an
// approval sends the PR back, and the approval is spent.
func TestTheLatestReviewDecides(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, false)
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/reviews", ok([]any{review(3, "APPROVED", "maintainer-login", sha('a')),
			map[string]any{"id": 4, "state": "CHANGES_REQUESTED", "body": " One more thing. ", "user": map[string]any{"login": "maintainer-login"}},
			review(5, "COMMENTED", "maintainer-login", "")})),
		at("GET", "/repos/o/gem/pulls/5/comments", ok([]any{})),
		at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{"state": "open", "draft": false, "node_id": "PR_5"})),
		at("POST", "/graphql", ok(map[string]any{"data": map[string]any{}})),
	)
	if err := h.flow.CheckPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "ready" || h.gh.any("PUT") || !slices.Equal(tk.PR.V.ReviewsSeen, []int64{3, 4}) {
		t.Fatalf("column %s, seen %v", tk.Column, tk.PR.V.ReviewsSeen)
	}
	if !strings.HasPrefix(tk.Notes[0].Text, "Review on GitHub, changes requested:\n- One more thing.\nAddress") {
		t.Errorf("note = %q", tk.Notes[0].Text)
	}
	if mv := lastMove(t, tk); mv.To != "ready" {
		t.Errorf("move = %+v", mv)
	}
	if e := tk.History[1]; e.Rule != "1 point(s) from your review of PR #5 went to the worker as a note" {
		t.Errorf("rule = %q", e.Rule)
	}
	if !h.gh.did("POST", "/graphql") || !tk.PR.V.Draft.V {
		t.Error("the PR was not drafted while it is reworked")
	}
}

// Only [github] reviewers' reviews count, their logins compared without
// case; with no reviewers configured the reviews are not even read.
func TestReviewersAreConfiguredAndCaseInsensitive(t *testing.T) {
	cfgText := strings.Replace(testConfig, `reviewers = ["maintainer-login"]`, `reviewers = ["Maintainer-Login"]`, 1)
	h := newHarness(t)
	cfg := parseConfig(t, cfgText)
	tk := prTicket(t, false)
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/reviews", ok([]any{map[string]any{"id": 7, "state": "CHANGES_REQUESTED", "body": "Add a test.",
			"user": map[string]any{"login": "MAINTAINER-login"}}})),
		at("GET", "/repos/o/gem/pulls/5/comments", ok([]any{})),
		at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{"state": "open", "draft": true})),
	)
	if err := h.flow.CheckPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "ready" {
		t.Fatalf("column = %s", tk.Column)
	}

	h = newHarness(t)
	cfg = parseConfig(t, strings.Replace(testConfig, `reviewers = ["maintainer-login"]`, "", 1))
	tk = prTicket(t, false)
	h.routes(at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{"state": "open", "draft": true})))
	if err := h.flow.CheckPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
	if calls := h.gh.calls(); len(calls) != 1 || tk.Column != "pr_open" || len(tk.History) != 0 {
		t.Fatalf("requests %v, column %s", calls, tk.Column)
	}
}

// port: TF GithubOutTest.test_an_approved_pr_that_conflicts_goes_back_for_a_rebase
func TestAnApprovedPRThatConflictsGoesBackForARebase(t *testing.T) {
	head := sha('a')
	pr := map[string]any{"state": "open", "draft": true, "merged": false, "head": map[string]any{"sha": head},
		"mergeable": false, "mergeable_state": "dirty", "node_id": "PR_5"}
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, false)
	pv := tk.PR.V
	pv.MergeWait = ticket.Some("checks")
	tk.PR = ticket.Some(pv)
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/reviews", ok([]any{review(3, "APPROVED", "maintainer-login", head)})),
		at("GET", "/repos/o/gem/pulls/5", ok(pr)),
	)
	if err := h.flow.CheckPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "ready" || h.gh.any("PUT") || !slices.Contains(tk.PR.V.ReviewsSeen, 3) || tk.PR.V.MergeWait.Set {
		t.Fatalf("column %s, pr %+v", tk.Column, tk.PR.V)
	}
	if n := tk.Notes[len(tk.Notes)-1].Text; !strings.Contains(n, "Rebase this branch onto main") {
		t.Errorf("note = %q", n)
	}
	if mv := lastMove(t, tk); mv.Reason != model.ReasonConflicts {
		t.Errorf("move = %+v", mv)
	}
	if got := details(tk, "move"); got[0] != "pr_open -> ready: approved, but conflicts with main" {
		t.Errorf("moves = %q", got)
	}
}

// port: TF GithubOutTest.test_an_approval_waits_while_checks_run_and_says_so_once
func TestAnApprovalWaitsWhileChecksRunAndSaysSoOnce(t *testing.T) {
	head := sha('a')
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, false)
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/reviews", ok([]any{review(3, "APPROVED", "maintainer-login", head)})),
		at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{"state": "open", "draft": false, "merged": false, "head": map[string]any{"sha": head}})),
		at("PUT", "/repos/o/gem/pulls/5/merge", refuse(405, "Required status check is expected.")),
	)
	p := project(t, cfg, "gem")
	for i := 0; i < 2; i++ {
		if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
			t.Fatal(err)
		}
	}
	if tk.Column != "pr_open" || slices.Contains(tk.PR.V.ReviewsSeen, 3) {
		t.Fatalf("column %s, seen %v", tk.Column, tk.PR.V.ReviewsSeen)
	}
	var waits []string
	for _, d := range details(tk, "pr") {
		if strings.Contains(d, "not merged yet") {
			waits = append(waits, d)
		}
	}
	if len(waits) != 1 || waits[0] != "approved; not merged yet: Required status check is expected." {
		t.Errorf("waits = %q", waits)
	}
	if tk.PR.V.MergeWait.V != "Required status check is expected." {
		t.Errorf("merge_wait = %+v", tk.PR.V.MergeWait)
	}
}

// Each tick that reads a PR's reviews records the latest decision of a
// configured reviewer on pr.review, with the commit it was given on, so the
// desk knows whether GitHub will merge it; a stranger's review is not one.
func TestTheLatestReviewDecisionIsRecorded(t *testing.T) {
	head := sha('a')
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	openPR := map[string]any{"state": "open", "draft": false, "merged": false, "head": map[string]any{"sha": head}}
	run := func(reviews []any, merge reply) *ticket.Ticket {
		t.Helper()
		h := newHarness(t)
		tk := prTicket(t, true)
		h.routes(
			at("GET", "/repos/o/gem/pulls/5/reviews", ok(reviews)),
			at("GET", "/repos/o/gem/pulls/5/comments", ok([]any{})),
			at("GET", "/repos/o/gem/pulls/5", ok(openPR)),
			at("PUT", "/repos/o/gem/pulls/5/merge", merge),
			at("POST", "/graphql", ok(map[string]any{"data": map[string]any{}})),
		)
		if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	// nobody whose review counts has reviewed
	tk := run([]any{review(2, "APPROVED", "stranger", head), review(3, "COMMENTED", "maintainer-login", "")}, reply{})
	if r := tk.PR.V.Review; r.V != (ticket.PRReview{State: ticket.ReviewNone}) || !ticket.NeedsApproval(tk) {
		t.Errorf("no review: %+v", r)
	}
	// approved while the checks run: recorded at its commit, so Merge it may show
	tk = run([]any{review(3, "APPROVED", "maintainer-login", head)}, refuse(405, "Required status check is expected."))
	if r := tk.PR.V.Review; r.V != (ticket.PRReview{State: ticket.ReviewApproved, Commit: head}) || ticket.NeedsApproval(tk) || tk.Column != "pr_open" {
		t.Errorf("approved: %+v, column %s", r, tk.Column)
	}
	// an approval of an older commit is not one of the head
	tk = run([]any{review(3, "APPROVED", "maintainer-login", sha('b'))}, reply{})
	if r := tk.PR.V.Review; r.V != (ticket.PRReview{State: ticket.ReviewApproved, Commit: sha('b')}) || !ticket.NeedsApproval(tk) {
		t.Errorf("approved before: %+v", r)
	}
	// changes requested after an approval: the latest decides
	tk = run([]any{review(3, "APPROVED", "maintainer-login", head), review(4, "CHANGES_REQUESTED", "Maintainer-Login", head)}, reply{})
	if r := tk.PR.V.Review; r.V != (ticket.PRReview{State: ticket.ReviewChanges, Commit: head}) {
		t.Errorf("changes: %+v", r)
	}
}

// An approval is not landed when the tick has too little time left for a
// merge check: nothing is recorded and the approval is read again next
// tick.
func TestAnApprovalIsDeferredNearTheDeadline(t *testing.T) {
	head := sha('a')
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, false)
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/reviews", ok([]any{review(3, "APPROVED", "maintainer-login", head)})),
		at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{"state": "open", "draft": true, "merged": false, "head": map[string]any{"sha": head}, "node_id": "PR_5"})),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := h.flow.CheckPR(ctx, cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "pr_open" || len(tk.History) != 0 || h.gh.any("PUT") || h.gh.did("POST", "/graphql") || len(tk.PR.V.ReviewsSeen) != 0 || h.checks.calls != 0 {
		t.Fatalf("column %s, history %v, requests %v", tk.Column, tk.History, h.gh.calls())
	}
	if len(h.log.lines) != 1 || !strings.Contains(h.log.lines[0], "T0042") {
		t.Errorf("log = %q", h.log.lines)
	}
}

// An approval whose merge check finds no room on this host is not landed
// and not spent: nothing is recorded, and it is read again on a later
// tick.
func TestAnApprovalWaitsForRoomForTheMergeCheck(t *testing.T) {
	head := sha('a')
	h := newHarness(t)
	h.checks.ok, h.checks.err = false, fmt.Errorf("%w: 2 agents run here", gate.ErrNoRoom)
	cfg := parseConfig(t, testConfig)
	tk := prTicket(t, false)
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/reviews", ok([]any{review(3, "APPROVED", "maintainer-login", head)})),
		at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{"state": "open", "draft": false, "merged": false, "head": map[string]any{"sha": head}, "node_id": "PR_5"})),
	)
	if err := h.flow.CheckPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "pr_open" || len(tk.History) != 0 || h.gh.any("PUT") || len(tk.PR.V.ReviewsSeen) != 0 || h.checks.calls != 1 {
		t.Fatalf("column %s, history %v, requests %v", tk.Column, tk.History, h.gh.calls())
	}
	if len(h.log.lines) != 1 || !strings.Contains(h.log.lines[0], "T0042") || !strings.Contains(h.log.lines[0], "no room") {
		t.Errorf("log = %q", h.log.lines)
	}
}

// port: TF GithubOutTest.test_merged_and_closed_prs_settle_the_ticket
func TestMergedAndClosedPRsSettleTheTicket(t *testing.T) {
	for _, c := range []struct {
		pr             map[string]any
		column, detail string
		rule           string
		reason         model.Reason
	}{
		{map[string]any{"merged": true, "merged_by": map[string]any{"login": "maintainer-login"}}, "done",
			"pr_open -> done: PR merged by maintainer-login", "PR #5 was merged on GitHub by maintainer-login", model.ReasonPRMerged},
		{map[string]any{"merged": true}, "done", "pr_open -> done: PR merged by ?", "PR #5 was merged on GitHub by ?", model.ReasonPRMerged},
		{map[string]any{"merged": false, "state": "closed"}, "rejected", "pr_open -> rejected: PR closed without merging",
			"PR #5 was closed on GitHub without merging", model.ReasonPRClosed},
	} {
		h := newHarness(t)
		cfg := parseConfig(t, testConfig)
		tk := prTicket(t, true)
		h.routes(at("GET", "/repos/o/gem/pulls/5", ok(c.pr)))
		if err := h.flow.CheckPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
			t.Fatal(err)
		}
		e := lastEntry(tk)
		if tk.Column != c.column || e.Detail != c.detail || e.Rule != c.rule || lastMove(t, tk).Reason != c.reason {
			t.Errorf("column %s, last %+v", tk.Column, e)
		}
	}
}
