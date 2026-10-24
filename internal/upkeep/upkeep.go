// Package upkeep keeps the dispatcher host's agent images current: the
// version check, which files a notice when a newer Claude Code, Pi,
// Crystal or ameba is out than config pins, and the weekly image
// rebuild, which builds and checks each image through imagebuild while
// the tick is held off, runs the projects' gate checks in the new gate
// image, and files a notice when a rebuild fails on the host that has
// the project checkouts.
//
// No model runs here. The notices go through the doors' Notice (one open
// ticket per title).
package upkeep

import (
	"context"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Logger is the factory log (*logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Releases is where the version check reads the latest releases
// (*github.Client: unauthenticated GitHub and the npm registry).
type Releases interface {
	LatestRelease(ctx context.Context, repo string) (string, error)
	NPMLatest(ctx context.Context, pkg string) (string, error)
}

// Noticer files a notice about the factory itself (doors.Service): nil
// when an open ticket already has the title.
type Noticer interface {
	Notice(ctx context.Context, title, body string) (*ticket.Ticket, error)
}

// Builder rebuilds images on this host (imagebuild.Runner, with no Lock:
// Rebuild holds the tick lock itself).
type Builder interface {
	All(ctx context.Context, specs []imagebuild.Spec) ([]imagebuild.Result, error)
}

// ProjectGate runs a project's gate checks on a commit in a given image
// (gate.Checks) and says what the project's base branch points at.
type ProjectGate interface {
	Checks(ctx context.Context, in gate.CheckIn) (ok bool, tail string, err error)
	BaseHead(ctx context.Context, p config.Project) (string, error)
}

// Service is upkeep on one factory root.
type Service struct {
	Board    *store.Board // the tick lock and the paths (images, logs)
	Releases Releases
	Notices  Noticer
	Builder  Builder
	Gate     ProjectGate
	Tagger   imagebuild.Tagger // the rollback's retag (podman); nil: no rollback here
	Clock    Clock             // nil: the system clock
	Log      Logger            // nil logs nothing
}

// Clock is the time source (images.json's stamps).
type Clock interface{ Now() time.Time }

func (s Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}
