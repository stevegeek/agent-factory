package present

import (
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// §6.3: an intent Foreman recorded shows as the tick's pending work, with
// the latest error since the request as "still trying".
func TestPendingIntents(t *testing.T) {
	at := "2026-10-18T10:00:00+00:00"
	cases := []struct {
		name      string
		pr        ticket.PR
		history   []ticket.Entry
		title     string
		stillLine string
	}{
		{"open", ticket.PR{OpenRequested: ticket.Some(at)}, nil, "Opening the PR; it shows here when it is open", ""},
		{"open, failing", ticket.PR{OpenRequested: ticket.Some(at)},
			[]ticket.Entry{{At: "2026-10-18T09:00:00+00:00", Event: "error", Detail: "old"},
				{At: "2026-10-18T10:05:00+00:00", Event: "error", Detail: "github: HTTP Error 502: Bad Gateway"}},
			"Opening the PR; it shows here when it is open", "Still trying to open the PR: github: HTTP Error 502: Bad Gateway"},
		{"an error before the request", ticket.PR{OpenRequested: ticket.Some(at)},
			[]ticket.Entry{{At: "2026-10-18T09:00:00+00:00", Event: "error", Detail: "old"}},
			"Opening the PR; it shows here when it is open", ""},
		{"merge", ticket.PR{Number: 12, MergeRequested: ticket.Some(at)}, nil, "Merging…", ""},
		{"hold", ticket.PR{Number: 12, HoldRequested: ticket.Some(at)},
			[]ticket.Entry{{At: "2026-10-18T10:01:00+00:00", Event: "error", Detail: "github: timeout"}},
			"Making the PR a draft again", "Still trying to make the PR a draft: github: timeout"},
	}
	for _, c := range cases {
		tk := &ticket.Ticket{PR: ticket.Some(c.pr), History: c.history}
		got := Pending(tk)
		if got == nil || got.Title != c.title {
			t.Errorf("%s: %+v", c.name, got)
			continue
		}
		if (c.stillLine == "") != (len(got.Lines) == 0) || c.stillLine != "" && got.Lines[0] != c.stillLine {
			t.Errorf("%s: lines %q", c.name, got.Lines)
		}
	}
	if Pending(&ticket.Ticket{}) != nil || Pending(&ticket.Ticket{PR: ticket.Some(ticket.PR{Number: 3})}) != nil {
		t.Error("nothing pending, yet a callout")
	}
}

// A merge GitHub refused for want of the maintainer's approval is dropped,
// not retried: no "Merging…" for it, nor for a request left over on a PR
// that waits for the approval.
func TestNoMergingWhileThePRWaitsForTheApproval(t *testing.T) {
	at := "2026-10-18T10:00:00+00:00"
	refused := []ticket.Entry{{At: "2026-10-18T10:01:00+00:00", Event: "pr",
		Detail: "not merged (asked " + at + "): waiting for your approval on GitHub"}}
	// dropped: the tick cleared the intent and recorded why
	dropped := &ticket.Ticket{Column: "pr_open", Gate: ticket.Some(ticket.Gate{Head: "abc"}),
		PR: ticket.Some(ticket.PR{Number: 2, MergeBlocked: ticket.Some(ticket.MergeBlockedApproval)}), History: refused}
	if c := Pending(dropped); c != nil {
		t.Errorf("a dropped merge: %+v", c)
	}
	// a request still on a PR that waits for the approval is not merging either
	left := ticket.Clone(dropped)
	pr := left.PR.V
	pr.MergeRequested = ticket.Some(at)
	left.PR = ticket.Some(pr)
	if c := Pending(left); c != nil {
		t.Errorf("a merge that waits for the approval: %+v", c)
	}
	// approved at the head: the request is the tick's again
	pr.Review = ticket.Some(ticket.PRReview{State: ticket.ReviewApproved, Commit: "abc"})
	left.PR = ticket.Some(pr)
	if c := Pending(left); c == nil || c.Title != "Merging…" {
		t.Errorf("an approved merge: %+v", c)
	}
}
