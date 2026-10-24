package helper

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/logx"
	"github.com/stevegeek/agent-factory/internal/podman"
)

// rebuild is the helper's weekly image rebuild (`factory-remote rebuild`,
// or `factory rebuild` through the bin/factory symlink): Python's
// rebuild(cfg) on a host with no project checkouts. Each image this host
// has (the pi image from [remote] image, and the claude image when the
// top-level image is set and present) is rebuilt under the tick lock,
// logged to logs/rebuild-<kind>.log and factory.log. On a host whose
// agent is claude, [remote] image is the claude image and the only one
// rebuilt. The version follows the dispatcher's rule (§15.1): an auto-updated tool's latest stable
// release when it is newer than the version the image has (the newer of
// the [images] floor and this host's images.json) and than a held one;
// images.json records a promoted update. The helper has no board, so a
// failure files no notice and runs no project checks; like Python it
// still exits 0 when an image failed.
func (s *Server) rebuild(ctx context.Context) error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	logs := filepath.Join(s.Root, "logs")
	images := filepath.Join(s.Root, "images")
	statePath := filepath.Join(s.Root, "images.json")
	flog := &logx.Log{Dir: logs, Clock: s.Clock, Stderr: s.Stderr}
	runner := imagebuild.Runner{
		Podman: podman.Client{Exec: s.Exec, Dir: s.Home, Env: s.env()},
		Clock:  s.Clock,
		Log:    flog,
		LogDir: logs,
		Lock:   filepath.Join(s.Root, ".tick.lock"),
	}
	state, serr := imagebuild.LoadState(statePath)
	if serr != nil {
		flog.Printf("rebuild: %v; building at the [images] pins", serr)
	}
	var specs []imagebuild.Spec
	for _, k := range imagebuild.Kinds() {
		tag := cfg.Image
		if k.Kind != "claude" {
			tag = imagebuild.TagFor(k.Kind, cfg.Image, cfg.Remote.Image)
		}
		if cfg.Remote.Agent == AgentClaude {
			// [remote] image is the claude image here: no pi image is kept
			tag = ""
			if k.Kind == "claude" {
				tag = cfg.Remote.Image
			}
		}
		auto := imagebuild.AutoUpdated(cfg.AutoUpdate, k.Kind)
		latest := ""
		if auto && tag != "" {
			v, err := s.latest(ctx, k.NPM)
			if err != nil {
				flog.Printf("rebuild %s: the latest release: %v; rebuilding the version it has", k.Kind, err)
			} else {
				latest = v
			}
		}
		d := imagebuild.Decide(cfg.Images[k.Kind], state[k.Kind], auto, latest)
		if d.Why != "" {
			flog.Printf("rebuild %s: not taking %s: %s", k.Kind, latest, d.Why)
		}
		pins := map[string]string{}
		for key, v := range cfg.Images {
			pins[key] = v
		}
		pins[k.Kind] = d.Target
		sp := imagebuild.SpecFor(k, images, tag, pins)
		sp.Update, sp.From = d.Update, d.From
		specs = append(specs, sp)
	}
	results, err := runner.All(ctx, specs)
	promoted := false
	for _, r := range results {
		if r.OK && r.Update {
			imagebuild.Promoted(state, r.Kind, imagebuild.Decision{Target: r.Version, From: r.From, Update: true}, s.Clock.Now())
			flog.Printf("rebuild %s: updated %s -> %s", r.Kind, r.From, r.Version)
			promoted = true
		}
	}
	if promoted && serr == nil {
		if werr := imagebuild.SaveState(statePath, state); werr != nil && err == nil {
			err = werr
		}
	}
	if err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	return nil
}

func (s *Server) latest(ctx context.Context, pkg string) (string, error) {
	if s.Latest != nil {
		return s.Latest(ctx, pkg)
	}
	return imagebuild.NPMLatest(ctx, nil, lookup(s.Environ, "FACTORY_NPM_REGISTRY"), pkg)
}
