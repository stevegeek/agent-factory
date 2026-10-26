package cli

import (
	"context"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/prflow"
)

func runPruneBranches(ctx context.Context, e *Env, a Args) error {
	return opsCLI{}.pruneBranches(ctx, e, a)
}

// pruneBranches is `factory prune-branches [--apply]`: one line per bot/*
// branch GitHub still has whose ticket is done with a merged PR. Without
// --apply it deletes nothing.
func (o opsCLI) pruneBranches(ctx context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	ts, _ := e.Board().Reader.List()
	f := &prflow.Flow{GitHub: o.githubClient(e, cfg), Clock: e.Clock}
	apply := a.Bool("apply")
	found, err := f.MergedBranches(ctx, cfg, ts, apply)
	refused := 0
	for _, b := range found {
		switch {
		case b.Err != nil:
			refused++
			fmt.Fprintf(e.Stdout, "could not delete %s %s (%s): %v\n", b.Repo, b.Branch, b.Ticket, b.Err)
		case b.Deleted:
			fmt.Fprintf(e.Stdout, "deleted %s %s (%s)\n", b.Repo, b.Branch, b.Ticket)
		default:
			fmt.Fprintf(e.Stdout, "%s %s (%s)\n", b.Repo, b.Branch, b.Ticket)
		}
	}
	if err != nil {
		return err
	}
	if !apply && len(found) > 0 {
		fmt.Fprintln(e.Stdout, "dry run: --apply deletes these")
	}
	if refused > 0 {
		return Refuse("%d of %d branches not deleted", refused, len(found))
	}
	return nil
}
