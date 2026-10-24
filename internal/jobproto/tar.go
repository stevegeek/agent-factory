package jobproto

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// File is one tar member: a name and its bytes.
type File struct {
	Name string
	Data []byte
}

// TarFlat is tar_bytes: a plain uncompressed tar of files in the
// given order, every member a regular file with mode 0644 and mtime, owned
// by uid and gid 0 with no user or group names.
func TarFlat(files []File, mtime time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if f.Name == "" {
			return nil, errors.New("tar: a member with no name")
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.Name,
			Size:     int64(len(f.Data)),
			Mode:     0o644,
			ModTime:  time.Unix(mtime.Unix(), 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("tar %s: %w", f.Name, err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, fmt.Errorf("tar %s: %w", f.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar: %w", err)
	}
	return buf.Bytes(), nil
}

// ErrEmptyTar is a tar with no bytes at all (tarfile's "empty file").
var ErrEmptyTar = errors.New("tar: empty file")

// UntarFlat is untar_bytes: the regular-file members whose name
// holds no "/", by name (a later member of the same name wins). Links,
// directories, devices and anything in a subdirectory are dropped, so
// nothing a helper sends can name a path outside one flat namespace.
//
// As tarfile does, an empty or unreadable first header is an error, a bad
// header after the first member ends the archive quietly, and a member cut
// short is an error.
func UntarFlat(b []byte) (map[string][]byte, error) {
	if len(b) == 0 {
		return nil, ErrEmptyTar
	}
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for first := true; ; first = false {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			if first {
				return nil, fmt.Errorf("tar: %w", err)
			}
			return out, nil
		}
		if !regular(hdr.Typeflag) || strings.Contains(hdr.Name, "/") || hdr.Name == "" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("tar %s: %w", hdr.Name, err)
		}
		out[hdr.Name] = data
	}
}

// regular is tarfile's isreg(): REGTYPE, AREGTYPE, CONTTYPE and GNU sparse.
func regular(t byte) bool {
	switch t {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		return true
	}
	return false
}
