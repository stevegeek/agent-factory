package prflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// MergePR is merge_pr, the maintainer's go-ahead for an
// auto-merge-eligible PR: it merges exactly the commit the gate checked.
//
// It refuses (a *Refusal) a ticket that is not in pr_open with a PR, one the
// gate did not call eligible, a PR that is closed or a draft, a PR that moved
// since the gate, and a merge GitHub refuses; one GitHub refuses for want of
// an approving review wraps ErrNeedsApproval. A failure retrying can fix (no
// answer, a 5xx) is returned as it is.
func (f *Flow) MergePR(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) error {
	if t.Column != string(model.ColPROpen) || !jsonx.Truthy(t.Tree().Get("pr")) {
		return refusef(string(t.ID) + " has no open PR")
	}
	g, _ := t.Gate.Get()
	if !g.AutoEligible {
		return refusef(string(t.ID) + " is not auto-merge eligible; review and merge it on GitHub")
	}
	n := t.PR.V.Number
	pr, err := f.GitHub.PR(ctx, p.GitHub, int64(n))
	if err != nil {
		return err
	}
	if pr.State != "open" || pr.Draft {
		what := jsonx.PyStr(pr.Raw.Get("state"))
		if pr.Draft {
			what = "a draft"
		}
		return refusef("PR #" + itoa(n) + " is " + what + "; nothing to merge")
	}
	if pr.HeadSHA != g.Head || pr.HeadSHA == "" {
		return refusef("PR #" + itoa(n) + " moved since the gate checked it; it needs the gate again")
	}
	err = f.land(ctx, cfg, p, t, pr, pr.HeadSHA, "PR merged by the factory on your go-ahead",
		"PR #"+itoa(n)+" was auto-merge eligible; you confirmed it in Foreman and the factory merged "+textx.Prefix(pr.HeadSHA, 7))
	var he *github.HTTPError
	if errors.As(err, &he) && !transient(err) {
		r := refusef("GitHub refused the merge of PR #" + itoa(n) + ": " + he.Message)
		if refusedForApproval(he.Code, he.Message) {
			r.err = ErrNeedsApproval
		}
		return r
	}
	return err
}

// refusedForApproval reports a merge GitHub refused (code, its message) for
// want of an approving review: a ruleset's or a branch protection's, in
// GitHub's words. Conservative: only the codes a refused merge comes with,
// and only words about an approval or a required review; anything else is
// some other refusal. code 0 is a merge answered "merged": false.
func refusedForApproval(code int, message string) bool {
	switch code {
	case 0, 403, 405, 422:
	default:
		return false
	}
	m := strings.ToLower(message)
	for _, words := range []string{
		"require approval", "requires approval", // rulesets: "New changes require approval from someone other than the last pusher"
		"approving review",                           // "At least 1 approving review is required by reviewers with write access"
		"review is required", "reviews are required", // "Review is required by reviewers with write access"
		"code owner review", // "Waiting on code owner review from ..."
	} {
		if strings.Contains(m, words) {
			return true
		}
	}
	return false
}

// land is land_pr: merge the PR at exactly head (a merge commit),
// close the ticket as done and delete the bot branch. GitHub's own rules
// still apply: a required check that is not green makes it refuse (an
// *HTTPError). First, if the base moved since the gate, the checks must
// still pass with it merged in; if not, the ticket goes back for a rework
// and the error wraps ErrMainMoved.
//
// Once GitHub has merged, land returns nil whatever happens to the branch
// or to the deploy it asks for (merged). pr is the PR as the caller last
// read it, for its labels.
func (f *Flow) land(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, pr *github.PR, head, detail, rule string) error {
	n := t.PR.V.Number
	ok, out, err := f.Checks.StillHoldsOnMain(ctx, gate.HoldIn{Config: cfg, Project: p, Ticket: t})
	if err != nil {
		return err
	}
	if !ok {
		return f.mainMoved(ctx, cfg, p, t, out)
	}
	res, err := f.GitHub.Merge(ctx, p.GitHub, int64(n), head)
	if err != nil {
		return err
	}
	if !res.Merged {
		msg := "?"
		if o, isObj := res.Raw.Object(); isObj {
			if m, has := o.Lookup("message"); has {
				msg = jsonx.PyStr(m)
			}
		}
		r := refusef("GitHub did not merge PR #" + itoa(n) + ": " + msg)
		if refusedForApproval(0, msg) {
			r.err = ErrNeedsApproval
		}
		return r
	}
	now := f.now()
	ticket.Move(t, string(model.ColDone), model.ReasonPRMerged, detail, rule, "", now)
	v := t.PR.V
	v.MergeWait = ticket.Opt[string]{}
	v.MergeBlocked = ticket.Opt[string]{}
	t.PR = ticket.Some(v)
	ticket.ClearQueue(t)
	sha, _ := res.Raw.Get("sha").Str()
	f.merged(ctx, p, t, pr, sha)
	if branch := t.Branch.Or(""); strings.HasPrefix(branch, "bot/") {
		if err := f.GitHub.DeleteRef(ctx, p.GitHub, branch); err != nil {
			ticket.Event(t, "error", "github: merged, but could not delete "+branch+" ("+errCode(err)+")", "",
				ticket.Errored{Text: textx.Prefix(pyErr(err), 250)}, now)
		}
	}
	return nil
}

// merged is a merge on a project with self_deploy (any other project's
// ticket stays as Python wrote it): it records the merge commit GitHub
// made on pr.merge_commit and asks for the deploy, held when the PR
// carries HoldDeployLabel. A deploy that cannot be asked for is an error
// event and a log line; the merge stands (the deployer files the notice).
func (f *Flow) merged(ctx context.Context, p config.Project, t *ticket.Ticket, pr *github.PR, commit string) {
	if p.SelfDeploy == "" {
		return
	}
	if commit != "" {
		v := t.PR.V
		v.MergeCommit = commit
		t.PR = ticket.Some(v)
	}
	if f.Deploys == nil {
		return
	}
	m := MergedPR{Commit: commit, Hold: hasLabel(pr, HoldDeployLabel)}
	if err := f.Deploys.Merged(ctx, p, t, m); err != nil {
		f.logf("%s: PR #%d merged, deploy not requested: %v", t.ID, t.PR.V.Number, err)
		ticket.Event(t, "error", "deploy not requested: "+textx.Prefix(err.Error(), 250), "",
			ticket.Errored{Text: textx.Prefix(err.Error(), 250)}, f.now())
	}
}

// hasLabel reports whether pr carries the label name.
func hasLabel(pr *github.PR, name string) bool {
	if pr == nil {
		return false
	}
	labels, _ := pr.Raw.Get("labels").Array()
	for _, l := range labels {
		if s, _ := l.Get("name").Str(); s == name {
			return true
		}
	}
	return false
}

// mainMoved is land_pr's rework: the merge check's output goes to
// logs/<id>/merge-check-output.txt, a note asks for a rebase, the ticket
// goes back to ready and its PR to draft.
func (f *Flow) mainMoved(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, out string) error {
	n := t.PR.V.Number
	if f.LogsDir == "" {
		return errors.New("merge check: no logs directory for merge-check-output.txt")
	}
	dir := filepath.Join(f.LogsDir, string(t.ID))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("merge check %s: %w", t.ID, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "merge-check-output.txt"), []byte(out), 0o666); err != nil {
		return fmt.Errorf("merge check %s: %w", t.ID, err)
	}
	now := f.now()
	ticket.ClearQueue(t)
	ticket.AddNote(t, "Before merging PR #"+itoa(n)+", "+p.Base+" had moved, and with it merged in the tests or lint fail "+
		"(output: logs/"+string(t.ID)+"/merge-check-output.txt, the end of it: "+textx.Clip(lastRunes(out, 600), 600)+"). Rebase onto "+
		p.Base+", fix what broke, run the tests, and say what it was in your summary.", "", nil, now)
	ticket.Move(t, string(model.ColReady), model.ReasonMainMoved, "not merged: "+p.Base+" moved and the checks fail with it",
		"merge check: PR #"+itoa(n)+" passes alone but not on "+p.Base+" as it is now", "", now)
	f.hold(ctx, cfg, t)
	return &Refusal{Message: p.Base + " moved and the checks fail with it merged in; sent back for a rework", err: ErrMainMoved}
}
