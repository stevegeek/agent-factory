package prflow

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// errorEvery is how often a kept intent's failure is written to the ticket.
const errorEvery = time.Hour

// Pass is the GitHub half of the tick's pass (tick_pass), run only
// when [github] installation_id is set: DrainIntents first, then each ticket
// in pr_open is followed on GitHub (CheckPR) and each in ready_for_pr has its
// PR opened when [github] open_prs = "auto" or it is a rework of a PR it
// already has. A ticket of a project without a GitHub repository is passed
// over. An error on a ticket becomes an error event on it, which is saved,
// except a failure to reach GitHub (no answer, or a 5xx): that is not the
// ticket's, so the ticket is left for the next pass and the failure goes to
// Reach, which says once when GitHub stays unreachable and once when it is
// back. A pass is a failure only when no call got an answer.
//
// ts is the tick's list of tickets; each ticket the pass visits is re-read
// under its lock and replaced in ts by what was saved. Pass returns an
// error only when it could not save, or when ctx ended (nothing is saved
// after that).
func (f *Flow) Pass(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error {
	if !cfg.HasGitHub() {
		return nil
	}
	merged, err := f.drain(ctx, cfg, ts)
	if err != nil {
		return err
	}
	var answered, failed bool
	var lastErr string
	err = f.Board.EachLocked(ctx, ts, func(t *ticket.Ticket) bool {
		return t.Column == string(model.ColPROpen) || t.Column == string(model.ColReadyForPR)
	}, func(t *ticket.Ticket) error {
		p, ok := cfg.Project(t.Project)
		if !ok || p.GitHub == "" || merged[t.ID] {
			return nil
		}
		var err error
		switch {
		case t.Column == string(model.ColPROpen) && jsonx.Truthy(t.Tree().Get("pr").Get("merge_requested")):
			return nil // a merge intent kept for the next tick: it is not followed meanwhile
		case t.Column == string(model.ColPROpen):
			err = f.CheckPR(ctx, cfg, *p, t)
		case cfg.GitHub.OpenPRs == "auto" || t.PR.V.Number != 0:
			// a rework of a PR you sent back goes out by itself: you
			// already asked for it
			_, err = f.OpenPR(ctx, cfg, *p, t)
		default:
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		switch {
		case unreachable(err):
			failed, lastErr = true, textx.Prefix(pyErr(err), 200)
		case err != nil && !errors.Is(err, ErrUnread): // an unread commit's move says why
			answered = true
			githubError(t, err, f.now())
		default:
			answered = true
		}
		return f.Board.Save(t)
	})
	if err == nil && (answered || failed) {
		f.noteReach(answered, lastErr)
	}
	return err
}

func (f *Flow) noteReach(ok bool, lastErr string) {
	if f.Reach == nil {
		return
	}
	line, err := f.Reach.Note(f.now(), ok, lastErr)
	if err != nil {
		f.logf("github: keeping reachability: %v", err)
	}
	if line != "" {
		f.logf("%s", line)
	}
}

// unreachable reports whether err is GitHub not answering (no answer, or
// a 5xx) rather than an answer about the ticket.
func unreachable(err error) bool {
	var ne *github.NetError
	var he *github.HTTPError
	return errors.As(err, &ne) || errors.As(err, &he) && he.Code >= 500
}

// DrainIntents carries out the intents Foreman records on a ticket's pr,
// first in the GitHub pass, over every ticket that has one: a hold
// (the PR back to draft) in any column, then an open in ready_for_pr or
// pr_open; then, over the tickets in pr_open, a merge request.
//
// Each intent is cleared in the save that records its outcome. One that
// GitHub will refuse again is cleared with what Python records ("not
// opened (asked <stamp>): …", "not merged (asked <stamp>): …", or an
// error event); one that retrying can fix is kept, with an error event at
// most once an hour. A merge GitHub refuses for want of the maintainer's
// approval is cleared too, with "not merged (asked <stamp>): waiting for
// your approval on GitHub (…)" and pr.merge_blocked = "approval". A merge is left for the next tick, with nothing
// recorded, when ctx's deadline leaves too little time for the merge check.
func (f *Flow) DrainIntents(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error {
	if !cfg.HasGitHub() {
		return nil
	}
	_, err := f.drain(ctx, cfg, ts)
	return err
}

func holdOrOpen(t *ticket.Ticket) bool {
	pr := t.PR.V
	if !t.PR.Present() {
		return false
	}
	return pr.HoldRequested.Set || pr.OpenRequested.Set && openColumn(t.Column)
}

func openColumn(c string) bool {
	return c == string(model.ColReadyForPR) || c == string(model.ColPROpen)
}

// drain is DrainIntents; it returns the tickets whose merge request it
// handled, which the rest of the pass leaves alone (Python's loop took the
// merge branch for them).
func (f *Flow) drain(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) (map[ticket.ID]bool, error) {
	err := f.Board.EachLocked(ctx, ts, holdOrOpen, func(t *ticket.Ticket) error {
		if t.PR.V.HoldRequested.Set {
			f.holdIntent(ctx, cfg, t)
		}
		if t.PR.V.OpenRequested.Set && openColumn(t.Column) {
			f.openIntent(ctx, cfg, t)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return f.Board.Save(t)
	})
	if err != nil {
		return nil, err
	}
	merged := map[ticket.ID]bool{}
	err = f.Board.EachLocked(ctx, ts, func(t *ticket.Ticket) bool {
		return t.Column == string(model.ColPROpen) && t.PR.Present() && jsonx.Truthy(t.Tree().Get("pr").Get("merge_requested"))
	}, func(t *ticket.Ticket) error {
		p, ok := cfg.Project(t.Project)
		if !ok || p.GitHub == "" {
			return nil
		}
		if !f.timeForMerge(ctx) {
			f.logf("%s: the merge of PR #%d waits for the next tick: too little time left in this one for a merge check",
				t.ID, t.PR.V.Number)
			return nil
		}
		merged[t.ID] = true
		f.mergeIntent(ctx, cfg, *p, t)
		if err := ctx.Err(); err != nil {
			return err
		}
		return f.Board.Save(t)
	})
	return merged, err
}

// asked is how an intent's stamp reads in "(asked <stamp>)": Python's
// f-string of the popped value.
func askedText(t *ticket.Ticket, key string) string {
	return jsonx.PyStr(t.Tree().Get("pr").Get(key))
}

func (f *Flow) holdIntent(ctx context.Context, cfg *config.Config, t *ticket.Ticket) {
	since := askedText(t, "hold_requested")
	n, err := f.holdOnce(ctx, cfg, t)
	switch {
	case err == nil:
		clearIntent(t, "hold")
	case transient(err):
		f.keptError(t, since, holdFailure(n, err), textx.Prefix(pyErr(err), 200))
	default:
		clearIntent(t, "hold")
		holdError(t, n, err, f.now())
	}
}

func (f *Flow) openIntent(ctx context.Context, cfg *config.Config, t *ticket.Ticket) {
	since := askedText(t, "open_requested")
	var err error
	if p, ok := cfg.Project(t.Project); !ok || p.GitHub == "" {
		err = refusef(t.Project + " has no GitHub repository")
	} else {
		_, err = f.OpenPR(ctx, cfg, *p, t)
	}
	f.settle(t, "open", since, err)
}

func (f *Flow) mergeIntent(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) {
	since := askedText(t, "merge_requested")
	err := f.MergePR(ctx, cfg, p, t)
	if errors.Is(err, ErrNeedsApproval) {
		// GitHub merges it only once the maintainer approves it, and that
		// approval merges it by itself (followReviews): drop the intent
		// rather than try it every tick, and say what it waits for
		clearIntent(t, "merge")
		ticket.ClearQueue(t)
		v := t.PR.V
		v.MergeBlocked = ticket.Some(ticket.MergeBlockedApproval)
		t.PR = ticket.Some(v)
		why := textx.Prefix(strings.Join(strings.Fields(pyErr(err)), " "), 250)
		ticket.Event(t, "pr", "not merged (asked "+since+"): waiting for your approval on GitHub ("+why+")", "",
			ticket.PRNote{Action: ticket.PRNotMerged, Number: v.Number, Why: why}, f.now())
		return
	}
	f.settle(t, "merge", since, err)
}

// settle records an open's or a merge's outcome and clears its intent,
// unless retrying can help.
func (f *Flow) settle(t *ticket.Ticket, which, since string, err error) {
	var he *github.HTTPError
	switch {
	case err == nil:
		clearIntent(t, which)
		clearQueue(t, which)
	case errors.Is(err, gate.ErrNoRoom):
		f.logf("%s: the merge of PR #%d waits for a later tick: %v", t.ID, t.PR.V.Number, err)
		noRoom(t, err, f.now())
	case transient(err):
		text := textx.Prefix(pyErr(err), 250)
		if which == "merge" && !errors.As(err, &he) {
			ticket.SetQueue(t, ticket.PRQueue{State: ticket.QueueMerging, Detail: text, Since: clock.Stamp(f.now())})
		}
		f.keptError(t, since, "github: "+text, text)
	case runtimeLike(err):
		clearIntent(t, which)
		clearQueue(t, which)
		not, action := "not merged", ticket.PRNotMerged
		if which == "open" {
			not, action = "not opened", ticket.PRNotOpened
		}
		why := textx.Prefix(pyErr(err), 250)
		ticket.Event(t, "pr", not+" (asked "+since+"): "+why, "",
			ticket.PRNote{Action: action, Number: t.PR.V.Number, Why: why}, f.now())
	default:
		clearIntent(t, which)
		clearQueue(t, which)
		githubError(t, err, f.now())
	}
}

// clearQueue drops a merge's wait once its intent is settled: a later
// approval queues it again.
func clearQueue(t *ticket.Ticket, which string) {
	if which == "merge" {
		ticket.ClearQueue(t)
	}
}

func clearIntent(t *ticket.Ticket, which string) {
	if !t.PR.Present() {
		return
	}
	v := t.PR.V
	switch which {
	case "hold":
		v.HoldRequested = ticket.Opt[string]{}
	case "open":
		v.OpenRequested = ticket.Opt[string]{}
	case "merge":
		v.MergeRequested = ticket.Opt[string]{}
	}
	t.PR = ticket.Some(v)
}

// keptError records a kept intent's failure (detail, and text, the error
// itself), unless an error event from GitHub was already written for it in
// the last hour.
func (f *Flow) keptError(t *ticket.Ticket, since, detail, text string) {
	now := f.now()
	asked, askedErr := clock.Parse(since)
	for i := len(t.History) - 1; i >= 0; i-- {
		e := t.History[i]
		if e.Event != "error" || !strings.HasPrefix(e.Detail, "github: ") {
			continue
		}
		when, err := clock.Parse(e.At)
		if err != nil {
			continue
		}
		if now.Sub(when) >= errorEvery {
			break
		}
		if askedErr != nil || !when.Before(asked) {
			return
		}
	}
	ticket.Event(t, "error", detail, "", ticket.Errored{Text: text}, now)
}

// githubError records GitHub's refusal as an error event.
func githubError(t *ticket.Ticket, err error, now time.Time) {
	text := textx.Prefix(pyErr(err), 250)
	ticket.Event(t, "error", "github: "+text, "", ticket.Errored{Text: text}, now)
}
