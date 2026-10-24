package cli

import (
	"os"
	"strings"
	"testing"
)

// fizzyDump writes a small made-up export under the root.
func fizzyDump(f *fixture) string {
	f.env.WriteFile(f.t, "dump/boards.json", `[{"id": "b1", "name": "Product"}, {"id": "b2", "name": "Archive"}]`)
	f.env.WriteFile(f.t, "dump/b1/cards.json", `[
{"id": "k1", "number": 1, "title": "One", "created_at": "2025-03-01T09:00:00Z", "creator": {"name": "Ana Example"},
 "url": "https://fizzy.example/1000/cards/1", "column": {"id": "c1", "name": "In progress"}, "tags": ["Bug"]},
{"id": "k2", "number": 2, "title": "Two", "closed": true}]`)
	f.env.WriteFile(f.t, "dump/b1/comments/1.json", `[{"id": "m1", "created_at": "2025-03-02T09:00:00Z", "creator": {"name": "Bo"}, "body": {"plain_text": "hi"}}]`)
	return f.env.Path("dump")
}

func TestFizzyImportThroughTheCommandLine(t *testing.T) {
	f := ticketFixture(t)
	dir := fizzyDump(f)
	code, out, errs := f.run("", "fizzy", "import", dir, "--map", "b1=sandbox", "--dry-run")
	if code != 0 || !strings.Contains(out, "would create 2, refresh 0, leave 0 unchanged, skip 0") ||
		!strings.Contains(out, "columns: doing 1, done 1") || !strings.Contains(out, "skipped (not mapped): b2 (Archive)") ||
		!strings.HasSuffix(out, "dry run: nothing written\n") {
		t.Fatalf("dry run: %d %q %q", code, out, errs)
	}
	if code, out, _ := f.run("", "ticket", "list"); code != 0 || strings.Contains(out, "T0001") {
		t.Fatalf("a dry run filed tickets: %q", out)
	}
	code, out, errs = f.run("", "fizzy", "import", dir, "--map", "b1=sandbox")
	if code != 0 || !strings.Contains(out, "created 2, refreshed 0, unchanged 0, skipped 0; 1 comments copied") {
		t.Fatalf("import: %d %q %q", code, out, errs)
	}
	if code, out, _ := f.run("", "fizzy", "import", dir, "--map", "b1=sandbox"); code != 0 || !strings.Contains(out, "created 0, refreshed 0, unchanged 2") {
		t.Errorf("rerun: %d %q", code, out)
	}
	if code, out, _ := f.run("", "check", "--board"); code != 0 || strings.Contains(out, "board:") {
		t.Errorf("check --board: %d %q", code, out)
	}
	// Imported text stays untrusted when a person sends it to the pipeline.
	r := f.ok("send", map[string]any{"id": "T0001", "project": "fac"}, "")
	sent, _ := r["factory_ticket"].(map[string]any)
	data, err := os.ReadFile(f.env.Path("board", "tickets", sent["id"].(string)+".json"))
	if err != nil || !strings.Contains(string(data), `"door": "import"`) {
		t.Errorf("the sent ticket's door: %v\n%s", err, data)
	}
	code, _, errs = f.run("", "fizzy", "import", dir, "--map", "b1=fac")
	if code != 1 || !strings.Contains(errs, `fac is a factory project`) || !strings.Contains(errs, `mode = "board"`) {
		t.Errorf("a factory project: %d %q", code, errs)
	}
	for _, argv := range [][]string{
		{"fizzy", "import", dir},
		{"fizzy", "import", "--map", "b1=sandbox"},
		{"fizzy", "import", dir, "--map", "b1"},
		{"fizzy", "export"},
		{"fizzy", "export", "--out", dir, "--map", "b1=sandbox"},
	} {
		if code, _, _ := f.run("", argv...); code != 2 {
			t.Errorf("%q: exit %d, want 2", argv, code)
		}
	}
}
