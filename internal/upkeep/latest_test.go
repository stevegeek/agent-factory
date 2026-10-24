package upkeep

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// trialBuilder records every All call; a trial answers with trial, a
// weekly rebuild with an OK result per spec.
type trialBuilder struct {
	board    *store.Board
	calls    [][]imagebuild.Spec
	trial    imagebuild.Result
	noImage  bool
	lockFree bool
	cancel   context.CancelFunc // ends the context during the trial
}

func (b *trialBuilder) All(ctx context.Context, specs []imagebuild.Spec) ([]imagebuild.Result, error) {
	b.calls = append(b.calls, specs)
	if b.board != nil {
		if l, err := b.board.TickLock(ctx, store.NoWait); err == nil {
			b.lockFree = true
			l.Unlock()
		}
	}
	var out []imagebuild.Result
	for _, sp := range specs {
		switch {
		case !sp.Trial:
			out = append(out, imagebuild.Result{Kind: sp.Kind.Kind, Tag: sp.Tag, OK: true, Summary: "ok", Version: sp.Pinned, From: sp.Pinned})
		case !b.noImage:
			r := b.trial
			if b.cancel != nil {
				b.cancel()
				r = imagebuild.Result{Err: context.Canceled, Summary: "error: context canceled"}
			}
			r.Kind, r.Tag = sp.Kind.Kind, sp.Tag
			out = append(out, r)
		}
	}
	return out, nil
}

func (b *trialBuilder) trials() []imagebuild.Spec {
	var out []imagebuild.Spec
	for _, c := range b.calls {
		for _, sp := range c {
			if sp.Trial {
				out = append(out, sp)
			}
		}
	}
	return out
}

func trialResult() imagebuild.Result {
	return imagebuild.Result{
		OK: true, Summary: "trial: 1 passes, 1 breaks, 1 already failing",
		Passed: []string{"gem"}, Broken: []string{"shard"}, AlreadyFailing: []string{"old"},
		Tails: map[string]string{"gem": "fine", "shard": strings.Repeat("x", 2000) + "1 failure\n", "old": "lint: 2 offences\n"},
	}
}

// trialConfig: a gem, a Crystal shard and a second shard with checkouts, a
// paused shard, and a project whose checkout is elsewhere.
func trialConfig(t *testing.T, extra string) string {
	dir := t.TempDir()
	return `
image = "localhost/factory-agent:scion"

[images]
claude = "2.1.296"
crystal = "1.20.1"

[remote]
image = "localhost/factory-pi:dev"
` + extra + `
[projects.gem]
path = "` + dir + `"
base = "main"

[projects.shard]
path = "` + dir + `"
base = "main"
kind = "crystal-shard"

[projects.old]
path = "` + dir + `"
base = "main"
language = "Crystal shard"

[projects.off]
path = "` + dir + `"
base = "main"
kind = "crystal-shard"
enabled = false

[projects.away]
path = "/nonexistent/checkout"
kind = "crystal-shard"
`
}

func trialReleases(crystal, ameba string) *fakeReleases {
	return &fakeReleases{github: func(repo string) (string, error) {
		if repo == "crystal-lang/crystal" {
			return crystal, nil
		}
		return ameba, nil
	}}
}

// The trial builds the gate image at the version it has with the newest
// Crystal (ameba as pinned: nothing newer), runs every enabled project
// with a checkout, files one notice and sets its clock.
func TestLatestTrialFilesOneNotice(t *testing.T) {
	f := newFixture(t)
	if err := imagebuild.SaveState(f.board.Paths.ImagesState(), imagebuild.State{"claude": {Version: "2.1.302", Previous: "2.1.296"}}); err != nil {
		t.Fatal(err)
	}
	b := &trialBuilder{board: f.board, trial: trialResult()}
	g := &fakeGate{ok: true}
	s := Service{Board: f.board, Builder: b, Gate: g, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	cfg := parse(t, trialConfig(t, ""))
	if err := s.Latest(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if b.lockFree {
		t.Fatal("the tick lock was free during the trial")
	}
	trials := b.trials()
	if len(b.calls) != 1 || len(trials) != 1 {
		t.Fatalf("calls %+v", b.calls)
	}
	sp := trials[0]
	if sp.Kind.Kind != "claude" || sp.Tag != "localhost/factory-agent:scion" || sp.Update ||
		!reflect.DeepEqual(sp.BuildArgs, []string{"CLAUDE_VERSION=2.1.302", "CRYSTAL_VERSION=1.21.0", "AMEBA_VERSION=1.7.0"}) {
		t.Fatalf("spec %+v", sp)
	}
	var keys []string
	for _, p := range sp.Projects {
		keys = append(keys, p.Key)
	}
	if want := []string{"gem", "shard", "old"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("projects %q", keys)
	}
	if _, _, err := sp.Projects[1].Run(t.Context(), "localhost/factory-agent:trial"); err != nil || g.ins[0].ID != "trial-shard" ||
		g.ins[0].Image != "localhost/factory-agent:trial" {
		t.Fatalf("check %+v %v", g.ins, err)
	}
	ts := f.tickets(t)
	if len(ts) != 1 {
		t.Fatalf("%d notices", len(ts))
	}
	tk := ts[0]
	if tk.Title != "Crystal 1.21.0 trial" || tk.Project != "factory" || tk.Column != "needs_human" {
		t.Fatalf("notice %q %q", tk.Title, tk.Column)
	}
	for _, want := range []string{
		"Crystal 1.21.0: 1 passes, 1 breaks, 1 already failing.\n\n",
		"with Crystal 1.21.0 (the images have Crystal 1.20.1, ameba 1.7.0)",
		"- Pass: gem\n- Break (pass in the live image): shard\n- Already failing (fail in the live image too): old\n",
		`set [images] crystal = "1.21.0" in config.toml and deploy`,
		"https://github.com/crystal-lang/crystal/releases",
		f.p.Logs + "/trial-claude.log",
		"## old (already failing)\n\n```\nlint: 2 offences\n```\n",
	} {
		if !strings.Contains(tk.Body, want) {
			t.Errorf("body has no %q:\n%s", want, tk.Body)
		}
	}
	if !strings.Contains(tk.Body, "## shard (breaks)\n\n```\n"+strings.Repeat("x", 1491)+"1 failure\n```") ||
		strings.Contains(tk.Body, strings.Repeat("x", 1492)) {
		t.Errorf("the broken tail is not its last 1500 code points:\n%s", tk.Body)
	}
	st, err := f.board.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := st.Clock(LatestName); !ok || at == "" {
		t.Fatal("no clock")
	}
}

// Both tools newer: both go into the title and the instructions.
func TestLatestTrialNamesEveryNewerTool(t *testing.T) {
	f := newFixture(t)
	b := &trialBuilder{trial: imagebuild.Result{OK: true, Passed: []string{"gem", "shard", "old"}}}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.8.0"), Log: f.log}
	if err := s.Latest(t.Context(), parse(t, trialConfig(t, ""))); err != nil {
		t.Fatal(err)
	}
	tk := f.tickets(t)[0]
	if tk.Title != "Crystal 1.21.0 and ameba 1.8.0 trial" || !strings.HasPrefix(tk.Body, "Crystal 1.21.0 and ameba 1.8.0: 3 pass, 0 break, 0 already failing.") ||
		!strings.Contains(tk.Body, `set [images] crystal = "1.21.0" and ameba = "1.8.0" in config.toml`) {
		t.Fatalf("%q\n%s", tk.Title, tk.Body)
	}
}

// A trial image that does not build files a failure notice with its log.
func TestLatestTrialFailedNotice(t *testing.T) {
	f := newFixture(t)
	b := &trialBuilder{trial: imagebuild.Result{OK: false, Summary: "build failed"}}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	if err := s.Latest(t.Context(), parse(t, trialConfig(t, ""))); err != nil {
		t.Fatal(err)
	}
	tk := f.tickets(t)[0]
	if tk.Title != "Crystal 1.21.0 trial failed" || !strings.Contains(tk.Body, "(the images have Crystal 1.20.1, ameba 1.7.0): build failed.") ||
		!strings.Contains(tk.Body, f.p.Logs+"/trial-claude.log") {
		t.Fatalf("%q\n%s", tk.Title, tk.Body)
	}
}

// Nothing newer than the pins (ameba's unpinned default counts): a log
// line, no build, no notice, and no clock, so the next weekly rebuild
// looks again.
func TestLatestNothingNewerBuildsNothing(t *testing.T) {
	f := newFixture(t)
	b := &trialBuilder{}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.20.1", "1.7.0"), Log: f.log}
	if err := s.Latest(t.Context(), parse(t, trialConfig(t, ""))); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 0 || len(f.tickets(t)) != 0 {
		t.Fatalf("calls %+v", b.calls)
	}
	if !slices.Contains(f.log.all(), "latest: nothing newer than Crystal 1.20.1, ameba 1.7.0; no trial") {
		t.Fatalf("log %q", f.log.all())
	}
	st, _ := f.board.LoadState()
	if _, ok := st.Clock(LatestName); ok {
		t.Fatal("clock set with no trial")
	}
}

// No enabled Crystal project with a checkout here: no lookup, no build.
// A failed lookup: no build either.
func TestLatestNeedsACrystalProjectAndTheReleases(t *testing.T) {
	f := newFixture(t)
	rel := trialReleases("1.21.0", "1.8.0")
	b := &trialBuilder{}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Releases: rel, Log: f.log}
	ruby := "image = \"localhost/factory-agent:scion\"\n[projects.gem]\npath = \"" + t.TempDir() + "\"\n" +
		"[projects.off]\npath = \"" + t.TempDir() + "\"\nkind = \"crystal-shard\"\nenabled = false\n"
	if err := s.Latest(t.Context(), parse(t, ruby)); err != nil {
		t.Fatal(err)
	}
	if len(rel.asked) != 0 || len(b.calls) != 0 {
		t.Fatalf("asked %q calls %d", rel.asked, len(b.calls))
	}
	s.Releases = &fakeReleases{github: func(string) (string, error) { return "", errors.New("HTTP 500") }}
	if err := s.Latest(t.Context(), parse(t, trialConfig(t, ""))); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 0 || !slices.Contains(f.log.all(), "latest Crystal: HTTP 500; no trial") {
		t.Fatalf("calls %d log %q", len(b.calls), f.log.all())
	}
}

// A newer release with no trial to build (the gate image not on this
// host, or no top-level image): the trial cannot say so, and Versions
// leaves Crystal and ameba to it, so the old "Update" notice is filed
// instead; no clock, so the next weekly rebuild tries again.
func TestLatestWithoutTheGateImage(t *testing.T) {
	for _, tc := range []struct {
		name, doc, log string
		b              *trialBuilder
	}{
		{"not on this host", "", "latest: the gate image localhost/factory-agent:scion is not on this host; no trial", &trialBuilder{noImage: true}},
		{"no image", "no-image", "latest: no gate image (top-level image) is configured; no trial", &trialBuilder{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			s := Service{Board: f.board, Builder: tc.b, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.8.0"), Log: f.log}
			doc := trialConfig(t, "")
			if tc.doc == "no-image" {
				doc = strings.Replace(doc, "image = \"localhost/factory-agent:scion\"\n", "", 1)
			}
			if err := s.Latest(t.Context(), parse(t, doc)); err != nil {
				t.Fatal(err)
			}
			var titles []string
			for _, tk := range f.tickets(t) {
				titles = append(titles, tk.Title)
			}
			slices.Sort(titles)
			if want := []string{"Update Crystal 1.20.1 -> 1.21.0", "Update ameba 1.7.0 -> 1.8.0"}; !reflect.DeepEqual(titles, want) {
				t.Fatalf("notices %q", titles)
			}
			if !slices.Contains(f.log.all(), tc.log) {
				t.Fatalf("log %q", f.log.all())
			}
			st, _ := f.board.LoadState()
			if _, ok := st.Clock(LatestName); ok {
				t.Fatal("clock set with no trial")
			}
		})
	}
}

// A trial that stops on an error (here a project with no base, through
// the real imagebuild runner) did not run every project: it files a
// "could not finish" notice, not "failed", and leaves the clock, so the
// next weekly rebuild tries again. A second try while that notice is open
// files no second one.
func TestLatestTrialThatCouldNotFinish(t *testing.T) {
	f := newFixture(t)
	calls := &podmanCalls{smoke: "ruby 3.3\nCrystal 1.21.0 [abc]\n1.7.0\n2.1.296 (Claude Code)\n"}
	var r imagebuild.Runner
	r.Podman.Exec = calls
	r.Clock = f.board.Clock
	r.LogDir = f.p.Logs
	s := Service{Board: f.board, Builder: r, Gate: &fakeGate{ok: true}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	cfg := parse(t, trialConfig(t, "\n[projects.nobase]\npath = \""+t.TempDir()+"\"\nkind = \"crystal-shard\"\n"))
	for range 2 {
		if err := s.Latest(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if !calls.has("podman", "rmi", "localhost/factory-agent:trial") {
		t.Fatalf("trial image kept: %q", calls.calls)
	}
	ts := f.tickets(t)
	if len(ts) != 1 {
		t.Fatalf("%d notices", len(ts))
	}
	tk := ts[0]
	if tk.Title != "Crystal 1.21.0 trial could not finish" || !strings.Contains(tk.Body, "[projects.nobase] has no base") ||
		strings.Contains(tk.Body, "built the gate image") || !strings.Contains(tk.Body, f.p.Logs+"/trial-claude.log") {
		t.Fatalf("%q\n%s", tk.Title, tk.Body)
	}
	st, _ := f.board.LoadState()
	if _, ok := st.Clock(LatestName); ok {
		t.Fatal("clock set by a trial that did not finish")
	}
}

// A trial cancelled part way: the cancel is the error, with no notice and
// no clock.
func TestLatestTrialCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := &trialBuilder{cancel: cancel}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	if err := s.Latest(ctx, parse(t, trialConfig(t, ""))); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if n := len(f.tickets(t)); n != 0 {
		t.Fatalf("%d notices", n)
	}
	st, _ := f.board.LoadState()
	if _, ok := st.Clock(LatestName); ok {
		t.Fatal("clock set by a cancelled trial")
	}
}

// The title has no counts, so a trial of the same release while its notice
// is still open files no second ticket, whatever the counts are now.
func TestLatestTrialNoticeIsOnePerRelease(t *testing.T) {
	f := newFixture(t)
	b := &trialBuilder{trial: trialResult()}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	cfg := parse(t, trialConfig(t, ""))
	if err := s.Latest(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	b.trial = imagebuild.Result{OK: true, Passed: []string{"gem", "shard", "old"}}
	if err := s.Latest(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := len(f.tickets(t)); n != 1 {
		t.Fatalf("%d notices", n)
	}
}

// The trial's clock is written under the state lock (the daily clock holds
// it from its read of state.json to its write): while another holds it,
// the clock is not set.
func TestLatestSetsItsClockUnderTheStateLock(t *testing.T) {
	f := newFixture(t)
	held, err := f.board.StateLock(t.Context(), store.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	s := Service{Board: f.board, Builder: &trialBuilder{trial: trialResult()}, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	if err := s.Latest(ctx, parse(t, trialConfig(t, ""))); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	st, _ := f.board.LoadState()
	if _, ok := st.Clock(LatestName); ok {
		t.Fatal("clock set without the state lock")
	}
}

// --latest on a host with no checkouts is refused.
func TestLatestNeedsTheCheckouts(t *testing.T) {
	f := newFixture(t)
	s := Service{Board: f.board, Builder: &trialBuilder{}, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	if err := s.Latest(t.Context(), parse(t, rebuildConfig+"[projects.a]\npath = \"/nonexistent/checkout\"\nkind = \"crystal-shard\"\n")); err == nil {
		t.Fatal("no error")
	}
}

// The weekly rebuild runs the trial after itself when latest_every_days
// have passed since the last one (never counts), not before, and not when
// it is 0.
func TestRebuildRunsTheTrialWhenDue(t *testing.T) {
	f := newFixture(t)
	b := &trialBuilder{trial: trialResult()}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{ok: true}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	cfg := parse(t, trialConfig(t, ""))
	if err := s.Rebuild(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 2 || len(b.trials()) != 1 || b.calls[0][0].Trial {
		t.Fatalf("calls %+v", b.calls)
	}
	if err := s.Rebuild(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 3 || len(b.trials()) != 1 {
		t.Fatalf("a trial again the same day: %d calls", len(b.calls))
	}
	st, err := f.board.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	st.SetClock(LatestName, "2026-01-01T00:00:00+00:00")
	if err := f.board.SaveState(st); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(b.trials()) != 2 {
		t.Fatalf("no trial after the days passed: %d", len(b.trials()))
	}
	st.SetClock(LatestName, "2026-01-01T00:00:00+00:00")
	if err := f.board.SaveState(st); err != nil {
		t.Fatal(err)
	}
	off := parse(t, trialConfig(t, "\n[upkeep]\nlatest_every_days = 0\n"))
	if err := s.Rebuild(t.Context(), off); err != nil {
		t.Fatal(err)
	}
	if len(b.trials()) != 2 {
		t.Fatal("a trial with latest_every_days = 0")
	}
}

// The trial is due once latest_every_days have passed, to the second: 29
// days after the last one it is not, 30 days after it is.
func TestRebuildTrialIsDueAfterExactlyTheDays(t *testing.T) {
	for _, tc := range []struct {
		ago  time.Duration
		want int
	}{{29 * 24 * time.Hour, 0}, {30*24*time.Hour - time.Second, 0}, {30 * 24 * time.Hour, 1}} {
		f := newFixture(t)
		b := &trialBuilder{trial: trialResult()}
		s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log, Clock: f.board.Clock}
		st, err := f.board.LoadState()
		if err != nil {
			t.Fatal(err)
		}
		st.SetClock(LatestName, clock.Stamp(testkit.Stamp().Add(-tc.ago)))
		if err := f.board.SaveState(st); err != nil {
			t.Fatal(err)
		}
		if err := s.Rebuild(t.Context(), parse(t, trialConfig(t, ""))); err != nil {
			t.Fatal(err)
		}
		if got := len(b.trials()); got != tc.want {
			t.Errorf("%v after the last trial: %d trials, want %d", tc.ago, got, tc.want)
		}
	}
}

// A helper (no checkouts) runs no trial from its weekly rebuild.
func TestRebuildOnAHelperRunsNoTrial(t *testing.T) {
	f := newFixture(t)
	b := &trialBuilder{trial: trialResult()}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Releases: trialReleases("1.21.0", "1.7.0"), Log: f.log}
	if err := s.Rebuild(t.Context(), parse(t, rebuildConfig+"[projects.a]\npath = \"/nonexistent/checkout\"\nkind = \"crystal-shard\"\n")); err != nil {
		t.Fatal(err)
	}
	if len(b.trials()) != 0 {
		t.Fatal("a helper ran a trial")
	}
}
