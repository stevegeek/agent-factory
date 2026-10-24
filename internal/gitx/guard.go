// Package gitx is git that writes: the guard that cleans a project's
// .git/config, the dispatcher's verbs on project clones (base refresh,
// bundles, the gate's clone, push), and Bare, git on the memory stores.
// Foreman's build may not link it; its read-only counterpart is gitread.
package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gitread"
)

// Logger is where the guard reports what it removed (logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Guarder is guard_repo: it strips a project's .git/config back to what a
// plain clone has before git runs there, because an agent's container can
// write that file. It implements gitread.Guard. One Guarder is shared by
// everything in a process, so that its (ino, mtime_ns, size) cache spares
// an unchanged config a second listing, as Python's _repo_checked did.
type Guarder struct {
	Exec execx.Execer
	Env  []string // the base environment; nil is this process's
	Log  Logger   // nil logs nothing

	mu   sync.Mutex
	seen map[string]gitread.StatKey
}

// NewGuarder makes a Guarder with an empty cache.
func NewGuarder(ex execx.Execer, env []string, log Logger) *Guarder {
	return &Guarder{Exec: ex, Env: env, Log: log}
}

// Check implements gitread.Guard.
func (g *Guarder) Check(ctx context.Context, path, github string) error {
	_, err := g.Guard(ctx, path, github)
	return err
}

// Guard cleans <path>/.git/config and returns what it removed (nil when
// the config was clean, unchanged since the last check, or absent). A .git
// that is not a directory is refused. A config that is a
// symlink or not a regular file is replaced, never followed. github is the
// project's owner/repo, or "": origin must then point at
// https://github.com/<github>.git or it is dropped.
func (g *Guarder) Guard(ctx context.Context, path, github string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, err := gitread.StatConfig(path)
	if err != nil || !st.Present {
		return nil, err
	}
	if key, ok := g.seen[st.Path]; ok && key == st.Key && !st.Link {
		return nil, nil
	}
	in, err := gitread.Inspect(ctx, g.Exec, g.env(), path, github)
	if err != nil {
		return nil, err
	}
	key := st.Key
	if len(in.Removed) > 0 {
		if err := g.rewrite(ctx, st.Path, in.Kept); err != nil {
			return in.Removed, fmt.Errorf("guard %s: %w", path, err)
		}
		if g.Log != nil {
			g.Log.Printf("%s: removed from .git/config (an agent can write it): %s", path, strings.Join(firstN(in.Removed, 8), ", "))
		}
		after, err := gitread.StatConfig(path)
		if err != nil {
			return in.Removed, err
		}
		key = after.Key
	}
	if g.seen == nil {
		g.seen = map[string]gitread.StatKey{}
	}
	g.seen[st.Path] = key
	return in.Removed, nil
}

func (g *Guarder) env() []string {
	if g.Env != nil {
		return g.Env
	}
	return os.Environ()
}

// rewrite builds a new config from kept (or a bare
// core.repositoryformatversion=0) through git config --add on a temp file
// beside it, then renames it over the old one.
func (g *Guarder) rewrite(ctx context.Context, cfg string, kept []gitread.Entry) error {
	tmp := cfg + ".factory-new"
	if err := removeAny(tmp); err != nil {
		return err
	}
	// O_EXCL|O_NOFOLLOW: a planted name is never written through
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if len(kept) == 0 {
		kept = []gitread.Entry{{Key: "core.repositoryformatversion", Value: "0"}}
	}
	env := gitread.SafeEnv(g.env())
	for _, e := range kept {
		argv := []string{"git", "config", "--file", tmp, "--add", e.Key, e.Value}
		res, err := g.Exec.Run(ctx, execx.Cmd{Argv: argv, Dir: "/", Env: env, Timeout: gitread.DefaultTimeout})
		if err == nil {
			err = execx.CheckErr(res)
		}
		if err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := removeAny(cfg); err != nil {
		return err
	}
	return os.Rename(tmp, cfg)
}

// removeAny removes a file, a link or a whole directory; a missing path
// is not an error.
func removeAny(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.IsDir():
		return fsx.RemoveTree(path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func firstN(xs []string, n int) []string {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}
