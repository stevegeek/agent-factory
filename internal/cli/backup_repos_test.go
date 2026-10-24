package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// The backup keeps what only the clone has: the commits of bot/ branches
// that no remote has, and the pinned untracked files. A pushed branch, a
// pinned directory and a project with nothing to keep add nothing.
func TestTheBackupSavesUnpushedBranchesAndPinnedFiles(t *testing.T) {
	env := testkit.Root(t).Environ()
	r := testkit.NewRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	r.Git("clone", "-q", "--bare", r.Dir, bare)
	r.Git("remote", "add", "origin", bare)
	r.Git("fetch", "-q", "origin")
	r.BranchWith("bot/pushed", map[string]string{"a.rb": "1\n"})
	r.Git("push", "-q", "origin", "bot/pushed")
	r.BranchWith("bot/new", map[string]string{"b.rb": "2\n"})
	r.Write(".gitignore", "Gemfile.lock\nvendor/\n")
	r.Commit("ignore")
	r.Git("push", "-q", "origin", "main")
	r.Write("Gemfile.lock", "GEM\n")
	r.Write("vendor/x.rb", "1\n")
	quiet := testkit.NewRepo(t)

	guard := gitx.NewGuarder(execx.OS{}, env, nil)
	var logged []string
	s := repoSaver{
		projects: []config.Project{
			{Key: "p", Path: r.Dir, Base: "main", PinnedUntracked: []string{"Gemfile.lock", "vendor", "../x"}},
			{Key: "q", Path: quiet.Dir, Base: "main"},
		},
		repo: func(p config.Project) *gitread.Repo {
			return &gitread.Repo{Path: p.Path, Base: p.Base, Exec: execx.OS{}, Env: env, Guard: guard}
		},
		logf: func(format string, args ...any) { logged = append(logged, format) },
	}
	dir := filepath.Join(t.TempDir(), "repos")
	if err := s.Save(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(logged) != 0 {
		t.Errorf("logged %q", logged)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "p", "untracked", "Gemfile.lock")); err != nil || string(b) != "GEM\n" {
		t.Errorf("Gemfile.lock %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p", "untracked", "vendor")); !os.IsNotExist(err) {
		t.Error("a pinned directory was saved")
	}
	if _, err := os.Stat(filepath.Join(dir, "q")); !os.IsNotExist(err) {
		t.Error("a project with nothing to keep left a folder")
	}
	out, err := exec.Command("git", "bundle", "list-heads", filepath.Join(dir, "p", "unpushed.bundle")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if heads := string(out); !strings.Contains(heads, "refs/heads/bot/new") || strings.Contains(heads, "bot/pushed") {
		t.Errorf("bundle heads %q", heads)
	}
}
