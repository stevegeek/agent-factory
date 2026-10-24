package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/backup"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/doors"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
)

// The backup group: `factory backup` (the daily timer),
// `factory backup keygen`, `factory backup verify` (the Sunday timer),
// `factory backup restore ARCHIVE --to DIR` and `factory backup adopt DIR`
// (a restored archive becomes a fresh master's state).

// The command loads config.toml itself (no Config: true): keygen, and a
// restore of a local archive, need only the key, so on a rebuilt machine
// with no factory root yet they run on [backup]'s defaults.
func backupCommand() Command {
	return Command{Name: "backup", Help: "back up the board, memory and logs to R2 (keygen, verify, restore, adopt)",
		Positionals: []Positional{
			{Name: "verb", Optional: true, Choices: []string{"keygen", "verify", "restore", "adopt"}},
			{Name: "archive", Optional: true, Help: "restore: a local archive, or its name in the bucket; adopt: the restored DIR"},
		},
		Flags: []Flag{{Name: "to", Metavar: "DIR", Help: "restore: an empty directory to restore into"}},
		Run:   runBackup}
}

// runBackup is `factory backup …`.
func runBackup(ctx context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		local := a.Pos("verb") == "restore" && isFile(a.Pos("archive"))
		if !errors.Is(err, fs.ErrNotExist) || !(a.Pos("verb") == "keygen" || a.Pos("verb") == "adopt" || local) {
			return err
		}
		if cfg, err = config.Parse(nil); err != nil { // no config.toml: [backup]'s defaults
			return err
		}
	}
	svc := backupService(e, cfg)
	switch a.Pos("verb") {
	case "keygen":
		path := cfg.Backup.KeyFile
		if path == "" {
			path = e.Paths().BackupKey()
		}
		key, err := backup.Keygen(path, e.Rand)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "wrote %s (0600). Keep this key somewhere safe: without it no backup can be read.\n%s\n", path, key)
		return nil
	case "verify":
		v, err := svc.Verify(ctx, cfg.Backup)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "%s restores: %d files, %d tickets\n", v.Key, v.Files, v.Tickets)
		return nil
	case "restore":
		if a.Pos("archive") == "" || a.Str("to") == "" {
			return Usage("backup restore needs ARCHIVE and --to DIR")
		}
		n, err := svc.Restore(ctx, cfg.Backup, a.Pos("archive"), a.Str("to"))
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "restored %d files into %s\n", n, a.Str("to"))
		return nil
	case "adopt":
		return runAdopt(ctx, e, svc, a.Pos("archive"))
	}
	if a.Pos("archive") != "" {
		return Usage("backup takes an archive only with restore")
	}
	_, err = svc.Run(ctx, cfg.Backup)
	return err
}

// runAdopt is `factory backup adopt DIR`: it prints what moved, what the
// root kept, what stayed in DIR, and how to take each project's unpushed
// commits from its bundle.
func runAdopt(ctx context.Context, e *Env, svc backup.Service, dir string) error {
	if dir == "" {
		return Usage("backup adopt needs DIR, the directory a restore wrote")
	}
	got, err := svc.Adopt(ctx, dir)
	if len(got.Moved) > 0 {
		fmt.Fprintf(e.Stdout, "moved into %s: %s\n", e.Paths().Root, strings.Join(got.Moved, ", "))
	}
	if err != nil {
		return err
	}
	if len(got.Kept) > 0 {
		fmt.Fprintf(e.Stdout, "kept (already here): %s\n", strings.Join(got.Kept, ", "))
	}
	fmt.Fprintf(e.Stdout, "left in %s: config.toml (deploy renders it), images.json (this host's images are its own)\n", dir)
	for _, p := range got.Repos {
		bundle := filepath.Join(dir, "repos", p, "unpushed.bundle")
		if isFile(bundle) {
			fmt.Fprintf(e.Stdout, "repos/%s: where its clone lacks them, git -C <clone of %s> fetch %s 'refs/heads/*:refs/heads/*'\n", p, p, bundle)
		} else {
			fmt.Fprintf(e.Stdout, "repos/%s: no unpushed commits; its pinned untracked files are in %s\n", p, filepath.Join(dir, "repos", p))
		}
	}
	return nil
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return path != "" && err == nil && fi.Mode().IsRegular()
}

func backupService(e *Env, cfg *config.Config) backup.Service {
	env := environList(e.Environ)
	guard := gitx.NewGuarder(e.Exec, env, e.Log())
	return backup.Service{
		Paths:   e.Paths(),
		Lock:    e.Board(),
		Notices: backupNotices{doors.Service{Board: e.Board(), Log: e.Log()}},
		Clock:   e.Clock,
		Log:     e.Log(),
		Random:  e.Rand,
		Host:    cfg.Backup.Name,
		Repos: repoSaver{
			projects: cfg.FactoryProjects(),
			repo: func(p config.Project) *gitread.Repo {
				return &gitread.Repo{Path: p.Path, Base: p.Base, GitHub: p.GitHub, Exec: e.Exec, Env: env, Guard: guard}
			},
			logf: e.Log().Printf,
		},
	}
}

// backupNotices files backup failures through the doors' deduplicated
// notice.
type backupNotices struct{ d doors.Service }

func (n backupNotices) Notice(ctx context.Context, title, body string) error {
	_, err := n.d.Notice(ctx, title, body)
	return err
}
