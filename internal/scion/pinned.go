package scion

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/scionread"
)

// Workspace is where local mode mounts agent name's worktree inside its
// container: the project is /repo-root, and Scion keeps its worktrees under
// the project's .scion/agents (render's provision patch).
func Workspace(name string) string { return "/repo-root/.scion/agents/" + name + "/workspace" }

// Pinned is one of the project's pinned untracked paths mounted into an
// agent's workspace: Rel, relative to the project, from Source on the host.
type Pinned struct {
	Rel      string
	Source   string
	ReadOnly bool
}

// PinnedVolumes is run-config text that mounts each pinned path at the same
// place in agent name's workspace. A fresh worktree lacks them (a library's
// gitignored lockfile, a shard's installed lib/), and without the lockfile
// bundler goes to fetch its git sources, into a read-only gem cache. The
// caller mounts a file from the run's own copy, writable (bundle check
// rewrites the lockfile even when nothing changed), and a directory from
// the project read-only, so no agent changes what the gate and every later
// agent are given. The entries follow a run config's volumes list.
func PinnedVolumes(name string, pins []Pinned) (string, error) {
	if err := scionread.CheckName(name); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range pins {
		if p.Rel == "" || !filepath.IsLocal(p.Rel) {
			return "", fmt.Errorf("pinned path %q is not inside the project", p.Rel)
		}
		if !filepath.IsAbs(p.Source) {
			return "", fmt.Errorf("pinned path %q: its source %q is not absolute", p.Rel, p.Source)
		}
		rel := filepath.ToSlash(filepath.Clean(p.Rel))
		src, _ := json.Marshal(filepath.Clean(p.Source))
		dst, _ := json.Marshal(Workspace(name) + "/" + rel)
		fmt.Fprintf(&b, "  - type: local\n    source: %s\n    target: %s\n", src, dst)
		if p.ReadOnly {
			b.WriteString("    read_only: true\n")
		}
	}
	return b.String(), nil
}
