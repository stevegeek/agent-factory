package memstore

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// port: TF MemoryTest.test_nothing_the_agent_plants_runs_or_leaves_the_copy
func TestReadCopyReadsOnlyPlainFilesWithoutFollowingLinks(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(base, "secret")
	write(t, secret, "token")
	top := filepath.Join(base, "copy")
	write(t, filepath.Join(top, "README.md"), "readme\n")
	write(t, filepath.Join(top, "knowledge", "x", "note.md"), "ok\n")
	write(t, filepath.Join(top, "knowledge", "x", ".gitattributes"), "* filter=evil\n")
	write(t, filepath.Join(top, "knowledge", "x", ".git", "config"), "[core]\n\tfsmonitor = touch /tmp/pwned\n")
	write(t, filepath.Join(top, ".git", "hooks", "post-commit"), "#!/bin/sh\n")
	write(t, filepath.Join(top, ".GIT"), "x")
	write(t, filepath.Join(top, "context", "planted.md"), "x\n")
	write(t, filepath.Join(top, "big.md"), strings.Repeat("x", MaxFile+1))
	write(t, filepath.Join(top, "edge.md"), strings.Repeat("x", MaxFile))
	write(t, filepath.Join(top, `odd"name.md`), "x")
	if err := os.Symlink(secret, filepath.Join(top, "knowledge", "stolen.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(top, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(top, "fifo.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(top, "locked", "hidden.md"), "x")
	if err := os.Chmod(filepath.Join(top, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(top, "unreadable.md"), "x")
	if err := os.Chmod(filepath.Join(top, "unreadable.md"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveCopy(top) })

	files, skipped, err := ReadCopy(top, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"README.md", "edge.md", "knowledge/x/.gitattributes", "knowledge/x/note.md"}
	if os.Geteuid() == 0 {
		want = append(want, "unreadable.md") // root reads a mode-000 file
	}
	if got := sortedKeys(files); !reflect.DeepEqual(got, want) {
		t.Errorf("read %v, want %v", got, want)
	}
	for _, d := range files {
		if bytes.Contains(d, []byte("token")) {
			t.Error("a link's target was read")
		}
	}
	sort.Strings(skipped)
	wantSkipped := []string{".GIT", ".git", "big.md", "fifo.md", "knowledge/stolen.md", "knowledge/x/.git", "linked", `odd"name.md`}
	if os.Geteuid() != 0 {
		wantSkipped = append(wantSkipped, "unreadable.md")
		sort.Strings(wantSkipped)
	}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Errorf("skipped %q, want %q", skipped, wantSkipped)
	}

	// the project context is read with skipContext false
	files, _, err = ReadCopy(top, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(files["context/planted.md"]) != "x\n" {
		t.Errorf("context not read: %v", sortedKeys(files))
	}
	// a top that is a link, or missing, is an error, never an empty copy
	link := filepath.Join(base, "toplink")
	if err := os.Symlink(top, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCopy(link, true); err == nil {
		t.Error("ReadCopy followed a linked top")
	}
	if _, _, err := ReadCopy(filepath.Join(base, "missing"), true); err == nil {
		t.Error("ReadCopy of a missing copy gave no error")
	}
}

func TestReadCopyStopsAtTheFileCount(t *testing.T) {
	top := t.TempDir()
	for i := range MaxFiles + 5 {
		write(t, filepath.Join(top, "n", strings.Repeat("a", 1)+itoa4(i)), "")
	}
	files, skipped, err := ReadCopy(top, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != MaxFiles || len(skipped) != 5 {
		t.Errorf("kept %d, skipped %d", len(files), len(skipped))
	}
}

func itoa4(i int) string {
	return string([]byte{byte('0' + i/1000%10), byte('0' + i/100%10), byte('0' + i/10%10), byte('0' + i%10)})
}

type member struct {
	name string
	typ  byte
	data string
	link string
}

func tarOf(t *testing.T, ms ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range ms {
		h := &tar.Header{Name: m.name, Typeflag: m.typ, Mode: 0o644, Size: int64(len(m.data)), Linkname: m.link}
		if m.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(m.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// port: TF MemoryTest.test_a_helper_host_gets_a_snapshot_and_its_tar_is_read_in_memory
func TestReadTarReadsInMemoryAndOnlyPlainFiles(t *testing.T) {
	data := tarOf(t,
		member{name: "./", typ: tar.TypeDir},
		member{name: "./knowledge/a.md", typ: tar.TypeReg, data: "fact\n"},
		member{name: "./tickets/T0003.md", typ: tar.TypeReg, data: "next: tests\n"},
		member{name: "./.git/hooks/post-commit", typ: tar.TypeReg, data: "#!/bin/sh\n"},
		member{name: "../escape.md", typ: tar.TypeReg, data: "x"},
		member{name: "/abs/b.md", typ: tar.TypeReg, data: "abs\n"},
		member{name: "./x/../c.md", typ: tar.TypeReg, data: "c\n"},
		member{name: "./knowledge/link.md", typ: tar.TypeSymlink, link: "/etc/passwd"},
		member{name: "./knowledge/hard.md", typ: tar.TypeLink, link: "knowledge/a.md"},
		member{name: "./dev", typ: tar.TypeChar},
		member{name: "./context/testing.md", typ: tar.TypeReg, data: "changed"},
		member{name: "./big.md", typ: tar.TypeReg, data: strings.Repeat("x", MaxFile+1)},
	)
	files, skipped, err := ReadTar(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"knowledge/a.md": "fact\n", "tickets/T0003.md": "next: tests\n", "abs/b.md": "abs\n", "c.md": "c\n"}
	got := map[string]string{}
	for p, d := range files {
		got[p] = string(d)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read %q, want %q", got, want)
	}
	wantSkipped := []string{".git/hooks/post-commit", "../escape.md", "knowledge/link.md", "knowledge/hard.md", "dev", "big.md"}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Errorf("skipped %q, want %q", skipped, wantSkipped)
	}
	// context only, as in Python's test: nothing read, nothing skipped
	files, skipped, err = ReadTar(tarOf(t,
		member{name: "./context/testing.md", typ: tar.TypeReg, data: "changed"},
		member{name: "./knowledge/a.md", typ: tar.TypeReg, data: "fact\n"}))
	if err != nil || !reflect.DeepEqual(sortedKeys(files), []string{"knowledge/a.md"}) || len(skipped) != 0 {
		t.Errorf("context tar: %v %q %v", sortedKeys(files), skipped, err)
	}
	// Python's tarfile refuses an empty file and a broken one; an archive
	// that is only its end marker is an empty copy
	for name, bad := range map[string][]byte{"empty": nil, "garbage": bytes.Repeat([]byte("x"), 1024),
		"short": make([]byte, 100), "truncated": data[:600]} {
		if _, _, err := ReadTar(bad); err == nil {
			t.Errorf("%s tar read without an error", name)
		}
	}
	files, _, err = ReadTar(make([]byte, 1024))
	if err != nil || len(files) != 0 {
		t.Errorf("end-marker-only tar: %v %v", files, err)
	}
}

func TestCheckoutWritesAFreshCopyAndRemoveCopyUnlocksIt(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "run", "memory", "t0003-w1")
	write(t, filepath.Join(dest, "stale.md"), "old")
	err := Checkout(Files{
		"README.md":          []byte("r"),
		"knowledge/INDEX.md": []byte("i"),
		"context/x.md":       []byte("never checked out"),
		"a/../b.md":          []byte("bad name"),
	}, dest)
	if err != nil {
		t.Fatal(err)
	}
	files, skipped, err := ReadCopy(dest, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(files); !reflect.DeepEqual(got, []string{"README.md", "knowledge/INDEX.md"}) || len(skipped) != 0 {
		t.Errorf("checked out %v (skipped %v)", got, skipped)
	}
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0o700 {
		t.Errorf("copy mode %v", st.Mode().Perm())
	}
	// WriteNew refuses to overwrite (Python's "xb")
	if err := WriteNew(dest, Files{"README.md": []byte("again")}); err == nil {
		t.Error("WriteNew overwrote a file")
	}
	if err := WriteNew(dest, Files{"context/INDEX.md": []byte("ctx")}); err != nil {
		t.Fatal(err)
	}
	// the agent locks a directory and plants a link to outside
	outside := filepath.Join(t.TempDir(), "keep.md")
	write(t, outside, "keep")
	write(t, filepath.Join(dest, "locked", "deep", "f.md"), "x")
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dest, "out")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(dest, "locked", "deep"), filepath.Join(dest, "locked"), dest} {
		if err := os.Chmod(d, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveCopy(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Errorf("copy left: %v", err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "keep" {
		t.Errorf("RemoveCopy reached through a link: %v", err)
	}
	if err := RemoveCopy(dest); err != nil {
		t.Errorf("removing a missing copy: %v", err)
	}
}

func sortedKeys(f Files) []string {
	out := keys(f)
	sort.Strings(out)
	return out
}
