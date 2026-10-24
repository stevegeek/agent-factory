package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gitread"
)

// pinned is what a local run mounts of the project's pinned_untracked
// (hostrun.Job.Pinned): each path the checkout has as a file or a
// directory, never a link (the mount would follow it), and that neither the
// base nor the ticket's branch tracks, so the checkout's copy never hides
// what the agent's own branch has there. The gate copies the same paths
// into its checkout; a helper job ships its own.
func pinned(ctx context.Context, repo *gitread.Repo, p *config.Project, branch string) ([]string, error) {
	if len(p.PinnedUntracked) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		return nil, fmt.Errorf("pinned_untracked: %w", err)
	}
	defer root.Close()
	var out []string
	for _, f := range p.PinnedUntracked {
		if f == "" || !filepath.IsLocal(f) {
			return nil, fmt.Errorf("[projects.%s] pinned_untracked %q is not a path inside the project", p.Key, f)
		}
		f = filepath.ToSlash(filepath.Clean(f))
		fi, err := root.Lstat(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("pinned_untracked %s: %w", f, err)
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			continue
		}
		tracked := false
		for _, rev := range []string{p.Base, branch} {
			if rev == "" || tracked {
				continue
			}
			if tracked, err = repo.CatFileExists(ctx, rev, f); err != nil {
				return nil, err
			}
		}
		if !tracked {
			out = append(out, f)
		}
	}
	return out, nil
}
