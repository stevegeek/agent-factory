package prflow

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// MergedBranch is a bot/* branch GitHub still has whose ticket is done
// with a merged PR.
type MergedBranch struct {
	Ticket  ticket.ID
	Repo    string
	Branch  string
	Deleted bool
	// Err is GitHub's refusal of the delete.
	Err error
}

// MergedBranches finds, for every configured project with a GitHub
// repository, the bot/* branches of ts that GitHub still has and whose
// ticket is done with a merged PR. With apply it deletes them; without,
// it only lists them. A refused delete is the branch's Err; the others
// still go.
func (f *Flow) MergedBranches(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket, apply bool) ([]MergedBranch, error) {
	seen := map[string]bool{}
	var out []MergedBranch
	for _, t := range ts {
		p, ok := cfg.Project(t.Project)
		branch := t.Branch.Or("")
		if !ok || p.GitHub == "" || !strings.HasPrefix(branch, "bot/") || !mergedDone(t) || seen[p.GitHub+" "+branch] {
			continue
		}
		seen[p.GitHub+" "+branch] = true
		if _, err := f.GitHub.BranchHead(ctx, p.GitHub, branch); err != nil {
			var he *github.HTTPError
			if errors.As(err, &he) && he.Code == 404 {
				continue
			}
			return out, err
		}
		out = append(out, MergedBranch{Ticket: t.ID, Repo: p.GitHub, Branch: branch})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Branch < out[j].Branch
	})
	if !apply {
		return out, nil
	}
	for i := range out {
		if err := f.GitHub.DeleteRef(ctx, out[i].Repo, out[i].Branch); err != nil {
			out[i].Err = err
			continue
		}
		out[i].Deleted = true
	}
	return out, nil
}

func mergedDone(t *ticket.Ticket) bool {
	if t.Column != string(model.ColDone) {
		return false
	}
	for i := len(t.History) - 1; i >= 0; i-- {
		if mv, ok := t.History[i].Data.(ticket.Moved); ok {
			return mv.Reason == model.ReasonPRMerged
		}
	}
	return false
}
