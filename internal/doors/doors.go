// Package doors is how work comes in without a person filing it
// (ARCHITECTURE.md §2.3, inventory-features §10-12): the weekly GitHub issue
// sweep, the daily clock (schedules, the monthly memory report and the
// weekly version check), the watches the tick runs, and notices, the
// needs_human tickets the factory files about itself.
//
// Every ticket is filed through store.Board, as Python's new_ticket, and a
// field Python sets after that first save (a sweep's source, a schedule's
// or watch's kind) is set and saved the same way, so the key order is
// Python's. Unlike Python, the new ticket's lock is held across both saves,
// so a tick never sees the ticket between them.
package doors

import (
	"context"
	"errors"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Logger is the factory log (*logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Issues is what the sweep reads of GitHub (*github.Client).
type Issues interface {
	OpenIssues(ctx context.Context, repo string) ([]github.Issue, error)
}

// MemoryReporter gathers the monthly memory report (*memory.Service).
// closed holds the ids of the tickets in done or rejected.
type MemoryReporter interface {
	Report(ctx context.Context, cfg *config.Config, closed map[string]bool) (string, bool)
}

// VersionChecker is the weekly version check the clock runs in-process
// (upkeep.Service, which files its own notices).
type VersionChecker interface {
	Versions(ctx context.Context, cfg *config.Config) error
}

// Service runs the doors on one factory root.
type Service struct {
	Board *store.Board // its Clock is the time of every stamp
	// Exec runs the watch commands; nil is execx.OS.
	Exec     execx.Execer
	Issues   Issues
	Memory   MemoryReporter // nil reports nothing
	Versions VersionChecker // nil checks nothing
	Log      Logger         // nil logs nothing
}

func (s Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func (s Service) exec() execx.Execer {
	if s.Exec == nil {
		return execx.OS{}
	}
	return s.Exec
}

// file is new_ticket followed, when then is given, by Python's
// `t[...] = ...; save_ticket(t)`: the ticket is saved as new_ticket saves
// it, then changed and saved again. The ticket's lock is held from before
// the first save to after the second.
func (s Service) file(ctx context.Context, in ticket.NewIn, then func(*ticket.Ticket)) (*ticket.Ticket, error) {
	b := s.Board
	id, err := b.NextID(ctx)
	if err != nil {
		return nil, fmt.Errorf("file %q: %w", in.Title, err)
	}
	l, err := b.Lock(ctx, id, store.Forever)
	if err != nil {
		return nil, fmt.Errorf("file %s: %w", id, err)
	}
	defer l.Unlock()
	t := ticket.New(id, in, b.Clock.Now())
	if err := b.Save(t); err != nil {
		return nil, fmt.Errorf("file %s: %w", id, err)
	}
	if then != nil {
		then(t)
		if err := b.Save(t); err != nil {
			return nil, fmt.Errorf("file %s: %w", id, err)
		}
	}
	return t, nil
}

// Notice is notice(title, body) (F:2825): a needs_human ticket about the
// factory itself, on the factory's own project and through the clock door,
// unless a ticket with that title is still open (in any column but done or
// rejected, on any project). It returns nil when it filed nothing.
func (s Service) Notice(ctx context.Context, title, body string) (*ticket.Ticket, error) {
	ts, _ := s.Board.Reader.List()
	for _, t := range ts {
		if t.Title == title && !closed(t.Column) {
			return nil, nil
		}
	}
	return s.file(ctx, ticket.NewIn{
		Project: model.NoticeProject, Title: title, Body: body,
		Door: string(model.DoorClock), Column: string(model.ColNeedsHuman),
	}, nil)
}

// DeployAskTitle is the title of a held deploy's ticket.
const DeployAskTitle = "Deploy the factory?"

// MaxDeployWhy is how many reasons a Deploy? ticket keeps: the latest, so
// a long hold does not grow the ticket and its ask without end.
const MaxDeployWhy = 20

// errAskMoved: the open Deploy? ticket was answered while DeployAsk took
// its lock.
var errAskMoved = errors.New("the deploy ask moved on")

// DeployAsk is a held deploy's ask: one Deploy? ticket on the factory's own
// project. While one is open in needs_human, a newer offer updates it (its
// Rev, a Why line, and a move in place that dates the ask anew) instead of
// filing a second, because deploying main covers the earlier merges.
// Otherwise it files one through the clock door. rev is for display only:
// a deploy always takes main's head when it runs. A board that does not
// read in full files nothing: the open ask may be the ticket it skipped.
func (s Service) DeployAsk(ctx context.Context, rev, why string) (*ticket.Ticket, error) {
	ts, problems := s.Board.Reader.List()
	for _, p := range problems {
		if p.Err != nil {
			return nil, fmt.Errorf("deploy ask: the board does not read (%s: %w), so no ask is filed", p.File, p.Err)
		}
	}
	for _, t := range ts {
		if !openDeployAsk(t) {
			continue
		}
		var out *ticket.Ticket
		err := s.Board.Changing(ctx, string(t.ID), store.Forever, func(t *ticket.Ticket) error {
			if !openDeployAsk(t) {
				return errAskMoved
			}
			d := t.Deploy.V
			d.Rev = rev
			if why != "" {
				d.Why = append(d.Why, why)
			}
			if len(d.Why) > MaxDeployWhy {
				d.Why = d.Why[len(d.Why)-MaxDeployWhy:]
			}
			t.Deploy = ticket.Some(d)
			ticket.Move(t, string(model.ColNeedsHuman), model.ReasonDeployHeld, "now "+rev, "", "", s.Board.Clock.Now())
			out = t
			return nil
		})
		if errors.Is(err, errAskMoved) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("deploy ask %s: %w", t.ID, err)
		}
		return out, nil
	}
	whys := []jsonx.Value{}
	if why != "" {
		whys = append(whys, jsonx.String(why))
	}
	deploy := jsonx.ObjectOf(jsonx.Member{Key: "rev", Value: jsonx.String(rev)}, jsonx.Member{Key: "why", Value: jsonx.ArrayOf(whys...)})
	return s.file(ctx, ticket.NewIn{
		Project: model.NoticeProject, Title: DeployAskTitle,
		Body: "Deploys are held, so a merge left this ask instead of deploying. Deploy it builds core main " +
			"with config main on the builder, installs it on the master and checks Foreman; a failed check " +
			"puts the running build back.",
		Door: string(model.DoorClock), Column: string(model.ColNeedsHuman),
		Fields: jsonx.Object{{Key: "deploy", Value: deploy}},
	}, func(t *ticket.Ticket) {
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonDeployHeld, "deploys are held", "", "", s.Board.Clock.Now())
	})
}

// openDeployAsk is a Deploy? ticket still waiting for the maintainer.
func openDeployAsk(t *ticket.Ticket) bool {
	return t.Project == model.NoticeProject && t.Column == string(model.ColNeedsHuman) && t.Deploy.Present() && !t.Deploy.V.Requested.Present()
}

func closed(column string) bool {
	return column == string(model.ColDone) || column == string(model.ColRejected)
}
