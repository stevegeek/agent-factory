package fsx

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// corpus builds what an agent might plant in its memory copy: links of
// every kind, a fifo, a socket, a device when we may make one, an
// unreadable directory and an oversized file, beside ordinary files.
func corpus(t *testing.T) (top, outside string) {
	t.Helper()
	base := t.TempDir()
	top = filepath.Join(base, "copy")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{top, outside, filepath.Join(top, "knowledge", "deep"), filepath.Join(top, "locked")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, data string) {
		if err := os.WriteFile(filepath.Join(top, rel), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "hello")
	write("knowledge/INDEX.md", "index")
	write("knowledge/deep/note.md", "deep")
	write("big.bin", strings.Repeat("x", 300))
	write("locked/secret.md", "unseen")
	if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte("host secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"to-file":      filepath.Join(outside, "passwd"),
		"to-dir":       outside,
		"dangling":     filepath.Join(outside, "nothing"),
		"knowledge/up": "..",
	} {
		if err := os.Symlink(target, filepath.Join(top, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(top, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(top, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	// a character device: only root may make one (skipped otherwise)
	_ = syscall.Mknod(filepath.Join(top, "null"), syscall.S_IFCHR|0o666, 1<<8|3)
	if err := os.Chmod(filepath.Join(top, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(top, "locked"), 0o755) })
	return top, outside
}

type walked struct {
	Dirs  map[string][2][]string // dir -> (dirs, files) names in walk order
	Order []string
	Read  map[string]string
	Kinds map[string]string
	Errs  map[string]string
}

func walk(t *testing.T, top string, max int64) walked {
	t.Helper()
	w := walked{Dirs: map[string][2][]string{}, Read: map[string]string{}, Kinds: map[string]string{}, Errs: map[string]string{}}
	err := WalkNoFollow(top, func(d *Dir) error {
		var dn, fn []string
		for _, e := range d.Dirs {
			dn = append(dn, e.Name)
			w.Kinds[e.Path] = kind(e.Mode)
		}
		for _, e := range d.Files {
			fn = append(fn, e.Name)
			w.Kinds[e.Path] = kind(e.Mode)
			b, err := d.Read(e, max)
			if err != nil {
				w.Errs[e.Path] = err.Error()
				continue
			}
			w.Read[e.Path] = string(b)
		}
		w.Dirs[d.Path] = [2][]string{dn, fn}
		w.Order = append(w.Order, d.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func kind(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "file"
	case m.IsDir():
		return "dir"
	case m&fs.ModeSymlink != 0:
		return "link"
	case m&fs.ModeNamedPipe != 0:
		return "fifo"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	}
	return "other"
}

func TestWalkNoFollowReadsOnlyRegularFilesAndNeverALink(t *testing.T) {
	top, _ := corpus(t)
	w := walk(t, top, 256)
	want := map[string]string{"README.md": "hello", "knowledge/INDEX.md": "index", "knowledge/deep/note.md": "deep"}
	for p, v := range want {
		if w.Read[p] != v {
			t.Errorf("%s: read %q, want %q", p, w.Read[p], v)
		}
	}
	for p, v := range w.Read {
		if _, ok := want[p]; !ok {
			t.Errorf("read %s (%q): only the regular files are read", p, v)
		}
	}
	for p, sub := range map[string]string{"big.bin": "too large", "to-file": "not a regular file", "dangling": "not a regular file",
		"fifo": "not a regular file", "sock": "not a regular file"} {
		if !strings.Contains(w.Errs[p], sub) {
			t.Errorf("%s: err %q, want %q", p, w.Errs[p], sub)
		}
	}
	for p, k := range map[string]string{"to-file": "link", "to-dir": "link", "dangling": "link", "fifo": "fifo", "sock": "socket",
		"knowledge": "dir", "locked": "dir", "README.md": "file", "knowledge/up": "link"} {
		if w.Kinds[p] != k {
			t.Errorf("%s: kind %q, want %q", p, w.Kinds[p], k)
		}
	}
	if _, ok := w.Kinds["null"]; ok && w.Kinds["null"] != "device" {
		t.Errorf("null: %q", w.Kinds["null"])
	}
	// links to directories are listed with the directories (as os.fwalk
	// lists them) but never entered; the unreadable directory is skipped
	if got := w.Order; !slices.Equal(sortedCopy(got), []string{".", "knowledge", "knowledge/deep"}) {
		t.Errorf("walked %v", got)
	}
	if dirs := w.Dirs["."][0]; !slices.Contains(dirs, "to-dir") || slices.Contains(dirs, "to-file") {
		t.Errorf("top-level dirs %v", dirs)
	}
	if dirs := w.Dirs["knowledge"][0]; !slices.Contains(dirs, "up") {
		t.Errorf("knowledge dirs %v", dirs)
	}
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

func TestWalkPruning(t *testing.T) {
	top, _ := corpus(t)
	var seen []string
	err := WalkNoFollow(top, func(d *Dir) error {
		seen = append(seen, d.Path)
		d.Dirs = slices.DeleteFunc(d.Dirs, func(e Entry) bool { return e.Name == "deep" })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(seen, "knowledge/deep") {
		t.Errorf("pruned directory walked: %v", seen)
	}
	seen = nil
	err = WalkNoFollow(top, func(d *Dir) error {
		seen = append(seen, d.Path)
		return fs.SkipAll
	})
	if err != nil || len(seen) != 1 {
		t.Errorf("SkipAll: %v %v", seen, err)
	}
	stop := errors.New("stop")
	if err := WalkNoFollow(top, func(*Dir) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("err %v", err)
	}
}

func TestWalkRefusesALinkedTop(t *testing.T) {
	top, _ := corpus(t)
	link := filepath.Join(filepath.Dir(top), "link")
	if err := os.Symlink(top, link); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := WalkNoFollow(link, func(*Dir) error { called = true; return nil }); err == nil || called {
		t.Errorf("walked through a linked top: %v", err)
	}
}

// A file swapped for a link between the listing and the read is not
// followed: the open is relative to the directory and refuses links.
func TestReadAfterASwapNeverFollows(t *testing.T) {
	top, outside := corpus(t)
	err := WalkNoFollow(top, func(d *Dir) error {
		for _, e := range d.Files {
			if e.Path != "README.md" {
				continue
			}
			p := filepath.Join(top, "README.md")
			if err := os.Remove(p); err != nil {
				return err
			}
			if err := os.Symlink(filepath.Join(outside, "passwd"), p); err != nil {
				return err
			}
			if b, err := d.Read(e, 1024); err == nil {
				t.Errorf("read %q through a swapped-in link", b)
			}
		}
		return fs.SkipAll
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadNoFollow(t *testing.T) {
	top, _ := corpus(t)
	if b, err := ReadNoFollow(filepath.Join(top, "README.md"), 5); err != nil || string(b) != "hello" {
		t.Errorf("read %q %v", b, err)
	}
	if _, err := ReadNoFollow(filepath.Join(top, "README.md"), 4); !errors.Is(err, ErrTooLarge) {
		t.Errorf("cap: %v", err)
	}
	for _, p := range []string{"to-file", "fifo", "sock", "dangling"} {
		if _, err := ReadNoFollow(filepath.Join(top, p), 1024); err == nil {
			t.Errorf("%s read", p)
		}
	}
}
