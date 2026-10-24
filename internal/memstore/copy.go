package memstore

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/fsx"
)

// Checkout is checkout_memory (F:1203): the run's own copy of files, in a
// fresh directory dest (mode 0700) that nothing runs in yet. Anything at
// dest is removed first. Paths that are not PathOK and the context folder
// are not written; every file is created new.
func Checkout(files Files, dest string) error {
	if err := RemoveCopy(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return fmt.Errorf("memory copy: %w", err)
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return fmt.Errorf("memory copy: %w", err)
	}
	keep := Files{}
	for p, d := range files {
		if PathOK(p) && !InContext(p) {
			keep[p] = d
		}
	}
	return WriteNew(dest, keep)
}

// WriteNew writes each file under dir, making its folders, and refuses to
// replace anything already there (Python's open(path, "xb")). The paths
// must be PathOK; the project context is written this way into a copy.
func WriteNew(dir string, files Files) error {
	for _, p := range sortedPaths(files) {
		if !PathOK(p) {
			return fmt.Errorf("memory copy: bad path %s", PyRepr(p))
		}
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o777); err != nil {
			return fmt.Errorf("memory copy: %w", err)
		}
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			return fmt.Errorf("memory copy: %w", err)
		}
		_, werr := f.Write(files[p])
		if err := errors.Join(werr, f.Close()); err != nil {
			return fmt.Errorf("memory copy: %w", err)
		}
	}
	return nil
}

// ReadCopy is read_copy (F:1217): a run's copy read back as files, regular
// files only, opened relative to their directory without following any
// link, at most MaxFile each and MaxFiles in all, then Capped. It returns
// what was left out too: links, devices, fifos, anything named .git, odd
// names, oversized and unreadable files. With skipContext the top-level
// context folder (given to the run read-only) is not read and not named.
// A top that is missing or a link is an error, never an empty copy.
func ReadCopy(top string, skipContext bool) (Files, []string, error) {
	files := Files{}
	var skipped []string
	err := fsx.WalkNoFollow(top, func(d *fsx.Dir) error {
		keep := d.Dirs[:0:0]
		for _, e := range d.Dirs {
			if skipContext && e.Path == ContextTop {
				continue
			}
			if e.Mode.IsDir() && PathOK(e.Path) {
				keep = append(keep, e)
			} else {
				skipped = append(skipped, e.Path)
			}
		}
		d.Dirs = keep
		for _, e := range d.Files {
			if skipContext && e.Path == ContextTop {
				continue
			}
			if !PathOK(e.Path) || len(files) >= MaxFiles {
				skipped = append(skipped, e.Path)
				continue
			}
			data, err := d.Read(e, MaxFile)
			if err != nil {
				skipped = append(skipped, e.Path)
				continue
			}
			files[e.Path] = data
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("memory copy: %w", err)
	}
	files, skipped = Capped(files, skipped)
	return files, skipped, nil
}

// ReadTar is read_memory_tar (F:1259): a helper host's copy, read from its
// tar in memory; nothing is written to disk. Names are normalised with a
// leading / dropped; directories and the context folder are passed over;
// anything not a regular file, with an odd name or over MaxFile, and any
// file past MaxFiles, is left out and named. Like Python's tarfile, an
// empty or broken archive is an error.
func ReadTar(data []byte) (Files, []string, error) {
	if len(data) == 0 {
		return nil, nil, errors.New("memory tar: empty file")
	}
	files := Files{}
	var skipped []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil && !errors.Is(err, tar.ErrInsecurePath) {
			return nil, nil, fmt.Errorf("memory tar: %w", err)
		}
		p := strings.TrimLeft(path.Clean(h.Name), "/")
		if h.Typeflag == tar.TypeDir || p == "." || InContext(p) {
			continue
		}
		if !regular(h.Typeflag) || !PathOK(p) || h.Size > MaxFile || len(files) >= MaxFiles {
			skipped = append(skipped, p)
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, MaxFile+1))
		if err != nil {
			return nil, nil, fmt.Errorf("memory tar: %w", err)
		}
		files[p] = b
	}
	files, skipped = Capped(files, skipped)
	return files, skipped, nil
}

// regular is tarfile's isfile(): the regular, contiguous and sparse types.
func regular(t byte) bool {
	return t == tar.TypeReg || t == tar.TypeRegA || t == tar.TypeCont || t == tar.TypeGNUSparse
}

// RemoveCopy is remove_copy (F:1264): the run's copy deleted even where
// the agent made directories unreadable, never through a link.
func RemoveCopy(dir string) error {
	if err := fsx.RemoveTree(dir); err != nil {
		return fmt.Errorf("memory copy: %w", err)
	}
	return nil
}
