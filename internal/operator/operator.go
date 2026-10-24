// Package operator is the tick's side of the manager as the factory's
// operator. The manager only
// proposes: config drafts and typed actions, recorded on its ticket as
// items. Settle decides what happens to each from the change itself,
// never from the manager's claim:
//
//   - a draft is applied to config main in a temp clone and judged by
//     internal/tiers; an action's tier is model's fixed table;
//   - never: refused, nothing is pushed;
//   - auto, on a ticket whose every word is the maintainer's: carried out;
//   - anything else waits for the maintainer's "Apply it" (requested),
//     after which it is judged again on the current main and carried out
//     unless it has become never.
//
// A draft lands as a config PR on a bot/operator-* branch that the tick
// opens (or reuses) and merges at the head it pushed; the merges of a pass
// give one deploy offer. Foreman never links this package: only the tick
// touches git, GitHub, systemd and the pause file.
package operator

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/logx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Limits of a land.
const (
	// mergePolls and mergePoll bound the wait for GitHub to work out a new
	// PR's mergeability: at most 30 s.
	mergePolls = 15
	mergePoll  = 2 * time.Second
	// maxPatch is the longest patch the tick applies (the finish clips to
	// it already).
	maxPatch = 50_000
	// noteRunes is how much of an error an item's note keeps.
	noteRunes = 300
	// gitTimeout bounds one git call in the temp clone.
	gitTimeout = 2 * time.Minute
	// detailRunes bounds the settle move's detail.
	detailRunes = 1500
)

// Deployer offers a deploy of main: the deploy Offer with rev "main", the doors
// asker and no hold of its own (it still respects the hold file and an
// open Deploy? ask).
type Deployer interface {
	Offer(ctx context.Context, cfg *config.Config, why, by string) error
}

// Switcher switches a project on, off or back to following config.toml
// (ops.Service).
type Switcher interface {
	SwitchProject(ctx context.Context, key, state, by string) (string, error)
}

// Service settles operator items. Repos gives a project's clone with the
// process's guard and safe git environment (as the gate's); Token is the
// App's installation token for a push; GitHub carries its own. Env is the
// tick's environment, for the transient units it starts. Sleep waits
// between mergeability polls (nil: a timer).
type Service struct {
	Paths    paths.Paths
	Board    *store.Board
	GitHub   *github.Client
	Token    func(context.Context) (string, error)
	Repos    func(config.Project) *gitread.Repo
	Exec     execx.Execer
	Deploys  Deployer
	Switches Switcher
	Clock    clock.Clock
	Log      *logx.Log
	Env      map[string]string
	Sleep    func(context.Context, time.Duration) error
}

func (s Service) now() time.Time {
	if s.Clock == nil {
		return clock.System{}.Now()
	}
	return s.Clock.Now()
}

func (s Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func (s Service) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pending reports an item the tick has to act on: new (not yet judged) or
// requested (the maintainer tapped Apply it).
func pending(state string) bool { return state == model.ItemNew || state == model.ItemRequested }

// wants reports a manager ticket on the desk with an item to act on.
func wants(t *ticket.Ticket) bool {
	if t.KindName() != string(model.KindManage) || t.Column != string(model.ColNeedsHuman) {
		return false
	}
	for _, d := range t.Drafts {
		if pending(ticket.DraftState(d)) {
			return true
		}
	}
	for _, a := range t.Actions {
		if pending(ticket.ActionState(a)) {
			return true
		}
	}
	return false
}

// offered is an item that joins the pass's one deploy offer: a merged
// draft or a deploy action, found again by its ticket, index and run.
type offered struct {
	id     ticket.ID
	draft  bool
	index  int
	run    int
	commit string // a draft's merge commit
}

// pass is what one Settle pass collects for its single deploy offer.
type pass struct {
	items []offered
	whys  []string // "<ticket>: <titles>"
}

// Settle acts on every manager ticket on the desk with a new or requested
// item, each under its lock: drafts are judged and landed or left to wait,
// actions run or wait, and each changed ticket is moved needs_human ->
// needs_human with operator_settled so that its ask follows. Then the
// pass's merged drafts and deploy actions give one deploy offer. Item
// failures are recorded on the items; only store and context errors are
// returned.
func (s Service) Settle(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error {
	if s.Board == nil {
		return errors.New("operator: no board")
	}
	p := &pass{}
	err := s.Board.EachLocked(ctx, ts, wants, func(t *ticket.Ticket) error { return s.settle(ctx, cfg, t, p) })
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// what merged is offered even when a later ticket failed to save
	if oerr := s.offer(ctx, cfg, ts, p); err == nil {
		err = oerr
	}
	return err
}

// settle acts on one ticket's pending items and saves it when any
// changed. A cancelled context saves nothing: the next pass finds the
// items as they were (a draft that merged meanwhile is then "already on
// config main").
func (s Service) settle(ctx context.Context, cfg *config.Config, t *ticket.Ticket, p *pass) error {
	trusted := ticket.OperatorTrusted(t, cfg.Maintainer())
	var lines, titles []string
	var offers []offered
	changed := false
	for i := range t.Drafts {
		if !pending(ticket.DraftState(t.Drafts[i])) {
			continue
		}
		changed = true
		line, merged := s.draft(ctx, cfg, t, i, trusted)
		if err := ctx.Err(); err != nil {
			return err
		}
		if line != "" {
			lines = append(lines, line)
		}
		if merged {
			d := t.Drafts[i]
			offers = append(offers, offered{id: t.ID, draft: true, index: i, run: d.Run, commit: d.Commit})
			titles = append(titles, d.Title)
		}
	}
	acted, more, deploys := s.actions(ctx, cfg, t, trusted)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !acted && !changed {
		return nil
	}
	lines, offers = append(lines, more...), append(offers, deploys...)
	if len(deploys) > 0 {
		titles = append(titles, "deploy main")
	}
	detail := ticket.SettledDetail(ticket.Items(t))
	if len(lines) > 0 {
		detail += "; " + strings.Join(lines, "; ")
	}
	ticket.SettleMove(t, textx.Clip(detail, detailRunes), "", s.now())
	if err := s.Board.Save(t); err != nil {
		return err
	}
	p.items = append(p.items, offers...)
	if len(titles) > 0 {
		p.whys = append(p.whys, string(t.ID)+": "+strings.Join(titles, ", "))
	}
	return nil
}

// offer makes the pass's one deploy offer, when anything merged or a
// deploy action ran. A failed offer is a log line and a note on each item
// it was for (a deploy action then failed), saved under the ticket's lock.
func (s Service) offer(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket, p *pass) error {
	if len(p.items) == 0 {
		return nil
	}
	var err error
	if s.Deploys == nil {
		err = errors.New("no deployer")
	} else {
		err = s.Deploys.Offer(ctx, cfg, strings.Join(p.whys, "; "), "manager")
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := textx.Prefix(err.Error(), noteRunes)
	s.logf("operator: merged; deploy not offered: %s (%s)", msg, strings.Join(p.whys, "; "))
	ids := map[ticket.ID]bool{}
	for _, o := range p.items {
		ids[o.id] = true
	}
	return s.Board.EachLocked(ctx, ts, func(t *ticket.Ticket) bool { return ids[t.ID] }, func(t *ticket.Ticket) error {
		changed := false
		for _, o := range p.items {
			if o.id != t.ID {
				continue
			}
			if o.draft && o.index < len(t.Drafts) {
				d := &t.Drafts[o.index]
				if d.State == model.ItemApplied && d.Run == o.run && d.Commit == o.commit {
					d.Note = joinNote(d.Note, "merged; deploy not offered: "+msg)
					changed = true
				}
			}
			if !o.draft && o.index < len(t.Actions) {
				a := &t.Actions[o.index]
				if a.State == model.ItemApplied && a.Run == o.run && a.Kind == string(model.ActionDeploy) {
					a.State, a.Note = model.ItemFailed, "deploy not offered: "+msg
					changed = true
				}
			}
		}
		if !changed {
			return nil
		}
		ticket.SettleMove(t, ticket.SettledDetail(ticket.Items(t))+"; deploy not offered: "+msg, "", s.now())
		return s.Board.Save(t)
	})
}

// joinNote appends more to a note.
func joinNote(note, more string) string {
	if note == "" {
		return more
	}
	return note + "; " + more
}
