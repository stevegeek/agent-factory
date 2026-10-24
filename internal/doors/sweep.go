package doors

import (
	"context"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Sweep caps, in code points (Python slices).
const (
	sweepTitleMax = 200
	sweepBodyMax  = 6000
)

type issueKey struct {
	project string
	issue   int64
}

// Sweep is sweep(cfg), the github door: every open issue of each
// enabled factory project with a repository (and no `sweep = false`) that
// no ticket was filed for yet becomes a triage ticket, door github
// (untrusted), with the issue as its source. An issue is known by
// (project, issue number), whatever its ticket's column. A repository whose
// issues cannot be fetched is logged and passed over. Only the first 50
// open issues are seen (no pagination).
func (s Service) Sweep(ctx context.Context, cfg *config.Config) error {
	known := map[issueKey]bool{}
	ts, _ := s.Board.Reader.List()
	for _, t := range ts {
		if src, ok := t.Source.Get(); ok {
			known[issueKey{t.Project, src.Issue}] = true
		}
	}
	for _, p := range cfg.FactoryProjects() {
		repo := p.GitHub
		if repo == "" || !p.Sweep || !cfg.Enabled(p.Key) {
			continue
		}
		issues, err := s.Issues.OpenIssues(ctx, repo)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("sweep %s: %w", repo, err)
			}
			s.logf("sweep %s: %v", repo, err)
			continue
		}
		for _, issue := range issues {
			if known[issueKey{p.Key, issue.Number}] {
				continue
			}
			body := textx.Prefix(issue.Body, sweepBodyMax)
			src := ticket.Source{Issue: issue.Number, URL: issue.HTMLURL}
			_, err := s.file(ctx, ticket.NewIn{
				Project: p.Key,
				Title:   textx.Prefix(issue.Title, sweepTitleMax),
				Body:    fmt.Sprintf("Reported as %s#%d.\n\n%s", repo, issue.Number, body),
				Door:    string(model.DoorGitHub),
				Column:  string(model.ColTriage),
			}, func(t *ticket.Ticket) { t.Source = ticket.Some(src) })
			if err != nil {
				return fmt.Errorf("sweep %s: %w", repo, err)
			}
		}
	}
	return nil
}
