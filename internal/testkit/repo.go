package testkit

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Repo is a real git repository in a temp dir, the Go form of the Python
// suite's RepoCase: branch main, a README and lib/thing.rb committed as
// "init". Git runs with an empty global config and no system config, and
// without the caller's git identity variables: its commits are by t at a
// fixed date.
type Repo struct {
	Dir string
	env []string
	t   testing.TB
}

// NewRepo creates the repository. The test is skipped when git is missing.
func NewRepo(t testing.TB) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Repo{Dir: t.TempDir(), t: t}
	r.env = append(WithoutGitIdentity(os.Environ()),
		"HOME="+home, "GIT_CONFIG_GLOBAL="+gitconfig, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05+00:00", "GIT_COMMITTER_DATE=2026-01-02T03:04:05+00:00")
	r.Git("init", "-q", "-b", "main")
	r.Git("config", "user.email", "t@example.com")
	r.Git("config", "user.name", "t")
	r.Git("config", "commit.gpgsign", "false")
	r.Write("README.md", "hello\n")
	r.Write("lib/thing.rb", "def thing; 1; end\n")
	r.Commit("init")
	return r
}

// Git runs git in the repository and returns its trimmed stdout. A failure
// fails the test.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return strings.TrimSpace(stdout.String())
}

// Write writes a file in the work tree, creating directories.
func (r *Repo) Write(path, text string) {
	r.t.Helper()
	full := filepath.Join(r.Dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// Commit stages everything and commits it.
func (r *Repo) Commit(msg string) {
	r.t.Helper()
	r.Git("add", "-A")
	r.Git("commit", "-q", "-m", msg)
}

// BranchWith creates branch name from main with the given files committed,
// then checks main out again.
func (r *Repo) BranchWith(name string, files map[string]string) {
	r.t.Helper()
	r.Git("checkout", "-q", "-b", name)
	for path, text := range files {
		r.Write(path, text)
	}
	r.Commit(name)
	r.Git("checkout", "-q", "main")
}
