package fsx

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

// Extract unpacks the tar on r into root, as Python's tarfile extractall
// with filter="data" does, or tighter:
//   - a leading "/" is stripped, and a member whose path leaves the root
//     (through "..") is refused;
//   - only regular files and directories are taken: links (which "data"
//     allows when they stay inside the destination), devices and fifos
//     refuse the whole input;
//   - a file's mode keeps only 0755 of its bits, loses its execute bits
//     unless the owner's is set, and is at least 0600; directories get the
//     default mode; owners are ignored; mtimes are kept.
//
// Every open goes through root, so nothing outside it can be reached even
// if the tree changes underneath. The caller caps what r can deliver: a
// tar is not compressed, so what lands on disk is never more than that.
// Both factory-remote (a helper's job input) and factory-pull (a desktop
// job) unpack with it.
func Extract(root *os.Root, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := memberPath(h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o777); err != nil {
				return err
			}
		case tar.TypeReg, 0: // 0 is the old AREGTYPE
			if err := writeMember(root, name, h, tr); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("%s is a link", clipName(h.Name))
		default:
			return fmt.Errorf("%s is a special file", clipName(h.Name))
		}
	}
}

// memberPath is a member's name as a path inside the root, or an error.
func memberPath(name string) (string, error) {
	rel := strings.TrimLeft(name, "/")
	clean := path.Clean(rel)
	if rel == "" || clean == "." {
		return ".", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0) {
		return "", fmt.Errorf("%s is outside the destination", clipName(name))
	}
	return clean, nil
}

func writeMember(root *os.Root, name string, h *tar.Header, r io.Reader) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o777); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Chmod(name, dataMode(h.Mode)); err != nil {
		return err
	}
	mt := h.ModTime
	return root.Chtimes(name, mt, mt)
}

// dataMode is the "data" filter's mode for a regular file.
func dataMode(m int64) fs.FileMode {
	mode := fs.FileMode(m) & 0o755
	if mode&0o100 == 0 {
		mode &^= 0o111
	}
	return mode | 0o600
}

// clipName is a member name cut to 200 code points for an error message.
func clipName(s string) string {
	const n = 200
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
