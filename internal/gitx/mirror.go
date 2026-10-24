package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// DefaultMirrorBase is where a Mirror fetches from: GitHub over https.
const DefaultMirrorBase = "https://github.com/"

// mirrorRepo is the owner/repo a Mirror fetches: no leading dot or dash
// in either part, so it can be neither a path nor an option.
var mirrorRepo = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*\z`) // re: go

// mirrorObject is a commit as the deploy names one: hex, abbreviated or
// whole.
var mirrorObject = regexp.MustCompile(`^[0-9a-f]{7,64}\z`) // re: go

// Mirror is a bare mirror of one repo's main that the self-deploy reads
// commits, files and trees from (deploys/core.git, deploys/config.git).
// Only refs/heads/main is ever fetched into it. Every git runs with
// gitread.SafeEnv and no terminal prompt.
type Mirror struct {
	Dir string
	// Base is the URL prefix the repo is fetched from (Base + owner/repo
	// + ".git"); "" is DefaultMirrorBase. Tests point it at file://.
	Base string
	Exec execx.Execer
	Env  []string // the base environment; nil is this process's
}

func (m Mirror) base() string {
	if m.Base == "" {
		return DefaultMirrorBase
	}
	return m.Base
}

func (m Mirror) env(extra ...string) []string {
	base := m.Env
	if base == nil {
		base = os.Environ()
	}
	var out []string
	for _, kv := range base {
		if !strings.HasPrefix(kv, "FACTORY_GH_TOKEN=") {
			out = append(out, kv)
		}
	}
	return gitread.SafeEnv(setVars(out, append([]string{"GIT_TERMINAL_PROMPT=0"}, extra...)...))
}

// git runs git --git-dir=<Dir> args with the mirror's environment.
func (m Mirror) git(ctx context.Context, c execx.Cmd, args ...string) (execx.Result, error) {
	c.Argv = append([]string{"git", "--git-dir=" + m.Dir}, args...)
	c.Dir = m.Dir
	if c.Env == nil {
		c.Env = m.env()
	}
	if c.Timeout == 0 {
		c.Timeout = gitread.DefaultTimeout
	}
	res, err := m.Exec.Run(ctx, c)
	if err != nil {
		return res, fmt.Errorf("git %s: %w", args[0], err)
	}
	return res, nil
}

// out is git's stripped stdout, or its failure as an error.
func (m Mirror) out(ctx context.Context, args ...string) (string, error) {
	res, err := m.git(ctx, execx.Cmd{}, args...)
	if err != nil {
		return "", err
	}
	if err := execx.CheckErr(res); err != nil {
		return "", err
	}
	return textx.Strip(string(res.Stdout)), nil
}

// Fetch brings refs/heads/main up to date with repo's main (owner/repo),
// creating the bare mirror first when Dir is missing. With a token, git's
// configured credential helpers are reset and the token goes to git only
// through FACTORY_GH_TOKEN in the environment, never argv; with none,
// no helper is set (core is public).
func (m Mirror) Fetch(ctx context.Context, repo, token string) error {
	if !mirrorRepo.MatchString(repo) {
		return fmt.Errorf("mirror: %q is not owner/repo", repo)
	}
	if _, err := os.Stat(m.Dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(m.Dir), 0o700); err != nil {
			return fmt.Errorf("mirror: %w", err)
		}
		argv := []string{"git", "init", "--bare", "-q", "-b", "main", m.Dir}
		if _, err := run(ctx, m.Exec, execx.Cmd{Argv: argv, Env: m.env(), Timeout: gitread.DefaultTimeout}); err != nil {
			return fmt.Errorf("mirror: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	var args []string
	env := m.env()
	if token != "" {
		args = []string{"-c", "credential.helper=", "-c", "credential.helper=" + credentialHelper}
		env = m.env("FACTORY_GH_TOKEN=" + token)
	}
	args = append(args, "fetch", "--quiet", "--prune", m.base()+repo+".git", "+refs/heads/main:refs/heads/main")
	res, err := m.git(ctx, execx.Cmd{Env: env, Timeout: FetchTimeout}, args...)
	if err == nil {
		err = execx.CheckErr(res)
	}
	if err != nil {
		return fmt.Errorf("mirror fetch %s: %w", repo, err)
	}
	return nil
}

// Resolve is the full sha of rev^{commit}: main, or a commit the mirror
// has.
func (m Mirror) Resolve(ctx context.Context, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return "", fmt.Errorf("mirror: %q is not a revision", rev)
	}
	sha, err := m.out(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil || sha == "" {
		return "", fmt.Errorf("mirror: %s is not a commit here", rev)
	}
	return sha, nil
}

// OnMain reports whether the commit sha is main or one of its ancestors.
func (m Mirror) OnMain(ctx context.Context, sha string) (bool, error) {
	if !mirrorObject.MatchString(sha) {
		return false, fmt.Errorf("mirror: %q is not a commit", sha)
	}
	res, err := m.git(ctx, execx.Cmd{}, "merge-base", "--is-ancestor", sha, "refs/heads/main")
	if err != nil {
		return false, err
	}
	switch res.Code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, execx.CheckErr(res)
}

// Has reports whether the mirror holds the commit sha.
func (m Mirror) Has(ctx context.Context, sha string) bool {
	if !mirrorObject.MatchString(sha) {
		return false
	}
	res, err := m.git(ctx, execx.Cmd{}, "cat-file", "-e", sha+"^{commit}")
	return err == nil && res.Code == 0
}

// Show is the file at path in the commit sha, refused past max bytes.
func (m Mirror) Show(ctx context.Context, sha, path string, max int64) ([]byte, error) {
	return m.capped(ctx, sha, path, max, "cat-file", "blob", sha+":"+path)
}

// ArchiveTree is a tar of the directory dir in the commit sha, its
// members relative to dir: git archive of a tree has no pax global
// header, which fsx.Extract refuses. It is refused past max bytes.
func (m Mirror) ArchiveTree(ctx context.Context, sha, dir string, max int64) ([]byte, error) {
	return m.capped(ctx, sha, dir, max, "archive", "--format=tar", sha+":"+dir)
}

func (m Mirror) capped(ctx context.Context, sha, path string, max int64, args ...string) ([]byte, error) {
	if !mirrorObject.MatchString(sha) {
		return nil, fmt.Errorf("mirror: %q is not a commit", sha)
	}
	if path == "" || strings.HasPrefix(path, "-") {
		return nil, fmt.Errorf("mirror: %q is not a path", path)
	}
	res, err := m.git(ctx, execx.Cmd{MaxOut: int(max) + 1}, args...)
	if err != nil {
		return nil, err
	}
	if err := execx.CheckErr(res); err != nil {
		return nil, fmt.Errorf("mirror %s %s: %w", args[0], path, err)
	}
	if res.Truncated || int64(len(res.Stdout)) > max {
		return nil, fmt.Errorf("mirror %s %s: larger than %d bytes", args[0], path, max)
	}
	return res.Stdout, nil
}

// Changed lists the files that differ between the commits from and to,
// limited to paths (git pathspecs: a directory, or a glob such as
// systemd/*.timer).
func (m Mirror) Changed(ctx context.Context, from, to string, paths ...string) ([]string, error) {
	for _, sha := range []string{from, to} {
		if !mirrorObject.MatchString(sha) {
			return nil, fmt.Errorf("mirror: %q is not a commit", sha)
		}
	}
	args := append([]string{"diff", "--name-only", "--no-renames", "-z", from, to, "--"}, paths...)
	res, err := m.git(ctx, execx.Cmd{}, args...)
	if err != nil {
		return nil, err
	}
	if err := execx.CheckErr(res); err != nil {
		return nil, fmt.Errorf("mirror diff: %w", err)
	}
	var out []string
	for _, name := range strings.Split(string(res.Stdout), "\x00") {
		if name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}
