package prflow

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// CheckPR is check_pr: a PR merged on GitHub closes the ticket as
// done, one closed without merging as rejected; an open one has its reviews
// followed.
func (f *Flow) CheckPR(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) error {
	n := t.PR.V.Number
	pr, err := f.GitHub.PR(ctx, p.GitHub, int64(n))
	if err != nil {
		return err
	}
	now := f.now()
	switch {
	case pr.Merged:
		who := "?"
		if v, ok := dictOr(pr.Raw, "merged_by").Object(); ok {
			if login, ok := v.Lookup("login"); ok {
				who = jsonx.PyStr(login)
			}
		}
		ticket.Move(t, string(model.ColDone), model.ReasonPRMerged, "PR merged by "+who,
			"PR #"+itoa(n)+" was merged on GitHub by "+who, "", now)
		sha, _ := pr.Raw.Get("merge_commit_sha").Str()
		f.merged(ctx, p, t, pr, sha)
	case pr.State == "closed":
		ticket.Move(t, string(model.ColRejected), model.ReasonPRClosed, "PR closed without merging",
			"PR #"+itoa(n)+" was closed on GitHub without merging", "", now)
	default:
		return f.followReviews(ctx, cfg, p, t, pr)
	}
	return nil
}

const reviewAsk = "Address each point. Where a point is a question, answer it in your rework; the rework is posted on the PR."

// followReviews is follow_reviews: a configured reviewer's review
// on GitHub (nobody else's) is an instruction, and the latest new one
// decides. Changes requested go back to the worker; an approval merges.
// Every new review is marked seen, so an approval that a later review
// overruled never merges the rework.
func (f *Flow) followReviews(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, pr *github.PR) error {
	reviewers := map[string]bool{}
	for _, r := range cfg.GitHub.Reviewers {
		reviewers[textx.Lower(r)] = true
	}
	if len(reviewers) == 0 {
		return nil
	}
	n := t.PR.V.Number
	all, err := f.GitHub.Reviews(ctx, p.GitHub, int64(n))
	if err != nil {
		return err
	}
	recordReview(t, all, reviewers)
	seen := t.PR.V.ReviewsSeen
	var unseen []github.Review
	for _, r := range all {
		if (r.State == "CHANGES_REQUESTED" || r.State == "APPROVED") && !slices.Contains(seen, r.ID) &&
			reviewers[textx.Lower(login(r))] {
			unseen = append(unseen, r)
		}
	}
	if len(unseen) == 0 {
		return nil
	}
	if unseen[len(unseen)-1].State == "APPROVED" {
		return f.approveToMerge(ctx, cfg, p, t, pr, unseen)
	}
	var changes []github.Review
	for _, r := range unseen {
		if r.State == "CHANGES_REQUESTED" {
			changes = append(changes, r)
		}
	}
	if err := f.pickUpChanges(ctx, cfg, p, t, changes); err != nil {
		return err
	}
	markSeen(t, unseen)
	return nil
}

// recordReview keeps the latest decision of a configured reviewer on
// pr.review, seen or not, with the commit it was given on: GitHub merges a
// PR the App pushed only once it is approved at its head, and the desk asks
// for that approval until it is (ticket.NeedsApproval). A ticket is changed
// only when the decision is new.
func recordReview(t *ticket.Ticket, all []github.Review, reviewers map[string]bool) {
	r := ticket.PRReview{State: ticket.ReviewNone}
	for _, x := range all {
		if !reviewers[textx.Lower(login(x))] {
			continue
		}
		switch x.State {
		case "APPROVED":
			r.State = ticket.ReviewApproved
		case "CHANGES_REQUESTED":
			r.State = ticket.ReviewChanges
		default:
			continue
		}
		r.Commit, _ = x.Raw.Get("commit_id").Str()
	}
	v := t.PR.V
	if old, ok := v.Review.Get(); ok && old == r {
		return
	}
	v.Review = ticket.Some(r)
	t.PR = ticket.Some(v)
}

// login is str((r.get("user") or {}).get("login", "")).
func login(r github.Review) string {
	return jsonx.PyStr(dictGet(dictOr(r.Raw, "user"), "login", jsonx.String("")))
}

// markSeen adds the reviews to pr.reviews_seen, kept sorted.
func markSeen(t *ticket.Ticket, rs []github.Review) {
	pr := t.PR.V
	ids := slices.Clone(pr.ReviewsSeen)
	for _, r := range rs {
		if !slices.Contains(ids, r.ID) {
			ids = append(ids, r.ID)
		}
	}
	slices.Sort(ids)
	pr.ReviewsSeen = ids
	t.PR = ticket.Some(pr)
}

// approveToMerge is approve_to_merge: the approval is the
// go-ahead, so the factory marks the PR ready and merges it, but only the
// commit that was approved, and only if that is the commit the gate checked.
// A PR that conflicts with the base goes back for a rebase. A merge GitHub
// refuses (usually the required checks still running) is said once, with
// the reviews left unseen so the next tick tries again.
func (f *Flow) approveToMerge(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, pr *github.PR, unseen []github.Review) error {
	n := t.PR.V.Number
	review := unseen[len(unseen)-1]
	commit := review.Raw.Get("commit_id")
	head := pr.Raw.Get("head").Get("sha")
	gated := t.Tree().Get("gate").Get("head")
	now := f.now()
	if !jsonx.Equal(commit, head) || !jsonx.Equal(head, gated) {
		markSeen(t, unseen)
		what := "PR #" + itoa(n) + " moved since the gate checked it"
		if !jsonx.Equal(commit, head) {
			what = "your approval is for " + textx.Prefix(jsonx.PyStr(commit), 7) + ", but PR #" + itoa(n) + " is now at " + textx.Prefix(jsonx.PyStr(head), 7)
		}
		ticket.Event(t, "pr", "not merged: "+what+"; approve the latest commit to merge it", "",
			ticket.PRNote{Action: ticket.PRNotMerged, Number: n, Why: what}, now)
		return nil
	}
	if pr.MergeableState == "dirty" {
		// the base moved under it: waiting will not help, a rebase will; the
		// approval is spent (a push dismisses it)
		markSeen(t, unseen)
		v := t.PR.V
		v.MergeWait = ticket.Opt[string]{}
		t.PR = ticket.Some(v)
		ticket.AddNote(t, "PR #"+itoa(n)+" was approved but conflicts with "+p.Base+": other changes were merged since. "+
			"Rebase this branch onto "+p.Base+", resolve the conflicts keeping both the reviewed change "+
			"and what was merged, run the tests, and say in your rework what conflicted and how you resolved it.", "", nil, now)
		ticket.Move(t, string(model.ColReady), model.ReasonConflicts, "approved, but conflicts with "+p.Base,
			"you approved PR #"+itoa(n)+", but it no longer merges cleanly; a worker rebases it, then it comes back to you", "", now)
		f.hold(ctx, cfg, t)
		return nil
	}
	if !f.timeForMerge(ctx) {
		f.logf("%s: approved PR #%d merges on the next tick: too little time left in this one for a merge check", t.ID, n)
		return nil
	}
	sha, _ := head.Str()
	err := func() error {
		if pr.Draft {
			if err := f.GitHub.SetDraft(ctx, pr.NodeID, false); err != nil {
				return err
			}
			v := t.PR.V
			v.Draft = ticket.Some(false)
			t.PR = ticket.Some(v)
		}
		return f.land(ctx, cfg, p, t, pr, sha, "PR merged by the factory on your approval",
			"you approved PR #"+itoa(n)+" at "+textx.Prefix(sha, 7)+" on GitHub, the commit the gate checked; the factory merged it")
	}()
	if err == nil {
		markSeen(t, unseen)
		return nil
	}
	if errors.Is(err, gate.ErrNoRoom) {
		f.logf("%s: approved PR #%d merges on a later tick: %v", t.ID, n, err)
		return nil
	}
	var he *github.HTTPError
	isHTTP := errors.As(err, &he)
	if transient(err) && !isHTTP {
		return err // no answer at all: the tick records it, and tries again
	}
	// usually the required checks are still running; say why once, then
	// keep trying each tick
	why := pyErr(err)
	if isHTTP {
		why = he.Message
	}
	if t.PR.V.MergeWait.Or("") != why || !t.PR.V.MergeWait.Present() {
		v := t.PR.V
		v.MergeWait = ticket.Some(why)
		t.PR = ticket.Some(v)
		ticket.Event(t, "pr", "approved; not merged yet: "+why, "",
			ticket.PRNote{Action: ticket.PRMergeWaiting, Number: n, Why: why}, f.now())
	}
	return nil
}

// pickUpChanges is pick_up_changes: the review bodies and the PR's
// line comments that belong to those reviews become one note for the
// worker, and the ticket goes back for rework with its PR a draft
// meanwhile.
func (f *Flow) pickUpChanges(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, reviews []github.Review) error {
	n := t.PR.V.Number
	var points []string
	ids := map[int64]bool{}
	for _, r := range reviews {
		if b := textx.Strip(r.Body); b != "" {
			points = append(points, b)
		}
		ids[r.ID] = true
	}
	// the PR's comment list carries file line numbers; the per-review list
	// only has diff positions
	comments, err := f.GitHub.ReviewComments(ctx, p.GitHub, int64(n))
	if err != nil {
		return err
	}
	for _, c := range comments {
		if rid, ok := c.Raw.Get("pull_request_review_id").Int64(); !ok || !ids[rid] {
			continue
		}
		line := jsonx.String("?")
		if l := c.Raw.Get("line"); jsonx.Truthy(l) {
			line = l
		} else if l := c.Raw.Get("original_line"); jsonx.Truthy(l) {
			line = l
		}
		points = append(points, "`"+jsonx.PyStr(c.Raw.Get("path"))+"` line "+jsonx.PyStr(line)+": "+
			textx.Strip(jsonx.PyStr(dictGet(c.Raw, "body", jsonx.String("")))))
	}
	items := make([]string, len(points))
	for i, pt := range points {
		items[i] = "- " + pt
	}
	note := "Review on GitHub, changes requested:\n" + strings.Join(items, "\n") + "\n" + reviewAsk
	now := f.now()
	ticket.AddNote(t, note, "", nil, now)
	markSeen(t, reviews)
	ticket.Move(t, string(model.ColReady), model.ReasonChangesRequested, "changes requested on GitHub",
		itoa(len(points))+" point(s) from your review of PR #"+itoa(n)+" went to the worker as a note", "", now)
	f.hold(ctx, cfg, t)
	return nil
}
