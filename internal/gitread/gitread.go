// Package gitread is read-only git on project repositories. No method here
// writes a repository, its config, its index or the network: the write verbs
// live in gitx, which Foreman's build may not link.
//
// An agent's container mounts its project's .git read-write, so anything
// git on the host reads from that .git could have been planted (hooks, and
// config that makes git run programs). Every git here therefore runs with
// SafeEnv, whose settings outrank the repository's config, and only after
// the Repo's Guard has checked .git/config against what a plain clone has.
package gitread

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// DefaultTimeout bounds a git call that has no timeout of its own (Python
// had none; the context's deadline normally ends it first).
const DefaultTimeout = 10 * time.Minute

// SafeConfig is SAFE_GIT_CONFIG: hooks to /dev/null, fsmonitor off and the
// ext:: transport refused, given to every host git through the environment.
func SafeConfig() [][2]string {
	return [][2]string{{"core.hooksPath", "/dev/null"}, {"core.fsmonitor", "false"}, {"protocol.ext.allow", "never"}}
}

// SafeEnv is safe_git_env(base): base (not modified) with no system config
// and SafeConfig as GIT_CONFIG_COUNT/KEY_i/VALUE_i. Variables it sets are
// removed from base first, then appended in Python's order.
func SafeEnv(base []string) []string {
	cfg := SafeConfig()
	set := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=" + strconv.Itoa(len(cfg))}
	for i, kv := range cfg {
		set = append(set, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	names := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		names[k] = true
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !names[k] {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// Guard checks a repository before git runs in it. gitx.Guarder cleans the
// config (the dispatcher's guard_repo); Verifier only refuses (Foreman).
type Guard interface {
	Check(ctx context.Context, path, github string) error
}

// Repo is a project's clone on this host.
type Repo struct {
	Path   string // the work tree
	Base   string // the project's base branch
	GitHub string // owner/repo when the project has one: origin must point there
	Exec   execx.Execer
	// Env is the base environment of every git (SafeEnv is applied to
	// it); nil means this process's environment, as Python's
	// safe_git_env() did.
	Env []string
	// Guard runs before every git; nil means Verifier with Exec and Env.
	Guard Guard
}

// BaseEnv is Env, or this process's environment when Env is nil.
func (r *Repo) BaseEnv() []string {
	if r.Env != nil {
		return r.Env
	}
	return os.Environ()
}

// Checked runs the guard: nil when git may run in the repository.
func (r *Repo) Checked(ctx context.Context) error {
	g := r.Guard
	if g == nil {
		g = Verifier{Exec: r.Exec, Env: r.Env}
	}
	return g.Check(ctx, r.Path, r.GitHub)
}

// run is the guard, then git -C <path> args with SafeEnv. It returns the
// result whatever the exit code; err is for a guard refusal or a command
// that could not run.
func (r *Repo) run(ctx context.Context, args ...string) (execx.Result, error) {
	if err := r.Checked(ctx); err != nil {
		return execx.Result{}, err
	}
	argv := append([]string{"git", "-C", r.Path}, args...)
	res, err := r.Exec.Run(ctx, execx.Cmd{Argv: argv, Env: SafeEnv(r.BaseEnv()), Timeout: DefaultTimeout})
	if err != nil {
		return res, fmt.Errorf("git %s: %w", args[0], err)
	}
	return res, nil
}

// git is Python's git(project, *args): checked, with the stdout stripped.
func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	res, err := r.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if err := execx.CheckErr(res); err != nil {
		return "", err
	}
	return textx.Strip(string(res.Stdout)), nil
}

// BranchExists is branch_exists: rev-parse --verify --quiet refs/heads/<b>.
func (r *Repo) BranchExists(ctx context.Context, branch string) (bool, error) {
	res, err := r.run(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return res.Code == 0, nil
}

// Head is branch_head: the commit refs/heads/<branch> points at.
func (r *Repo) Head(ctx context.Context, branch string) (string, error) {
	return r.git(ctx, "rev-parse", "refs/heads/"+branch)
}

// CurrentBranch is the checked-out branch ("" when detached).
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	return r.git(ctx, "branch", "--show-current")
}

// CommitsAhead is commits_ahead: rev-list --count <base>..<branch>.
func (r *Repo) CommitsAhead(ctx context.Context, branch string) (int, error) {
	out, err := r.git(ctx, "rev-list", "--count", r.Base+".."+branch)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("rev-list --count: %w", err)
	}
	return n, nil
}

// ChangedFiles is changed_files: diff --name-only <base>...<branch>.
func (r *Repo) ChangedFiles(ctx context.Context, branch string) ([]string, error) {
	out, err := r.git(ctx, "diff", "--name-only", r.Base+"..."+branch)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// TouchedFiles are the paths the branch touches, a rename at both ends:
// diff --name-only --no-renames <base>...<branch>. The walls read this, so
// moving a wall file away is a change to the walls.
func (r *Repo) TouchedFiles(ctx context.Context, branch string) ([]string, error) {
	out, err := r.git(ctx, "diff", "--name-only", "--no-renames", r.Base+"..."+branch)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// KeptFiles is what the branch publishes as paths: the changed files
// without the deleted ones (those are public already), of the whole diff
// (diff --name-only --diff-filter=d <base>...<branch>) and then of each
// commit (git log --name-only --no-renames --diff-filter=d <base>..<branch>),
// each once. The push publishes every commit, so a path one commit adds
// and a later one removes is kept.
func (r *Repo) KeptFiles(ctx context.Context, branch string) ([]string, error) {
	whole, err := r.git(ctx, "diff", "--name-only", "-z", "--diff-filter=d", r.Base+"..."+branch)
	if err != nil {
		return nil, err
	}
	each, err := r.git(ctx, "log", "--name-only", "-z", "--no-renames", "--diff-filter=d", "--format=", r.Base+".."+branch)
	if err != nil {
		return nil, err
	}
	return unique(append(strings.Split(whole, "\x00"), strings.Split(each, "\x00")...)), nil
}

// Messages are the commit messages from the base to the branch (git log
// --format=%B%x00 <base>..<branch>), newest first, each trimmed.
func (r *Repo) Messages(ctx context.Context, branch string) ([]string, error) {
	out, err := r.git(ctx, "log", "--format=%B%x00", r.Base+".."+branch)
	if err != nil {
		return nil, err
	}
	var msgs []string
	for _, m := range strings.Split(out, "\x00") {
		if m = textx.Strip(m); m != "" {
			msgs = append(msgs, m)
		}
	}
	return msgs, nil
}

// NumstatEntry is one line of diff --numstat. A binary file has Binary set
// and counts zero lines.
type NumstatEntry struct {
	Added, Deleted int
	Binary         bool
	Path           string
}

// Numstat is diff --numstat <base>...<branch>.
func (r *Repo) Numstat(ctx context.Context, branch string) ([]NumstatEntry, error) {
	out, err := r.git(ctx, "diff", "--numstat", r.Base+"..."+branch)
	if err != nil {
		return nil, err
	}
	var entries []NumstatEntry
	for _, line := range textx.SplitLines(out) {
		a, rest, ok1 := strings.Cut(line, "\t")
		d, path, ok2 := strings.Cut(rest, "\t")
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("diff --numstat: unexpected line %q", line)
		}
		e := NumstatEntry{Path: path, Binary: !textx.IsDigits(a) || !textx.IsDigits(d)}
		e.Added, e.Deleted = digits(a), digits(d)
		entries = append(entries, e)
	}
	return entries, nil
}

// DiffSize is diff_size: lines added plus lines deleted, binary files 0.
func (r *Repo) DiffSize(ctx context.Context, branch string) (int, error) {
	entries, err := r.Numstat(ctx, branch)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, e := range entries {
		total += e.Added + e.Deleted
	}
	return total, nil
}

// AddedLines is what classify() reads: the added lines of
// diff -U0 <base>...<branch>, without their "+".
func (r *Repo) AddedLines(ctx context.Context, branch string) ([]string, error) {
	out, err := r.git(ctx, "diff", "-U0", r.Base+"..."+branch)
	if err != nil {
		return nil, err
	}
	return addedLines(textx.SplitLines(out)), nil
}

// textDiff makes git print every file's lines, whatever its attributes or
// bytes say (a file git calls binary prints none), with no program of the
// repository's.
func textDiff() []string { return []string{"--text", "--no-textconv", "--no-ext-diff"} }

// PublishedLines are the added lines the branch publishes, without their
// "+": those of the whole diff (diff -U0 <base>...<branch>) and then of
// each commit (git log -p -U0 --diff-filter=d <base>..<branch>), each once,
// all read as text and split at "\n" only (Python's splitlines would cut
// "+ok\rhostx" in two and drop the second half). The push publishes every
// commit, so a line one commit adds and a later one removes is there.
func (r *Repo) PublishedLines(ctx context.Context, branch string) ([]string, error) {
	whole, err := r.git(ctx, append(append([]string{"diff", "-U0"}, textDiff()...), r.Base+"..."+branch)...)
	if err != nil {
		return nil, err
	}
	each, err := r.git(ctx, append(append([]string{"log", "-p", "-U0"}, textDiff()...), "--diff-filter=d", "--format=", r.Base+".."+branch)...)
	if err != nil {
		return nil, err
	}
	return unique(append(addedLines(strings.Split(whole, "\n")), addedLines(strings.Split(each, "\n"))...)), nil
}

// addedLines are the "+" lines of a patch's hunks, without the "+". A
// file's header runs from its "diff " line to its first "@@", so "+++ b/x"
// is skipped there and an added "++i;" is kept.
func addedLines(patch []string) []string {
	var added []string
	header := false
	for _, l := range patch {
		switch {
		case strings.HasPrefix(l, "diff "):
			header = true
		case strings.HasPrefix(l, "@@"):
			header = false
		case !header && strings.HasPrefix(l, "+"):
			added = append(added, l[1:])
		}
	}
	return added
}

// CatFileExists is cat-file -e <rev>:<file>: whether the file is in that
// commit (review_prompt reads the base's REVIEW.md, never the branch's).
// Python skipped the guard here; the port guards it like every other git.
func (r *Repo) CatFileExists(ctx context.Context, rev, file string) (bool, error) {
	res, err := r.run(ctx, "cat-file", "-e", rev+":"+file)
	if err != nil {
		return false, err
	}
	return res.Code == 0, nil
}

// MinVersion is the oldest git the factory supports (merge-tree
// --write-tree needs 2.38).
const MinVersion = "2.48"

// CheckVersion runs git --version and refuses a git older than MinVersion.
// It returns the version string. Callers check once per process.
func CheckVersion(ctx context.Context, ex execx.Execer, env []string) (string, error) {
	res, err := ex.Run(ctx, execx.Cmd{Argv: []string{"git", "--version"}, Env: env, Timeout: time.Minute})
	if err != nil {
		return "", fmt.Errorf("git --version: %w", err)
	}
	if err := execx.CheckErr(res); err != nil {
		return "", err
	}
	out := textx.Strip(string(res.Stdout))
	v, ok := strings.CutPrefix(out, "git version ")
	if !ok {
		return "", fmt.Errorf("git --version said %q: the factory needs git %s or newer", out, MinVersion)
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("git version %s: the factory needs git %s or newer", v, MinVersion)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || major < 2 || major == 2 && minor < 48 {
		return "", fmt.Errorf("git version %s: the factory needs git %s or newer", v, MinVersion)
	}
	return v, nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range textx.SplitLines(s) {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// unique is the non-empty strings of ss, each once, in their first order.
func unique(ss []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// digits is int(s) if s.isdigit() else 0, for numstat's ASCII counts.
func digits(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
