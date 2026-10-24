package doors

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/store"
)

const day = 24 * time.Hour

// port: TF DoorsTest.test_clock_fires_once_per_interval
func TestClockFiresOncePerInterval(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, sweepConfig)
	for range 2 {
		if err := f.s.Clock(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.tickets(t)); n != 1 {
		t.Fatalf("%d tickets, want 1", n)
	}
	st, err := f.s.Board.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.SetScheduleRun("rails", "g", "2000-01-01T00:00:00+00:00")
	if err := f.s.Board.SaveState(st); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Clock(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	ts := f.tickets(t)
	if len(ts) != 2 {
		t.Fatalf("%d tickets, want 2", len(ts))
	}
	tk := ts[0]
	if tk.Project != "g" || tk.Title != "Rails?" || tk.Body != "check" || tk.Door != "clock" || tk.Column != "triage" || tk.Kind.Set {
		t.Fatalf("ticket %+v", tk)
	}
}

// The clock holds the state lock from its read of state.json to its
// write, so the latest-toolchain trial's write cannot land in between and
// drop the schedule runs (a duplicate ticket the next day): while another
// holds it, the clock waits and files nothing.
func TestClockHoldsTheStateLock(t *testing.T) {
	f := newFixture(t)
	held, err := f.s.Board.StateLock(t.Context(), fsx.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := f.s.Clock(ctx, parse(t, sweepConfig)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("while the lock is held: %v", err)
	}
	if n := len(f.tickets(t)); n != 0 {
		t.Fatalf("%d tickets filed without the lock", n)
	}
	held.Unlock()
	if err := f.s.Clock(t.Context(), parse(t, sweepConfig)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Board.StateLock(t.Context(), fsx.NoWait); err != nil {
		t.Fatalf("the clock kept the lock: %v", err)
	}
}

// port: TF DoorsTest.test_a_task_schedule_files_a_check_that_reports_back_naming_the_repo
func TestTaskScheduleFilesACheckNamingTheRepo(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, `
[projects.g]
github = "o/g"

[[schedules]]
name = "hygiene"
every_days = 30
kind = "task"
title = "Repo hygiene"
body = "Look at https://github.com/{github} for merged branches."
`)
	if err := f.s.Clock(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	tk := f.tickets(t)[0]
	if tk.KindName() != "task" || tk.Column != "ready" || tk.Door != "clock" {
		t.Fatalf("kind %q column %q door %q", tk.KindName(), tk.Column, tk.Door)
	}
	if !strings.Contains(tk.Body, "https://github.com/o/g ") {
		t.Fatalf("body %q", tk.Body)
	}
	// kind is set after new_ticket's save, so it follows updated
	if got := tail(keyOrder(t, f.raw(t, tk.ID)), 2); !reflect.DeepEqual(got, []string{"updated", "kind"}) {
		t.Fatalf("last keys %v", got)
	}
}

// A task schedule with findings = true files a task that asks its agent for
// structured findings; one without asks for none.
func TestAFindingsScheduleFilesAFindingTask(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, `
[projects.g]
github = "o/g"

[[schedules]]
name = "bug-hunt"
every_days = 30
kind = "task"
findings = true
title = "Bug hunt"
body = "Look for bugs."

[[schedules]]
name = "hygiene"
every_days = 30
kind = "task"
title = "Repo hygiene"
body = "Look."
`)
	if err := f.s.Clock(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	asked := map[string]bool{}
	for _, tk := range f.tickets(t) {
		asked[tk.Title] = tk.FindingsAsked.Or(false)
	}
	if !asked["Bug hunt"] || asked["Repo hygiene"] {
		t.Errorf("findings asked: %v", asked)
	}
}

func TestClockProjectsKindsAndColumns(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, `
[projects.a]
github = "o/a"

[projects.quiet]
clock = false

[projects.off]
enabled = false

[projects.notes]
mode = "board"

[[schedules]]
name = "every"
every_days = 1
title = "Every"
body = "repo {github}."

[[schedules]]
name = "named"
every_days = 1
title = "Named"
body = "b {github}"
projects = ["quiet", "factory", "nowhere", "off", "notes"]
column = "ready"

[[schedules]]
name = "scout"
every_days = 1
kind = "prospect"
title = "Scout"
body = "look"
projects = ["a"]

[[schedules]]
name = "idea"
every_days = 1
kind = "intent"
title = "Idea"
body = "i"
projects = ["a"]
`)
	if err := f.s.Clock(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	type row struct{ project, title, body, column, kind string }
	var got []row
	for _, tk := range f.tickets(t) {
		got = append(got, row{tk.Project, tk.Title, tk.Body, tk.Column, tk.KindName()})
	}
	want := []row{
		// every factory project but the ones with clock = false (and the paused one)
		{"a", "Every", "repo o/a.", "triage", ""},
		// a named list keeps out clock = false, the paused and the board project too; "factory"
		// and an unknown key are allowed (quirk: the tick later fails the unknown one)
		{"factory", "Named", "b ", "ready", ""},
		{"nowhere", "Named", "b ", "ready", ""},
		{"a", "Scout", "look", "ready", "prospect"},
		// a schedule's intent kind is not one the clock files as such: a change, in its column
		{"a", "Idea", "i", "triage", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filed\n%v\nwant\n%v", got, want)
	}
	if !f.log.has("clock: named names notes, a board project; skipped") {
		t.Fatalf("log %v", f.log.all())
	}
}

// A schedule records its run per project, and only where it filed: a
// skipped project (board, paused, clock = false) is not recorded.
func TestClockRecordsARunOnlyWhereItFiled(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, `
[projects.notes]
mode = "board"

[projects.off]
enabled = false

[projects.quiet]
clock = false

[projects.a]

[[schedules]]
name = "s"
every_days = 7
title = "t"
body = "b"
projects = ["notes", "off", "quiet", "a"]
`)
	if err := f.s.Clock(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.p.State)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"schedules\": {\n    \"s\": {\n      \"a\": \"2026-01-02T03:04:05+00:00\"\n    }\n  },\n" +
		"  \"clock\": {\n    \"memory-report\": \"2026-01-02T03:04:05+00:00\",\n    \"versions\": \"2026-01-02T03:04:05+00:00\"\n  }\n}"
	if string(data) != want {
		t.Fatalf("state.json\n%s\nwant\n%s", data, want)
	}
}

// A schedule is due per project: one switched on (or added) gets it on the
// next daily clock, not when the schedule next fires elsewhere. A
// schedule's run from before runs were kept per project still counts for
// each project, so nothing fires twice after the change.
func TestClockSchedulesAreDuePerProject(t *testing.T) {
	f := newFixture(t)
	const text = `
[projects.a]

[projects.b]
%s

[[schedules]]
name = "s"
every_days = 7
title = "t"
body = "b"
`
	run := func(b string) []string {
		t.Helper()
		before := len(f.tickets(t))
		if err := f.s.Clock(t.Context(), parse(t, strings.Replace(text, "%s", b, 1))); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, tk := range f.tickets(t)[before:] {
			got = append(got, tk.Project)
		}
		return got
	}
	// the old per-schedule stamp, a day ago, holds for every project
	st, err := f.s.Board.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.SetClock("s", "2026-01-01T03:04:05+00:00")
	if err := f.s.Board.SaveState(st); err != nil {
		t.Fatal(err)
	}
	if got := run("enabled = false"); got != nil {
		t.Fatalf("filed %v the day after the schedule's old run", got)
	}
	f.advance(6 * day)
	if got := run("enabled = false"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("filed %v, want a (b is paused)", got)
	}
	f.advance(day)
	if got := run(""); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("filed %v, want b on the day it is switched on", got)
	}
	f.advance(6 * day)
	if got := run(""); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("filed %v, want a a week after its run", got)
	}
	f.advance(day)
	if got := run(""); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("filed %v, want b a week after its run", got)
	}
}

func TestClockMemoryReportAndVersionsCadence(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, "")
	f.report.text = "The agents' memory stores …"
	run := func() {
		t.Helper()
		if err := f.s.Clock(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if f.report.calls != 1 || f.versions.calls != 1 {
		t.Fatalf("first run: report %d versions %d", f.report.calls, f.versions.calls)
	}
	ts := f.tickets(t)
	if len(ts) != 1 || ts[0].Title != "Memory stores: sizes and stale notes" || ts[0].Project != "factory" ||
		ts[0].Column != "needs_human" || ts[0].Door != "clock" || ts[0].Body != f.report.text {
		t.Fatalf("notice %+v", ts)
	}
	f.advance(7*day - time.Second)
	run()
	if f.report.calls != 1 || f.versions.calls != 1 {
		t.Fatalf("before 7 days: report %d versions %d", f.report.calls, f.versions.calls)
	}
	f.advance(time.Second) // 7 days exactly: the version check is due (>=)
	run()
	if f.report.calls != 1 || f.versions.calls != 2 {
		t.Fatalf("at 7 days: report %d versions %d", f.report.calls, f.versions.calls)
	}
	f.advance(23 * day) // 30 days after the first run
	run()
	if f.report.calls != 2 || f.versions.calls != 3 {
		t.Fatalf("at 30 days: report %d versions %d", f.report.calls, f.versions.calls)
	}
	// the first notice is still open: the second report files nothing new
	if n := len(f.tickets(t)); n != 1 {
		t.Fatalf("%d tickets", n)
	}
}

func TestClockReportGetsTheClosedTickets(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, "")
	for _, col := range []string{"done", "rejected", "ready"} {
		if _, err := f.s.Board.FileTicket(t.Context(), newIn("p", col, col)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.Clock(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"T0001": true, "T0002": true}; !reflect.DeepEqual(f.report.closed, want) {
		t.Fatalf("closed %v", f.report.closed)
	}
}

func TestClockRefusesABrokenStateFile(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, sweepConfig)
	if err := os.WriteFile(f.p.State, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Clock(t.Context(), cfg); err == nil {
		t.Fatal("a state.json that does not parse ran the clock")
	}
	if n := len(f.tickets(t)); n != 0 {
		t.Fatalf("%d tickets", n)
	}
}

func TestClockKeepsUnknownStateKeys(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, "")
	if err := os.WriteFile(f.p.State, []byte(`{"other": 1, "clock": {"x": "2026-01-01T00:00:00+00:00"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Clock(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	b := store.Board{Paths: f.p}
	st, err := b.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if at, _ := st.Clock("x"); at != "2026-01-01T00:00:00+00:00" {
		t.Fatalf("x = %q", at)
	}
	data, _ := os.ReadFile(f.p.State)
	if !strings.HasPrefix(string(data), "{\n  \"other\": 1,") {
		t.Fatalf("state.json %s", data)
	}
}
