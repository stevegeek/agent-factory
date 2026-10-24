package fsx

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type member struct {
	hdr  tar.Header
	data string
}

func tarOf(t *testing.T, ms ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range ms {
		h := m.hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(m.data))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func reg(name, data string, mode int64) member {
	return member{tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode}, data}
}

func extractInto(t *testing.T, in []byte) (string, error) {
	t.Helper()
	top := t.TempDir()
	dest := filepath.Join(top, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	return dest, Extract(root, bytes.NewReader(in))
}

func TestExtractTakesFilesAndDirsWithDataModes(t *testing.T) {
	dest, err := extractInto(t, tarOf(t,
		reg("/abs.md", "a", 0o644),
		reg("memory/notes/x.md", "m", 0o4777),
		reg("noexec", "n", 0o646),
		member{tar.Header{Typeflag: tar.TypeDir, Name: "empty/", Mode: 0o7777}, ""},
	))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "abs.md")); string(b) != "a" {
		t.Errorf("abs.md = %q", b)
	}
	for name, want := range map[string]os.FileMode{"memory/notes/x.md": 0o755, "noexec": 0o644} {
		fi, err := os.Lstat(filepath.Join(dest, name))
		if err != nil || fi.Mode() != want {
			t.Errorf("%s: %v %v, want %v", name, fi.Mode(), err, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(dest, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty: %v", err)
	}
}

func TestExtractRefusesLinksSpecialsAndEscapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    member
		want string
	}{
		{"dot-dot", reg("../escape", "x", 0o644), "outside the destination"},
		{"dot-dot inside", reg("a/../../escape", "x", 0o644), "outside the destination"},
		{"symlink", member{tar.Header{Typeflag: tar.TypeSymlink, Name: "l", Linkname: "/etc/passwd"}, ""}, "is a link"},
		{"hard link", member{tar.Header{Typeflag: tar.TypeLink, Name: "h", Linkname: "x"}, ""}, "is a link"},
		{"fifo", member{tar.Header{Typeflag: tar.TypeFifo, Name: "f"}, ""}, "special file"},
		{"long name", reg("../"+strings.Repeat("é", 300), "x", 0o644), "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest, err := extractInto(t, tarOf(t, tc.m))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "escape")); err == nil {
				t.Error("written outside the root")
			}
		})
	}
}

// A directory planted as a link inside the root is not followed out of it.
func TestExtractStaysInsideThroughAPlantedLink(t *testing.T) {
	top := t.TempDir()
	dest, outside := filepath.Join(top, "dest"), filepath.Join(top, "outside")
	for _, d := range []string{dest, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dest, "memory")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := Extract(root, bytes.NewReader(tarOf(t, reg("memory/x", "x", 0o644)))); err == nil {
		t.Error("extracted through a link")
	}
	if _, err := os.Lstat(filepath.Join(outside, "x")); err == nil {
		t.Error("written outside the root")
	}
}
