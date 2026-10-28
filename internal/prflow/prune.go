package prflow

import (
	"context"
	"errors"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// PruneBranch deletes the bot/* branch of t, a ticket closed by hand with
// no PR, on GitHub (when the project has a repository there and GitHub is
// set up) and in the project's clone, and clears t's prune request. A
// branch that is already gone is no error; any other failure is an error
// event on t and the error returned, and the branch is not asked for again.
func (f *Flow) PruneBranch(ctx context.Context, cfg *config.Config, t *ticket.Ticket) error {
	t.PruneRequested = ticket.Opt[string]{}
	branch := t.Branch.Or("")
	p, ok := cfg.Project(t.Project)
	if !strings.HasPrefix(branch, "bot/") || !ok {
		return nil
	}
	var errs []error
	if p.GitHub != "" && cfg.HasGitHub() && f.GitHub != nil {
		var he *github.HTTPError
		if err := f.GitHub.DeleteRef(ctx, p.GitHub, branch); err != nil && !(errors.As(err, &he) && (he.Code == 404 || he.Code == 422)) {
			errs = append(errs, err)
			ticket.Event(t, "error", "github: could not delete "+branch+" ("+errCode(err)+")", "",
				ticket.Errored{Text: textx.Prefix(pyErr(err), 250)}, f.now())
		}
	}
	if f.Git != nil {
		exists, err := f.Git.BranchExists(ctx, *p, branch)
		if err == nil && exists {
			err = f.Git.DeleteBranch(ctx, *p, branch)
		}
		if err != nil {
			errs = append(errs, err)
			ticket.Event(t, "error", "could not delete the local "+branch, "",
				ticket.Errored{Text: textx.Prefix(err.Error(), 250)}, f.now())
		}
	}
	return errors.Join(errs...)
}

// drainPrunes carries out the prune requests Foreman records, whether or
// not GitHub is set up: the local branch goes either way.
func (f *Flow) drainPrunes(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error {
	return f.Board.EachLocked(ctx, ts, func(t *ticket.Ticket) bool { return t.PruneRequested.Set }, func(t *ticket.Ticket) error {
		if err := f.PruneBranch(ctx, cfg, t); err != nil {
			f.logf("%s: %v", t.ID, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return f.Board.Save(t)
	})
}
