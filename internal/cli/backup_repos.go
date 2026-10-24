package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
)

// maxPinnedBackup caps one pinned file in the backup (a lockfile is a few
// KiB; anything this big is skipped).
const maxPinnedBackup = 16 << 20

// repoSaver is the backup's repos/ (backup.RepoSaver): for each factory
// project, repos/<project>/unpushed.bundle with the bot/ branches' commits
// no remote has, and repos/<project>/untracked/<f> for each pinned
// untracked regular file. A project that cannot be saved is logged and
// passed over, so one broken clone does not cost the day's backup.
type repoSaver struct {
	projects []config.Project
	repo     func(p config.Project) *gitread.Repo
	logf     func(format string, args ...any)
}

func (r repoSaver) Save(ctx context.Context, dir string) error {
	for _, p := range r.projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.Path == "" || !filepath.IsLocal(p.Key) {
			continue
		}
		if st, err := os.Stat(p.Path); err != nil || !st.IsDir() {
			continue
		}
		if err := r.save(ctx, p, filepath.Join(dir, p.Key)); err != nil {
			r.logf("backup: repos: %s passed over: %v", p.Key, err)
		}
	}
	return nil
}

func (r repoSaver) save(ctx context.Context, p config.Project, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := (gitx.Writer{Repo: r.repo(p)}).BundleUnpushed(ctx, filepath.Join(dir, "unpushed.bundle")); err != nil {
		return err
	}
	for _, f := range p.PinnedUntracked {
		if f == "" || !filepath.IsLocal(f) {
			continue
		}
		data, err := fsx.ReadNoFollow(filepath.Join(p.Path, f), maxPinnedBackup)
		if errors.Is(err, fsx.ErrNotRegular) || errors.Is(err, os.ErrNotExist) || errors.Is(err, fsx.ErrTooLarge) {
			continue // a shard's lib/ comes back with shards install
		}
		if err != nil {
			return err
		}
		out := filepath.Join(dir, "untracked", f)
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(out, data, 0o600); err != nil {
			return err
		}
	}
	// a project with nothing to keep leaves no folder
	os.Remove(dir)
	return nil
}
