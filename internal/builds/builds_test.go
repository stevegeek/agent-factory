package builds

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jobproto"
)

const commit, goV = "35956371f2ff", "go1.27.1"

// answer is a builder's answer; edit changes BUILD's text before it is packed.
func answer(t *testing.T, edit func(build string) string, extra ...jobproto.File) []byte {
	t.Helper()
	bins := map[string][]byte{"factory": []byte("factory bin"), "foreman": []byte("foreman bin"), "factory-remote": []byte("remote bin")}
	var b strings.Builder
	for _, n := range []string{"factory", "foreman", "factory-remote"} {
		sum := sha256.Sum256(bins[n])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), n)
	}
	fmt.Fprintf(&b, "commit %s go %s\n", commit, goV)
	build := b.String()
	if edit != nil {
		build = edit(build)
	}
	files := []jobproto.File{{Name: "factory", Data: bins["factory"]}, {Name: "foreman", Data: bins["foreman"]},
		{Name: "factory-remote", Data: bins["factory-remote"]}, {Name: "BUILD", Data: []byte(build)}}
	out, err := jobproto.TarFlat(append(files, extra...), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPutStoresAVerifiedBuild(t *testing.T) {
	dir := t.TempDir()
	if err := Put(dir, commit, goV, "helpera", answer(t, nil)); err != nil {
		t.Fatal(err)
	}
	if !Has(dir, commit) {
		t.Fatal("no candidate")
	}
	for name, mode := range map[string]os.FileMode{"factory": 0o755, "foreman": 0o755, "factory-remote": 0o755, "BUILD": 0o644, "HOST": 0o644} {
		fi, err := os.Stat(filepath.Join(dir, commit, name))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", name, fi, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, commit, "HOST")); string(b) != "helpera\n" {
		t.Errorf("HOST %q", b)
	}
}

func TestPutRefusesWhatDoesNotCheck(t *testing.T) {
	for name, a := range map[string][]byte{
		"a wrong sha":          answer(t, func(b string) string { return strings.Replace(b, b[:4], "0000", 1) }),
		"another commit":       answer(t, func(b string) string { return strings.Replace(b, "commit "+commit, "commit 000000000000", 1) }),
		"another toolchain":    answer(t, func(b string) string { return strings.Replace(b, "go "+goV, "go go1.26.0", 1) }),
		"an extra file":        answer(t, nil, jobproto.File{Name: "factory-pull", Data: []byte("x")}),
		"a missing BUILD line": answer(t, func(b string) string { return strings.SplitN(b, "\n", 2)[1] }),
	} {
		dir := t.TempDir()
		if err := Put(dir, commit, goV, "helpera", a); err == nil || Has(dir, commit) {
			t.Errorf("%s: stored (%v)", name, err)
		}
	}
	files, _ := jobproto.UntarFlat(answer(t, nil))
	var three []jobproto.File
	for _, n := range []string{"factory", "foreman", "BUILD"} {
		three = append(three, jobproto.File{Name: n, Data: files[n]})
	}
	missing, _ := jobproto.TarFlat(three, time.Unix(1_700_000_000, 0))
	if err := Put(t.TempDir(), commit, goV, "helpera", missing); err == nil {
		t.Error("a missing binary stored")
	}
	if err := Put(t.TempDir(), "35956371F2FF", goV, "helpera", answer(t, nil)); err == nil {
		t.Error("a bad commit name stored")
	}
}

// The source sent to a builder: the tree's directories and regular files,
// executables kept executable, and the Go its go.mod pins.
func TestSourceTar(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "cmd", "factory"), 0o755)
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module x\n\ngo 1.25\n\ntoolchain go1.27.1\n"), 0o644)
	os.WriteFile(filepath.Join(src, "build.sh"), []byte("#!/bin/bash\n"), 0o755)
	os.WriteFile(filepath.Join(src, "cmd", "factory", "main.go"), []byte("package main\n"), 0o644)
	b, pin, err := SourceTar(src)
	if err != nil || pin != "go1.27.1" {
		t.Fatalf("%q %v", pin, err)
	}
	modes := map[string]int64{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		modes[h.Name] = h.Mode
	}
	if modes["build.sh"] != 0o755 || modes["go.mod"] != 0o644 || modes["cmd/factory/main.go"] != 0o644 || modes["cmd/"] == 0 {
		t.Errorf("members %v", modes)
	}
	os.Symlink("/etc/passwd", filepath.Join(src, "link"))
	if _, _, err := SourceTar(src); err == nil {
		t.Error("a link was sent")
	}
	if _, _, err := SourceTar(t.TempDir()); err == nil {
		t.Error("a tree with no go.mod was sent")
	}
}

func TestPutKeepsAnExistingCandidate(t *testing.T) {
	dir := t.TempDir()
	if err := Put(dir, commit, goV, "first", answer(t, nil)); err != nil {
		t.Fatal(err)
	}
	if err := Put(dir, commit, goV, "second", answer(t, nil)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, commit, "HOST")); string(b) != "first\n" {
		t.Errorf("replaced: HOST %q", b)
	}
}

func TestPutClearsStaleTemps(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".tmp-"+commit+"-1")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Put(dir, commit, goV, "helpera", answer(t, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale temp dir is left")
	}
}

func TestPutKeepsTheNewestFive(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i := range 6 {
		c := fmt.Sprintf("%012x", i)
		if err := os.MkdirAll(filepath.Join(dir, c), 0o755); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(filepath.Join(dir, c), base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	if err := Put(dir, commit, goV, "helpera", answer(t, nil)); err != nil {
		t.Fatal(err)
	}
	es, _ := os.ReadDir(dir)
	var left []string
	for _, e := range es {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			left = append(left, e.Name())
		}
	}
	if want := "000000000002 000000000003 000000000004 000000000005 " + commit; strings.Join(left, " ") != want {
		t.Errorf("left %v, want %s", left, want)
	}
}
