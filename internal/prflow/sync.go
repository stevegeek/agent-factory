package prflow

import (
	"context"
	"errors"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Changer changes one ticket under its lock and saves it when fn returns
// nil (store.Board.Changing with the CLI's wait: StoreChanger).
type Changer interface {
	Change(ctx context.Context, name string, fn func(*ticket.Ticket) error) error
}

// StoreChanger is a Changer over the board, waiting as long as a CLI
// command does for the tick to let go of a ticket.
type StoreChanger struct{ Board *store.Board }

// Change is store.Board.Changing with store.CLIWait.
func (s StoreChanger) Change(ctx context.Context, name string, fn func(*ticket.Ticket) error) error {
	return s.Board.Changing(ctx, name, store.CLIWait, fn)
}

// ConfigSource gives the config as it is now (*config.Source).
type ConfigSource interface {
	Current() (*config.Config, error)
}

// Sync is the CLI's PR actions (ops.PRActions, §6.1, §7.3): done at once,
// in the calling process, which holds the App's credentials. Foreman passes
// intents instead.
type Sync struct {
	Flow   *Flow
	Board  Changer
	Config ConfigSource
}

// factoryOnly is factory_ticket_only: a board ticket has no PRs.
func factoryOnly(t *ticket.Ticket) error {
	if t.Board() {
		return &Refusal{Code: "factory_ticket", Message: string(t.ID) + " is on a board project; use `factory ticket …`"}
	}
	return nil
}

// Open is cmd_pr (F:4608): the ticket's PR is opened (or updated) now, and
// its URL returned. Nothing is saved when it fails.
func (s Sync) Open(ctx context.Context, id ticket.ID) (string, error) {
	cfg, err := s.Config.Current()
	if err != nil {
		return "", err
	}
	var url string
	err = s.Board.Change(ctx, string(id), func(t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		p, ok := cfg.Project(t.Project)
		if !ok || p.GitHub == "" {
			return refusef(t.Project + " has no GitHub repository")
		}
		pr, err := s.Flow.OpenPR(ctx, cfg, *p, t)
		if err != nil {
			return err
		}
		url = pr.URL
		return nil
	})
	return url, err
}

// Hold is the hold_pr that cmd_move does when a ticket leaves pr_open for
// anything but done or rejected: the PR goes back to draft before the
// command returns. A failure is saved on the ticket and returned as a
// *NotHeldError, which the CLI prints as a warning.
func (s Sync) Hold(ctx context.Context, id ticket.ID) error {
	cfg, err := s.Config.Current()
	if err != nil {
		return err
	}
	var held error
	err = s.Board.Change(ctx, string(id), func(t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		held = s.Flow.HoldPR(ctx, cfg, t)
		var nh *NotHeldError
		if held != nil && !errors.As(held, &nh) {
			return held
		}
		return nil
	})
	if err != nil {
		return err
	}
	return held
}
