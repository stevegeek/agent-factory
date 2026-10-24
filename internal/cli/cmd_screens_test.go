package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// factory screens relays the running local agents' screens through scion
// look while Foreman's marker is fresh: T0008's agent runs here, T0009's on
// the helper, which is never looked at. It exits at once on a stale marker.
func TestScreensRelaysTheLocalAgents(t *testing.T) {
	f := goldenRoot(t)
	ex := &fakeExec{answer: func(argv []string) (int, string) {
		if len(argv) > 1 && strings.HasSuffix(argv[0], "scion") && argv[1] == "look" {
			return 0, "$ rake test\nall green\n"
		}
		return 1, ""
	}}
	marker := f.env.Path("foreman-state", "watching")
	os.MkdirAll(filepath.Dir(marker), 0o700)

	// stale: no round
	os.WriteFile(marker, nil, 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(marker, old, old)
	if code, _, errs := opsRun(t, f, opsCLI{}, ex, "screens"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(ex.argv) != 0 {
		t.Errorf("a stale marker ran %q", ex.argv)
	}

	// fresh (by the real clock, whatever FACTORY_TEST_NOW says): a round
	// at once; the run ends when its context does
	os.Chtimes(marker, time.Now(), time.Now())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	e := &Env{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}, Environ: f.env.Vars, Exec: ex}
	e.init()
	go func() { done <- runScreens(ctx, e, Args{}) }()
	screen := f.env.Path("screens", "t0008-w1.txt")
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Lstat(screen); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no screen written: %q", ex.argv)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(screen); string(b) != "$ rake test\nall green\n" {
		t.Errorf("screen %q", b)
	}
	es, _ := os.ReadDir(f.env.Path("screens"))
	if len(es) != 1 {
		t.Errorf("screens: %v", es)
	}
	for _, a := range ex.argv {
		if !strings.HasSuffix(a[0], "scion") || !slices.Equal(a[1:], []string{"look", "t0008-w1", "--plain", "-n", "400"}) {
			t.Errorf("ran %q", a)
		}
	}
}
