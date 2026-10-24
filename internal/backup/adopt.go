package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
)

// Adopted is what Adopt did: the archive names it moved into the root, the
// foreman-state files the root already had and kept, and the projects
// whose repos/ stayed in the restored directory.
type Adopted struct{ Moved, Kept, Repos []string }

// adoptItem is one thing adopt moves: its name in the restored directory
// and its place in the root.
type adoptItem struct{ name, dst string }

// adoptItems are what adopt moves whole. config.toml (deploy renders it)
// and images.json (this host's images) are not among them, nor is
// foreman-state/, which moves file by file.
func adoptItems(p paths.Paths) []adoptItem {
	return []adoptItem{
		{"board", p.Board}, {"memory", p.Memory}, {"context", p.Context}, {"logs", p.Logs},
		{"state.json", p.State}, {"pauses.json", p.Pauses}, {"watches.json", p.Watches},
		{"api-tokens.json", p.Tokens}, {"usage.jsonl", p.UsageLog()},
	}
}

// Adopt is `factory backup adopt DIR`: it moves a restored archive's
// state into this root, which must be a fresh master's: no board, no tick
// running, and every target missing or an empty directory (all checked
// before anything moves). It moves only by rename, so DIR must be on the
// root's filesystem. The root's foreman-state/ may already hold files
// (the login secret, copied by hand); they are kept, but one the archive
// also has refuses the adopt: it is Foreman's own from a start here (a
// push key it made would unbind every device). config.toml, images.json
// and repos/ stay in DIR.
func (s Service) Adopt(ctx context.Context, dir string) (Adopted, error) {
	var out Adopted
	if err := s.refuseAdopt(dir); err != nil {
		return out, err
	}
	if _, err := checkRestored(dir); err != nil {
		return out, fmt.Errorf("adopt: %s is not a restored archive: %w", dir, err)
	}
	if has, err := hasTickets(s.Paths.Tickets); err != nil || has {
		if err != nil {
			return out, fmt.Errorf("adopt: %w", err)
		}
		return out, errors.New("adopt: this root already has a board: adopt only into a fresh master")
	}
	l, err := s.Lock.TickLock(ctx, fsx.NoWait)
	if errors.Is(err, fsx.ErrBusy) {
		return out, errors.New("adopt: a tick is running here; stop the timers and try again")
	}
	if err != nil {
		return out, fmt.Errorf("adopt: tick lock: %w", err)
	}
	defer l.Unlock()

	srcDev, err := device(dir)
	if err != nil {
		return out, fmt.Errorf("adopt: %w", err)
	}
	var moves []adoptItem
	for _, it := range adoptItems(s.Paths) {
		if _, err := os.Lstat(filepath.Join(dir, it.name)); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return out, fmt.Errorf("adopt: %w", err)
		}
		if err := freeTarget(it.dst); err != nil {
			return out, err
		}
		moves = append(moves, it)
	}
	state := filepath.Join(dir, "foreman-state")
	var stateFiles []string
	if entries, err := os.ReadDir(state); err == nil {
		for _, e := range entries {
			if !e.Type().IsRegular() {
				return out, fmt.Errorf("adopt: %s is not a regular file; a restore writes only files there", filepath.Join(state, e.Name()))
			}
			stateFiles = append(stateFiles, e.Name())
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return out, fmt.Errorf("adopt: %w", err)
	}
	fstate := s.Paths.ForemanState()
	var kept []string
	if have, err := os.ReadDir(fstate); err == nil {
		for _, e := range have {
			if e.Name() != foremanSecret && slices.Contains(stateFiles, e.Name()) {
				return out, fmt.Errorf("adopt: %s is here and in %s: Foreman has run in this root (its own push key would unbind every device); move the root's foreman-state/ files aside, all but %s, and try again", filepath.Join(fstate, e.Name()), state, foremanSecret)
			}
			kept = append(kept, "foreman-state/"+e.Name())
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return out, fmt.Errorf("adopt: %w", err)
	}
	targets := []string{fstate}
	for _, it := range moves {
		targets = append(targets, it.dst)
	}
	for _, t := range targets {
		d, err := device(nearest(t))
		if err != nil {
			return out, fmt.Errorf("adopt: %w", err)
		}
		if d != srcDev {
			return out, fmt.Errorf("adopt: %s and %s are on different filesystems (adopt moves by rename): restore under the factory user's home", dir, t)
		}
	}

	defer func() {
		if len(out.Moved)+len(out.Kept) > 0 {
			s.logf("backup adopt: from %s moved %s; kept %s", dir, strings.Join(out.Moved, ", "), strings.Join(out.Kept, ", "))
		}
	}()
	for _, it := range moves {
		if err := ctx.Err(); err != nil {
			return out, halfDone(err, dir, out.Moved)
		}
		if err := removeEmptyTree(it.dst); err != nil {
			return out, halfDone(err, dir, out.Moved)
		}
		if err := os.MkdirAll(filepath.Dir(it.dst), 0o700); err != nil {
			return out, halfDone(err, dir, out.Moved)
		}
		if err := os.Rename(filepath.Join(dir, it.name), it.dst); err != nil {
			return out, halfDone(err, dir, out.Moved)
		}
		out.Moved = append(out.Moved, it.name)
	}

	if err := os.MkdirAll(fstate, 0o700); err != nil {
		return out, halfDone(err, dir, out.Moved)
	}
	if err := os.Chmod(fstate, 0o700); err != nil {
		return out, halfDone(err, dir, out.Moved)
	}
	out.Kept = kept
	for _, name := range stateFiles {
		if slices.Contains(kept, "foreman-state/"+name) {
			continue // only foreman-secret: the root's stays, the archive's is left in dir
		}
		if err := moveNoReplace(filepath.Join(state, name), filepath.Join(fstate, name)); err != nil {
			return out, halfDone(err, dir, out.Moved)
		}
		out.Moved = append(out.Moved, "foreman-state/"+name)
	}

	if entries, err := os.ReadDir(filepath.Join(dir, "repos")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				out.Repos = append(out.Repos, e.Name())
			}
		}
	}
	return out, nil
}

// foremanSecret is the one foreman-state file no archive holds (it is
// copied by hand), so the root's is always kept.
const foremanSecret = "foreman-secret"

// halfDone is an adopt that failed after something moved: what moved,
// and what is still in dir for the maintainer to move by hand (a second
// adopt refuses a root that has a board).
func halfDone(err error, dir string, moved []string) error {
	if len(moved) == 0 {
		return fmt.Errorf("adopt: %w", err)
	}
	return fmt.Errorf("adopt: %w (moved so far: %s; still in %s: %s; move the rest into the root by hand, as a second adopt refuses a root with a board)",
		err, strings.Join(moved, ", "), dir, strings.Join(leftIn(dir), ", "))
}

// leftIn lists what adopt would still move from dir: its entries but
// config.toml, images.json and repos/, with foreman-state/'s files one
// by one.
func leftIn(dir string) []string {
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		switch e.Name() {
		case "config.toml", "images.json", "repos":
		case "foreman-state":
			files, _ := os.ReadDir(filepath.Join(dir, e.Name()))
			for _, f := range files {
				left = append(left, "foreman-state/"+f.Name())
			}
		default:
			left = append(left, e.Name())
		}
	}
	if len(left) == 0 {
		return []string{"nothing"}
	}
	return left
}

// moveNoReplace moves a file within one filesystem without replacing
// one at dst (a rename would): link, then unlink the source.
func moveNoReplace(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		return err // EEXIST when dst appeared since the checks
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("%s is in the root now, but: %w", dst, err)
	}
	return nil
}

// refuseAdopt refuses DIR when it is the root or inside it.
func (s Service) refuseAdopt(dir string) error {
	in, err := within(s.Paths.Root, dir)
	if err != nil {
		return fmt.Errorf("adopt: %w", err)
	}
	if in {
		return fmt.Errorf("adopt: %s is the root or inside it; restore elsewhere under the factory user's home", dir)
	}
	return nil
}

// within reports whether path is root or inside it, symlinks followed in
// both (a path that does not exist yet resolves through its nearest
// ancestor that does).
func within(root, path string) (bool, error) {
	r, err := resolve(root)
	if err != nil {
		return false, err
	}
	p, err := resolve(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// resolve is path made absolute with its symlinks evaluated.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	near := nearest(abs)
	real, err := filepath.EvalSymlinks(near)
	if err != nil {
		return "", err
	}
	rest, err := filepath.Rel(near, abs)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, rest), nil
}

// hasTickets reports a ticket file in the tickets directory.
func hasTickets(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			return true, nil
		}
	}
	return false, nil
}

// freeTarget passes a target that is missing, or a directory holding
// nothing but empty directories.
func freeTarget(dst string) error {
	fi, err := os.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("adopt: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("adopt: %s already exists; adopt only into a fresh master (move it aside first)", dst)
	}
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return fmt.Errorf("adopt: %s is not empty (it holds %s); adopt only into a fresh master (move it aside first)", dst, p)
		}
		return nil
	})
	return err
}

// removeEmptyTree removes a directory of empty directories, deepest first
// (os.Remove refuses one that is not empty, so nothing that appeared since
// freeTarget is lost).
func removeEmptyTree(dst string) error {
	var dirs []string
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dirs = append(dirs, p)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Remove(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// nearest is path or its nearest ancestor that exists.
func nearest(path string) string {
	for {
		if _, err := os.Lstat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// device is the filesystem a path is on (st_dev).
func device(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: no device number", path)
	}
	return uint64(st.Dev), nil // int32 on darwin
}
