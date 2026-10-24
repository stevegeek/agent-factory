package pipeline

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A local agent is given the checkout's pinned untracked paths (a library's
// gitignored lockfile) to mount, but not one the project tracks, one that
// is missing, or a link.
func TestALocalAgentGetsThePinnedUntrackedPaths(t *testing.T) {
	h := newHarness(t)
	h.repo.Write(".gitignore", "Gemfile.lock\nvendor/\n")
	h.repo.Commit("ignore")
	h.repo.Write("Gemfile.lock", "GEM\n")
	h.repo.Write("vendor/x.rb", "1\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(h.repo.Dir, "linked")); err != nil {
		t.Fatal(err)
	}
	h.config(strings.Replace(defaultConfig, "lint = \"true\"\n",
		"lint = \"true\"\npinned_untracked = [\"Gemfile.lock\", \"vendor\", \"lib\", \"missing.lock\", \"linked\"]\n", 1))
	h.file("p", "x", "triage", nil)
	h.tick()
	if len(h.local.started) != 1 {
		t.Fatalf("started %+v", h.local.started)
	}
	if got := h.local.started[0].Pinned; !slices.Equal(got, []string{"Gemfile.lock", "vendor"}) {
		t.Errorf("pinned %q", got)
	}
}
