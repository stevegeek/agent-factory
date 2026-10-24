package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

type recLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

// write replaces the config the way people and deploy do (a new file
// renamed into place), so the inode changes even within one mtime tick.
func write(t *testing.T, path, text string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func newSource(t *testing.T, text string) (*Source, string, *recLog) {
	t.Helper()
	env := testkit.Root(t)
	path := env.WriteConfig(t, text)
	log := &recLog{}
	return &Source{Path: path, Log: log}, path, log
}

func TestSourceFollowsTheFile(t *testing.T) {
	src, path, log := newSource(t, "[board]\npeople = [\"alice\"]\n[projects.a]\n")
	c1, err := src.Current()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := src.Current()
	if err != nil || c2 != c1 {
		t.Fatalf("an unchanged file is not re-read: %p %p %v", c1, c2, err)
	}
	if log.count() != 0 {
		t.Errorf("the first read logs nothing: %q", log.lines)
	}

	// (a) a new person, (b) a removed project, (c) a mode change: all seen
	// on the next call
	write(t, path, "[board]\npeople = [\"alice\", \"bob\"]\n[projects.b]\nmode = \"board\"\n")
	c3, err := src.Current()
	if err != nil {
		t.Fatal(err)
	}
	if c3 == c1 || len(c3.People()) != 2 {
		t.Fatalf("the change is not seen: %v", c3.People())
	}
	if _, ok := c3.Project("a"); ok {
		t.Error("a removed project is still there")
	}
	if m, _ := c3.ProjectMode("b"); m != ModeBoard {
		t.Errorf("mode = %q", m)
	}
	if log.count() != 1 || !strings.Contains(log.lines[0], "re-read") {
		t.Errorf("a re-read logs one line: %q", log.lines)
	}
}

// A change of size or mtime alone (an in-place write) is seen too.
func TestSourceSeesAnInPlaceWrite(t *testing.T) {
	src, path, _ := newSource(t, "max_agents = 1\n")
	if c, _ := src.Current(); c.MaxAgents != 1 {
		t.Fatal(c.MaxAgents)
	}
	if err := os.WriteFile(path, []byte("max_agents = 22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, _ := src.Current(); c.MaxAgents != 22 {
		t.Fatalf("size change: %d", c.MaxAgents)
	}
	if err := os.WriteFile(path, []byte("max_agents = 33\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if c, _ := src.Current(); c.MaxAgents != 33 {
		t.Fatalf("mtime change: %d", c.MaxAgents)
	}
}

// A broken edit keeps the last good config and logs once per file state;
// the next good edit is taken.
func TestSourceKeepsTheLastGoodConfig(t *testing.T) {
	src, path, log := newSource(t, "max_agents = 4\n")
	good, err := src.Current()
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "max_agents = = 5\n")
	for range 3 {
		c, err := src.Current()
		if err != nil || c != good {
			t.Fatalf("a broken file: %v, %v", c, err)
		}
	}
	if log.count() != 1 || !strings.Contains(log.lines[0], "keeping the last good config") || !strings.Contains(log.lines[0], "line 1") {
		t.Fatalf("logged %q", log.lines)
	}
	write(t, path, "max_agents = = 6\n")
	src.Current()
	if log.count() != 2 {
		t.Fatalf("a second broken state logs again: %q", log.lines)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if c, err := src.Current(); err != nil || c != good {
		t.Fatalf("a missing file: %v %v", c, err)
	}
	if log.count() != 3 {
		t.Fatalf("a missing file logs once: %q", log.lines)
	}
	write(t, path, "max_agents = 7\n")
	if c, err := src.Current(); err != nil || c.MaxAgents != 7 {
		t.Fatalf("a fixed file: %v %v", c, err)
	}
}

func TestSourceWithNoGoodConfigReturnsTheError(t *testing.T) {
	src, path, _ := newSource(t, "[a]\n[a]\n")
	if _, err := src.Current(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("Current = %v", err)
	}
	if _, err := src.Current(); err == nil {
		t.Fatal("the second call found a config")
	}
	missing := &Source{Path: filepath.Join(t.TempDir(), "none.toml"), Log: &recLog{}}
	if _, err := missing.Current(); err == nil {
		t.Fatal("a missing config with nothing before it")
	}
	write(t, path, "max_agents = 9\n")
	if c, err := src.Current(); err != nil || c.MaxAgents != 9 {
		t.Fatalf("after the fix: %v %v", c, err)
	}
}

// Foreman calls Current from many requests at once.
func TestSourceConcurrent(t *testing.T) {
	src, path, _ := newSource(t, "max_agents = 1\n")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				if i == 0 && j%10 == 0 {
					write(t, path, fmt.Sprintf("max_agents = %d\n", j+1))
				}
				if _, err := src.Current(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if c, _ := src.Current(); c.MaxAgents != 41 {
		t.Errorf("last write not seen: %d", c.MaxAgents)
	}
}

func TestSourceNilLog(t *testing.T) {
	env := testkit.Root(t)
	path := env.WriteConfig(t, "max_agents = 1\n")
	src := &Source{Path: path}
	if _, err := src.Current(); err != nil {
		t.Fatal(err)
	}
	write(t, path, "bad = = 1\n")
	if _, err := src.Current(); err != nil {
		t.Fatal(err)
	}
}
