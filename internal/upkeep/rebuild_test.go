package upkeep

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/store"
)

// fakeBuilder records the specs and answers with fixed results; during All
// it checks whether the tick lock is free.
type fakeBuilder struct {
	board    *store.Board
	specs    []imagebuild.Spec
	results  []imagebuild.Result
	err      error
	lockFree bool
	run      func(ctx context.Context, specs []imagebuild.Spec)
}

func (b *fakeBuilder) All(ctx context.Context, specs []imagebuild.Spec) ([]imagebuild.Result, error) {
	b.specs = specs
	if b.board != nil {
		if l, err := b.board.TickLock(ctx, store.NoWait); err == nil {
			b.lockFree = true
			l.Unlock()
		}
	}
	if b.run != nil {
		b.run(ctx, specs)
	}
	return b.results, b.err
}

// fakeGate records the checks asked for.
type fakeGate struct {
	ins   []gate.CheckIn
	heads []string
	ok    bool
}

func (g *fakeGate) Checks(_ context.Context, in gate.CheckIn) (bool, string, error) {
	g.ins = append(g.ins, in)
	return g.ok, "checks output", nil
}

func (g *fakeGate) BaseHead(_ context.Context, p config.Project) (string, error) {
	g.heads = append(g.heads, p.Key+"@"+p.Base)
	return "head-of-" + p.Key, nil
}

const rebuildConfig = `
image = "localhost/factory-agent:scion"

[images]
claude = "2.1.296"
crystal = "1.20.1"

[remote]
image = "localhost/factory-pi:dev"
`

func TestRebuildHoldsTheTickLockAndBuildsEachImage(t *testing.T) {
	f := newFixture(t)
	b := &fakeBuilder{board: f.board}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Log: f.log}
	if err := s.Rebuild(t.Context(), parse(t, rebuildConfig)); err != nil {
		t.Fatal(err)
	}
	if b.lockFree {
		t.Fatal("the tick lock was free while the images were built")
	}
	if l, err := f.board.TickLock(t.Context(), store.NoWait); err != nil {
		t.Fatalf("the tick lock is still held after the rebuild: %v", err)
	} else {
		l.Unlock()
	}
	type spec struct {
		kind, tag, dir, pinned string
		args                   []string
	}
	var got []spec
	for _, sp := range b.specs {
		got = append(got, spec{sp.Kind.Kind, sp.Tag, sp.ContextDir, sp.Pinned, sp.BuildArgs})
	}
	want := []spec{
		{"claude", "localhost/factory-agent:scion", filepath.Join(f.p.Images, "scion-agent"), "2.1.296", []string{"CLAUDE_VERSION=2.1.296", "CRYSTAL_VERSION=1.20.1"}},
		{"pi", "localhost/factory-pi:dev", filepath.Join(f.p.Images, "pi"), "", []string{"CRYSTAL_VERSION=1.20.1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("specs\n%+v\nwant\n%+v", got, want)
	}
}

func TestRebuildWaitsForARunningTick(t *testing.T) {
	f := newFixture(t)
	held, err := f.board.TickLock(t.Context(), store.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	built := false
	s := Service{Board: f.board, Builder: &fakeBuilder{run: func(context.Context, []imagebuild.Spec) { built = true }}, Gate: &fakeGate{}, Notices: f.doors, Log: f.log}
	done := make(chan error, 1)
	go func() { done <- s.Rebuild(ctx, parse(t, rebuildConfig)) }()
	cancel() // the rebuild waits for the tick (blocking), so only the context ends it
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	held.Unlock()
	if built {
		t.Fatal("built while a tick held the lock")
	}
}

func TestRebuildRunsTheProjectsChecksInTheGateImageOnly(t *testing.T) {
	f := newFixture(t)
	repo := t.TempDir()
	cfg := parse(t, rebuildConfig+`
[projects.a]
path = "`+repo+`"
base = "trunk"

[projects.off]
path = "`+repo+`"
base = "main"
enabled = false

[projects.gone]
path = "/nonexistent/checkout"

[projects.notes]
mode = "board"
path = "`+repo+`"
`)
	g := &fakeGate{ok: true}
	var outputs []string
	b := &fakeBuilder{run: func(ctx context.Context, specs []imagebuild.Spec) {
		for _, sp := range specs {
			for _, p := range sp.Projects {
				ok, out, err := p.Run(ctx, "localhost/factory-agent:candidate")
				if err != nil || !ok {
					t.Errorf("%s: %v %v", p.Key, ok, err)
				}
				outputs = append(outputs, sp.Kind.Kind+":"+p.Key+"@"+p.Base+":"+out)
			}
		}
	}}
	s := Service{Board: f.board, Builder: b, Gate: g, Notices: f.doors, Log: f.log}
	if err := s.Rebuild(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	// every enabled factory project with a checkout here; not the board
	// one, and not a paused one (Python checked those too), which is
	// logged
	if want := []string{"claude:a@trunk:checks output"}; !reflect.DeepEqual(outputs, want) {
		t.Fatalf("checks %v", outputs)
	}
	if len(g.ins) != 1 {
		t.Fatalf("ins %+v", g.ins)
	}
	if !slices.ContainsFunc(f.log.all(), func(l string) bool { return strings.Contains(l, "off") && strings.Contains(l, "paused") }) {
		t.Fatalf("log %q", f.log.all())
	}
	in := g.ins[0]
	if in.ID != "rebuild-a" || in.Ref != "head-of-a" || in.Image != "localhost/factory-agent:candidate" || in.Project.Key != "a" || in.Config != cfg {
		t.Fatalf("check in %+v", in)
	}
	if !reflect.DeepEqual(g.heads, []string{"a@trunk"}) {
		t.Fatalf("heads %v", g.heads)
	}
}

func TestRebuildPiImageGetsTheChecksWhenItIsTheGateImage(t *testing.T) {
	f := newFixture(t)
	cfg := parse(t, "image = \"localhost/one:x\"\n[remote]\nimage = \"localhost/one:x\"\n[projects.a]\npath = \""+t.TempDir()+"\"\n")
	b := &fakeBuilder{}
	s := Service{Board: f.board, Builder: b, Gate: &fakeGate{}, Notices: f.doors, Log: f.log}
	if err := s.Rebuild(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, sp := range b.specs {
		if len(sp.Projects) != 1 {
			t.Fatalf("%s: %d project checks (Python checks when the tag is the gate image's)", sp.Kind.Kind, len(sp.Projects))
		}
	}
}

func TestRebuildFailureNoticeOnlyWhereTheCheckoutsAre(t *testing.T) {
	failed := []imagebuild.Result{
		{Kind: "claude", Tag: "localhost/factory-agent:scion", OK: false, Summary: "smoke test failed (expected Claude Code 2.1.296)"},
		{Kind: "pi", Tag: "localhost/factory-pi:dev", OK: true, Summary: "ok"},
	}
	for _, tc := range []struct {
		name     string
		checkout bool
	}{{"dispatcher", true}, {"helper", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			doc := rebuildConfig + "[projects.a]\npath = \"/nonexistent/checkout\"\n"
			if tc.checkout {
				doc = rebuildConfig + "[projects.a]\npath = \"" + t.TempDir() + "\"\n"
			}
			s := Service{Board: f.board, Builder: &fakeBuilder{results: failed}, Gate: &fakeGate{}, Notices: f.doors, Log: f.log}
			if err := s.Rebuild(t.Context(), parse(t, doc)); err != nil {
				t.Fatal(err)
			}
			ts := f.tickets(t)
			if !tc.checkout {
				if len(ts) != 0 {
					t.Fatalf("a host with no checkouts filed %d notices", len(ts))
				}
				return
			}
			if len(ts) != 1 {
				t.Fatalf("%d notices", len(ts))
			}
			tk := ts[0]
			wantBody := "smoke test failed (expected Claude Code 2.1.296). The previous image is still in use. Log: " +
				f.p.Logs + "/rebuild-claude.log on the host."
			if tk.Title != "Weekly Claude Code image rebuild failed" || tk.Body != wantBody || tk.Column != "needs_human" || tk.Project != "factory" {
				t.Fatalf("notice %q %q %q", tk.Title, tk.Body, tk.Column)
			}
		})
	}
}

func TestRebuildPassesOnTheBuildersError(t *testing.T) {
	f := newFixture(t)
	s := Service{Board: f.board, Builder: &fakeBuilder{err: errors.New("rebuild: podman: not found")}, Gate: &fakeGate{}, Notices: f.doors, Log: f.log}
	if err := s.Rebuild(t.Context(), parse(t, rebuildConfig)); err == nil || !strings.Contains(err.Error(), "podman") {
		t.Fatalf("err %v", err)
	}
}

// The Python UpkeepTest's two rebuild tests, through upkeep with the real
// imagebuild runner and podman's argv recorded (imagebuild's own tests
// cover the same at its level).
type podmanCalls struct {
	calls [][]string
	smoke string
}

func (p *podmanCalls) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	p.calls = append(p.calls, c.Argv)
	r := execx.Result{Argv: c.Argv}
	if len(c.Argv) > 1 && c.Argv[1] == "run" {
		r.Stdout = []byte(p.smoke)
	}
	return r, nil
}

func (p *podmanCalls) has(argv ...string) bool {
	return slices.ContainsFunc(p.calls, func(c []string) bool { return slices.Equal(c, argv) })
}

func rebuildThroughImagebuild(t *testing.T, smoke string) *podmanCalls {
	t.Helper()
	f := newFixture(t)
	calls := &podmanCalls{smoke: smoke}
	var r imagebuild.Runner // its podman.Client is filled in field by field: the row has no podman
	r.Podman.Exec = calls
	r.Clock = f.board.Clock
	r.LogDir = f.p.Logs
	s := Service{Board: f.board, Builder: r, Gate: &fakeGate{}, Notices: f.doors, Log: f.log}
	cfg := parse(t, "image = \"localhost/factory-agent:scion\"\n[images]\nclaude = \"2.1.296\"\n[remote]\nimage = \"localhost/factory-pi:dev\"\n")
	if err := s.Rebuild(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	return calls
}

// port: TF UpkeepTest.test_rebuild_swaps_the_tag_only_after_checks_pass
func TestRebuildSwapsTheTagOnlyAfterChecksPass(t *testing.T) {
	calls := rebuildThroughImagebuild(t, "ruby 3.3\n2.1.296 (Claude Code)\n")
	i := slices.IndexFunc(calls.calls, func(c []string) bool { return len(c) > 1 && c[1] == "build" })
	if i < 0 || !slices.Contains(calls.calls[i], "CLAUDE_VERSION=2.1.296") {
		t.Fatalf("calls %q", calls.calls)
	}
	if !calls.has("podman", "tag", "localhost/factory-agent:candidate", "localhost/factory-agent:scion") {
		t.Fatalf("no swap: %q", calls.calls)
	}
}

// port: TF UpkeepTest.test_rebuild_keeps_the_old_image_when_the_version_is_wrong
func TestRebuildKeepsTheOldImageWhenTheVersionIsWrong(t *testing.T) {
	calls := rebuildThroughImagebuild(t, "2.1.999 (Claude Code)\n")
	if calls.has("podman", "tag", "localhost/factory-agent:candidate", "localhost/factory-agent:scion") {
		t.Fatalf("swapped: %q", calls.calls)
	}
	if !calls.has("podman", "rmi", "localhost/factory-agent:candidate") {
		t.Fatalf("candidate kept: %q", calls.calls)
	}
}
