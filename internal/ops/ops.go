// Package ops is the maintainer's verbs and the answer service: add, move,
// note, reply, ask, approve, redirect, host, merge, pr, result, look and
// migrate, and what an answer on Foreman's desk means. They write the board
// and hold no credentials, so the CLI and Foreman call the same code
// in-process, under the flocks the tick takes.
//
// Every verb reads the config once (Config.Current) and works on that
// snapshot. A verb that changes a ticket does it under the ticket's lock on a
// fresh read and saves it once, at the end; a refusal saves nothing. Who a
// verb acts as is the maintainer (the first of [board] people), except an
// answer or a new ticket from another Foreman user, which records that
// user's name.
//
// The work that needs GitHub goes through PRActions: the CLI passes a
// synchronous implementation (package prflow), Foreman passes Intents, which
// only records what the tick should do. Adapters the verbs read through
// (git, Scion, helper hosts, the panel's rules) are small interfaces declared
// here; a nil one means that part is unavailable.
package ops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// ConfigSource is the config as it is now (*config.Source).
type ConfigSource interface {
	Current() (*config.Config, error)
}

// Waker starts a tick now (wake.Waker).
type Waker interface {
	Wake(ctx context.Context) error
}

// PRActions is the GitHub work a verb needs. Both methods are called
// under the ticket's lock with the ticket in hand, and the ticket is saved
// after them; they record on it what they did.
type PRActions interface {
	// Hold turns t's open PR back to draft while it is reworked (hold_pr),
	// or records that the tick should. It records its own events; an error
	// means the PR is still open for merging, which the person is warned of.
	Hold(ctx context.Context, cfg *config.Config, t *ticket.Ticket) error
	// Open pushes t's branch and opens or updates its PR (open_pr), or
	// records that the tick should. It returns the PR's URL, "" when it only
	// recorded the request. An error leaves the ticket unsaved.
	Open(ctx context.Context, cfg *config.Config, t *ticket.Ticket) (string, error)
	// Prune deletes the bot/* branch of t, closed with no PR, here and on
	// GitHub, or records that the tick should. It records its own events;
	// an error leaves the branch, which the person is warned of.
	Prune(ctx context.Context, cfg *config.Config, t *ticket.Ticket) error
}

// Branches says whether a project's clone has a branch
// (gitread.Repo.BranchExists; GitBranches adapts it).
type Branches interface {
	BranchExists(ctx context.Context, p config.Project, branch string) (bool, error)
}

// Screens reads an agent's terminal here (scionread.Reader.Look). dir is
// the workspace Scion runs the agent in.
type Screens interface {
	Look(ctx context.Context, dir, name string, lines int) (string, error)
}

// JobTails reads the tail of a job's output on a helper host (package
// jobtail): read-only.
type JobTails interface {
	Tail(ctx context.Context, host config.HostCfg, job string) (string, error)
}

// BoardNoter adds a person's comment to a board ticket's conversation (the
// ticket API's note verb, acting as the maintainer). It takes the ticket's
// lock itself.
type BoardNoter interface {
	BoardNote(ctx context.Context, id ticket.ID, text string) error
}

// ProspectPanel is the blind panel's rules as migrate needs them (package
// panel): whether a proposal survives the votes, its votes as filed on its
// ticket, and the proposal ticket file_proposal files.
type ProspectPanel interface {
	Survives(i int, votes []ticket.Vote) bool
	Votes(i int, votes []ticket.Vote) []ticket.PanelVote
	// FileProposal is proposal i (1-based) of prospect t as a new ticket
	// with id, its panel event recorded and, when at is not "", every time
	// on it back-dated to at. The caller saves it.
	FileProposal(id ticket.ID, t *ticket.Ticket, i int, at string, now time.Time) *ticket.Ticket
}

// Logger is the factory log (*logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Service is the maintainer's verbs on one board.
type Service struct {
	Board  *store.Board
	Config ConfigSource
	Clock  clock.Clock // nil: the system clock

	Wake  Waker      // nil: nothing is woken
	PR    PRActions  // nil: verbs that need GitHub refuse
	Git   Branches   // nil: an Ask never names the parent's branch
	Scion Screens    // nil: look reads only the agent's log here
	Relay Relayed    // non-nil: look reads local screens from the relay, never Scion
	Jobs  JobTails   // nil: look shows nothing for a helper host's job
	Notes BoardNoter // nil: a note on a board ticket is refused
	Panel ProspectPanel
	Log   Logger
	// AskOf is the ask a ticket puts to the maintainer, as Foreman shows it
	// (present.AskFor). nil: the stored ask, in an ask column.
	AskOf func(t *ticket.Ticket) *ticket.Ask
	// Wait is how long a verb waits for a ticket the tick holds: zero is
	// the CLI's 60 s, which Foreman's answers use too; negative tries once.
	Wait time.Duration
}

// Refused is a request a verb will not carry out: Python's sys.exit(msg)
// and Refused(code, msg). Its message is what the person reads; the CLI
// exits 1 with it.
type Refused struct {
	Code    string // bad_request, not_found, conflict, busy, factory_ticket
	Message string
}

func (r *Refused) Error() string { return r.Message }

// Refusal codes.
const (
	BadRequest    = "bad_request"
	Conflict      = "conflict"
	FactoryTicket = "factory_ticket"
)

func refuse(code, format string, args ...any) error {
	return &Refused{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrStale is an answer to an ask that has moved on since the person saw it.
var ErrStale = errors.New("the ticket changed since the page was opened")

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC().Truncate(time.Second)
	}
	return s.Clock.Now()
}

func (s *Service) stamp() string { return clock.Stamp(s.now()) }

func (s *Service) config() (*config.Config, error) {
	if s.Config == nil {
		return nil, errors.New("ops: no config")
	}
	cfg, err := s.Config.Current()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// waitFor is d as the store's lock wait (fsx.Wait), def when d is zero and
// "try once" when it is negative.
func waitFor[W ~int64](d time.Duration, def W) W {
	switch {
	case d == 0:
		return def
	case d < 0:
		return 0
	}
	return W(d)
}

func (s *Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

// wake starts a tick after a save that asked for one. A failed wake does
// not undo the change: the next timer tick does the work.
func (s *Service) wake(ctx context.Context) {
	if s.Wake == nil {
		return
	}
	if err := s.Wake.Wake(ctx); err != nil {
		s.logf("wake: %v", err)
	}
}

// factoryOnly is factory_ticket_only: a board ticket is changed through the
// ticket API.
func factoryOnly(t *ticket.Ticket) error {
	if t.Board() {
		return refuse(FactoryTicket, "%s is on a board project; use `factory ticket …`", t.ID)
	}
	return nil
}

// change is changing(): the ticket under its lock, saved once if fn
// returns nil. After the save, a tick is woken if fn asked for one or the
// save recorded a new intent.
func (s *Service) change(ctx context.Context, name string, fn func(c *call, t *ticket.Ticket) error) error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	c := &call{s: s, cfg: cfg, now: s.now()}
	err = s.Board.Changing(ctx, name, waitFor(s.Wait, store.CLIWait), func(t *ticket.Ticket) error {
		before := intents(t)
		if err := fn(c, t); err != nil {
			return err
		}
		if intents(t) != before {
			c.wake = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if c.wake {
		s.wake(ctx)
	}
	return nil
}

// call is one verb's state: its config snapshot, its clock reading, and
// what it printed or asked for.
type call struct {
	s    *Service
	cfg  *config.Config
	now  time.Time
	as   string // the person acting, "" for the maintainer
	wake bool
	out  []string
	// closed is a ticket the call closed by hand, whose open questions
	// close once it is saved
	closed ticket.ID
}

// by is who the call records as acting: the maintainer (the first of
// [board] people), or the Foreman user it acts as.
func (c *call) by() string {
	if c.as != "" {
		return c.as
	}
	return c.cfg.Maintainer()
}

// actAs makes the call act as a Foreman user ("" is the maintainer).
// A name the history already gives a meaning to is refused: the factory's,
// or the maintainer's own, which would make the user's answers theirs.
func (c *call) actAs(name string) error {
	if name == "" {
		return nil
	}
	if name == "factory" || name == c.cfg.Maintainer() {
		return refuse(BadRequest, "%s is the factory's or the maintainer's name on the board; give the Foreman user another", name)
	}
	c.as = name
	return nil
}

func (c *call) print(line string) { c.out = append(c.out, line) }

// intents is the PR intents a ticket holds, to see whether a change added
// one.
func intents(t *ticket.Ticket) [2]string {
	pr, ok := t.PR.Get()
	if !ok {
		return [2]string{}
	}
	return [2]string{pr.HoldRequested.Or(""), pr.OpenRequested.Or("")}
}

// projectOf is cfg["projects"].get(key).
func projectOf(cfg *config.Config, key string) (config.Project, bool) {
	p, ok := cfg.Project(key)
	if !ok {
		return config.Project{}, false
	}
	return *p, true
}

func isFactoryProject(cfg *config.Config, key string) bool {
	for _, p := range cfg.FactoryProjects() {
		if p.Key == key {
			return true
		}
	}
	return false
}

// openColumn reports a column a ticket is still open in.
func closedColumn(col string) bool {
	return col == string(model.ColDone) || col == string(model.ColRejected)
}
