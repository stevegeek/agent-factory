// Package security is the mechanical half of the security stage
// (ARCHITECTURE.md §2.3, inventory-pipeline §7.4): the tier a change is
// classified into from its files and added lines (no model involved), the
// questions each tier asks the security agent, and the outcome, which is
// decided from the agent's answers rather than its own verdict.
//
// The package reads no files and runs no git: callers pass what git said
// (gate.ClassifyBranch does both for a branch).
package security

import (
	"regexp"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// MaxFiles is the number of changed files above which a change is tier 4
// whatever the files are.
const MaxFiles = 15

// DefaultTier3Paths is TIER3_PATHS: build files, manifests and lockfiles.
func DefaultTier3Paths() []string {
	return []string{"*.gemspec", "Gemfile", "Gemfile.lock", "gemfiles/*", "Rakefile", ".github/*", "bin/*",
		"*extconf.rb", "Appraisals", "package.json", "*.lock", "shard.yml", "Makefile", "script/*"}
}

// DefaultTextPaths is TEXT_PATHS: files that are only text.
func DefaultTextPaths() []string {
	return []string{"*.md", "*.txt", "*.rdoc", "docs/*", "LICENSE*", "CHANGELOG*", ".standard.yml", ".rubocop.yml",
		"*.rbs", "sig/*", ".ameba.yml", ".editorconfig"}
}

// Patterns are the fnmatch patterns Classify reads paths with (§2.4.8: a
// later language registry supplies other ones).
type Patterns struct {
	Tier3 []string // build or dependencies
	Text  []string // text only
}

// DefaultPatterns are the patterns of a project that sets neither list.
func DefaultPatterns() Patterns {
	return Patterns{Tier3: DefaultTier3Paths(), Text: DefaultTextPaths()}
}

// ProjectPatterns are a project's patterns: its tier3_paths and text_paths
// where it sets them (an empty list included), else the defaults.
func ProjectPatterns(p config.Project) Patterns {
	pats := DefaultPatterns()
	if p.Tier3Set {
		pats.Tier3 = append([]string{}, p.Tier3Paths...)
	}
	if p.TextSet {
		pats.Text = append([]string{}, p.TextPaths...)
	}
	return pats
}

// tier2Content is TIER2_CONTENT behind \A: Python's leading \b becomes
// textx.SearchAtBoundary, which tries it at each word boundary in turn.
//
// py: \b(eval|instance_eval|class_eval|send|public_send|constantize|system|exec|spawn|Marshal|YAML\.(unsafe_)?load|JSON\.parse|open\(|html_safe|raw\(|sanitize|find_by_sql|to_sql|Regexp\.new|Kernel\.|Process\.(run|exec|new)|from_json|from_yaml|YAML\.parse|pointerof|Pointer|unsafe_as|\.unsafe_)
var tier2Content = regexp.MustCompile(`\A(eval|instance_eval|class_eval|send|public_send|constantize|system|exec|spawn|Marshal|YAML\.(unsafe_)?load|JSON\.parse|open\(|html_safe|raw\(|sanitize|find_by_sql|to_sql|Regexp\.new|Kernel\.|Process\.(run|exec|new)|from_json|from_yaml|YAML\.parse|pointerof|Pointer|unsafe_as|\.unsafe_)`)

// tier2Path is TIER2_PATH with re.I: (?i), and FoldI for the dotted and
// dotless i Python's case folding also matches.
//
// py: (pars|render|serial|encod|decod|request|response|http|json|yaml|sql|html|escap|input)
var tier2Path = regexp.MustCompile(`(?i)(` + textx.FoldI(`pars|render|serial|encod|decod|request|response|http|json|yaml|sql|html|escap|input`) + `)`)

// Tier2Content reports whether an added line calls a Ruby or Crystal sink
// (TIER2_CONTENT.search).
func Tier2Content(line string) bool {
	_, ok := tier2ContentMatch(line)
	return ok
}

func tier2ContentMatch(line string) (string, bool) {
	loc := textx.SearchAtBoundary(tier2Content, line)
	if loc == nil {
		return "", false
	}
	return line[loc[0]:loc[1]], true
}

// Tier2Path reports whether a path names a boundary: parsing, rendering,
// encoding, requests, input (TIER2_PATH.search).
func Tier2Path(path string) bool {
	_, ok := tier2PathMatch(path)
	return ok
}

func tier2PathMatch(path string) (string, bool) {
	loc := tier2Path.FindStringIndex(path)
	if loc == nil {
		return "", false
	}
	return path[loc[0]:loc[1]], true
}

func matchesAny(name string, pats []string) bool {
	for _, p := range pats {
		if textx.Fnmatch(name, p) {
			return true
		}
	}
	return false
}

// Classify is classify(project, branch) on what git said: files are the
// changed paths (diff --name-only base...branch), added the added lines
// (diff -U0, without their "+").
//
// Each file is tier 3 if it matches a tier 3 pattern, else 0 if it matches
// a text pattern, else 2 if its path names a boundary, else 1; any added
// line calling a sink adds tier 2. No files is tier 0. Both 2 and 3, or more
// than MaxFiles files, is tier 4; otherwise the highest tier wins. Patterns
// use fnmatch, where * matches "/".
func Classify(files, added []string, pats Patterns) int {
	var tiers [4]bool
	for _, f := range files {
		switch {
		case matchesAny(f, pats.Tier3):
			tiers[3] = true
		case matchesAny(f, pats.Text):
			tiers[0] = true
		case Tier2Path(f):
			tiers[2] = true
		default:
			tiers[1] = true
		}
	}
	for _, l := range added {
		if Tier2Content(l) {
			tiers[2] = true
			break
		}
	}
	if tiers[2] && tiers[3] || len(files) > MaxFiles {
		return 4
	}
	for tier := 3; tier > 0; tier-- {
		if tiers[tier] {
			return tier
		}
	}
	return 0
}
