package store

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// StateLock is one lock for whoever reads, changes and writes state.json
// (the daily clock, the latest-toolchain trial): a second taker waits.
func TestStateLock(t *testing.T) {
	f := newFixture(t)
	l, err := f.b.StateLock(t.Context(), fsx.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.StateLock(t.Context(), fsx.NoWait); !errors.Is(err, fsx.ErrBusy) {
		t.Fatalf("a second taker: %v", err)
	}
	l.Unlock()
	l, err = f.b.StateLock(t.Context(), fsx.NoWait)
	if err != nil {
		t.Fatalf("once let go: %v", err)
	}
	l.Unlock()
}

// state.json: {"clock": {name: stamp}}, json.dump(indent=2); a missing
// file is empty, a broken one an error (Python's load_state raises).
func TestState(t *testing.T) {
	f := newFixture(t)
	s, err := f.b.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Clock("weekly"); ok {
		t.Error("an empty state has a clock")
	}
	s.SetClock("weekly", "2026-01-02T03:04:05+00:00")
	s.SetClock("memory-report", "2026-01-01T00:00:00+00:00")
	if err := f.b.SaveState(s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.p.State)
	want := "{\n  \"clock\": {\n    \"weekly\": \"2026-01-02T03:04:05+00:00\",\n    \"memory-report\": \"2026-01-01T00:00:00+00:00\"\n  }\n}"
	if string(b) != want {
		t.Errorf("state.json:\n%s", testkit.Diff(want, string(b)))
	}
	// unknown keys and the order survive a round trip; a set keeps its place
	os.WriteFile(f.p.State, []byte("{\n  \"other\": [1.0],\n  \"clock\": {\n    \"b\": \"x\",\n    \"a\": \"y\"\n  }\n}"), 0o644)
	s, _ = f.b.LoadState()
	if v, ok := s.Clock("b"); !ok || v != "x" {
		t.Errorf("clock b %q", v)
	}
	s.SetClock("b", "z")
	f.b.SaveState(s)
	b, _ = os.ReadFile(f.p.State)
	if string(b) != "{\n  \"other\": [\n    1.0\n  ],\n  \"clock\": {\n    \"b\": \"z\",\n    \"a\": \"y\"\n  }\n}" {
		t.Errorf("state.json:\n%s", b)
	}
	os.WriteFile(f.p.State, []byte("{"), 0o644)
	if _, err := f.b.LoadState(); err == nil {
		t.Error("a broken state.json read")
	}
	noTemps(t, f.p.Root)
}

// A schedule's runs per project: {"schedules": {name: {project: stamp}}},
// beside the clock, which they leave alone.
func TestScheduleRuns(t *testing.T) {
	f := newFixture(t)
	s, err := f.b.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock("weekly", "old")
	if _, ok := s.ScheduleRun("weekly", "a"); ok {
		t.Error("an empty state has a schedule run")
	}
	s.SetScheduleRun("weekly", "a", "2026-01-02T03:04:05+00:00")
	s.SetScheduleRun("weekly", "b", "2026-01-03T03:04:05+00:00")
	s.SetScheduleRun("monthly", "a", "2026-01-04T03:04:05+00:00")
	s.SetScheduleRun("weekly", "a", "2026-01-05T03:04:05+00:00")
	if err := f.b.SaveState(s); err != nil {
		t.Fatal(err)
	}
	s, _ = f.b.LoadState()
	if v, ok := s.ScheduleRun("weekly", "a"); !ok || v != "2026-01-05T03:04:05+00:00" {
		t.Errorf("weekly a %q", v)
	}
	if v, ok := s.Clock("weekly"); !ok || v != "old" {
		t.Errorf("clock weekly %q", v)
	}
	b, _ := os.ReadFile(f.p.State)
	want := "{\n  \"clock\": {\n    \"weekly\": \"old\"\n  },\n  \"schedules\": {\n    \"weekly\": {\n" +
		"      \"a\": \"2026-01-05T03:04:05+00:00\",\n      \"b\": \"2026-01-03T03:04:05+00:00\"\n    },\n" +
		"    \"monthly\": {\n      \"a\": \"2026-01-04T03:04:05+00:00\"\n    }\n  }\n}"
	if string(b) != want {
		t.Errorf("state.json:\n%s", testkit.Diff(want, string(b)))
	}
}

// watches.json: {name: {at, hash}}, json.dump(indent=2); missing or broken
// is empty.
func TestWatches(t *testing.T) {
	f := newFixture(t)
	w := f.b.LoadWatches()
	if at, _, has := w.Get("w"); at != "" || has {
		t.Error("empty")
	}
	w.SetAt("w", "2026-01-02T03:04:05+00:00")
	if _, _, has := w.Get("w"); has {
		t.Error("a hash before one was set")
	}
	w.SetHash("w", "abc")
	w.SetAt("v", "2026-01-01T00:00:00+00:00")
	if err := f.b.SaveWatches(w); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.p.Watches)
	want := "{\n  \"w\": {\n    \"at\": \"2026-01-02T03:04:05+00:00\",\n    \"hash\": \"abc\"\n  },\n  \"v\": {\n    \"at\": \"2026-01-01T00:00:00+00:00\"\n  }\n}"
	if string(b) != want {
		t.Errorf("watches.json:\n%s", testkit.Diff(want, string(b)))
	}
	w = f.b.LoadWatches()
	if at, hash, has := w.Get("w"); at != "2026-01-02T03:04:05+00:00" || hash != "abc" || !has {
		t.Errorf("%q %q %v", at, hash, has)
	}
	os.WriteFile(f.p.Watches, []byte("[1"), 0o644)
	if w := f.b.LoadWatches(); len(w.Names()) != 0 {
		t.Error("a broken watches.json is empty")
	}
	os.WriteFile(f.p.Watches, []byte(`{"w": "junk", "v": {"at": "x"}}`), 0o644)
	w = f.b.LoadWatches()
	w.SetAt("w", "y")
	if at, _, _ := w.Get("w"); at != "y" {
		t.Error("a watch whose state is not an object starts again")
	}
}

// pauses.json: {host: until}, json.dump with no indent; expired entries are
// ignored on read and dropped by the next pause.
func TestPauses(t *testing.T) {
	f := newFixture(t)
	now := testkit.Stamp()
	if p := f.b.LoadPauses(); len(p) != 0 {
		t.Errorf("%v", p)
	}
	os.WriteFile(f.p.Pauses, []byte(`{"old": "2026-01-02T03:00:00+00:00", "helper": "2026-01-02T04:00:00+00:00", "bad": "soon", "num": 5}`), 0o644)
	p := f.b.LoadPauses()
	if len(p) != 1 || p[0].Host != "helper" || !p[0].Until.Equal(now.Add(56*time.Minute-5*time.Second)) {
		t.Errorf("pauses %+v", p)
	}
	if !f.b.Paused("helper") || f.b.Paused("old") || f.b.Paused("local") {
		t.Error("Paused")
	}
	until, err := f.b.PauseHost("local", "usage limit", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if until != "2026-01-02T03:34:05+00:00" {
		t.Errorf("until %s", until)
	}
	b, _ := os.ReadFile(f.p.Pauses)
	if string(b) != `{"helper": "2026-01-02T04:00:00+00:00", "local": "2026-01-02T03:34:05+00:00"}` {
		t.Errorf("pauses.json %s", b)
	}
	if got := f.log.all(); len(got) != 1 || got[0] != "local paused until 2026-01-02T03:34:05+00:00: usage limit" {
		t.Errorf("log %q", got)
	}
	// a pause again keeps the host's place
	f.b.PauseHost("helper", "again", time.Hour)
	b, _ = os.ReadFile(f.p.Pauses)
	if !strings.HasPrefix(string(b), `{"helper": "2026-01-02T04:04:05+00:00", "local"`) {
		t.Errorf("pauses.json %s", b)
	}
	os.WriteFile(f.p.Pauses, []byte("{"), 0o644)
	if p := f.b.LoadPauses(); len(p) != 0 {
		t.Error("a broken pauses.json is empty")
	}
	noTemps(t, f.p.Root)
}

// A shorter pause never cuts a longer one: a usage-limit pause of 30
// minutes leaves the operator's 24 hours in place.
func TestPauseHostKeepsTheLaterUntil(t *testing.T) {
	f := newFixture(t)
	long, err := f.b.PauseHost("helper", "manager T-1: maintenance", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.b.PauseHost("helper", "usage limit", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got != long || got != "2026-01-03T03:04:05+00:00" {
		t.Errorf("until %s, want %s", got, long)
	}
	b, _ := os.ReadFile(f.p.Pauses)
	if string(b) != `{"helper": "2026-01-03T03:04:05+00:00"}` {
		t.Errorf("pauses.json %s", b)
	}
	// a longer one still extends it
	if got, _ := f.b.PauseHost("helper", "again", 48*time.Hour); got != "2026-01-04T03:04:05+00:00" {
		t.Errorf("until %s", got)
	}
}

func TestResumeHostRemovesOnlyThat(t *testing.T) {
	f := newFixture(t)
	f.b.PauseHost("helper", "a", time.Hour)
	f.b.PauseHost("local", "b", time.Hour)
	ok, err := f.b.ResumeHost("helper", "manager T-2: done")
	if err != nil || !ok {
		t.Fatalf("resume %v %v", ok, err)
	}
	if f.b.Paused("helper") || !f.b.Paused("local") {
		t.Error("only helper resumes")
	}
	b, _ := os.ReadFile(f.p.Pauses)
	if string(b) != `{"local": "2026-01-02T04:04:05+00:00"}` {
		t.Errorf("pauses.json %s", b)
	}
	if got := f.log.all(); len(got) != 3 || got[2] != "helper resumed: manager T-2: done" {
		t.Errorf("log %q", got)
	}
	noTemps(t, f.p.Root)
}

func TestResumeHostNotPaused(t *testing.T) {
	f := newFixture(t)
	ok, err := f.b.ResumeHost("helper", "x")
	if err != nil || ok {
		t.Errorf("resume %v %v", ok, err)
	}
	if _, err := os.Stat(f.p.Pauses); err == nil {
		t.Error("nothing to resume writes nothing")
	}
	// an expired pause is not a pause
	os.WriteFile(f.p.Pauses, []byte(`{"helper": "2026-01-02T03:00:00+00:00"}`), 0o644)
	if ok, _ := f.b.ResumeHost("helper", "x"); ok {
		t.Error("an expired pause resumes nothing")
	}
	if got := f.log.all(); len(got) != 0 {
		t.Errorf("log %q", got)
	}
}
