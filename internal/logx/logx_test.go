package logx

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

var at = time.Date(2026, 10, 18, 9, 5, 7, 999_000_000, time.FixedZone("x", 3600))

func TestALineIsPythonsShapeOnStderrAndInTheLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs") // made on first use, as Python does
	var stderr bytes.Buffer
	l := &Log{Dir: dir, Clock: testkit.Fixed{T: at}, Stderr: &stderr}
	l.Printf("T%04d moved: %s", 42, "ready -> working")
	if err := l.Print("multi\nline"); err != nil {
		t.Fatal(err)
	}
	// UTC, seconds (truncated), "+00:00", never Z
	want := "2026-10-18T08:05:07+00:00 T0042 moved: ready -> working\n2026-10-18T08:05:07+00:00 multi\nline\n"
	if stderr.String() != want {
		t.Errorf("stderr %q", stderr.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "factory.log"))
	if err != nil || string(b) != want {
		t.Errorf("log %q %v", b, err)
	}
}

func TestTheLogIsNeverWrittenThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "factory.log")); err != nil {
		t.Fatal(err)
	}
	l := &Log{Dir: dir, Clock: testkit.Fixed{T: at}}
	if err := l.Print("planted"); err == nil {
		t.Error("a symlinked factory.log was written")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Errorf("the link's target changed: %q", b)
	}
}

func TestRotationNeverCopiesALinkedFile(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(dir, FileName+".20990101T000000Z")
	hard := filepath.Join(dir, FileName+".20990102T000000Z")
	if err := os.Symlink(secret, sym); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, hard); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	l := &Log{Dir: dir, Clock: testkit.Fixed{T: at}, MaxSize: 10}
	for i := 0; i < 3; i++ { // fill, rotate, rotate again (which compresses the older ones)
		if err := l.Print(strings.Repeat("x", 20)); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".gz") {
			continue
		}
		f, _ := os.Open(filepath.Join(dir, e.Name()))
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(zr)
		f.Close()
		if strings.Contains(string(b), "SECRET") {
			t.Errorf("%s holds the linked file's content", e.Name())
		}
	}
	if _, err := os.Lstat(sym); err != nil {
		t.Errorf("the symlink was touched: %v", err)
	}
}

// Name is another log in its own directory (Foreman's foreman.log in
// foreman-state/): the same lines, never through a link, rotated and kept
// by the same rules under its own name, with the lock in that directory.
func TestANamedLogIsItsOwnFileWithTheSameRules(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	l := &Log{Dir: dir, Name: "foreman.log", Clock: testkit.Fixed{T: at}, Stderr: &stderr, MaxSize: 100, Settle: time.Nanosecond}
	for i := range 20 {
		if err := l.Print(fmt.Sprintf("line %02d %s", i, strings.Repeat("x", 20))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Errorf("factory.log was written: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "foreman.log"))
	if err != nil || !strings.HasSuffix(string(b), " line 19 "+strings.Repeat("x", 20)+"\n") {
		t.Errorf("foreman.log %q %v", b, err)
	}
	if !strings.Contains(stderr.String(), " line 00 ") {
		t.Errorf("stderr %q", stderr.String())
	}
	gz, _ := filepath.Glob(filepath.Join(dir, "foreman.log."+at.UTC().Format("20060102T150405Z")+"*.gz"))
	if len(gz) < 2 {
		t.Errorf("rotated and compressed: %v", gz)
	}
	if _, err := os.Lstat(filepath.Join(dir, LockName)); err != nil {
		t.Errorf("the rotation lock is not in the log's directory: %v", err)
	}
	// never through a link
	os.Remove(filepath.Join(dir, "foreman.log"))
	target := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(target, []byte("keep\n"), 0o600)
	os.Symlink(target, filepath.Join(dir, "foreman.log"))
	if err := l.Print("planted"); err == nil {
		t.Error("a symlinked foreman.log was written")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Errorf("the link's target changed: %q", b)
	}
}

func TestStamp(t *testing.T) {
	if got := Stamp(time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)); got != "2026-01-02T03:04:05+00:00" {
		t.Errorf("Stamp %q", got)
	}
}

func TestNoStderrWhenUnset(t *testing.T) {
	dir := t.TempDir()
	l := &Log{Dir: dir, Clock: testkit.Fixed{T: at}}
	if err := l.Print("quiet"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "factory.log")); !strings.HasSuffix(string(b), " quiet\n") {
		t.Errorf("log %q", b)
	}
}

func TestAFailedWriteStillReachesStderr(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o644)
	var stderr bytes.Buffer
	l := &Log{Dir: file, Clock: testkit.Fixed{T: at}, Stderr: &stderr} // a file where the directory should be
	if err := l.Print("hello"); err == nil {
		t.Error("no error")
	}
	l.Printf("again")
	if !strings.Contains(stderr.String(), " hello\n") || !strings.Contains(stderr.String(), "log: ") {
		t.Errorf("stderr %q", stderr.String())
	}
}

func TestRotationKeepsEveryFileCompressed(t *testing.T) {
	dir := t.TempDir()
	l := &Log{Dir: dir, Clock: testkit.Fixed{T: at}, MaxSize: 100, Settle: time.Nanosecond}
	for i := range 40 {
		if err := l.Print(fmt.Sprintf("line %02d %s", i, strings.Repeat("x", 20))); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	var rotated []string
	sizes := map[string]int64{}
	for _, e := range entries {
		info, _ := e.Info()
		sizes[e.Name()] = info.Size()
		if strings.HasPrefix(e.Name(), "factory.log.") {
			rotated = append(rotated, e.Name())
		}
	}
	// every rotated file is kept, as factory.log.<stamp>[-n].gz
	stamp := at.UTC().Format("20060102T150405Z")
	if len(rotated) < 5 {
		t.Fatalf("only %d rotated files: %v", len(rotated), rotated)
	}
	// every one compressed but the newest, which a writer that opened the
	// log just before the rename may still append to
	plain := 0
	for _, n := range rotated {
		if !strings.HasPrefix(n, "factory.log."+stamp) {
			t.Errorf("rotated file %s", n)
		}
		if !strings.HasSuffix(n, ".gz") {
			plain++
		}
	}
	if plain != 1 {
		t.Errorf("%d rotated files uncompressed, want only the newest: %v", plain, rotated)
	}
	if sizes["factory.log"] > 100+60 {
		t.Errorf("current file %d bytes", sizes["factory.log"])
	}
	// every line is still somewhere, once
	all := ""
	cur, _ := os.ReadFile(filepath.Join(dir, "factory.log"))
	all += string(cur)
	for _, n := range rotated {
		all += readLog(t, filepath.Join(dir, n))
	}
	for i := range 40 {
		if c := strings.Count(all, fmt.Sprintf("line %02d ", i)); c != 1 {
			t.Errorf("line %02d is in the logs %d times", i, c)
		}
	}
	if !strings.Contains(string(cur), "line 39") {
		t.Errorf("current %q", cur)
	}
	// rotation takes the leaf lock file, which stays
	if _, ok := sizes[".log.lock"]; !ok {
		t.Errorf("no .log.lock: %v", sizes)
	}
}

// A rotation lock held elsewhere never blocks a line: it is appended to
// the current file at once, and the rotation waits for a later line.
func TestABusyRotationLockDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	l := &Log{Dir: dir, Clock: testkit.Fixed{T: at}, MaxSize: 10}
	if err := l.Print("a line long enough to rotate"); err != nil {
		t.Fatal(err)
	}
	held, err := fsx.Lock(t.Context(), filepath.Join(dir, LockName), fsx.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := l.Print("second"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > RotateWait+time.Second {
		t.Errorf("the line waited %v", d)
	}
	cur, _ := os.ReadFile(filepath.Join(dir, "factory.log"))
	if !strings.Contains(string(cur), "second") || !strings.Contains(string(cur), "rotate") {
		t.Errorf("current %q", cur)
	}
	held.Unlock()
	if err := l.Print("third"); err != nil {
		t.Fatal(err)
	}
	cur, _ = os.ReadFile(filepath.Join(dir, "factory.log"))
	if strings.Contains(string(cur), "second") {
		t.Errorf("not rotated once the lock was free: %q", cur)
	}
}

// Writers in several goroutines (each its own Log, like separate
// processes) lose no line and tear none, across rotations.
func TestConcurrentWritersLoseNothing(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := &Log{Dir: dir, Clock: testkit.Fixed{T: at}, MaxSize: 4096}
			for i := range 100 {
				if err := l.Print(fmt.Sprintf("w%d-%03d", w, i)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "factory.log") {
			continue
		}
		b := readLog(t, filepath.Join(dir, e.Name()))
		for _, line := range strings.Split(strings.TrimSuffix(b, "\n"), "\n") {
			_, msg, ok := strings.Cut(line, "+00:00 ")
			if !ok || seen[msg] {
				t.Fatalf("bad or repeated line %q in %s", line, e.Name())
			}
			seen[msg] = true
		}
	}
	if len(seen) != 600 {
		t.Errorf("%d lines, want 600", len(seen))
	}
}

// readLog is a log file's text, gunzipped when it is a rotated one.
func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".gz") {
		return string(b)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(out)
}
