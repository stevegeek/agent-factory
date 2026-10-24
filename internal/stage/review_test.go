package stage

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const head = "abcdef0123456789abcdef0123456789abcdef01"

func reviewing(t *testing.T, extraStarts int) *ticket.Ticket {
	tk := running(t, "T0001", "review", "review", `"head_before_review": "`+head+`"`)
	for i := range extraStarts {
		tk.History = append(tk.History, ticket.Entry{At: "x", Event: "start", Detail: "work t0001-w" + string(rune('2'+i)) + " on b"})
	}
	return tk
}

func TestReviewFilesPreExistingBugsAndRecordsTheHead(t *testing.T) {
	// port: TF TransitionTest.test_review_files_pre_existing_bugs_and_a_rereview_converges
	tk := running(t, "T0001", "review", "review", `"head_before_review": "`+head+`"`)
	tk.Title, tk.Body, tk.Branch = "Return two", "Make thing return 2.", ticket.Some("bot/r")
	r := report(`{"verdict": "changes", "runtime_behaviour": "yes",
		"findings": ["[important] lib/thing.rb:1: no test covers the new value"],
		"pre_existing": [{"title": "thing ignores its argument", "detail": "lib/thing.rb:1"}, "junk"]}`)
	in := FinishIn{Ticket: tk, Report: r, Facts: Facts{Head: head}}
	if n := Needs(in); n != 1 {
		t.Fatalf("needs %d", n)
	}
	out := finish(t, in)
	got := out.Ticket
	if got.Column != "ready" || got.ReviewedHead.Or("") != head || reason(got) != model.ReasonReviewChanges {
		t.Errorf("%s %v %s", got.Column, got.ReviewedHead, reason(got))
	}
	if len(out.File) != 1 {
		t.Fatalf("filed %d", len(out.File))
	}
	f := out.File[0]
	if f.Title != "thing ignores its argument" || f.Column != "triage" || f.Door != "peer" || f.Parent.Or("") != "T0001" ||
		f.Body != "Found while reviewing T0001 (not introduced by it).\n\nlib/thing.rb:1" || f.Project != "gem" {
		t.Errorf("filed %+v", f)
	}
	var filed *ticket.Entry
	for i := range got.History {
		if got.History[i].Event == "filed" {
			filed = &got.History[i]
		}
	}
	if filed == nil || filed.Detail != string(f.ID)+": pre-existing, found in review: thing ignores its argument" {
		t.Errorf("filed event %+v", filed)
	}
	// new_ticket saved the ticket, then the parent was added: parent comes after updated
	f.Updated = "2026-10-17T09:30:00+00:00"
	enc := string(ticket.Encode(f))
	if i, j, k := strings.Index(enc, `"ask"`), strings.Index(enc, `"updated"`), strings.Index(enc, `"parent"`); !(i < j && j < k) {
		t.Errorf("key order:\n%s", enc)
	}

	// the re-review prompt converges on the earlier findings
	prompt := For(model.StageReview).Prompt(PromptIn{Ticket: got, Project: gemProject(t)})
	for _, want := range []string{"re-review after a rework", "no test covers the new value", "git diff " + head + "..HEAD"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "REVIEW.md") {
		t.Error("no REVIEW.md at the base")
	}
	if !strings.Contains(For(model.StageReview).Prompt(PromptIn{Ticket: got, Project: gemProject(t), ReviewMD: true}), "REVIEW.md") {
		t.Error("REVIEW.md at the base is named")
	}
}

func TestPreExistingBugsAreCapped(t *testing.T) {
	// MAX_PRE_EXISTING: only the first three entries count, junk included; titles 150, details 3000
	long := strings.Repeat("word ", 700)
	r := report(`{"verdict": "pass", "pre_existing": [
		{"title": "  one  ", "detail": null}, {"title": "   "}, {"title": "` + strings.Repeat("t", 200) + `", "detail": "` + long + `"},
		{"title": "four"}, {"title": "five"}]}`)
	in := FinishIn{Ticket: reviewing(t, 0), Report: r, Facts: Facts{Head: head}}
	out := finish(t, in)
	if len(out.File) != 2 {
		t.Fatalf("filed %d", len(out.File))
	}
	if out.File[0].Title != "one" || out.File[0].Body != "Found while reviewing T0001 (not introduced by it).\n\n" {
		t.Errorf("first: %q %q", out.File[0].Title, out.File[0].Body)
	}
	if n := len([]rune(out.File[1].Title)); n != 150 || !strings.HasSuffix(out.File[1].Title, "…") {
		t.Errorf("title of %d", n)
	}
	detail := strings.TrimPrefix(out.File[1].Body, "Found while reviewing T0001 (not introduced by it).\n\n")
	if n := len([]rune(detail)); n != 3000 {
		t.Errorf("detail of %d", n)
	}
	if out.Ticket.Column != "security" {
		t.Errorf("column %s", out.Ticket.Column)
	}
	if _, err := Finish(FinishIn{Ticket: reviewing(t, 0), Report: r, Facts: Facts{Head: head}, Config: cfg(t, ""), Now: now,
		IDs: []ticket.ID{"T0100"}}); err == nil {
		t.Error("too few ids must fail")
	}
}

func TestReviewerThatCommitsIsRejected(t *testing.T) {
	// port: TF TransitionTest.test_reviewer_that_commits_is_rejected
	out := finish(t, FinishIn{Ticket: reviewing(t, 0), Report: report(`{"verdict": "pass", "pre_existing": [{"title": "x"}]}`),
		Facts: Facts{Head: strings.Repeat("0", 40)}})
	if out.Ticket.Column != "needs_human" || reason(out.Ticket) != model.ReasonReviewerCommitted || len(out.File) != 0 ||
		last(out.Ticket).Detail != "review -> needs_human: review agent changed the branch; reviewers must not commit" {
		t.Errorf("%s %s %q", out.Ticket.Column, reason(out.Ticket), last(out.Ticket).Detail)
	}
	// a ticket that never recorded the head it started from fails the same way
	tk := running(t, "T0001", "security", "security", "")
	if out := finish(t, FinishIn{Ticket: tk, Report: report(`{"verdict": "pass"}`), Facts: Facts{Head: head}}); reason(out.Ticket) != model.ReasonReviewerCommitted {
		t.Errorf("no head before: %s", reason(out.Ticket))
	}
}

func TestReviewPassGoesToSecurity(t *testing.T) {
	// port: TF TransitionTest.test_review_pass_goes_to_security
	out := finish(t, FinishIn{Ticket: reviewing(t, 0), Report: report(`{"verdict": "pass"}`), Facts: Facts{Head: head}})
	if out.Ticket.Column != "security" || reason(out.Ticket) != model.ReasonReviewPassed {
		t.Errorf("%s %s", out.Ticket.Column, reason(out.Ticket))
	}
}

func TestReviewChangesRetriesOnceThenNeedsHuman(t *testing.T) {
	// port: TF TransitionTest.test_review_changes_retries_once_then_needs_human
	out := finish(t, FinishIn{Ticket: reviewing(t, 0), Report: report(`{"verdict": "changes"}`), Facts: Facts{Head: head}})
	if out.Ticket.Column != "ready" || last(out.Ticket).Detail != "review -> ready: review asked for changes" {
		t.Errorf("%s %q", out.Ticket.Column, last(out.Ticket).Detail)
	}
	out = finish(t, FinishIn{Ticket: reviewing(t, 1), Report: report(`{"verdict": "changes"}`), Facts: Facts{Head: head}})
	if out.Ticket.Column != "needs_human" || reason(out.Ticket) != model.ReasonReviewExhausted ||
		last(out.Ticket).Detail != "review -> needs_human: review still asks for changes after the last attempt" {
		t.Errorf("%s %s %q", out.Ticket.Column, reason(out.Ticket), last(out.Ticket).Detail)
	}
}

func TestReworkRoundsResetAfterAPassedReview(t *testing.T) {
	// after the review passed, a GitHub "changes requested" rework
	// starts a new round, so the reviewer may still send it back once
	afterPass := func(reworks int) *ticket.Ticket {
		tk := reviewing(t, 1)
		passed := ticket.Moved{From: "review", To: "security", Reason: model.ReasonReviewPassed, By: "factory"}
		tk.History = append(tk.History, ticket.Entry{At: "x", Event: "move", Detail: "review -> security", Type: passed.EventType(), Data: passed})
		for i := range reworks {
			tk.History = append(tk.History, ticket.Entry{At: "x", Event: "start", Detail: "work t0001-w" + string(rune('3'+i)) + " on b"})
		}
		return tk
	}
	tk := afterPass(1)
	if n, r := ticket.Starts(tk, model.StageWork), ticket.WorkRounds(tk); n != 3 || r != 1 {
		t.Errorf("starts %d rounds %d: agents are still numbered over the ticket's life", n, r)
	}
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"verdict": "changes"}`), Facts: Facts{Head: head}})
	if out.Ticket.Column != "ready" || reason(out.Ticket) != model.ReasonReviewChanges {
		t.Errorf("first rework after a pass: %s %s", out.Ticket.Column, reason(out.Ticket))
	}
	out = finish(t, FinishIn{Ticket: afterPass(2), Report: report(`{"verdict": "changes"}`), Facts: Facts{Head: head}})
	if out.Ticket.Column != "needs_human" || reason(out.Ticket) != model.ReasonReviewExhausted {
		t.Errorf("second rework after a pass: %s %s", out.Ticket.Column, reason(out.Ticket))
	}
}

func gemProject(t *testing.T) *configProject {
	c := cfg(t, projectsTOML(map[string]map[string]string{"gem": {"path": "/x", "base": "main"}}))
	p, _ := c.Project("gem")
	return p
}
