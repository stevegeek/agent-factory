package prflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// OpenPR is open_pr: it opens the ticket's PR, or updates it when
// the ticket's PR is still open on its branch (a rework), and moves the
// ticket to pr_open.
//
// The ticket must be in ready_for_pr with a gate, and its branch must still
// be at the commit the gate checked; on a hygiene project public review
// must have passed that commit and the texts the PR carries. The branch is pushed, then the PR is
// created (a draft unless the gate called it eligible for automatic
// approval) or its title, body and draft state follow the new gate. A 422
// on create means the branch has an open PR already, which is adopted. A
// rework after reviews posts "Updated after your review." with the worker's
// summary, and the configured reviewers are asked for a review; neither
// failing stops the PR.
//
// t is changed only once GitHub has the PR, or when public review has not
// passed the commit, which moves it to the gate (the error wraps
// ErrUnread); any other error leaves the pushed branch and possibly the PR
// on GitHub, and the ticket as it was.
func (f *Flow) OpenPR(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (ticket.PR, error) {
	g, hasGate := t.Gate.Get()
	if t.Column != string(model.ColReadyForPR) || !hasGate {
		return ticket.PR{}, refusef(string(t.ID) + " is not ready for a PR")
	}
	if p.GitHub == "" {
		return ticket.PR{}, refusef(t.Project + " has no GitHub repository")
	}
	branch := t.Branch.Or("")
	head, err := f.Git.Head(ctx, p, branch)
	if err != nil {
		return ticket.PR{}, err
	}
	if head != g.Head {
		return ticket.PR{}, refusef("the branch moved after Gate 1 checked it; send it through the gate again")
	}
	if p.Hygiene && !PublicPassed(cfg, p, t, g.Head) {
		// nothing reaches GitHub unread, whatever brought the ticket here:
		// it goes back to the gate, which has public review read it first
		ticket.Move(t, string(model.ColGate), model.ReasonPublicUnread,
			"public review has not passed the commit Gate 1 checked; it reads it first", "", "", f.now())
		return ticket.PR{}, &Refusal{Message: "public review has not passed the commit Gate 1 checked; sent to public review", err: ErrUnread}
	}
	title, body := PRTitle(cfg, p, t), PRBody(cfg, t)
	if p.Hygiene {
		// the last look at exactly what is published; the hits name terms,
		// so the refusal only counts them
		hits := gate.Hygiene(cfg.Hygiene.Deny, gate.HygieneIn{Branch: branch,
			Texts: map[string]string{"the PR title": title, "the PR body": body}})
		if len(hits) > 0 {
			return ticket.PR{}, refusef(fmt.Sprintf("public-repo hygiene: the PR's title, branch or body has %d finding(s); nothing was pushed", len(hits)))
		}
	}
	if err := f.push(ctx, p, branch); err != nil {
		return ticket.PR{}, err
	}
	repo := p.GitHub
	owner, _, _ := strings.Cut(repo, "/")
	draft := !g.AutoEligible
	pr, err := f.openPROf(ctx, repo, t)
	if err != nil {
		return ticket.PR{}, err
	}
	updated := pr != nil
	if pr == nil {
		pr, err = f.GitHub.CreatePR(ctx, repo, github.NewPR{Title: title, Body: body, Head: branch, Base: p.Base, Draft: draft})
		var he *github.HTTPError
		if errors.As(err, &he) && he.Code == 422 {
			existing, ferr := f.GitHub.FindOpenPR(ctx, repo, owner, branch)
			if ferr != nil {
				return ticket.PR{}, ferr
			}
			if existing == nil {
				return ticket.PR{}, err
			}
			pr, err, updated = existing, nil, true
		}
		if err != nil {
			return ticket.PR{}, err
		}
	}
	if updated {
		// a rework: the same PR carries the new commits; its text and draft
		// state follow the new gate
		pr, err = f.GitHub.UpdatePR(ctx, repo, pr.Number, title, body)
		if err != nil {
			return ticket.PR{}, err
		}
		if !pr.HasDraft || pr.Draft != draft {
			if err := f.GitHub.SetDraft(ctx, pr.NodeID, draft); err != nil {
				return ticket.PR{}, err
			}
			pr.Draft, pr.HasDraft = draft, true
		}
	}
	isDraft := draft
	if pr.HasDraft {
		isDraft = pr.Draft
	}
	now := f.now()
	cur := t.PR.V
	if !t.PR.Present() {
		cur = ticket.PR{}
	}
	cur.Number, cur.URL, cur.Draft = int(pr.Number), pr.HTMLURL, ticket.Some(isDraft)
	// a refusal was about the commits before this push; the review on
	// pr.review names its commit, so it needs nothing
	cur.MergeBlocked = ticket.Opt[string]{}
	t.PR = ticket.Some(cur)
	detail := "PR " + pr.HTMLURL
	if isDraft {
		detail = "draft " + detail
	}
	if updated {
		detail += " updated"
	}
	ticket.Move(t, string(model.ColPROpen), model.ReasonPROpened, detail, "", "", now)
	if updated && len(cur.ReviewsSeen) > 0 {
		// what the rework did answers the review's points and questions;
		// public text, no internal ids
		text := textx.Strip("Updated after your review.\n\n" + reworkText(t) + "\n\n" + cfg.Identity.PRFooter)
		if err := f.GitHub.Comment(ctx, repo, pr.Number, text); err != nil {
			ticket.Event(t, "error", "github: could not comment on PR #"+itoa(int(pr.Number))+" ("+errCode(err)+")", "",
				ticket.Errored{Text: textx.Prefix(pyErr(err), 250)}, now)
		}
	}
	f.requestReview(ctx, cfg, p, t)
	return t.PR.V, nil
}

// reworkText is what the rework that last updated the PR did: its entry
// under Updates, or the work summary when that rework recorded none.
func reworkText(t *ticket.Ticket) string {
	opened := ""
	for _, e := range t.History[:max(0, len(t.History)-1)] {
		if m, ok := e.Data.(ticket.Moved); ok && m.Reason == model.ReasonPROpened {
			opened = e.At
		}
	}
	if n := len(t.Updates); n > 0 && t.Updates[n-1].At >= opened {
		return t.Updates[n-1].Text
	}
	return textx.Strip(jsonx.PyStr(dictGet(dictOr(t.Tree().Get("reports"), "work"), "summary", jsonx.String(""))))
}

// prTitleMax is the longest PR title made from a summary, in code points.
const prTitleMax = 72

// PRTitle is the PR's title: the ticket's, unless the project is a hygiene
// project and the title has a deny term (tickets cannot be renamed); then
// it is the first sentence of the work summary, clipped to prTitleMax, or
// a neutral one when there is no summary. A summary with a term has
// failed Gate 1 already.
func PRTitle(cfg *config.Config, p config.Project, t *ticket.Ticket) string {
	if !p.Hygiene || !denied(cfg.Hygiene.Deny, t.Title) {
		return t.Title
	}
	summary := textx.Fold(jsonx.PyStr(dictGet(dictOr(t.Tree().Get("reports"), "work"), "summary", jsonx.String(""))))
	if summary == "" {
		return "A change"
	}
	return textx.Clip(textx.SentenceSplit(summary)[0], prTitleMax)
}

// openPROf is open_pr_of: the ticket's PR when it is still open on
// its branch. A PR that is gone (404) is not an error: a new one is opened.
func (f *Flow) openPROf(ctx context.Context, repo string, t *ticket.Ticket) (*github.PR, error) {
	n := t.PR.V.Number
	if !t.PR.Present() || n == 0 {
		return nil, nil
	}
	pr, err := f.GitHub.PR(ctx, repo, int64(n))
	var he *github.HTTPError
	if errors.As(err, &he) && he.Code == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pr.State == "open" && pr.HeadRef == t.Branch.Or("") {
		return pr, nil
	}
	return nil, nil
}

// requestReview is request_review: the configured reviewers are
// asked, with a "review requested" event. A failure is only an error event:
// the PR is open either way.
func (f *Flow) requestReview(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) {
	reviewers := cfg.GitHub.Reviewers
	if len(reviewers) == 0 {
		return
	}
	who := strings.Join(reviewers, ", ")
	if err := f.GitHub.RequestReviewers(ctx, p.GitHub, int64(t.PR.V.Number), reviewers); err != nil {
		ticket.Event(t, "error", "github: review request for "+who+" failed ("+errCode(err)+")", "",
			ticket.Errored{Text: textx.Prefix(pyErr(err), 250)}, f.now())
		return
	}
	ticket.Event(t, "review requested", who, "", ticket.ReviewRequested{Reviewers: reviewers}, f.now())
}

// errCode is the HTTP status of a refusal (Python's e.code), or the error
// itself when GitHub did not answer (where Python's tick would have recorded
// the whole exception instead).
func errCode(err error) string {
	var he *github.HTTPError
	if errors.As(err, &he) {
		return itoa(he.Code)
	}
	return textx.Prefix(err.Error(), 200)
}
