package sysinfo

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
)

// GitHub is Root/github.json: whether the tick's GitHub pass reaches
// GitHub at all, across every call it makes. The tick writes it (through
// GitHubRecord) and Foreman shows it on every page (GitHubStatus).
type GitHub struct {
	LastOK       time.Time `json:"last_ok,omitzero"`
	Failed       int       `json:"failed_passes,omitempty"`
	FailingSince time.Time `json:"failing_since,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
	Down         bool      `json:"unreachable,omitempty"`
}

// GitHubDownAfter is how many passes in a row must fail to reach GitHub
// before it is said to be unreachable: one lost pass is a blip.
const GitHubDownAfter = 2

// GitHubFile is where the tick keeps GitHub's reachability.
func GitHubFile(p paths.Paths) string { return filepath.Join(p.Root, "github.json") }

// Note records one pass: ok when any call got an answer from GitHub,
// otherwise lastErr is why none did. It returns a line to log when the
// state flips, and "" otherwise.
func (g *GitHub) Note(now time.Time, ok bool, lastErr string) string {
	if ok {
		was, failed := g.Down, g.Failed
		*g = GitHub{LastOK: now}
		if was {
			return fmt.Sprintf("github: reachable again after %d failed passes", failed)
		}
		return ""
	}
	if g.Failed == 0 {
		g.FailingSince = now
	}
	g.Failed++
	g.LastError = lastErr
	if g.Down || g.Failed < GitHubDownAfter {
		return ""
	}
	g.Down = true
	return fmt.Sprintf("github: unreachable since %s (last error: %s)", g.FailingSince.Format("15:04"), lastErr)
}

// Line is the board's status line, with times in loc.
func (g GitHub) Line(loc *time.Location) string {
	last := "never"
	if !g.LastOK.IsZero() {
		last = g.LastOK.In(loc).Format("2 Jan 15:04")
	}
	if g.Down {
		return fmt.Sprintf("GitHub: unreachable since %s (last error: %s) · last good poll %s",
			g.FailingSince.In(loc).Format("15:04"), g.LastError, last)
	}
	return "GitHub: connected · last poll " + last
}

// ReadGitHub loads github.json, never through a link. A missing file
// matches fs.ErrNotExist.
func ReadGitHub(path string) (GitHub, error) {
	b, err := fsx.ReadNoFollow(path, maxFile)
	if err != nil {
		return GitHub{}, err
	}
	var g GitHub
	if err := json.Unmarshal(b, &g); err != nil {
		return GitHub{}, fmt.Errorf("%s: %w", path, err)
	}
	return g, nil
}

// WriteGitHub replaces github.json atomically.
func WriteGitHub(path string, g GitHub) error {
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("sysinfo: %w", err)
	}
	if err := fsx.WriteAtomic(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("sysinfo: %w", err)
	}
	return nil
}

// GitHubRecord keeps github.json under Paths for the tick's GitHub pass.
type GitHubRecord struct{ Paths paths.Paths }

// Note reads the state (a missing or unreadable file starts afresh),
// records the pass and writes it back. The line is GitHub.Note's.
func (r GitHubRecord) Note(now time.Time, ok bool, lastErr string) (string, error) {
	path := GitHubFile(r.Paths)
	g, _ := ReadGitHub(path)
	line := g.Note(now, ok, lastErr)
	return line, WriteGitHub(path, g)
}

// GitHubStatus is the board's GitHub line and whether GitHub is
// unreachable, or "" when the tick has not recorded one.
func GitHubStatus(p paths.Paths) (string, bool) {
	if p.Root == "" {
		return "", false
	}
	g, err := ReadGitHub(GitHubFile(p))
	if err != nil {
		return "", false
	}
	return g.Line(time.Local), g.Down
}
