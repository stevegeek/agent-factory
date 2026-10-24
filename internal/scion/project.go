package scion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// gitTimeout bounds the two git calls of EnsureProject (Python's git() has
// none; a local status or checkout takes milliseconds).
const gitTimeout = 120 * time.Second

// EnsureProject gives the checkout at dir its own Scion project
// (ensure_scion_project). Scion runs an agent in the project whose
// .scion it finds; a checkout without one falls back to the global project,
// which mounts the checkout itself instead of a worktree.
//
// When .scion/project-id is missing it runs `scion init`, fails if there is
// still no marker, undoes init's change to .gitignore (deleting the file if
// scion made it, else `git checkout -- .gitignore`) so base fast-forwards
// stay clean, and adds ".scion/" to .git/info/exclude once.
//
// Unlike Python, the exclude file is opened inside .git with os.Root: an
// agent can write .git, and a planted symlink must not make the factory
// append to a file outside it.
func (c Client) EnsureProject(ctx context.Context, dir string) error {
	marker := filepath.Join(dir, ".scion", "project-id")
	if exists(marker) {
		return nil
	}
	if err := c.Init(ctx, dir); err != nil {
		return err
	}
	if !exists(marker) {
		return fmt.Errorf("%w in %s", ErrNoProject, dir)
	}
	status, err := c.git(ctx, dir, "status", "--porcelain", "--", ".gitignore")
	if err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(status, "??"): // scion made it; the repository had none
		if err := os.Remove(filepath.Join(dir, ".gitignore")); err != nil {
			return fmt.Errorf("scion project: %w", err)
		}
	case status != "":
		if _, err := c.git(ctx, dir, "checkout", "--", ".gitignore"); err != nil {
			return err
		}
	}
	if err := addExclude(dir, ".scion/"); err != nil {
		return fmt.Errorf("scion project: %w", err)
	}
	if c.Logf != nil {
		c.Logf("scion project initialised in %s", dir)
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// git is Python git(project, *args): guard, then `git -C dir args`, checked,
// in the base (safe git) environment, without the token; stdout stripped.
func (c Client) git(ctx context.Context, dir string, args ...string) (string, error) {
	if err := c.guard(ctx, dir); err != nil {
		return "", err
	}
	argv := append([]string{"git", "-C", dir}, args...)
	res, err := c.Exec.Run(ctx, execx.Cmd{Argv: argv, Env: c.Reader.Env, Timeout: gitTimeout})
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	if err := execx.CheckErr(res); err != nil {
		return "", err
	}
	return textx.Strip(res.Out()), nil
}

// ErrNotDir is a .git that is not a plain directory.
var ErrNotDir = errors.New(".git is not a directory")

// addExclude appends line to .git/info/exclude unless a line of the file is
// already exactly it, without following any link out of .git.
func addExclude(dir, line string) error {
	gitdir := filepath.Join(dir, ".git")
	st, err := os.Lstat(gitdir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s: %w", gitdir, ErrNotDir)
	}
	root, err := os.OpenRoot(gitdir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll("info", 0o755); err != nil {
		return err
	}
	if fi, err := root.Lstat("info/exclude"); err == nil && !fi.Mode().IsRegular() {
		return &fs.PathError{Op: "open", Path: filepath.Join(gitdir, "info", "exclude"), Err: errors.New("not a regular file")}
	}
	f, err := root.OpenFile("info/exclude", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if slices.Contains(textx.SplitLines(string(b)), line) {
		return nil
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return f.Close()
}
