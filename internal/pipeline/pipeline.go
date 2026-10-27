// Package pipeline is the tick: the watches, then up to three passes over
// the board, each of which reconciles tickets stranded in Working, polls
// every running agent (finishing, requeueing or losing it), runs the GitHub
// pass (the intents Foreman records first), refreshes the bases and starts
// what waits: a gate here, or a stage's agent on the host routing picks.
//
// The decision order is Python's tick_pass, step for step. What is
// different is said where it is: a ticket file that cannot be read is passed
// over and noticed instead of crashing the tick; the tick runs
// under a deadline below its unit's and starts no gate it could not finish;
// a cancelled tick saves nothing it had not saved at a commit
// point.
//
// Commit points are Python's: the write-ahead save of an agent before its
// launch, and the save at the end of each ticket's step in each loop. Every
// step works on the clone store.Board.EachLocked hands it, under the
// ticket's lock; when the context ends, the clone is dropped unsaved and
// the tick returns the context's error.
//
// The pipeline declares the adapters it consumes (Deps). The real ones are
// hostrun.Local, hostrun.Jobs (through JobHosts), gate.Runner, prflow.Flow
// and doors.Service.
package pipeline

import (
	"context"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/memory"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const (
	// DefaultDeadline bounds a whole tick: below the factory@tick unit's
	// TimeoutStartSec of 45 minutes, so systemd never kills a tick half-way
	// through a step.
	DefaultDeadline = 42 * time.Minute
	// DefaultReserve is what must be left of the deadline before a gate
	// starts (up to 30 minutes of checks); prflow keeps the same reserve
	// for a merge and its merge check.
	DefaultReserve = 32 * time.Minute
	// maxPasses is how often one tick goes round when wakes keep arriving.
	maxPasses = 3
)

// Deps are the tick's adapters. Board, Config, Paths, Clock, Local, Screen,
// Helpers and Git are required; Memory, Gate, GitHub, Doors and Issues may
// be nil, which leaves runs without memory, gates unrun, GitHub unfollowed,
// watches and notices out and verifiers without the tracker.
type Deps struct {
	Board  *store.Board
	Config ConfigSource
	Paths  paths.Paths
	Clock  clock.Clock
	Log    Logger // nil logs nothing

	Local   LocalAgents  // this host's Scion (*hostrun.Local)
	Screen  LocalScreens // the same, for its screens and run configs
	Helpers Helpers      // the helper hosts ([hosts.*])
	Git     Repos        // the projects' clones, guarded (gate.Runner)
	Memory  *memory.Service
	Gate    Gater // Gate 1 (gate.Runner)
	// Early starts Gate 1's checks of a head the work stage left
	// (gate.Early); nil starts none, and every gate runs its own checks.
	Early   EarlyChecks
	GitHub  GitHub // the PR flow (*prflow.Flow)
	Doors   Doors  // the watches and the notices (doors.Service)
	Issues  Tracker
	Deploys Deployer // the self-deploy's tick part (deploy.Service); nil does nothing
	// Operator settles the manager's drafts and actions under the rights
	// tiers (operator.Service); nil does nothing
	Operator Operator

	// Deadline bounds the tick; 0 is DefaultDeadline.
	Deadline time.Duration
	// Reserve is what a gate needs left of the deadline; 0 is
	// DefaultReserve.
	Reserve time.Duration
}

// Deployer is the self-deploy's part of each pass (deploy.Service.Drain):
// it starts the deploys a Deploy? answer asked for and notices a deploy a
// reboot cut off. The tick never deploys itself.
type Deployer interface {
	Drain(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error
}

// Operator is the operator's part of each pass (operator.Service.Settle):
// it judges the manager's new and tapped items, lands or runs those the
// rights tiers allow, and offers one deploy. The pipeline reaches it only
// through this interface.
type Operator interface {
	Settle(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error
}

// ConfigSource gives the config as it is now (*config.Source): the tick
// reads it before its first pass and again before each later one.
type ConfigSource interface {
	Current() (*config.Config, error)
}

// Logger is the factory log (*logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// LocalAgents runs agents on this host (*hostrun.Local).
type LocalAgents interface {
	// Running is the agents Scion runs here, or an error when it cannot
	// say (which is never an empty set).
	Running(ctx context.Context, cfg *config.Config) (map[string]bool, error)
	// Report is the agent's report.json, if it left one.
	Report(name string) ([]byte, bool, error)
	Start(ctx context.Context, j hostrun.Job) error
	Remove(ctx context.Context, dir string, id ticket.ID, name string, keepBranch bool) error
}

// LocalScreens is what the tick reads of a local agent that has gone quiet
// (*hostrun.Local), and the run config it removes for one that vanished.
type LocalScreens interface {
	Quiet(name string, quietFor time.Duration) bool
	Look(ctx context.Context, dir, name string, lines int) (string, error)
	DropRunConfig(name string) error
}

// Helper runs agents as jobs on one helper host (*hostrun.Jobs).
type Helper interface {
	Start(ctx context.Context, j hostrun.Job) error
	Poll(ctx context.Context, name string) (string, error)
	Collect(ctx context.Context, id ticket.ID, name string) (*hostrun.Result, error)
	Remove(ctx context.Context, id ticket.ID, name string) error
}

// Helpers finds a configured helper host by its [hosts.<name>] key, and
// names the pull hosts that are away (not called in lately, or whose own
// report says no worker can take a job), which get no new jobs. Capacity
// is the pull hosts that report fewer running workers than their slots,
// with that count; GivenBack is why a pull host gave a claimed job back.
type Helpers interface {
	Helper(cfg *config.Config, name string) (Helper, bool)
	Away(cfg *config.Config) []string
	Capacity(cfg *config.Config) map[string]int
	GivenBack(cfg *config.Config, host, job string) (string, bool)
}

// Repos is git on a project's clone, guarded (gate.Runner.Repo).
type Repos interface {
	Repo(p config.Project) *gitread.Repo
}

// Gater runs Gate 1 on a ticket's branch (gate.Runner), or decides it from
// checks a gate job ran on another host, or from the early checks' result
// it kept for the ticket when that result's whole key still holds (Kept).
type Gater interface {
	Run(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (ticket.Gate, error)
	Decide(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, in gate.Remote) (ticket.Gate, error)
	Kept(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (gate.Remote, bool, error)
}

// EarlyChecks starts Gate 1's checks of head for the ticket in a unit of
// their own (gate.Early) and returns at once: the tick never waits for
// them. Running is the ticket and head of the ones that run on this host
// now, if any.
type EarlyChecks interface {
	Start(ctx context.Context, id ticket.ID, head string) error
	Running() (id ticket.ID, head string, ok bool)
}

// GitHub is the PR flow (*prflow.Flow): the pass over the tickets with
// PRs (intents first), and the update of a rework's PR after its gate.
type GitHub interface {
	Pass(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error
	OpenPR(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (ticket.PR, error)
}

// Tracker reads a repository's open issues and pull requests without
// credentials (*github.Client): what a verifier checks findings against.
// nil leaves the verifier the board's tickets only.
type Tracker interface {
	OpenItems(ctx context.Context, repo string) ([]github.Issue, error)
}

// Doors are the watch door and the notices (doors.Service).
type Doors interface {
	Watches(ctx context.Context, cfg *config.Config) error
	Notice(ctx context.Context, title, body string) (*ticket.Ticket, error)
}
