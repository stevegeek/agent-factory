package doors

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
)

// The CONFIG of the Python tests: two board projects and a factory one.
const ticketsConfig = `
[board]
people = ["alice", "bob"]

[projects.sandbox]
mode = "board"

[projects.other]
mode = "board"

[projects.fac]
path = "/nonexistent"
github = "o/fac"
`

// port: TL BoardApartTest.test_a_watch_skips_a_board_project
func TestWatchSkipsABoardProject(t *testing.T) {
	f := newFixture(t)
	f.s.Exec = execx.OS{}
	outFile := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(outFile, []byte("something"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := parse(t, ticketsConfig+`
[[watches]]
name = "w"
every_minutes = 0
project = "sandbox"
command = ["cat", "`+outFile+`"]
title = "t"
body = "{output}"
file_first = true
`)
	if err := f.s.Watches(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := len(f.tickets(t)); n != 0 {
		t.Fatalf("%d tickets on a board project", n)
	}
	if !f.log.has("watch w names sandbox, a board project; skipped") {
		t.Fatalf("log %v", f.log.all())
	}
	// the hash is recorded, so the same output is not reconsidered
	if _, h, has := f.s.Board.LoadWatches().Get("w"); !has || h != hash("something") {
		t.Fatalf("hash %q %v", h, has)
	}
}

// port: TT ModeTest.test_clock_skips_a_board_project_a_schedule_names
func TestClockSkipsABoardProjectAScheduleNames(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, ticketsConfig+`
[[schedules]]
name = "s"
every_days = 1
title = "Check"
body = "b"
projects = ["sandbox", "fac"]
`)
	if err := f.s.Clock(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range f.tickets(t) {
		got = append(got, tk.Project)
	}
	if !reflect.DeepEqual(got, []string{"fac"}) {
		t.Fatalf("projects %v", got)
	}
}

func TestSweepSkipsBoardProjects(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, ticketsConfig+"\n[projects.sandbox2]\nmode = \"board\"\ngithub = \"o/s\"\n")
	if err := f.s.Sweep(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.issues.asked, []string{"o/fac"}) {
		t.Fatalf("asked %v", f.issues.asked)
	}
}
