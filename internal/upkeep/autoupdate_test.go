package upkeep

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// resultsFor answers each spec as the builder would: ok, at its version.
func resultsFor(ok bool, summary string) func(specs []imagebuild.Spec) []imagebuild.Result {
	return func(specs []imagebuild.Spec) []imagebuild.Result {
		var out []imagebuild.Result
		for _, s := range specs {
			if s.Tag == "" {
				continue
			}
			out = append(out, imagebuild.Result{Kind: s.Kind.Kind, Tag: s.Tag, OK: ok, Summary: summary,
				Version: s.Pinned, Update: s.Update, From: s.From})
		}
		return out
	}
}

type answering struct {
	fakeBuilder
	answer func([]imagebuild.Spec) []imagebuild.Result
}

func (a *answering) All(ctx context.Context, specs []imagebuild.Spec) ([]imagebuild.Result, error) {
	a.specs = specs
	return a.answer(specs), nil
}

const autoConfig = `
image = "localhost/factory-agent:scion"

[images]
claude = "2.1.296"
auto_update = ["claude"]

[projects.a]
path = "%REPO%"
base = "main"
`

func autoFixture(t *testing.T, latest string, ok bool) (*fixture, *answering, Service, string) {
	t.Helper()
	f := newFixture(t)
	cfg := strings.ReplaceAll(autoConfig, "%REPO%", t.TempDir())
	b := &answering{answer: resultsFor(ok, "2.1.302 (Claude Code)")}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{ok: true}, Notices: f.doors, Log: f.log,
		Releases: &fakeReleases{npm: func(string) (string, error) { return latest, nil }},
		Clock:    testkit.Fixed{T: testkit.Stamp()}}
	return f, b, s, cfg
}

// a newer release than the floor is built, promoted, recorded in
// images.json, and announced; the pi image, not auto-updated, keeps its
// version.
func TestRebuildTakesANewerRelease(t *testing.T) {
	f, b, s, cfg := autoFixture(t, "2.1.302", true)
	if err := s.Rebuild(t.Context(), parse(t, cfg)); err != nil {
		t.Fatal(err)
	}
	sp := b.specs[0]
	if sp.Pinned != "2.1.302" || !sp.Update || sp.From != "2.1.296" || sp.BuildArgs[0] != "CLAUDE_VERSION=2.1.302" {
		t.Errorf("claude spec %+v", sp)
	}
	if b.specs[1].Update {
		t.Errorf("pi is not in auto_update: %+v", b.specs[1])
	}
	st, err := imagebuild.LoadState(f.p.ImagesState())
	if err != nil || st["claude"].Version != "2.1.302" || st["claude"].Previous != "2.1.296" || st["claude"].Since == "" {
		t.Errorf("images.json %+v %v", st, err)
	}
	ts := f.tickets(t)
	if len(ts) != 1 || ts[0].Title != "Claude Code updated 2.1.296 -> 2.1.302" ||
		!strings.Contains(ts[0].Body, "`factory rebuild --rollback claude`") || !strings.Contains(ts[0].Body, ":previous") {
		t.Fatalf("notice %+v", ts)
	}
	// the next week: images.json's version is the one the image has
	b.answer = resultsFor(true, "2.1.302 (Claude Code)")
	if err := s.Rebuild(t.Context(), parse(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if sp := b.specs[0]; sp.Pinned != "2.1.302" || sp.Update {
		t.Errorf("the second week rebuilds 2.1.302 as it is: %+v", sp)
	}
}

// A failed update keeps the image and images.json, and the notice names
// the version it tried.
func TestAFailedUpdateNamesTheVersion(t *testing.T) {
	f, _, s, cfg := autoFixture(t, "2.1.302", false)
	s.Builder = &answering{answer: resultsFor(false, "smoke test failed (expected Claude Code 2.1.302)")}
	if err := s.Rebuild(t.Context(), parse(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.p.ImagesState()); err == nil {
		t.Error("images.json written for a failed update")
	}
	var titles, bodies []string
	for _, tk := range f.tickets(t) {
		titles, bodies = append(titles, tk.Title), append(bodies, tk.Body)
	}
	if len(titles) != 2 || titles[0] != "Weekly Claude Code image rebuild failed" || strings.Contains(bodies[1], "trying") ||
		!strings.Contains(bodies[0], "trying Claude Code 2.1.302 (the image has 2.1.296)") {
		t.Errorf("notices %q %q", titles, bodies)
	}
}

// A floor newer than the latest release, and a lookup that fails, both
// rebuild the version the image has.
func TestRebuildWithoutAnUpdate(t *testing.T) {
	_, b, s, cfg := autoFixture(t, "2.1.200", true)
	if err := s.Rebuild(t.Context(), parse(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if sp := b.specs[0]; sp.Pinned != "2.1.296" || sp.Update {
		t.Errorf("floor newer: %+v", sp)
	}
	s.Releases = &fakeReleases{npm: func(string) (string, error) { return "", os.ErrDeadlineExceeded }}
	if err := s.Rebuild(t.Context(), parse(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if sp := b.specs[0]; sp.Pinned != "2.1.296" || sp.Update {
		t.Errorf("lookup failed: %+v", sp)
	}
}

type podTag struct{ calls [][2]string }

func (p *podTag) Tag(_ context.Context, src, dst string) (execx.Result, error) {
	p.calls = append(p.calls, [2]string{src, dst})
	return execx.Result{}, nil
}

// --rollback retags :previous and holds the version left; the next
// rebuild does not retry it; --unhold lets it be tried again.
func TestRollbackHoldsAndUnholdReleases(t *testing.T) {
	f, b, s, cfg := autoFixture(t, "2.1.302", true)
	tag := &podTag{}
	s.Tagger = tag
	if err := s.Rebuild(t.Context(), parse(t, cfg)); err != nil {
		t.Fatal(err)
	}
	c := parse(t, cfg)
	ts, err := s.Rollback(t.Context(), c, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Version != "2.1.296" || ts.Held != "2.1.302" ||
		len(tag.calls) != 1 || tag.calls[0] != [2]string{"localhost/factory-agent:previous", "localhost/factory-agent:scion"} {
		t.Errorf("rollback %+v %v", ts, tag.calls)
	}
	if err := s.Rebuild(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if sp := b.specs[0]; sp.Pinned != "2.1.296" || sp.Update {
		t.Errorf("a held version was retried: %+v", sp)
	}
	if _, err := s.Unhold(t.Context(), "claude"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if sp := b.specs[0]; sp.Pinned != "2.1.302" || !sp.Update {
		t.Errorf("after --unhold: %+v", sp)
	}
	if _, err := s.Unhold(t.Context(), "pi"); err == nil {
		t.Error("unhold of nothing held")
	}
	if _, err := s.Rollback(t.Context(), c, "ruby"); err == nil {
		t.Error("rollback of an unknown kind")
	}
	_ = f
}

// Every promoted rebuild retags the image it replaces as :previous, so a
// rebuild with no update after an update leaves :previous at the updated
// version: images.json says so, and a rollback then neither claims the old
// version back nor holds anything.
func TestRollbackAfterARebuildWithNoUpdate(t *testing.T) {
	f, b, s, cfg := autoFixture(t, "2.1.302", true)
	tag := &podTag{}
	s.Tagger = tag
	c := parse(t, cfg)
	if err := s.Rebuild(t.Context(), c); err != nil { // 2.1.296 -> 2.1.302
		t.Fatal(err)
	}
	if err := s.Rebuild(t.Context(), c); err != nil { // a week on: 2.1.302 again
		t.Fatal(err)
	}
	if sp := b.specs[0]; sp.Pinned != "2.1.302" || sp.Update {
		t.Fatalf("second rebuild: %+v", sp)
	}
	st, err := imagebuild.LoadState(f.board.Paths.ImagesState())
	if err != nil {
		t.Fatal(err)
	}
	if got := st["claude"]; got.Version != "2.1.302" || got.Previous != "2.1.302" {
		t.Fatalf("images.json after the second rebuild: %+v", got)
	}
	ts, err := s.Rollback(t.Context(), c, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Version != "2.1.302" || ts.Held != "" || len(tag.calls) != 1 {
		t.Errorf("rollback %+v %v", ts, tag.calls)
	}
	st, _ = imagebuild.LoadState(f.board.Paths.ImagesState())
	if got := st["claude"]; got.Version != "2.1.302" || got.Held != "" {
		t.Errorf("images.json after the rollback: %+v", got)
	}
}

// An image never updated (no images.json entry, at its floor) rolls back
// to :previous, which has the floor too: nothing is recorded or held.
func TestRollbackWithNoEntry(t *testing.T) {
	f, _, s, cfg := autoFixture(t, "2.1.296", true)
	tag := &podTag{}
	s.Tagger = tag
	c := parse(t, cfg)
	if err := s.Rebuild(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	ts, err := s.Rollback(t.Context(), c, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Held != "" || len(tag.calls) != 1 {
		t.Errorf("rollback %+v %v", ts, tag.calls)
	}
	st, _ := imagebuild.LoadState(f.board.Paths.ImagesState())
	if _, ok := st["claude"]; ok {
		t.Errorf("an entry was made: %+v", st)
	}
}
