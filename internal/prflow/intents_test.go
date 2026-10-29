package prflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The intents: Foreman records "hold the PR", "open the PR" and "merge it"
// on a ticket's pr; the tick carries them out first in its GitHub pass, a
// hold in any column, then an open in ready_for_pr or pr_open, then merges
// in pr_open. An intent is cleared only in the save that records what
// happened; a failure retrying can fix keeps it.
//
// These test the intent cases. The Python factory never
// had these intents, so there is no Python test or golden to compare with.

const intentConfig = testConfig

var intentNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const asked = "2026-03-01T11:00:00+00:00"

func intentHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.clock = nil
	h.flow.Clock = fixedClock(intentNow)
	h.board.clock = fixedClock(intentNow)
	return h
}

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

// intentTicket is a ticket "Fix it" on bot/fix-it with PR
// number n, plus the pr fields given (JSON members).
func (h *harness) intentTicket(n int, column, project string, number int, pr string) *ticket.Ticket {
	h.t.Helper()
	if pr != "" {
		pr = ", " + pr
	}
	tk := decode(h.t, fmt.Sprintf(`{"id": "T%04d", "project": %q, "title": "Fix it", "body": "", "door": "human",
		"column": %q, "kind": null, "branch": "bot/fix-it", "agent": null, "reports": {}, "created": %q, "history": [],
		"gate": {"auto_eligible": true, "head": %q, "blockers": []},
		"pr": {"number": %d, "url": "https://example.test/pr/%d", "draft": false%s}}`,
		n, project, column, asked, sha('a'), number, number, pr))
	h.board.put(h.t, tk)
	return tk
}

func (h *harness) drain(cfgText string) {
	h.t.Helper()
	if err := h.flow.DrainIntents(context.Background(), parseConfig(h.t, cfgText), h.board.all(h.t)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) pass(ctx context.Context, cfgText string) {
	h.t.Helper()
	if err := h.flow.Pass(ctx, parseConfig(h.t, cfgText), h.board.all(h.t)); err != nil {
		h.t.Fatal(err)
	}
}

func prRoute(n int, pr map[string]any) route {
	return at("GET", fmt.Sprintf("/repos/o/gem/pulls/%d", n), ok(pr))
}

// A hold runs in any column and puts the PR back to draft.
func TestAHoldRunsInAnyColumnAndPutsThePRBackToDraft(t *testing.T) {
	h := intentHarness(t)
	var ids []ticket.ID
	for i, c := range []string{"ready", "rejected", "triage"} {
		ids = append(ids, h.intentTicket(i+1, c, "gem", 7, `"hold_requested": "`+asked+`"`).ID)
	}
	h.routes(prRoute(7, map[string]any{"state": "open", "draft": false, "node_id": "N"}), at("POST", "/graphql", ok(map[string]any{})))
	h.drain(intentConfig)
	for _, id := range ids {
		got := h.board.load(t, id)
		if got.PR.V.HoldRequested.Set || !got.PR.V.Draft.V || lastEntry(got).Detail != "PR #7 back to draft while it is reworked" {
			t.Errorf("%s: pr %+v, last %+v", id, got.PR.V, lastEntry(got))
		}
	}
	var seq []string
	for _, c := range h.gh.calls() {
		seq = append(seq, c.String())
	}
	if strings.Join(seq, ",") != strings.TrimSuffix(strings.Repeat("GET /repos/o/gem/pulls/7,POST /graphql,", 3), ",") {
		t.Errorf("requests = %v", seq)
	}
}

// An open runs only in ready_for_pr or pr_open.
func TestAnOpenRunsOnlyInReadyForPROrPROpen(t *testing.T) {
	h := intentHarness(t)
	a := h.intentTicket(1, "ready_for_pr", "gem", 0, `"open_requested": "`+asked+`"`)
	b := h.intentTicket(2, "pr_open", "gem", 7, `"open_requested": "`+asked+`"`)
	c := h.intentTicket(3, "ready", "gem", 8, `"open_requested": "`+asked+`"`)
	before := string(h.board.files[c.ID])
	h.setHead("bot/fix-it", sha('a'))
	h.routes(
		at("POST", "/repos/o/gem/pulls/12/requested_reviewers", ok(map[string]any{})),
		at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 12, "html_url": "https://github.com/o/gem/pull/12", "draft": false})),
	)
	h.drain(intentConfig)
	ga, gb := h.board.load(t, a.ID), h.board.load(t, b.ID)
	if ga.PR.V.OpenRequested.Set || ga.Column != "pr_open" || ga.PR.V.Number != 12 {
		t.Errorf("a: column %s, pr %+v", ga.Column, ga.PR.V)
	}
	// pr_open is drained too, but open_pr itself wants ready_for_pr
	if gb.PR.V.OpenRequested.Set || lastEntry(gb).Event != "pr" || lastEntry(gb).Detail != "not opened (asked "+asked+"): T0002 is not ready for a PR" {
		t.Errorf("b: pr %+v, last %+v", gb.PR.V, lastEntry(gb))
	}
	if string(h.board.files[c.ID]) != before {
		t.Error("an open in another column is left as it is, as merge_requested is")
	}
	if len(h.git.pushes) != 1 {
		t.Errorf("pushes = %v", h.git.pushes)
	}
}

// A failed open is said once and cleared.
func TestAFailedOpenIsSaidOnceAndCleared(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "ready_for_pr", "gem", 0, `"open_requested": "`+asked+`"`)
	n := h.intentTicket(2, "ready_for_pr", "local", 0, `"open_requested": "2026-03-01T11:30:00+00:00"`)
	h.setHead("bot/fix-it", sha('b')) // moved since the gate
	h.drain(intentConfig)
	got := h.board.load(t, tk.ID)
	if got.PR.V.OpenRequested.Set || lastEntry(got).Event != "pr" ||
		lastEntry(got).Detail != "not opened (asked "+asked+"): the branch moved after Gate 1 checked it; send it through the gate again" {
		t.Errorf("pr %+v, last %+v", got.PR.V, lastEntry(got))
	}
	if d := lastEntry(h.board.load(t, n.ID)).Detail; d != "not opened (asked 2026-03-01T11:30:00+00:00): local has no GitHub repository" {
		t.Errorf("local: %q", d)
	}
	if len(got.History) != 1 || len(h.git.pushes) != 0 {
		t.Errorf("history %v, pushes %v", got.History, h.git.pushes)
	}
}

// Any other failure is an error event: a refusal GitHub will repeat is
// said and cleared.
func TestAnyOtherFailureIsAnErrorEvent(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "ready_for_pr", "gem", 0, `"open_requested": "`+asked+`"`)
	h.setHead("bot/fix-it", sha('a'))
	h.routes(at("POST", "/repos/o/gem/pulls", refuse(403, "Resource not accessible by integration")))
	h.drain(intentConfig)
	got := h.board.load(t, tk.ID)
	if got.PR.V.OpenRequested.Set || lastEntry(got).Event != "error" || lastEntry(got).Detail != "github: HTTP Error 403: Forbidden" {
		t.Errorf("pr %+v, last %+v", got.PR.V, lastEntry(got))
	}
}

// A failure retrying can fix (no answer, a 5xx) keeps the intent, with at
// most one error event per intent per hour, and the next tick that gets
// through clears it.
func TestATransientFailureKeepsTheIntent(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "ready_for_pr", "gem", 0, `"open_requested": "`+asked+`"`)
	h.setHead("bot/fix-it", sha('a'))
	h.routes(at("POST", "/repos/o/gem/pulls", reply{NetErr: errNoAnswer}))
	for i := 0; i < 3; i++ {
		h.drain(intentConfig)
	}
	got := h.board.load(t, tk.ID)
	if !got.PR.V.OpenRequested.Set || got.Column != "ready_for_pr" {
		t.Fatalf("pr %+v", got.PR.V)
	}
	if errs := details(got, "error"); len(errs) != 1 || !strings.HasPrefix(errs[0], "github: ") || !strings.Contains(errs[0], "connection refused") {
		t.Fatalf("errors = %q", errs)
	}
	// an hour on, the outage is said again
	later := intentNow.Add(61 * time.Minute)
	h.flow.Clock, h.board.clock = fixedClock(later), fixedClock(later)
	h.drain(intentConfig)
	if errs := details(h.board.load(t, tk.ID), "error"); len(errs) != 2 {
		t.Fatalf("errors = %q", errs)
	}
	// GitHub answers again
	h.routes(
		at("POST", "/repos/o/gem/pulls/12/requested_reviewers", ok(map[string]any{})),
		at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 12, "html_url": "https://github.com/o/gem/pull/12", "draft": false})),
	)
	h.drain(intentConfig)
	got = h.board.load(t, tk.ID)
	if got.PR.V.OpenRequested.Set || got.Column != "pr_open" {
		t.Fatalf("pr %+v, column %s", got.PR.V, got.Column)
	}
}

// Hold, then open, then merge, first in the GitHub pass.
func TestHoldThenOpenThenMergeFirstInTheGitHubPass(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "pr_open", "gem", 7, `"hold_requested": "2026-03-01T11:00:00+00:00", "open_requested": "2026-03-01T11:00:01+00:00", "merge_requested": "2026-03-01T11:00:02+00:00"`)
	held := h.intentTicket(2, "ready", "gem", 8, `"hold_requested": "`+asked+`"`)
	h.routes(
		at("PUT", "/repos/o/gem/pulls/7/merge", ok(map[string]any{"merged": true})),
		prRoute(7, map[string]any{"state": "open", "draft": false, "node_id": "PR_7", "head": map[string]any{"sha": sha('a')}}),
		prRoute(8, map[string]any{"state": "open", "draft": false, "node_id": "PR_8"}),
		at("POST", "/graphql", ok(map[string]any{})),
	)
	h.pass(context.Background(), intentConfig)
	var seq []string
	for _, c := range h.gh.calls() {
		seq = append(seq, c.String())
	}
	want := []string{
		"GET /repos/o/gem/pulls/7", "POST /graphql", // hold T0001
		// open T0001: refused by open_pr (it is in pr_open), no request
		"GET /repos/o/gem/pulls/8", "POST /graphql", // hold T0002
		"GET /repos/o/gem/pulls/7", "PUT /repos/o/gem/pulls/7/merge", "DELETE /repos/o/gem/git/refs/heads/bot/fix-it", // merge T0001
	}
	if strings.Join(seq, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(seq, "\n"), strings.Join(want, "\n"))
	}
	got := h.board.load(t, tk.ID)
	if pr := got.PR.V; pr.HoldRequested.Set || pr.OpenRequested.Set || pr.MergeRequested.Set || got.Column != "done" {
		t.Errorf("pr %+v, column %s", pr, got.Column)
	}
	if h.board.load(t, held.ID).PR.V.HoldRequested.Set {
		t.Error("the hold on T0002 was not drained")
	}
}

// Without the GitHub App nothing is drained.
func TestWithoutTheGitHubAppNothingIsDrained(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "ready_for_pr", "gem", 0, `"open_requested": "`+asked+`"`)
	before := string(h.board.files[tk.ID])
	h.pass(context.Background(), strings.Replace(intentConfig, "installation_id = 42", "", 1))
	if string(h.board.files[tk.ID]) != before || len(h.gh.calls()) != 0 {
		t.Fatalf("drained without the App: %v", h.gh.calls())
	}
}

// A ticket with no intent is not touched.
func TestATicketWithNoIntentIsNotTouched(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "pr_open", "gem", 7, "")
	before := string(h.board.files[tk.ID])
	h.drain(intentConfig)
	if string(h.board.files[tk.ID]) != before || len(h.board.saves) != 0 {
		t.Fatal("a ticket without an intent was saved")
	}
}

// A merge intent: a refusal GitHub will repeat is said and cleared; one
// retrying can fix is kept, with an error event.
func TestAMergeIntentIsClearedOnlyWithItsOutcome(t *testing.T) {
	for _, c := range []struct {
		name   string
		pr     reply
		kept   bool
		event  string
		detail string
	}{
		{"draft", ok(map[string]any{"state": "open", "draft": true}), false, "pr", "not merged (asked " + asked + "): PR #7 is a draft; nothing to merge"},
		{"gone", refuse(404, "Not Found"), false, "error", "github: HTTP Error 404: Not Found"},
		{"5xx", refuse(502, "Bad Gateway"), true, "error", "github: HTTP Error 502: Bad Gateway"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := intentHarness(t)
			tk := h.intentTicket(1, "pr_open", "gem", 7, `"merge_requested": "`+asked+`"`)
			h.routes(at("GET", "/repos/o/gem/pulls/7", c.pr))
			h.pass(context.Background(), intentConfig)
			got := h.board.load(t, tk.ID)
			if got.PR.V.MergeRequested.Set != c.kept || got.Column != "pr_open" {
				t.Fatalf("pr %+v", got.PR.V)
			}
			if e := lastEntry(got); e.Event != c.event || e.Detail != c.detail {
				t.Errorf("last = %+v", e)
			}
			// the merge was tried this pass: check_pr does not also run
			if n := len(h.gh.calls()); n != 1 {
				t.Errorf("requests = %v", h.gh.calls())
			}
		})
	}
}

// The ruleset's "New changes require approval from someone other than the
// last pusher" (the App pushed): a merge GitHub refuses until the maintainer
// approves is dropped, not tried every tick, and the ticket says it waits
// for the approval (pr.merge_blocked). The approval, when it comes, is the
// go-ahead and merges it; the block goes with the merge.
func TestAMergeRefusedForWantOfAnApprovalIsDropped(t *testing.T) {
	const ruleset = "Repository rule violations found\n\nNew changes require approval from someone other than the last pusher.\n\n"
	for _, c := range []struct {
		name  string
		merge reply
	}{
		{"ruleset, 405", refuse(405, ruleset)},
		{"ruleset, 422", refuse(422, ruleset)},
		{"branch protection", refuse(405, "At least 1 approving review is required by reviewers with write access.")},
		{"not merged, said in the answer", ok(map[string]any{"merged": false, "message": ruleset})},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := intentHarness(t)
			tk := h.intentTicket(1, "pr_open", "gem", 7, `"merge_requested": "`+asked+`"`)
			h.routes(
				at("PUT", "/repos/o/gem/pulls/7/merge", c.merge),
				prRoute(7, map[string]any{"state": "open", "draft": false, "head": map[string]any{"sha": sha('a')}}),
			)
			h.drain(intentConfig)
			got := h.board.load(t, tk.ID)
			pr := got.PR.V
			if pr.MergeRequested.Set || pr.MergeBlocked.Or("") != ticket.MergeBlockedApproval || got.Column != "pr_open" {
				t.Fatalf("pr %+v, column %s", pr, got.Column)
			}
			if !ticket.NeedsApproval(got) {
				t.Error("the ticket does not wait for the approval")
			}
			e := lastEntry(got)
			if e.Event != "pr" || !strings.HasPrefix(e.Detail, "not merged (asked "+asked+"): waiting for your approval on GitHub") {
				t.Errorf("last = %+v", e)
			}
			if errs := details(got, "error"); len(errs) != 0 {
				t.Errorf("errors = %q", errs)
			}
			// dropped: the next tick asks GitHub nothing more about it
			n := len(h.gh.calls())
			h.drain(intentConfig)
			if len(h.gh.calls()) != n {
				t.Errorf("tried again: %v", h.gh.calls()[n:])
			}
		})
	}

	// any other refusal is as before: dropped with GitHub's words, and no block
	h := intentHarness(t)
	tk := h.intentTicket(1, "pr_open", "gem", 7, `"merge_requested": "`+asked+`"`)
	h.routes(
		at("PUT", "/repos/o/gem/pulls/7/merge", refuse(405, "Pull Request is not mergeable")),
		prRoute(7, map[string]any{"state": "open", "draft": false, "head": map[string]any{"sha": sha('a')}}),
	)
	h.drain(intentConfig)
	got := h.board.load(t, tk.ID)
	if got.PR.V.MergeRequested.Set || got.PR.V.MergeBlocked.Set ||
		lastEntry(got).Detail != "not merged (asked "+asked+"): GitHub refused the merge of PR #7: Pull Request is not mergeable" {
		t.Errorf("pr %+v, last %+v", got.PR.V, lastEntry(got))
	}

	// the approval comes: the tick merges on it, and the block goes
	h = intentHarness(t)
	tk = h.intentTicket(1, "pr_open", "gem", 7, `"merge_blocked": "approval", "review": {"state": "none"}`)
	h.routes(
		at("GET", "/repos/o/gem/pulls/7/reviews", ok([]any{review(9, "APPROVED", "maintainer-login", sha('a'))})),
		at("PUT", "/repos/o/gem/pulls/7/merge", ok(map[string]any{"merged": true})),
		prRoute(7, map[string]any{"state": "open", "draft": false, "merged": false, "head": map[string]any{"sha": sha('a')}}),
	)
	h.pass(context.Background(), intentConfig)
	got = h.board.load(t, tk.ID)
	if got.Column != "done" || got.PR.V.MergeBlocked.Set {
		t.Errorf("column %s, pr %+v", got.Column, got.PR.V)
	}
}

// A merge is not started when the tick's deadline leaves too little time
// for the merge check: the intent stays, nothing is recorded on the ticket,
// and the log says so.
func TestAMergeDeferredUnderTheDeadlineKeepsItsIntent(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "pr_open", "gem", 7, `"merge_requested": "`+asked+`"`)
	before := string(h.board.files[tk.ID])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	h.pass(ctx, intentConfig)
	if string(h.board.files[tk.ID]) != before || len(h.gh.calls()) != 0 {
		t.Fatalf("deferred merge touched the ticket or GitHub: %v", h.gh.calls())
	}
	if len(h.log.lines) != 1 || !strings.Contains(h.log.lines[0], "T0001") || !strings.Contains(h.log.lines[0], "next tick") {
		t.Errorf("log = %q", h.log.lines)
	}
}

// A merge whose merge check finds no room on this host (agents using it)
// keeps its intent with nothing recorded on the ticket, and the log says
// it waits.
func TestAMergeWithNoRoomForItsCheckKeepsItsIntent(t *testing.T) {
	h := intentHarness(t)
	h.checks.ok, h.checks.err = false, fmt.Errorf("%w: 1 agents run here", gate.ErrNoRoom)
	tk := h.intentTicket(1, "pr_open", "gem", 7, `"merge_requested": "`+asked+`"`)
	h.routes(prRoute(7, map[string]any{"state": "open", "draft": false, "head": map[string]any{"sha": sha('a')}}))
	h.pass(context.Background(), intentConfig)
	got := h.board.load(t, tk.ID)
	if !got.PR.V.MergeRequested.Set || got.Column != "pr_open" || len(got.History) != 0 || h.gh.any("PUT") {
		t.Fatalf("pr %+v, history %+v, requests %v", got.PR.V, got.History, h.gh.calls())
	}
	if len(h.log.lines) != 1 || !strings.Contains(h.log.lines[0], "T0001") || !strings.Contains(h.log.lines[0], "no room") {
		t.Errorf("log = %q", h.log.lines)
	}
}

// A hold GitHub could not do: retryable keeps the intent, anything else
// clears it with Python's event; a ticket with no PR or no repository has
// nothing to hold.
func TestHoldIntentOutcomes(t *testing.T) {
	h := intentHarness(t)
	flaky := h.intentTicket(1, "ready", "gem", 7, `"hold_requested": "`+asked+`"`)
	gone := h.intentTicket(2, "ready", "gem", 8, `"hold_requested": "`+asked+`"`)
	none := h.intentTicket(3, "ready", "local", 9, `"hold_requested": "`+asked+`"`)
	closed := h.intentTicket(4, "ready", "gem", 10, `"hold_requested": "`+asked+`"`)
	h.routes(at("GET", "/repos/o/gem/pulls/7", refuse(503, "Service Unavailable")),
		at("GET", "/repos/o/gem/pulls/8", refuse(404, "Not Found")),
		prRoute(10, map[string]any{"state": "closed"}))
	h.drain(intentConfig)
	g := h.board.load(t, flaky.ID)
	if !g.PR.V.HoldRequested.Set || lastEntry(g).Detail != "github: could not turn PR #7 back to draft: HTTP Error 503: Service Unavailable" {
		t.Errorf("flaky: pr %+v, last %+v", g.PR.V, lastEntry(g))
	}
	g = h.board.load(t, gone.ID)
	if g.PR.V.HoldRequested.Set || lastEntry(g).Detail != "github: could not turn PR #8 back to draft: HTTP Error 404: Not Found" {
		t.Errorf("gone: pr %+v, last %+v", g.PR.V, lastEntry(g))
	}
	for _, id := range []ticket.ID{none.ID, closed.ID} {
		g = h.board.load(t, id)
		if g.PR.V.HoldRequested.Set || len(g.History) != 0 || g.PR.V.Draft.V {
			t.Errorf("%s: pr %+v, history %v", id, g.PR.V, g.History)
		}
	}
}

// A cancelled tick saves nothing: the intent is still there for the
// next one.
func TestACancelledDrainSavesNothing(t *testing.T) {
	h := intentHarness(t)
	h.intentTicket(1, "ready", "gem", 7, `"hold_requested": "`+asked+`"`)
	ctx, cancel := context.WithCancel(context.Background())
	h.routes(route{Method: "GET", Prefix: "/repos/o/gem/pulls/7", Reply: func(_ jsonx.Value) reply {
		cancel()
		return reply{NetErr: context.Canceled}
	}})
	err := h.flow.Pass(ctx, parseConfig(t, intentConfig), h.board.all(t))
	if err == nil || len(h.board.saves) != 0 {
		t.Fatalf("err %v, saves %v", err, h.board.saves)
	}
}

// The rest of the pass is Python's GitHub loop: pr_open is followed, a
// ready_for_pr ticket is opened when [github] open_prs = "auto" or it is a
// rework of a PR, and left alone (unsaved) otherwise.
func TestOpenPRsAutoAndReworksOpenByThemselves(t *testing.T) {
	for _, c := range []struct {
		name   string
		auto   bool
		number int
		opened bool
	}{
		{"manual", false, 0, false},
		{"auto", true, 0, true},
		{"rework", false, 5, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := intentHarness(t)
			cfgText := intentConfig
			if c.auto {
				cfgText = strings.Replace(cfgText, "installation_id = 42", "installation_id = 42\nopen_prs = \"auto\"", 1)
			}
			tk := h.intentTicket(1, "ready_for_pr", "gem", c.number, "")
			before := string(h.board.files[tk.ID])
			h.setHead("bot/fix-it", sha('a'))
			h.routes(
				at("GET", "/repos/o/gem/pulls/5", refuse(404, "Not Found")),
				at("POST", "/repos/o/gem/pulls/12/requested_reviewers", ok(map[string]any{})),
				at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 12, "html_url": "https://github.com/o/gem/pull/12", "draft": false})),
			)
			h.pass(context.Background(), cfgText)
			got := h.board.load(t, tk.ID)
			if (got.Column == "pr_open") != c.opened {
				t.Fatalf("column %s", got.Column)
			}
			if !c.opened && (string(h.board.files[tk.ID]) != before || len(h.board.saves) != 0) {
				t.Error("a ticket left alone was saved")
			}
		})
	}
}

// An error in the loop is an error event on the ticket, which is saved.
func TestAGitHubErrorInThePassIsAnEvent(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "pr_open", "gem", 7, "")
	h.routes(at("GET", "/repos/o/gem/pulls/7", refuse(404, "Not Found")))
	h.pass(context.Background(), intentConfig)
	got := h.board.load(t, tk.ID)
	if e := lastEntry(got); e.Event != "error" || e.Detail != "github: HTTP Error 404: Not Found" {
		t.Errorf("last = %+v", e)
	}
	if got.Updated != "2026-03-01T12:00:00+00:00" {
		t.Errorf("updated = %s", got.Updated)
	}
}

// The tick half of the merge button: the request cmd_merge (ops) records is
// merged by the next tick's GitHub pass, and the request goes with it.
//
// port: TF GithubOutTest.test_merge_it_only_asks_and_the_tick_merges
func TestTheTickMergesWhatWasAsked(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "pr_open", "gem", 7, `"merge_requested": "`+asked+`"`)
	h.routes(
		at("PUT", "/repos/o/gem/pulls/7/merge", ok(map[string]any{"merged": true})),
		prRoute(7, map[string]any{"state": "open", "draft": false, "node_id": "PR_7", "head": map[string]any{"sha": sha('a')}}),
	)
	h.pass(context.Background(), intentConfig)
	got := h.board.load(t, tk.ID)
	if got.PR.V.MergeRequested.Set || got.Column != "done" || !h.gh.did("PUT", "/repos/o/gem/pulls/7/merge") {
		t.Fatalf("pr %+v, column %s, requests %v", got.PR.V, got.Column, h.gh.calls())
	}
	if !strings.Contains(string(h.board.files[tk.ID]), `"pr": {`) || strings.Contains(string(h.board.files[tk.ID]), "merge_requested") {
		t.Errorf("file:\n%s", h.board.files[tk.ID])
	}
}

// A merge that waited for room and is then refused drops its wait with
// its intent: the ticket no longer reads as queued to merge.
func TestARefusedMergeDropsItsWait(t *testing.T) {
	h := intentHarness(t)
	h.checks.ok, h.checks.err = false, &gate.RoomError{Running: 2, Max: 2, Memory: "1g"}
	tk := h.intentTicket(1, "pr_open", "gem", 7, `"merge_requested": "`+asked+`"`)
	h.routes(
		at("PUT", "/repos/o/gem/pulls/7/merge", refuse(405, "Pull Request is not mergeable")),
		prRoute(7, map[string]any{"state": "open", "draft": false, "head": map[string]any{"sha": sha('a')}}),
	)
	h.pass(context.Background(), intentConfig)
	if got := h.board.load(t, tk.ID); got.PR.V.Queue.V.State != ticket.QueueRoom || !ticket.MergeQueued(got) {
		t.Fatalf("no room: pr %+v", got.PR.V)
	}
	h.checks.ok, h.checks.err = true, nil
	h.pass(context.Background(), intentConfig)
	got := h.board.load(t, tk.ID)
	if got.PR.V.MergeRequested.Set || got.PR.V.Queue.Present() || ticket.MergeQueued(got) || !h.gh.any("PUT") {
		t.Errorf("refused: pr %+v, requests %v", got.PR.V, h.gh.calls())
	}
}

// While its merge check runs, a merge reads as merging on the board, and
// the wait goes once it lands.
func TestAMergeCheckThatRunsIsSavedAsMerging(t *testing.T) {
	h := intentHarness(t)
	tk := h.intentTicket(1, "pr_open", "gem", 7, `"merge_requested": "`+asked+`"`)
	h.routes(
		at("PUT", "/repos/o/gem/pulls/7/merge", ok(map[string]any{"merged": true})),
		prRoute(7, map[string]any{"state": "open", "draft": false, "head": map[string]any{"sha": sha('a')}}),
	)
	var during ticket.PRQueue
	h.checks.run = func() { during = h.board.load(t, tk.ID).PR.V.Queue.V }
	h.pass(context.Background(), intentConfig)
	if during.State != ticket.QueueMerging || during.Detail != "" || during.Since == "" {
		t.Errorf("during the check: queue %+v", during)
	}
	got := h.board.load(t, tk.ID)
	if got.Column != "done" || got.PR.V.Queue.Present() {
		t.Errorf("column %s, pr %+v", got.Column, got.PR.V)
	}
}
