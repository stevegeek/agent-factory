package ops

import (
	"context"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gitread"
)

// GitBranches is Branches over read-only git: Repo is the template (its
// Exec, Env and Guard) that each project's clone is read with. With a
// gitread.Verifier as the guard (Foreman's), a tampered .git/config is
// refused, never rewritten.
type GitBranches struct {
	Repo gitread.Repo
}

// BranchExists implements Branches.
func (g GitBranches) BranchExists(ctx context.Context, p config.Project, branch string) (bool, error) {
	r := g.Repo
	r.Path, r.Base, r.GitHub = p.Path, p.Base, p.GitHub
	return r.BranchExists(ctx, branch)
}
