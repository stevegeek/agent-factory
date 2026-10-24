package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// A dry run of the tick on a copy of the live board (FACTORY_LIVE_SNAPSHOT
// names a private snapshot; skipped when unset): with every host faked and
// the projects' clones absent, the tick goes through every ticket without
// failing, every file still reads as a ticket afterwards, none is lost, and
// a board project's tickets are left byte for byte.
// It runs as the config is, and again with every paused project switched on.
func TestATickOnACopyOfTheLiveBoard(t *testing.T) {
	snap := testkit.LiveSnapshot(t)
	cfg, err := os.ReadFile(filepath.Join(snap, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("as-is", func(t *testing.T) { liveTick(t, snap, string(cfg)) })
	t.Run("unpaused", func(t *testing.T) {
		liveTick(t, snap, strings.ReplaceAll(string(cfg), "enabled = false", "enabled = true"))
	})
}

func liveTick(t *testing.T, snap, cfg string) {
	h := newHarness(t)
	h.env.WriteConfig(t, cfg)
	names, _ := filepath.Glob(filepath.Join(snap, "board", "tickets", "T*.json"))
	if len(names) == 0 {
		t.Skip("the snapshot has no tickets")
	}
	before := map[string][]byte{}
	for _, n := range append(names, filepath.Join(snap, "board", "tickets", ".last-id")) {
		data, err := os.ReadFile(n)
		if err != nil {
			continue
		}
		dst := filepath.Join(h.paths.Tickets, filepath.Base(n))
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
		before[filepath.Base(n)] = data
	}
	h.clock.t = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	h.local.alive = true
	for name := range h.helpers {
		h.helpers[name].startErr = remoteDown(name)
	}
	if err := Tick(context.Background(), h.deps()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	for name, data := range before {
		if filepath.Ext(name) != ".json" {
			continue
		}
		after, err := os.ReadFile(filepath.Join(h.paths.Tickets, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		old, _, err := ticket.Decode(data)
		if err != nil {
			continue // unreadable before: passed over, and noticed
		}
		if _, _, err := ticket.Decode(after); err != nil {
			t.Errorf("%s no longer reads: %v", name, err)
			continue
		}
		if mode, known := mustConfig(t, h).ProjectMode(old.Project); known && mode == "board" && !bytes.Equal(after, data) {
			t.Errorf("%s is a board project's ticket and changed", name)
		}
	}
	t.Logf("%d tickets; %d adapter calls; %d log lines", len(names), len(h.calls.names()), len(h.log.lines))
}
