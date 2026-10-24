package deploy

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/fsx"
)

func TestHelperBundleLeavesOutSciontool(t *testing.T) {
	f := newFixture(t)
	f.write("bin/factory-remote", "remote\n")
	f.write("systemd/factory-remote.service", "[Unit]\n")
	f.write("images/pi/Containerfile", "FROM x\n")
	f.write("images/scion-agent/Containerfile", "FROM y\n")
	f.write("images/scion-agent/sciontool", "23 MB\n")
	f.write("images/other/Containerfile", "not sent\n")
	b, err := HelperBundle(f.p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	for _, want := range []string{"bin/factory-remote", "bin/BUILD", "bin/pi-run", "config.toml", "systemd/factory-remote.service",
		"images/pi/Containerfile", "images/scion-agent/Containerfile"} {
		if !slices.Contains(names, want) {
			t.Errorf("the bundle lacks %s: %q", want, names)
		}
	}
	for _, name := range names {
		if strings.Contains(name, "sciontool") || strings.HasPrefix(name, "images/other") || strings.HasPrefix(name, "bin/factory\x00") || name == "bin/foreman" {
			t.Errorf("the bundle holds %s", name)
		}
	}
	// it unpacks through the helper's own extract
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := fsx.Extract(root, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dir, "bin/pi-run")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("pi-run %v, %v", st, err)
	}
}

func TestHelperBundleRefusesALink(t *testing.T) {
	f := newFixture(t)
	f.write("images/pi/Containerfile", "FROM x\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(f.p.Root, "images/pi/passwd")); err != nil {
		t.Fatal(err)
	}
	if _, err := HelperBundle(f.p); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("HelperBundle = %v", err)
	}
	if err := os.Remove(filepath.Join(f.p.Root, "images/pi/passwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.p.Root, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(f.p.Root, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := HelperBundle(f.p); err == nil {
		t.Error("a linked config.toml was bundled")
	}
}
