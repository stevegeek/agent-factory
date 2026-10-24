package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/paths"
)

// item is one thing the archive holds: a file or a tree on this host, and
// its name in the archive.
type item struct {
	src, name string
	tree      bool
}

// neverNames are files no archive holds, wherever they are found (§15.2):
// they are re-creatable, or the maintainer holds them.
func neverNames() []string {
	return []string{"claude-oauth-token", "github-app.pem", "anthropic-api-key", "foreman-secret",
		"backup-key", "r2-credentials", "id_ed25519", "id_rsa", "factory-remote"}
}

// excluded reports whether a file (by its name in the archive) is left out.
// The push key is in only when pushKey; Foreman's sessions and its wake
// trigger never are; lock and temporary files never are.
func excluded(name string, pushKey bool) bool {
	base := path.Base(name)
	for _, n := range neverNames() {
		if base == n {
			return true
		}
	}
	if base == "foreman-vapid.pem" {
		return !pushKey
	}
	if strings.HasPrefix(name, "foreman-state/") && (strings.Contains(base, "session") || base == "wake") {
		return true
	}
	if strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
		return true // no other private key, under any name
	}
	return strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".tmp") || base == ".tick.again" ||
		strings.Contains(name, "/.locks/")
}

// lockedItems are copied while the tick lock is held, so no ticket or
// memory store is torn mid-save.
func lockedItems(p paths.Paths) []item {
	return []item{{p.Board, "board", true}, {p.Memory, "memory", true}}
}

// otherItems are copied after the lock is let go.
func otherItems(p paths.Paths) []item {
	return []item{
		{p.Context, "context", true},
		{p.Config, "config.toml", false},
		{p.Tokens, "api-tokens.json", false},
		{p.ForemanState(), "foreman-state", true},
		{p.State, "state.json", false},
		{p.Pauses, "pauses.json", false},
		{p.Watches, "watches.json", false},
		{p.ImagesState(), "images.json", false},
		{p.Logs, "logs", true},
	}
}

// stage copies items into dir (missing ones are skipped), regular files
// and directories only: a symlink is never followed or kept.
func stage(ctx context.Context, items []item, dir string, pushKey bool) error {
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		fi, err := os.Lstat(it.src)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		if !it.tree {
			if fi.Mode().IsRegular() && !excluded(it.name, pushKey) {
				if err := copyFile(it.src, filepath.Join(dir, it.name)); err != nil {
					return err
				}
			}
			continue
		}
		if !fi.IsDir() {
			continue
		}
		err = filepath.WalkDir(it.src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(it.src, p)
			if err != nil {
				return err
			}
			name := path.Join(it.name, filepath.ToSlash(rel))
			dst := filepath.Join(dir, filepath.FromSlash(name))
			switch {
			case d.IsDir():
				return os.MkdirAll(dst, 0o700)
			case d.Type().IsRegular() && !excluded(name, pushKey):
				return copyFile(p, dst)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("backup: staging %s: %w", it.name, err)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed meanwhile (a rotated log, a finished run)
		}
		return fmt.Errorf("backup: %w", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("backup: %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// writeTar writes dir as a gzipped tar into w: names relative to dir,
// sorted, regular files and directories only, with fixed owners.
func writeTar(ctx context.Context, w io.Writer, dir string, mtime time.Time) error {
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && (d.IsDir() || d.Type().IsRegular()) {
			rel, _ := filepath.Rel(dir, p)
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	sort.Strings(names)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(n))
		fi, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		h := &tar.Header{Name: n, ModTime: fi.ModTime().UTC().Truncate(time.Second), Format: tar.FormatPAX}
		if fi.IsDir() {
			h.Typeflag, h.Name, h.Mode = tar.TypeDir, n+"/", 0o700
			h.ModTime = mtime
		} else {
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, 0o600, fi.Size()
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		if fi.IsDir() {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		_, err = io.CopyN(tw, f, fi.Size())
		f.Close()
		if err != nil {
			return fmt.Errorf("backup: %s: %w", n, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// extract unpacks a gzipped tar into dir through os.Root: no member can
// leave dir, and only regular files and directories are made.
func extract(ctx context.Context, r io.Reader, dir string) (files int, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	defer root.Close()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return files, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, fmt.Errorf("restore: %w", err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "" || path.IsAbs(name) || name != path.Clean(name) || strings.HasPrefix(name, "../") || name == ".." {
			return files, fmt.Errorf("restore: refusing member %q", h.Name)
		}
		local := filepath.FromSlash(name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(local, 0o700); err != nil {
				return files, fmt.Errorf("restore: %w", err)
			}
		case tar.TypeReg:
			if d := filepath.Dir(local); d != "." {
				if err := root.MkdirAll(d, 0o700); err != nil {
					return files, fmt.Errorf("restore: %w", err)
				}
			}
			f, err := root.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return files, fmt.Errorf("restore: %w", err)
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return files, fmt.Errorf("restore: %s: %w", name, err)
			}
			files++
		default:
			return files, fmt.Errorf("restore: refusing member %q (not a file or directory)", h.Name)
		}
	}
	// the gzip stream must end with the tar: nothing trailing
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return files, fmt.Errorf("restore: %w", err)
	}
	return files, nil
}
