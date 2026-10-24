package upkeep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/store"
)

// Rebuild is rebuild(cfg) (F:2928), `factory rebuild`, weekly: each agent
// image this host has is rebuilt with fresh OS packages, and kept only if
// the new one passes its smoke test and, for the gate image, the projects'
// tests (imagebuild does the build, the checks and the swap, and writes
// logs/rebuild-<kind>.log).
//
// The version (§15.1): a tool in [images] auto_update is built at its
// latest stable release when that is newer than the version the image
// has (the newer of the [images] pin, now a floor, and images.json) and
// than a held one; images.json records it once it is promoted, and a
// notice says so. Anything else is rebuilt at the version it has.
//
// The tick lock is held for the whole rebuild, waiting for a running tick
// (only the context ends the wait): no agent or gate starts while images
// change, and a timer tick meanwhile leaves .tick.again. A failed rebuild
// files a notice, on the host with the project checkouts only (the
// dispatcher; a helper's board is not read). The error is for a rebuild
// that could not run at all (podman missing, the lock, a cancel) or a
// notice that could not be filed.
//
// Then, on the host with the checkouts, when [upkeep] latest_every_days
// have passed since the last one, the latest-toolchain trial runs
// (latest.go), still under the lock.
func (s Service) Rebuild(ctx context.Context, cfg *config.Config) error {
	b := s.Board
	if err := os.MkdirAll(b.Paths.Root, 0o777); err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	l, err := b.TickLock(ctx, store.Forever) // no agents start while we build; waits for a running tick
	if err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	defer l.Unlock()
	state, serr := imagebuild.LoadState(b.Paths.ImagesState())
	if serr != nil {
		// a broken images.json: the pins alone, and the file is not rewritten
		s.logf("rebuild: %v; building at the [images] pins", serr)
	}
	specs := s.specs(ctx, cfg, state)
	results, err := s.Builder.All(ctx, specs)
	// only the dispatcher (the host with the project checkouts) has a board anyone reads
	board := hasCheckout(cfg)
	promoted := false
	for _, r := range results {
		name := kindName(r.Kind)
		if r.OK && !r.Update && imagebuild.Rebuilt(state, r.Kind, r.From) {
			promoted = true // :previous now holds the live image's own version
		}
		if r.OK && r.Update {
			imagebuild.Promoted(state, r.Kind, imagebuild.Decision{Target: r.Version, From: r.From, Update: true}, s.now())
			promoted = true
			s.logf("rebuild %s: updated %s %s -> %s", r.Kind, name, orDefault(r.From), r.Version)
			if board {
				title := fmt.Sprintf("%s updated %s -> %s", name, orDefault(r.From), r.Version)
				body := fmt.Sprintf("The weekly rebuild updated %s from %s to %s: the new image passed its smoke test "+
					"and the projects' checks. The previous image is kept as :previous; `factory rebuild --rollback %s` "+
					"goes back to it (and holds %s until a newer release is out). Move this ticket to done.",
					name, orDefault(r.From), r.Version, r.Kind, r.Version)
				if _, nerr := s.Notices.Notice(ctx, title, body); nerr != nil {
					return fmt.Errorf("rebuild %s: %w", r.Kind, nerr)
				}
			}
		}
		if r.OK || !board {
			continue
		}
		title := fmt.Sprintf("Weekly %s image rebuild failed", name)
		body := fmt.Sprintf("%s. The previous image is still in use. Log: %s/rebuild-%s.log on the host.", r.Summary, b.Paths.Logs, r.Kind)
		if r.Update {
			body = fmt.Sprintf("%s, trying %s %s (the image has %s). The previous image is still in use. Log: %s/rebuild-%s.log on the host.",
				r.Summary, name, r.Version, orDefault(r.From), b.Paths.Logs, r.Kind)
		}
		if _, nerr := s.Notices.Notice(ctx, title, body); nerr != nil {
			return fmt.Errorf("rebuild %s: %w", r.Kind, nerr)
		}
	}
	if promoted && serr == nil {
		if werr := imagebuild.SaveState(b.Paths.ImagesState(), state); werr != nil {
			return errors.Join(err, fmt.Errorf("rebuild: %w", werr))
		}
	}
	// the monthly latest-toolchain trial, once the weekly rebuild has run
	if err == nil && board && cfg.Upkeep.LatestEveryDays > 0 {
		due, derr := s.latestDue(cfg)
		if derr != nil {
			return derr
		}
		if due {
			return s.latest(ctx, cfg, state)
		}
	}
	return err
}

// Rollback is `factory rebuild --rollback KIND`: the image goes back to
// :previous, and images.json holds the version it leaves (§15.1).
func (s Service) Rollback(ctx context.Context, cfg *config.Config, kind string) (imagebuild.ToolState, error) {
	return s.changeState(ctx, kind, func(st imagebuild.State) (imagebuild.ToolState, error) {
		if s.Tagger == nil {
			return imagebuild.ToolState{}, fmt.Errorf("rollback %s: no podman here", kind)
		}
		ts, err := imagebuild.Rollback(ctx, s.Tagger, st, kind, imagebuild.TagFor(kind, cfg.Image, cfg.Remote.Image), s.now())
		switch {
		case err != nil:
		case ts.Held == "":
			s.logf("rebuild %s: rolled back to :previous, which has the same version (%s); nothing is held", kind, orDefault(ts.Version))
		default:
			s.logf("rebuild %s: rolled back to :previous (%s); %s is held", kind, orDefault(ts.Version), orDefault(ts.Held))
		}
		return ts, err
	})
}

// Unhold is `factory rebuild --unhold KIND`: the held version may be tried
// again by the next rebuild.
func (s Service) Unhold(ctx context.Context, kind string) (imagebuild.ToolState, error) {
	return s.changeState(ctx, kind, func(st imagebuild.State) (imagebuild.ToolState, error) {
		if !imagebuild.Unhold(st, kind) {
			return st[kind], fmt.Errorf("unhold %s: no version is held", kind)
		}
		s.logf("rebuild %s: no version held any more", kind)
		return st[kind], nil
	})
}

// changeState runs fn on images.json under the tick lock and saves it.
func (s Service) changeState(ctx context.Context, kind string, fn func(imagebuild.State) (imagebuild.ToolState, error)) (imagebuild.ToolState, error) {
	known := false
	for _, k := range imagebuild.Kinds() {
		known = known || k.Kind == kind
	}
	if !known {
		return imagebuild.ToolState{}, fmt.Errorf("no image kind %q (claude or pi)", kind)
	}
	l, err := s.Board.TickLock(ctx, store.Forever)
	if err != nil {
		return imagebuild.ToolState{}, fmt.Errorf("rebuild: %w", err)
	}
	defer l.Unlock()
	st, err := imagebuild.LoadState(s.Board.Paths.ImagesState())
	if err != nil {
		return imagebuild.ToolState{}, err
	}
	ts, err := fn(st)
	if err != nil {
		return ts, err
	}
	return ts, imagebuild.SaveState(s.Board.Paths.ImagesState(), st)
}

func (s Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

func kindName(kind string) string {
	for _, k := range imagebuild.Kinds() {
		if k.Kind == kind {
			return k.Name
		}
	}
	return kind
}

func orDefault(v string) string {
	if v == "" {
		return "the default"
	}
	return v
}

// specs are the images to rebuild, in IMAGES order: the claude image is
// the top-level image, the pi image [remote] image; the image that is the
// gate's also runs the checks of every factory project that has a
// checkout here, except a paused one (enabled = false), which is logged:
// Python checked those too, and a paused Crystal project's checks take
// 1.8 GB at night for nothing.
func (s Service) specs(ctx context.Context, cfg *config.Config, state imagebuild.State) []imagebuild.Spec {
	var out []imagebuild.Spec
	for _, k := range imagebuild.Kinds() {
		tag := imagebuild.TagFor(k.Kind, cfg.Image, cfg.Remote.Image)
		d := s.decide(ctx, cfg, k, tag, state)
		pins := map[string]string{}
		for key, v := range cfg.Images {
			pins[key] = v
		}
		pins[k.Kind] = d.Target
		sp := imagebuild.SpecFor(k, s.Board.Paths.Images, tag, pins)
		sp.Update, sp.From = d.Update, d.From
		if tag != "" && tag == cfg.Image {
			for _, p := range cfg.FactoryProjects() {
				switch {
				case !isDir(p.Path):
				case !p.Enabled:
					s.logf("rebuild %s: not running the checks of %s: it is paused (enabled = false)", k.Kind, p.Key)
				default:
					sp.Projects = append(sp.Projects, s.projectCheck(cfg, p, "rebuild-"+p.Key))
				}
			}
		}
		out = append(out, sp)
	}
	return out
}

// decide is the version one image is built at: the latest release only
// for an auto-updated tool whose image this host has.
func (s Service) decide(ctx context.Context, cfg *config.Config, k imagebuild.Kind, tag string, state imagebuild.State) imagebuild.Decision {
	latest := ""
	if tag != "" && s.Releases != nil && imagebuild.AutoUpdated(cfg.AutoUpdate, k.Kind) {
		v, err := s.Releases.NPMLatest(ctx, k.NPM)
		if err != nil {
			s.logf("rebuild %s: the latest release: %v; rebuilding the version it has", k.Kind, err)
		} else {
			latest = v
		}
	}
	d := imagebuild.Decide(cfg.Images[k.Kind], state[k.Kind], imagebuild.AutoUpdated(cfg.AutoUpdate, k.Kind), latest)
	if d.Why != "" {
		s.logf("rebuild %s: not taking %s: %s", k.Kind, latest, d.Why)
	}
	return d
}

// projectCheck runs a project's gate checks on its base's head in the
// candidate image, checked out as gate/<id> (rebuild-<project>, or
// trial-<project> for the latest-toolchain trial).
func (s Service) projectCheck(cfg *config.Config, p config.Project, id string) imagebuild.ProjectCheck {
	return imagebuild.ProjectCheck{
		Key: p.Key, Base: p.Base,
		Run: func(ctx context.Context, image string) (bool, string, error) {
			if p.Base == "" {
				return false, "", fmt.Errorf("[projects.%s] has no base", p.Key)
			}
			head, err := s.Gate.BaseHead(ctx, p)
			if err != nil {
				return false, "", err
			}
			return s.Gate.Checks(ctx, gate.CheckIn{
				Config: cfg, Project: p, ID: id, Ref: head, Image: image,
			})
		},
	}
}

// hasCheckout reports a factory project whose checkout is on this host.
func hasCheckout(cfg *config.Config) bool {
	for _, p := range cfg.FactoryProjects() {
		if isDir(p.Path) {
			return true
		}
	}
	return false
}

// isDir is os.path.isdir: false for "" and for anything not a directory
// (following symlinks, as Python does).
func isDir(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(filepath.Clean(path))
	return err == nil && fi.IsDir()
}
