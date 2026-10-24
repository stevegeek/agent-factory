package cli

import (
	"context"
	"errors"
	"strconv"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deploy"
	"github.com/stevegeek/agent-factory/internal/prflow"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// offerer is how a merge asks for a deploy (deploy.Service.Offer).
type offerer interface {
	Offer(ctx context.Context, cfg *config.Config, a deploy.Asker, rev, why, by string, hold bool) error
}

// operatorDeploys is the operator's operator.Deployer: the deploy Offer of
// main with the doors' asker and no hold of its own, so a hold file or an
// open Deploy? ask still turns it into a Deploy? line.
type operatorDeploys struct {
	Deploy offerer
	Asks   deploy.Asker
}

func (o operatorDeploys) Offer(ctx context.Context, cfg *config.Config, why, by string) error {
	return o.Deploy.Offer(ctx, cfg, o.Asks, "main", why, by, false)
}

// selfDeploys is the PR flow's prflow.Deployer: a merge of the factory's
// own core or config offers a deploy of main, which holds the merge (every
// deploy is core main with config main, so a config merge offers the same
// as a core one). The deploy fetches main itself, so the merge commit is
// for the reason and the notice. A failed offer files a notice and
// returns its error; the merge stands, and the next merge's deploy carries
// this one's change. The notice does not say no request waits: Start
// writes it before it can fail (no user bus, systemd-run).
type selfDeploys struct {
	Config  *config.Config
	Deploy  offerer
	Asks    deploy.Asker
	Notices deploy.Noticer
}

func (s selfDeploys) Merged(ctx context.Context, p config.Project, t *ticket.Ticket, m prflow.MergedPR) error {
	what := p.Key + " " + textx.Prefix(orText(m.Commit, "?"), 12)
	why := "PR #" + strconv.Itoa(t.PR.V.Number) + " merged (" + what + ")"
	err := s.Deploy.Offer(ctx, s.Config, s.Asks, "main", why, "factory", m.Hold)
	if err == nil {
		return nil
	}
	if s.Notices != nil {
		body := "The factory merged PR #" + strconv.Itoa(t.PR.V.Number) + " of " + p.Key + " (" + string(t.ID) +
			") but could not start its deploy: " + err.Error() + "\n\nThe merge stands. A request may be waiting: " +
			"`factory deploy status` shows it and `factory deploy run` runs it. Otherwise deploy main with " +
			"`factory deploy start`, or let the next merge's deploy carry it."
		if nerr := s.Notices.Notice(ctx, "Deploy not started: "+what, body); nerr != nil {
			return errors.Join(err, nerr)
		}
	}
	return err
}
