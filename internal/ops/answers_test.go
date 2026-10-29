package ops_test

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func values(as []ops.Answer) string {
	var out []string
	for _, a := range as {
		v := a.Value
		if a.Href != "" {
			v = "link"
		}
		out = append(out, v)
	}
	return strings.Join(out, ",")
}

// GitHub merges a PR the App pushed only once the maintainer approves its
// head there: until then the review ask sends them to GitHub and offers no
// "Merge it" (the factory merges by itself once the approval comes); once
// the head is approved and the PR still waits, "Merge it" is back.
func TestReviewAnswersFollowTheApproval(t *testing.T) {
	head := strings.Repeat("a", 40)
	review := &ticket.Ask{Kind: "review"}
	eligible := func(pr ticket.PR) *ticket.Ticket {
		pr.Number, pr.URL, pr.Draft = 23, "https://github.com/example/widgets/pull/23", ticket.Some(false)
		return &ticket.Ticket{ID: "T0001", Column: "pr_open", PR: ticket.Some(pr), Gate: ticket.Some(ticket.Gate{AutoEligible: true, Head: head})}
	}
	for _, c := range []struct {
		name string
		pr   ticket.PR
	}{
		{"no review yet", ticket.PR{Review: ticket.Some(ticket.PRReview{State: ticket.ReviewNone})}},
		{"changes requested", ticket.PR{Review: ticket.Some(ticket.PRReview{State: ticket.ReviewChanges, Commit: head})}},
		{"approved before a rework", ticket.PR{Review: ticket.Some(ticket.PRReview{State: ticket.ReviewApproved, Commit: strings.Repeat("b", 40)})}},
		{"a merge refused for an approval", ticket.PR{MergeBlocked: ticket.Some(ticket.MergeBlockedApproval)}},
	} {
		as := ops.Answers(eligible(c.pr), review)
		if values(as) != "link,send_back,link" {
			t.Errorf("%s: %+v", c.name, as)
			continue
		}
		if a := as[0]; !a.Primary || a.Label != "Approve PR #23 on GitHub" || a.Href != "https://github.com/example/widgets/pull/23" ||
			!strings.Contains(a.Sub, "merges it by itself on the next tick") {
			t.Errorf("%s: approve: %+v", c.name, a)
		}
		if !as[2].Danger || as[2].Label != "Close it on GitHub" {
			t.Errorf("%s: close: %+v", c.name, as[2])
		}
	}
	// approved at the head and still open (checks running, or a merge refused before the approval)
	for name, pr := range map[string]ticket.PR{
		"approved":            {Review: ticket.Some(ticket.PRReview{State: ticket.ReviewApproved, Commit: head})},
		"approved after a no": {Review: ticket.Some(ticket.PRReview{State: ticket.ReviewApproved, Commit: head}), MergeBlocked: ticket.Some(ticket.MergeBlockedApproval)},
		"never read":          {},
	} {
		if as := ops.Answers(eligible(pr), review); values(as) != "merge,link,send_back,link" || as[0].Label != "Merge it" {
			t.Errorf("%s: %+v", name, as)
		}
	}
	// not eligible, approved, and waiting for room for the merge check
	tk := eligible(ticket.PR{Review: ticket.Some(ticket.PRReview{State: ticket.ReviewApproved, Commit: head}),
		Queue: ticket.Some(ticket.PRQueue{State: ticket.QueueRoom, Running: 3, Max: 3, Since: "2026-01-02T03:04:05+00:00"})})
	tk.Gate = ticket.Some(ticket.Gate{Head: head})
	as := ops.Answers(tk, review)
	if values(as) != "link,send_back,link" || as[0].Label != "Open PR #23 on GitHub" || strings.Contains(as[0].Sub, "approve it") {
		t.Errorf("queued: %+v", as)
	}
}

// Every ask kind has its answers, each a value the service carries out or
// a link.
func TestEveryAskKindHasAnswers(t *testing.T) {
	tk := &ticket.Ticket{ID: "T0001", Column: "needs_human"}
	for _, k := range model.AskKinds() {
		a := &ticket.Ask{Kind: string(k.Kind), RetryColumn: ticket.Some("ready")}
		as := ops.Answers(tk, a)
		if len(as) == 0 {
			t.Errorf("ask kind %s has no answers", k.Kind)
		}
		primary := 0
		for _, x := range as {
			if x.Primary {
				primary++
			}
			if x.Value == "" && x.Href == "" {
				t.Errorf("%s: an answer that does nothing: %+v", k.Kind, x)
			}
		}
		// review without a PR link offers only the send-back
		if primary > 1 || primary == 0 && k.Kind != model.AskReview {
			t.Errorf("%s: %d primary answers", k.Kind, primary)
		}
	}
	if ops.Answers(tk, &ticket.Ask{Kind: "nonsense"}) != nil {
		t.Errorf("an unknown kind offers nothing")
	}
}

// The answers half of Foreman's TestLabelsAndAnswers, with
// TestReadyForPRIsOnTheDesk, TestMergeOnlyForAnEligiblePROnYourWord,
// TestNoteButtonAndDraftReasons and TestManagerDraftsAreShownAndAnswered.
func TestAnswersOffered(t *testing.T) {
	pr := func(draft bool) ticket.Opt[ticket.PR] {
		return ticket.Some(ticket.PR{Number: 23, URL: "https://github.com/example/widgets/pull/23", Draft: ticket.Some(draft)})
	}
	review := &ticket.Ask{Kind: "review"}
	t1 := &ticket.Ticket{ID: "T0001", Column: "pr_open", PR: pr(true), Gate: ticket.Some(ticket.Gate{})}
	as := ops.Answers(t1, review)
	if values(as) != "link,send_back,link" || !as[0].Primary || !as[2].Danger || as[2].Label != "Close it on GitHub" {
		t.Errorf("review of a draft PR: %+v", as)
	}
	if !as[1].NeedsNote || as[1].NoteFor != "to send it back" || as[1].Sub != "a worker reworks it; the PR is a draft meanwhile" {
		t.Errorf("send back is tied to the note: %+v", as[1])
	}
	// an eligible PR: Merge it first, then read it
	t1.PR, t1.Gate = pr(false), ticket.Some(ticket.Gate{AutoEligible: true})
	as = ops.Answers(t1, review)
	if values(as) != "merge,link,send_back,link" || !as[0].Primary || as[1].Href == "" {
		t.Errorf("eligible: %+v", as)
	}
	// a PR URL that is not http(s) is never a link
	t1.PR = ticket.Some(ticket.PR{Number: 23, URL: "javascript:alert(1)"})
	if as := ops.Answers(t1, review); values(as) != "send_back" {
		t.Errorf("unsafe PR link: %+v", as)
	}
	// ready_for_pr, with and without an ask, with and without a PR to update
	t2 := &ticket.Ticket{ID: "T0002", Column: "ready_for_pr", PR: pr(true)}
	if as := ops.Answers(t2, &ticket.Ask{Kind: "open"}); values(as) != "open_pr,send_back" || as[0].Label != "Update PR #23" {
		t.Errorf("open: %+v", as)
	}
	if as := ops.Answers(t2, nil); len(as) != 1 || as[0].Label != "Update PR #23" {
		t.Errorf("a reworked ticket should update its PR: %+v", as)
	}
	t2.PR = ticket.Opt[ticket.PR]{}
	if as := ops.Answers(t2, nil); len(as) != 1 || as[0].Label != "Open the PR" {
		t.Errorf("open without a PR: %+v", as)
	}
	if ops.Answers(&ticket.Ticket{Column: "needs_human"}, nil) != nil {
		t.Errorf("no ask, nothing to answer")
	}
	// not now on an issue says the issue stays open
	t3 := &ticket.Ticket{Column: "needs_human", Source: ticket.Some(ticket.Source{Issue: 18})}
	if as := ops.Answers(t3, &ticket.Ask{Kind: "start"}); as[1].Sub != "the issue stays open on GitHub" || !as[1].Danger {
		t.Errorf("not now on an issue: %+v", as[1])
	}
	if as := ops.Answers(&ticket.Ticket{}, &ticket.Ask{Kind: "start"}); as[1].Sub != "the ticket closes" {
		t.Errorf("not now: %+v", as[1])
	}
	if got := values(ops.Answers(t3, &ticket.Ask{Kind: "decide"})); got != "start,reply,drop" {
		t.Errorf("decide: %s", got)
	}
	// unstick offers a retry only where there is somewhere to go back to
	as = ops.Answers(t3, &ticket.Ask{Kind: "unstick", RetryColumn: ticket.Some("gate")})
	if values(as) != "retry,retry_note,drop" || as[0].Sub != "back to "+model.ColumnLabel("gate") {
		t.Errorf("unstick: %+v", as)
	}
	if got := values(ops.Answers(t3, &ticket.Ask{Kind: "unstick", RetryColumn: ticket.Nil[string]()})); got != "drop" {
		t.Errorf("unstick with nowhere to retry: %s", got)
	}
	for kind, want := range map[string]string{"design": "approve,redirect", "proposal": "file,drop", "notice": "done",
		"question": "answer", "draft": "applied,send_back,drop"} {
		if got := values(ops.Answers(t3, &ticket.Ask{Kind: kind})); got != want {
			t.Errorf("%s: %s, want %s", kind, got, want)
		}
	}
}
