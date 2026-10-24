package ops_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// copyBoard copies a snapshot's config and ticket files into a fresh root.
// With noAsks, every ticket loses its stored ask first, so migrate has the
// real histories to read asks back from.
func copyBoard(t *testing.T, snap, root string, noAsks bool) {
	t.Helper()
	for _, rel := range []string{"config.toml"} {
		data, err := os.ReadFile(filepath.Join(snap, rel))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, rel), data, 0o644)
	}
	names, _ := filepath.Glob(filepath.Join(snap, "board", "tickets", "T*.json"))
	names = append(names, filepath.Join(snap, "board", "tickets", ".last-id"))
	dst := filepath.Join(root, "board", "tickets")
	os.MkdirAll(dst, 0o755)
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			continue
		}
		if v, err := jsonx.Parse(data); noAsks && err == nil {
			if o, ok := v.Object(); ok && o.Index("ask") >= 0 {
				o.Delete("ask")
				data = jsonx.EncodePython(o.Value(), 2)
			}
		}
		os.WriteFile(filepath.Join(dst, filepath.Base(n)), data, 0o644)
	}
}

// Migrate on a copy of the live board runs clean and backfills once: a
// second migrate prints nothing and changes no file (FACTORY_LIVE_SNAPSHOT
// names a private snapshot; skipped when unset). Before the cutover this
// test also compared every file with Python's migrate, byte for byte.
func TestMigrateTheLiveBoardOnce(t *testing.T) {
	snap := testkit.LiveSnapshot(t)
	for _, noAsks := range []bool{false, true} {
		t.Run(map[bool]string{false: "as-is", true: "without-asks"}[noAsks], func(t *testing.T) { migrateLive(t, snap, noAsks) })
	}
}

func migrateLive(t *testing.T, snap string, noAsks bool) {
	w := newWorld(t, "")
	copyBoard(t, snap, w.env.Root, noAsks)
	lines, err := w.svc.Migrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := w.files()
	again, err := w.svc.Migrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) > 0 {
		t.Errorf("a second migrate printed\n%s", strings.Join(again, "\n"))
	}
	for name, data := range w.files() {
		if first[name] != data {
			t.Errorf("%s changed on the second migrate", name)
		}
	}
	t.Logf("%d files, %d lines", len(first), len(lines))
}
