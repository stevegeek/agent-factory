package prflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Repos is the Git the dispatcher uses: each project's clone as Repo gives
// it (gate.Runner.Repo, which carries the process's guard and safe git
// environment), read through gitread and pushed through gitx.
type Repos struct {
	Repo func(config.Project) *gitread.Repo
}

// BranchExists is branch_exists.
func (r Repos) BranchExists(ctx context.Context, p config.Project, branch string) (bool, error) {
	return r.Repo(p).BranchExists(ctx, branch)
}

// Head is branch_head.
func (r Repos) Head(ctx context.Context, p config.Project, branch string) (string, error) {
	return r.Repo(p).Head(ctx, branch)
}

// Push is push_branch: a forced push of the bot/* branch to the project's
// GitHub repository, the token in the environment only.
func (r Repos) Push(ctx context.Context, p config.Project, branch, token string) error {
	return gitx.Writer{Repo: r.Repo(p)}.Push(ctx, branch, token)
}

// maxBranchTries bounds pick_branch's search for a free name.
const maxBranchTries = 10000

// PickBranch is pick_branch: bot/<slug of the title>, with -2, -3,
// ... added until no local branch has the name. The name carries no ticket
// id, because it is public; on a hygiene project a title with a deny term
// gives bot/change, since a merge keeps the branch name in history.
func PickBranch(ctx context.Context, g BranchChecker, p config.Project, title string, deny []string) (string, error) {
	base := "bot/" + textx.Slugify(title)
	if p.Hygiene && denied(deny, title) {
		base = "bot/change"
	}
	name := base
	for n := 2; n < maxBranchTries; n++ {
		taken, err := g.BranchExists(ctx, p, name)
		if err != nil {
			return "", fmt.Errorf("pick a branch for %q: %w", title, err)
		}
		if !taken {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return "", errors.New("pick a branch: every name up to " + name + " is taken")
}

// denied reports a deny term in text.
func denied(deny []string, text string) bool {
	return len(gate.Hygiene(deny, gate.HygieneIn{Branch: text})) > 0
}

// push is push_branch's refusal, the token, then the push. The refusal
// comes first, so nothing is minted for a branch that would never be
// pushed.
func (f *Flow) push(ctx context.Context, p config.Project, branch string) error {
	if !strings.HasPrefix(branch, "bot/") {
		return refusef("refusing to push " + branch + ": the factory only pushes bot/* branches")
	}
	if f.GitHub == nil || f.GitHub.Auth == nil {
		return refusef("no GitHub App token for the push")
	}
	tok, err := f.GitHub.Auth.Token(ctx)
	if err != nil {
		return fmt.Errorf("github: installation token: %w", err)
	}
	return f.Git.Push(ctx, p, branch, tok)
}
