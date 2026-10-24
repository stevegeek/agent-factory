package upkeep

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/store"
)

// The latest-toolchain trial (P5 phase B): once every [upkeep]
// latest_every_days (default 30; 0 is off), the weekly rebuild on the host
// with the checkouts also builds a trial of the gate image with the newest
// Crystal and ameba, runs every project's checks in it (a failure runs
// again in the live image, to tell a break from one already there), and
// files one notice with the outcome. The trial is the rebuild itself with
// Spec.Trial set: it never tags, and the live image never changes. Ruby
// comes from the distro with no pin, so it gets no trial.

// LatestName is the trial's clock in state.json.
const LatestName = "latest-toolchain"

// trialTail is how much of a project's output the notice keeps.
const trialTail = 1500

// Latest is `factory rebuild --latest`: a latest-toolchain trial now,
// whatever its clock says, under the tick lock (waiting for a running
// tick). It needs the project checkouts, so it runs only on the host that
// has them. The error is for a trial that could not run (the lock, podman,
// a cancel) or a notice that could not be filed.
func (s Service) Latest(ctx context.Context, cfg *config.Config) error {
	if !hasCheckout(cfg) {
		return errors.New("latest: no project has a checkout on this host; run it where the projects are")
	}
	l, err := s.Board.TickLock(ctx, store.Forever)
	if err != nil {
		return fmt.Errorf("latest: %w", err)
	}
	defer l.Unlock()
	state, serr := imagebuild.LoadState(s.Board.Paths.ImagesState())
	if serr != nil {
		s.logf("latest: %v; trying at the [images] pins", serr)
	}
	return s.latest(ctx, cfg, state)
}

// trialOn reports whether the trial runs on this host: it is on, a gate
// image (top-level image) is configured, and some enabled factory project
// with a checkout here has the crystal toolchain. A gate image podman does
// not have here is found only when the trial runs, which then files the
// "Update" notices itself.
func trialOn(cfg *config.Config) bool {
	_, ok := gateKind(cfg)
	return cfg.Upkeep.LatestEveryDays > 0 && ok && len(trialToolchainProjects(cfg)) > 0
}

// gateKind is the image kind whose tag is the top-level image (the gate's).
func gateKind(cfg *config.Config) (imagebuild.Kind, bool) {
	if cfg.Image == "" {
		return imagebuild.Kind{}, false
	}
	for _, k := range imagebuild.Kinds() {
		if imagebuild.TagFor(k.Kind, cfg.Image, cfg.Remote.Image) == cfg.Image {
			return k, true
		}
	}
	return imagebuild.Kind{}, false
}

// trialToolchainProjects are the enabled factory projects with a checkout
// here and the crystal toolchain: a trial runs only when there is one.
func trialToolchainProjects(cfg *config.Config) []string {
	var keys []string
	for _, p := range cfg.FactoryProjects() {
		if p.Enabled && isDir(p.Path) && cfg.Toolchain(p) == "crystal" {
			keys = append(keys, p.Key)
		}
	}
	return keys
}

// latestDue reports whether latest_every_days have passed since the
// trial's clock (never run counts as due).
func (s Service) latestDue(cfg *config.Config) (bool, error) {
	st, err := s.Board.LoadState()
	if err != nil {
		return false, fmt.Errorf("latest: %w", err)
	}
	prev, ok := st.Clock(LatestName)
	if !ok {
		return true, nil
	}
	at, err := clock.Parse(prev)
	if err != nil {
		return false, fmt.Errorf("latest: state.json clock.%s: %w", LatestName, err)
	}
	return s.now().Sub(at) >= time.Duration(cfg.Upkeep.LatestEveryDays)*24*time.Hour, nil
}

// newTool is a toolchain with a release newer than the images have.
type newTool struct {
	tool   Tool
	pinned string // what the images have
	latest string // the newest release
}

// latest runs one trial, with the lock held. With no Crystal project
// here, no release newer than the pins, or a release lookup that fails, it
// logs a line and builds nothing; with a newer release but no gate image
// to try it in, it files the "Update" notices Versions leaves to the
// trial. The clock is set only once a trial has run every project, so a
// new release is tried on the first weekly rebuild after it, and a trial
// that stopped on an error is tried again the next week.
func (s Service) latest(ctx context.Context, cfg *config.Config, state imagebuild.State) error {
	if len(trialToolchainProjects(cfg)) == 0 {
		s.logf("latest: no enabled Crystal project has a checkout here; no trial")
		return nil
	}
	if s.Releases == nil {
		s.logf("latest: no release lookups here; no trial")
		return nil
	}
	pins := map[string]string{}
	for k, v := range cfg.Images {
		pins[k] = v
	}
	var fresh []newTool
	var have []string
	for _, tool := range Tools() {
		if tool.Toolchain != "crystal" {
			continue
		}
		pinned, ok := cfg.Images[tool.Key]
		if !ok {
			pinned = tool.Default
		}
		have = append(have, tool.Name+" "+pinned)
		pins[tool.Key] = pinned
		v, err := s.Releases.LatestRelease(ctx, tool.Repo)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("latest %s: %w", tool.Name, err)
			}
			s.logf("latest %s: %v; no trial", tool.Name, err)
			return nil
		}
		if newer(v, pinned) {
			pins[tool.Key] = v
			fresh = append(fresh, newTool{tool: tool, pinned: pinned, latest: v})
		}
	}
	if len(fresh) == 0 {
		s.logf("latest: nothing newer than %s; no trial", strings.Join(have, ", "))
		return nil
	}
	kind, ok := gateKind(cfg)
	if !ok {
		s.logf("latest: no gate image (top-level image) is configured; no trial")
		return s.updateNotices(ctx, fresh)
	}
	pins[kind.Kind] = imagebuild.Effective(cfg.Images[kind.Kind], state[kind.Kind])
	sp := imagebuild.SpecFor(kind, s.Board.Paths.Images, cfg.Image, pins)
	sp.Trial = true
	for _, p := range cfg.FactoryProjects() {
		if p.Enabled && isDir(p.Path) {
			sp.Projects = append(sp.Projects, s.projectCheck(cfg, p, "trial-"+p.Key))
		}
	}
	results, err := s.Builder.All(ctx, []imagebuild.Spec{sp})
	if err != nil {
		return fmt.Errorf("latest: %w", err)
	}
	if len(results) == 0 {
		s.logf("latest: the gate image %s is not on this host; no trial", cfg.Image)
		return s.updateNotices(ctx, fresh)
	}
	r := results[0]
	if r.Err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("latest: %w", cerr)
		}
		// not every project ran: a notice, and no clock, so next week tries again
		s.logf("latest: the trial could not finish: %v", r.Err)
		title, body := s.unfinishedNotice(fresh, have, r)
		if _, err := s.Notices.Notice(ctx, title, body); err != nil {
			return fmt.Errorf("latest: %w", err)
		}
		return nil
	}
	title, body := s.trialNotice(fresh, have, r)
	if _, err := s.Notices.Notice(ctx, title, body); err != nil {
		return fmt.Errorf("latest: %w", err)
	}
	return s.setLatestClock(ctx)
}

// setLatestClock records the trial in state.json, under the state lock the
// daily clock holds from its read to its write.
func (s Service) setLatestClock(ctx context.Context) error {
	l, err := s.Board.StateLock(ctx, store.Forever)
	if err != nil {
		return fmt.Errorf("latest: %w", err)
	}
	defer l.Unlock()
	st, err := s.Board.LoadState()
	if err != nil {
		return fmt.Errorf("latest: %w", err)
	}
	st.SetClock(LatestName, clock.Stamp(s.now()))
	if err := s.Board.SaveState(st); err != nil {
		return fmt.Errorf("latest: %w", err)
	}
	return nil
}

// updateNotices are Versions' "Update" notices for the newer releases, for
// a host where the trial cannot build.
func (s Service) updateNotices(ctx context.Context, fresh []newTool) error {
	for _, n := range fresh {
		if err := s.updateNotice(ctx, n.tool, n.pinned, n.latest); err != nil {
			return fmt.Errorf("latest: %w", err)
		}
	}
	return nil
}

// trialWhat is the trial's toolchains for its notices ("Crystal 1.21.0 and
// ameba 1.8.0"), their [images] settings and their release notes.
func trialWhat(fresh []newTool) (what string, takes, notes []string) {
	var names []string
	for _, n := range fresh {
		names = append(names, n.tool.Name+" "+n.latest)
		takes = append(takes, fmt.Sprintf("%s = \"%s\"", n.tool.Key, n.latest))
		notes = append(notes, n.tool.Changelog)
	}
	return strings.Join(names, " and "), takes, notes
}

func (s Service) trialLog(r imagebuild.Result) string {
	return fmt.Sprintf("%s/trial-%s.log", s.Board.Paths.Logs, r.Kind)
}

// unfinishedNotice is a trial that stopped on an error (a project's
// checkout that would not fetch, a missing base, a podman failure) before
// every project ran.
func (s Service) unfinishedNotice(fresh []newTool, have []string, r imagebuild.Result) (string, string) {
	what, _, notes := trialWhat(fresh)
	return what + " trial could not finish", fmt.Sprintf("The latest-toolchain trial of %s (the images have %s) "+
		"stopped before every project's checks ran: %s. The live image is unchanged, and the weekly rebuild "+
		"tries the trial again. Log: %s on the host. Release notes: %s. Move this ticket to done once it is looked at.",
		what, strings.Join(have, ", "), r.Err, s.trialLog(r), strings.Join(notes, ", "))
}

// trialNotice is the trial's one notice: "Crystal 1.21.0 trial", its body
// opening with the counts ("6 pass, 1 breaks, 1 already failing"), or
// "... trial failed" when the trial image did not build or pass its smoke
// test. The title has no counts, so a trial of the same release while its
// notice is open files no second one.
func (s Service) trialNotice(fresh []newTool, have []string, r imagebuild.Result) (string, string) {
	what, takes, notes := trialWhat(fresh)
	log := s.trialLog(r)
	if !r.OK {
		return what + " trial failed", fmt.Sprintf("The latest-toolchain trial tried the gate image with %s "+
			"(the images have %s): %s. The live image is unchanged. Log: %s on the host. Release notes: %s. "+
			"Move this ticket to done once it is looked at.",
			what, strings.Join(have, ", "), r.Summary, log, strings.Join(notes, ", "))
	}
	list := func(keys []string) string {
		if len(keys) == 0 {
			return "none"
		}
		return strings.Join(keys, ", ")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s.\n\n", what, imagebuild.TrialCounts(r))
	fmt.Fprintf(&b, "The latest-toolchain trial built the gate image with %s (the images have %s), ran every "+
		"project's checks in it, and ran each failure again in the live image. The live image is unchanged.\n\n",
		what, strings.Join(have, ", "))
	fmt.Fprintf(&b, "- Pass: %s\n- Break (pass in the live image): %s\n- Already failing (fail in the live image too): %s\n\n",
		list(r.Passed), list(r.Broken), list(r.AlreadyFailing))
	fmt.Fprintf(&b, "To take it: set [images] %s in config.toml and deploy. The weekly rebuild then "+
		"builds it and keeps the old image if a project regresses, so fix the projects that break first "+
		"(file a fix ticket for each). Release notes: %s. Log: %s on the host. Then move this ticket to done.\n",
		strings.Join(takes, " and "), strings.Join(notes, ", "), log)
	for _, group := range []struct {
		keys []string
		how  string
	}{{r.Broken, "breaks"}, {r.AlreadyFailing, "already failing"}} {
		for _, k := range group.keys {
			fmt.Fprintf(&b, "\n## %s (%s)\n\n```\n%s\n```\n", k, group.how, lastRunes(strings.TrimRight(r.Tails[k], "\n"), trialTail))
		}
	}
	return what + " trial", b.String()
}

// lastRunes is the last n code points of s.
func lastRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[len(r)-n:])
}
