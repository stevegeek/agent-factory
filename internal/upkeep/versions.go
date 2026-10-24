package upkeep

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
)

// Tool is one row of Python's TOOLS table: a toolchain inside the
// images, checked against its GitHub releases, only when some factory
// project's kind has that toolchain (config.Toolchain).
type Tool struct {
	Key       string // the [images] pin's key
	Name      string
	Repo      string // owner/repo on GitHub
	Default   string // what the Containerfiles build when config pins none
	Toolchain string // a config.ToolchainNames() entry
	Changelog string
}

// Tools is the TOOLS table, in Python's order: a fresh copy.
func Tools() []Tool {
	return []Tool{
		{Key: "crystal", Name: "Crystal", Repo: "crystal-lang/crystal", Default: "1.20.1", Toolchain: "crystal",
			Changelog: "https://github.com/crystal-lang/crystal/releases"},
		{Key: "ameba", Name: "ameba", Repo: "crystal-ameba/ameba", Default: "1.7.0", Toolchain: "crystal",
			Changelog: "https://github.com/crystal-ameba/ameba/releases"},
	}
}

// Versions is check_versions(cfg), `factory versions` and the
// clock's weekly check: for each toolchain a factory project uses, and each
// image whose version config pins, a release newer than the pin files a
// notice saying how to take it. A lookup that fails is logged and passed
// over. The error is a notice that could not be filed.
//
// Where the latest-toolchain trial runs (trialOn), the Crystal and ameba
// notices are left to it: its notice says the same with evidence, each
// project's checks in the new toolchain (and where it finds it cannot
// build, it files these notices itself).
func (s Service) Versions(ctx context.Context, cfg *config.Config) error {
	used := map[string]bool{}
	for _, p := range cfg.FactoryProjects() {
		used[cfg.Toolchain(p)] = true
	}
	if trialOn(cfg) {
		delete(used, "crystal")
	}
	for _, tool := range Tools() {
		if !used[tool.Toolchain] {
			continue
		}
		pinned, ok := cfg.Images[tool.Key]
		if !ok {
			pinned = tool.Default
		}
		latest, err := s.Releases.LatestRelease(ctx, tool.Repo)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("versions %s: %w", tool.Name, err)
			}
			s.logf("versions %s: %v", tool.Name, err)
			continue
		}
		if !newer(latest, pinned) {
			continue
		}
		if err := s.updateNotice(ctx, tool, pinned, latest); err != nil {
			return fmt.Errorf("versions %s: %w", tool.Name, err)
		}
	}
	// the pin is a floor, and the version an image has is the newer
	// of it and images.json. With no images.json (or one that does not
	// read) that is the pin, as in Python.
	state := imagebuild.State{}
	if s.Board != nil {
		st, err := imagebuild.LoadState(s.Board.Paths.ImagesState())
		if err != nil {
			s.logf("versions: %v; comparing with the [images] pins", err)
		} else {
			state = st
		}
	}
	for _, k := range imagebuild.Kinds() {
		floor := cfg.Images[k.Kind]
		if floor == "" {
			continue
		}
		pinned := imagebuild.Effective(floor, state[k.Kind])
		latest, err := s.Releases.NPMLatest(ctx, k.NPM)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("versions %s: %w", k.Name, err)
			}
			s.logf("versions %s: %v", k.Name, err)
			continue
		}
		if !newer(latest, pinned) {
			continue
		}
		have := "the images pin " + pinned
		if pinned != floor {
			have = fmt.Sprintf("the images have %s (the weekly rebuild updated them; the [images] pin %s is a floor)", pinned, floor)
		}
		body := fmt.Sprintf("%s %s is out; %s.\n\nChangelog: %s\n\n"+
			"To take it: set [images] %s = \"%s\" in config.toml and deploy. The weekly "+
			"rebuild picks it up and keeps the old image if the new one fails its checks "+
			"(or run `factory rebuild` on the host now). Then move this ticket to done.",
			k.Name, latest, have, k.Changelog, k.Kind, latest)
		if _, err := s.Notices.Notice(ctx, fmt.Sprintf("Update %s %s -> %s", k.Name, pinned, latest), body); err != nil {
			return fmt.Errorf("versions %s: %w", k.Name, err)
		}
	}
	return nil
}

// newer is version_tuple(latest) > version_tuple(pinned) (imagebuild.Newer).
func newer(latest, pinned string) bool { return imagebuild.Newer(latest, pinned) }

// pyTitle is str.title() of a lower-case ASCII word.
func pyTitle(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// updateNotice is the "Update <tool> <pinned> -> <latest>" notice: a newer
// release, and how to take it.
func (s Service) updateNotice(ctx context.Context, tool Tool, pinned, latest string) error {
	body := fmt.Sprintf("%s %s is out; the images have %s.\n\nRelease notes: %s\n\n"+
		"To take it: set [images] %s = \"%s\" in config.toml and deploy. The weekly rebuild "+
		"runs every project's tests in the new image, so it is also the check that each "+
		"%s project builds and passes on %s. If one fails, the old image "+
		"stays and the rebuild log says which; file a fix ticket for it. Then move this ticket to done.",
		tool.Name, latest, pinned, tool.Changelog, tool.Key, latest, pyTitle(tool.Toolchain), latest)
	_, err := s.Notices.Notice(ctx, fmt.Sprintf("Update %s %s -> %s", tool.Name, pinned, latest), body)
	return err
}
