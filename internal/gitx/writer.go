package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Timeouts Python set on its git calls.
const (
	FetchTimeout = 120 * time.Second // refresh_base's fetch of origin
	PushTimeout  = 300 * time.Second // push_branch
)

// Writer is the dispatcher's git on a project clone. Every call runs the
// Repo's guard first (a *Guarder in the dispatcher) and git with
// gitread.SafeEnv, as Python's git() did.
type Writer struct {
	Repo *gitread.Repo
}

// git is Python's git(project, *args) with an optional timeout (zero is
// gitread.DefaultTimeout): guarded, checked, stdout stripped.
func (w Writer) git(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return w.gitEnv(ctx, timeout, gitread.SafeEnv(w.Repo.BaseEnv()), args...)
}

func (w Writer) gitEnv(ctx context.Context, timeout time.Duration, env []string, args ...string) (string, error) {
	if err := w.Repo.Checked(ctx); err != nil {
		return "", err
	}
	if timeout == 0 {
		timeout = gitread.DefaultTimeout
	}
	argv := append([]string{"git", "-C", w.Repo.Path}, args...)
	return run(ctx, w.Repo.Exec, execx.Cmd{Argv: argv, Env: env, Timeout: timeout})
}

// run runs c and is the stripped stdout, or Python run(check=True)'s error.
func run(ctx context.Context, ex execx.Execer, c execx.Cmd) (string, error) {
	res, err := ex.Run(ctx, c)
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(head(c.Argv, 4), " "), err)
	}
	if err := execx.CheckErr(res); err != nil {
		return "", err
	}
	return textx.Strip(string(res.Stdout)), nil
}

func head(xs []string, n int) []string {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

// RefreshBase is refresh_base: fast-forward the clone's base branch to
// origin's, so new agents start from what has been merged. With the base
// checked out it merges --ff-only; otherwise it fetches base:base (which
// fails when the local base has diverged).
func (w Writer) RefreshBase(ctx context.Context) error {
	base := w.Repo.Base
	if _, err := w.git(ctx, FetchTimeout, "fetch", "-q", "origin", base); err != nil {
		return err
	}
	cur, err := w.git(ctx, 0, "branch", "--show-current")
	if err != nil {
		return err
	}
	if cur == base {
		_, err = w.git(ctx, 0, "merge", "-q", "--ff-only", "origin/"+base)
	} else {
		_, err = w.git(ctx, 0, "fetch", "-q", "origin", base+":"+base)
	}
	return err
}

// Bundle is repo_bundle: a git bundle of refs/heads/<r> for each of refs,
// read back into memory (the helper job's repo.bundle).
func (w Writer) Bundle(ctx context.Context, refs []string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "factory-bundle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "repo.bundle")
	args := []string{"bundle", "create", "-q", path}
	for _, r := range refs {
		args = append(args, "refs/heads/"+r)
	}
	if _, err := w.git(ctx, 0, args...); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// BundleUnpushed writes to path a bundle of the bot/ branches' commits that
// no remote-tracking branch has (work the factory has not pushed yet), and
// says whether there were any; with none, nothing is written.
func (w Writer) BundleUnpushed(ctx context.Context, path string) (bool, error) {
	n, err := w.git(ctx, 0, "rev-list", "--count", "--branches=bot/*", "--not", "--remotes")
	if err != nil || n == "0" {
		return false, err
	}
	if _, err := w.git(ctx, 0, "bundle", "create", "-q", path, "--branches=bot/*", "--not", "--remotes"); err != nil {
		return false, err
	}
	return true, nil
}

// FetchBundle is fetch_branch: a forced update of refs/heads/<branch>, and
// nothing else, from a bundle the helper sent back, with every object
// checked (transfer.fsckObjects): the bundle was made where an agent ran.
func (w Writer) FetchBundle(ctx context.Context, branch string, bundle []byte) error {
	if branch == "" {
		return errors.New("fetch a bundle: no branch")
	}
	dir, err := os.MkdirTemp("", "factory-bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "out.bundle")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	_, err = w.git(ctx, 0, "-c", "transfer.fsckObjects=true", "fetch", "-q", path, "+"+ref+":"+ref)
	return err
}

// Clone is a fresh clone the factory made (the gate's checkout), so git
// runs there with the safe environment but without a guard.
type Clone struct {
	Dir  string
	Exec execx.Execer
	Env  []string // the base environment
}

// CloneNoCheckout is the first step of checks_on_fresh_checkout: the guard,
// then git clone -q --no-hardlinks --no-checkout <project> <dest>. Objects
// and refs are copied; the project's hooks and config are not.
func (w Writer) CloneNoCheckout(ctx context.Context, dest string) (*Clone, error) {
	if err := w.Repo.Checked(ctx); err != nil {
		return nil, err
	}
	env := w.Repo.BaseEnv()
	argv := []string{"git", "clone", "-q", "--no-hardlinks", "--no-checkout", w.Repo.Path, dest}
	if _, err := run(ctx, w.Repo.Exec, execx.Cmd{Argv: argv, Env: gitread.SafeEnv(env), Timeout: gitread.DefaultTimeout}); err != nil {
		return nil, err
	}
	return &Clone{Dir: dest, Exec: w.Repo.Exec, Env: env}, nil
}

// CheckoutDetach checks ref out, detached.
func (c *Clone) CheckoutDetach(ctx context.Context, ref string) error {
	argv := []string{"git", "-C", c.Dir, "checkout", "-q", "--detach", ref}
	_, err := run(ctx, c.Exec, execx.Cmd{Argv: argv, Env: gitread.SafeEnv(c.Env), Timeout: gitread.DefaultTimeout})
	return err
}

// Merge merges commit into the checkout as the factory. A conflict is an
// outcome, not an error: ok is false and out is git's stdout then stderr.
func (c *Clone) Merge(ctx context.Context, commit string) (ok bool, out string, err error) {
	argv := []string{"git", "-C", c.Dir, "-c", "user.name=factory", "-c", "user.email=factory@localhost",
		"merge", "-q", "--no-edit", commit}
	res, err := c.Exec.Run(ctx, execx.Cmd{Argv: argv, Env: gitread.SafeEnv(c.Env), Timeout: gitread.DefaultTimeout})
	if err != nil {
		return false, string(res.Stdout) + string(res.Stderr), fmt.Errorf("git merge: %w", err)
	}
	return res.Code == 0, string(res.Stdout) + string(res.Stderr), nil
}

// DeleteBranch deletes a local bot/* branch. Like Push it refuses any
// other name, so a ticket's branch field can never name the base.
func (w Writer) DeleteBranch(ctx context.Context, branch string) error {
	if !strings.HasPrefix(branch, "bot/") {
		return fmt.Errorf("refusing to delete %s: the factory only deletes bot/* branches", branch)
	}
	_, err := w.git(ctx, 0, "branch", "-D", branch)
	return err
}

// credentialHelper answers git's credential request from the environment,
// so the token is never in argv or a remote URL.
const credentialHelper = `!f() { echo username=x-access-token; echo "password=$FACTORY_GH_TOKEN"; }; f`

// Push is push_branch: a forced push of refs/heads/<branch> to
// https://github.com/<Repo.GitHub>.git (never the agent-writable origin),
// with the configured credential helpers reset and token handed over in
// FACTORY_GH_TOKEN only. Anything but a bot/* branch is refused.
func (w Writer) Push(ctx context.Context, branch, token string) error {
	if !strings.HasPrefix(branch, "bot/") {
		return fmt.Errorf("refusing to push %s: the factory only pushes bot/* branches", branch)
	}
	if w.Repo.GitHub == "" {
		return fmt.Errorf("refusing to push %s: the project has no GitHub repository", branch)
	}
	env := setVars(w.Repo.BaseEnv(), "FACTORY_GH_TOKEN="+token, "GIT_TERMINAL_PROMPT=0")
	ref := "refs/heads/" + branch
	_, err := w.gitEnv(ctx, PushTimeout, gitread.SafeEnv(env),
		"-c", "credential.helper=", "-c", "credential.helper="+credentialHelper,
		"push", "--force", "--quiet", "https://github.com/"+w.Repo.GitHub+".git", ref+":"+ref)
	return err
}

// InitWorkspace is manager_project's bootstrap: when Repo.Path has no .git,
// git init -q -b main (plain environment: a directory the factory owns),
// then README.md written and committed as "workspace" by the factory with
// gpg signing off. An existing workspace is left as it is.
func (w Writer) InitWorkspace(ctx context.Context, readme string) error {
	dir := w.Repo.Path
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
		return nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	argv := []string{"git", "init", "-q", "-b", "main", dir}
	if _, err := run(ctx, w.Repo.Exec, execx.Cmd{Argv: argv, Env: w.Repo.BaseEnv(), Timeout: gitread.DefaultTimeout}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o666); err != nil {
		return err
	}
	if _, err := w.git(ctx, 0, "add", "README.md"); err != nil {
		return err
	}
	_, err := w.git(ctx, 0, "-c", "user.name=factory", "-c", "user.email=factory@localhost", "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "workspace")
	return err
}

// setVars is dict(base, K=V, …): base with each variable replaced or added.
func setVars(base []string, vars ...string) []string {
	out := make([]string, 0, len(base)+len(vars))
	out = append(out, base...)
	for _, kv := range vars {
		k, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, e := range out {
			if ek, _, _ := strings.Cut(e, "="); ek == k {
				out[i], replaced = kv, true
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}
