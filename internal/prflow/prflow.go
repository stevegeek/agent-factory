// Package prflow is the factory's pull requests: the PR body, opening and
// updating a PR, following a PR's reviews, the maintainer's go-ahead and the
// merge queue, putting a PR back to draft while it is reworked, the intents
// Foreman records for the tick to carry out, and the synchronous actions the
// CLI passes to ops.
//
// It ports pick_branch, pr_body, open_pr, open_pr_of, request_review,
// check_pr, follow_reviews, approve_to_merge, pick_up_changes, merge_pr,
// land_pr, hold_pr, drain_intents and the GitHub half of tick_pass.
// The decisions, the text
// written to tickets and to GitHub, and the order of requests are Python's.
//
// Security: the factory merges only on a maintainer's explicit go-ahead
// (a merge request for an auto-eligible PR, or an approving review by a
// configured reviewer), only at exactly the commit the gate checked, and only
// after the checks still pass on a base that moved. Only [github] reviewers'
// reviews are read as instructions. It pushes only bot/* branches, with the
// token in the environment, never in argv or a URL.
//
// Two deliberate changes from Python: an intent is cleared only in the save
// that records its outcome, and a failure that retrying can fix (no answer,
// a 5xx, a rate limit, a deferral under the tick's deadline) keeps it, with
// at most one error event per intent per hour.
package prflow

import (
	"context"
	"errors"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// DefaultReserve is how much of the tick's deadline must remain before a
// merge (and its merge check, up to 30 minutes) starts: the tick
// runs for at most 42 minutes, under the unit's 45.
const DefaultReserve = 32 * time.Minute

// Git is what the flow needs of a project's clone: its branch heads, the
// push and the deletion of a bot/* branch. Repos is the real one; every
// call is guarded.
type Git interface {
	BranchChecker
	Head(ctx context.Context, p config.Project, branch string) (string, error)
	Push(ctx context.Context, p config.Project, branch, token string) error
	DeleteBranch(ctx context.Context, p config.Project, branch string) error
}

// BranchChecker says whether a local branch exists (pick_branch).
type BranchChecker interface {
	BranchExists(ctx context.Context, p config.Project, branch string) (bool, error)
}

// MergeChecker is the merge queue's check (gate.Runner): when the base moved
// since the gate, do the checks still pass with it merged in?
type MergeChecker interface {
	StillHoldsOnMain(ctx context.Context, in gate.HoldIn) (ok bool, out string, err error)
}

// Board is how the tick's GitHub pass reads and saves tickets
// (*store.Board).
type Board interface {
	EachLocked(ctx context.Context, ts []*ticket.Ticket, pred func(*ticket.Ticket) bool, fn func(*ticket.Ticket) error) error
	Save(t *ticket.Ticket) error
}

// HoldDeployLabel is the PR label that holds the deploy a merge of one of
// the factory's own projects asks for: the maintainer is asked first.
const HoldDeployLabel = "hold-deploy"

// MergedPR is a merge the flow made or found on a self-deploy project:
// GitHub's merge commit, and whether the PR carried HoldDeployLabel.
type MergedPR struct {
	Commit string
	Hold   bool
}

// Deployer asks for the deploy of a merged change to the factory itself
// (the cli's selfDeploys, over deploy.Service.Offer). The merge stands
// whatever it answers.
type Deployer interface {
	Merged(ctx context.Context, p config.Project, t *ticket.Ticket, m MergedPR) error
}

// Reach keeps whether the GitHub pass reaches GitHub at all
// (sysinfo.GitHubRecord). Note gets one pass's outcome and returns a line
// to log when that flips.
type Reach interface {
	Note(now time.Time, ok bool, lastErr string) (string, error)
}

// Logger is the factory log (logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Flow carries out the PR flow for one process.
type Flow struct {
	// GitHub is the App's client. Its Auth is also the token of the push.
	GitHub *github.Client
	Git    Git
	Checks MergeChecker
	// Board is used by DrainIntents and Pass, which save the tickets they
	// change; the single-ticket methods only change the ticket given.
	Board Board
	// LogsDir is ROOT/logs, where a failed merge check leaves
	// <id>/merge-check-output.txt.
	LogsDir string
	Clock   clock.Clock
	Log     Logger // nil logs nothing
	// Reserve is the time a merge needs left on the context's deadline;
	// 0 is DefaultReserve. A context without a deadline always has time.
	Reserve time.Duration
	// Deploys is told of each merge on a project with self_deploy; nil
	// asks for no deploys.
	Deploys Deployer
	// Reach is told after each Pass whether GitHub answered; nil keeps
	// nothing.
	Reach Reach
}

func (f *Flow) now() time.Time {
	if f.Clock == nil {
		return clock.System{}.Now()
	}
	return f.Clock.Now()
}

func (f *Flow) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log.Printf(format, args...)
	}
}

// timeForMerge reports whether a merge may start under ctx's deadline.
func (f *Flow) timeForMerge(ctx context.Context) bool {
	dl, ok := ctx.Deadline()
	if !ok {
		return true
	}
	reserve := f.Reserve
	if reserve <= 0 {
		reserve = DefaultReserve
	}
	return time.Until(dl) >= reserve
}

// Refusal is a request the flow will not carry out, in Python's words: what
// Python raised as a RuntimeError (or a Refused). Retrying does not help.
// Code is set for the refusals ticketapi names ("factory_ticket").
type Refusal struct {
	Code    string
	Message string
	err     error
}

func (r *Refusal) Error() string { return r.Message }

// Unwrap gives the cause, when there is one (ErrMainMoved, ErrNeedsApproval,
// ErrUnread).
func (r *Refusal) Unwrap() error { return r.err }

func refusef(msg string) *Refusal { return &Refusal{Message: msg} }

// ErrUnread is a hygiene project's commit OpenPR would not push because
// public review has not passed it: the ticket went back to the gate, which
// has it read first. It is wrapped in a *Refusal.
var ErrUnread = errors.New("public review has not passed the commit")

// ErrMainMoved is a merge the merge check stopped: the base moved and the
// checks fail with it merged in, so the ticket went back for a rework. It is
// wrapped in a *Refusal.
var ErrMainMoved = errors.New("the base moved and the checks fail with it merged in")

// ErrNeedsApproval is a merge GitHub refused because its rules want an
// approving review of the PR's head first (the ruleset's "New changes
// require approval from someone other than the last pusher": the App
// pushed it). Waiting will not fix it; the maintainer's approval will, and
// that approval merges it by itself. It is wrapped in a *Refusal.
var ErrNeedsApproval = errors.New("GitHub merges it only once you approve it")

// errDeferred is a merge left for the next tick by the deadline.
var errDeferred = errors.New("deferred to the next tick: too little time left for a merge")

// NotHeldError is a PR that could not be put back to draft: it is still
// open for merging. The failure is already on the ticket as an error event.
type NotHeldError struct {
	Number int
	Err    error
}

func (e *NotHeldError) Error() string {
	return "PR #" + itoa(e.Number) + " is still open for merging; it could not be turned back to draft"
}

func (e *NotHeldError) Unwrap() error { return e.Err }

// transient reports whether retrying err later can help: GitHub did not
// answer, answered 5xx or rate-limited, or the context ended.
func transient(err error) bool {
	return github.IsTransient(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errDeferred)
}

// runtimeLike reports whether Python would have raised err as a
// RuntimeError, which the tick records as a "pr" event ("not merged (asked
// …)", "not opened (asked …)"), rather than an error event. GitHub's HTTP
// refusals and failures to answer were urllib errors, not RuntimeErrors.
func runtimeLike(err error) bool {
	var he *github.HTTPError
	var ne *github.NetError
	if errors.As(err, &he) || errors.As(err, &ne) {
		var r *Refusal
		return errors.As(err, &r)
	}
	return !transient(err)
}

// pyErr is Python's str(e) of an error the flow saw: GitHub's HTTP errors as
// urllib wrote them, everything else its message.
func pyErr(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Message
	}
	var he *github.HTTPError
	if errors.As(err, &he) {
		return he.Error()
	}
	return err.Error()
}
