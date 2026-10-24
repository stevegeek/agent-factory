package screens

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

var start = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

// fakeScion answers scion look from a table, recording each call.
type fakeScion struct {
	mu      sync.Mutex
	calls   []string
	screens map[string]string
	errs    map[string]error
	busy    int // looks running now
	most    int // the most that ran at once
	hold    time.Duration
	ctxs    []bool // whether each call had a deadline
}

func (f *fakeScion) Look(ctx context.Context, dir, name string, lines int) (string, error) {
	f.mu.Lock()
	_, deadline := ctx.Deadline()
	f.calls = append(f.calls, dir+" look "+name+" --plain -n "+strconv.Itoa(lines))
	f.ctxs = append(f.ctxs, deadline)
	f.busy++
	f.most = max(f.most, f.busy)
	f.mu.Unlock()
	time.Sleep(f.hold)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy--
	return f.screens[name], f.errs[name]
}

func (f *fakeScion) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	slices.Sort(out)
	return out
}

type logRec struct{ lines []string }

func (l *logRec) Printf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

type relayWorld struct {
	t      *testing.T
	root   string
	clock  *testkit.Step
	scion  *fakeScion
	agents []Agent
	relay  *Relay
	sleeps int
}

func newRelayWorld(t *testing.T) *relayWorld {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "foreman-state"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := &relayWorld{t: t, root: root, clock: testkit.NewStep(start, 0), scion: &fakeScion{screens: map[string]string{}, errs: map[string]error{}}}
	w.relay = &Relay{
		Dir:    filepath.Join(root, "screens"),
		Marker: filepath.Join(root, "foreman-state", "watching"),
		Agents: func(context.Context) ([]Agent, error) { return slices.Clone(w.agents), nil },
		Scion:  w.scion,
		Clock:  w.clock,
		Sleep: func(_ context.Context, d time.Duration) error {
			w.sleeps++
			w.clock.Advance(d)
			return nil
		},
	}
	return w
}

// watched sets the marker's mtime to at (Foreman's touch).
func (w *relayWorld) watched(at time.Time) {
	w.t.Helper()
	if err := os.WriteFile(w.relay.Marker, nil, 0o600); err != nil {
		w.t.Fatal(err)
	}
	if err := os.Chtimes(w.relay.Marker, at, at); err != nil {
		w.t.Fatal(err)
	}
}

func (w *relayWorld) screen(name string) string {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(w.relay.Dir, name+".txt"))
	if err != nil {
		w.t.Fatalf("screen %s: %v", name, err)
	}
	return string(b)
}

func (w *relayWorld) names() []string {
	w.t.Helper()
	es, err := os.ReadDir(w.relay.Dir)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// The relay runs while the marker is fresh and exits once Foreman has
// stopped touching it; a stale marker at the start runs no round.
func TestTheRelayRunsWhileTheMarkerIsFresh(t *testing.T) {
	w := newRelayWorld(t)
	w.agents = []Agent{{Name: "t0001-w1", Dir: "/srv/gem"}}
	w.scion.screens["t0001-w1"] = "$ make test\n"
	w.watched(start)
	if err := w.relay.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// rounds every Round until the marker is Fresh old
	rounds := int(Fresh / Round)
	if calls := w.scion.take(); len(calls) != rounds || calls[0] != "/srv/gem look t0001-w1 --plain -n 400" {
		t.Errorf("%d looks (want %d): %v", len(calls), rounds, calls)
	}
	if got := w.screen("t0001-w1"); got != "$ make test\n" {
		t.Errorf("screen %q", got)
	}
	// a touch while it runs keeps it going
	w.watched(w.clock.Now())
	w.relay.Sleep = func(_ context.Context, d time.Duration) error {
		w.sleeps++
		w.clock.Advance(d)
		if w.sleeps == 3 {
			w.watched(w.clock.Now())
		}
		return nil
	}
	w.sleeps = 0
	w.relay.Run(context.Background())
	if n := len(w.scion.take()); n != rounds+3 {
		t.Errorf("a touch partway: %d rounds, want %d", n, rounds+3)
	}
	// stale: nothing runs
	w.watched(w.clock.Now().Add(-Fresh))
	w.relay.Run(context.Background())
	if calls := w.scion.take(); len(calls) != 0 {
		t.Errorf("a stale marker ran %v", calls)
	}
	// no marker at all: nothing runs
	os.Remove(w.relay.Marker)
	w.relay.Run(context.Background())
	if calls := w.scion.take(); len(calls) != 0 {
		t.Errorf("no marker ran %v", calls)
	}
	// a cancelled run stops
	w.watched(w.clock.Now())
	ctx, cancel := context.WithCancel(context.Background())
	w.relay.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	if err := w.relay.Run(ctx); err != nil {
		t.Errorf("cancelled: %v", err)
	}
	if n := len(w.scion.take()); n != 1 {
		t.Errorf("cancelled after %d rounds", n)
	}
}

// Each round writes every running local agent's screen, capped to its
// last lines and bytes, and deletes the screens of agents that stopped;
// a failed look writes nothing.
func TestARoundWritesCapsAndPrunes(t *testing.T) {
	w := newRelayWorld(t)
	w.agents = []Agent{{Name: "t0001-w1", Dir: "/srv/gem"}, {Name: "t0002-w1", Dir: "/srv/app"}, {Name: "t0003-w1", Dir: "/srv/app"}}
	var long strings.Builder
	for i := range 1000 {
		long.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	w.scion.screens["t0001-w1"] = long.String()
	w.scion.screens["t0002-w1"] = strings.Repeat("x", 300) + "\n" + strings.Repeat("é", MaxBytes) + "\nlast\n"
	w.scion.errs["t0003-w1"] = errors.New("exit 1")
	if err := w.relay.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := w.scion.take(); len(calls) != 3 {
		t.Errorf("looks %v", calls)
	}
	got := w.screen("t0001-w1")
	if lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n"); len(lines) != Lines || lines[0] != "line 600" || lines[len(lines)-1] != "line 999" {
		t.Errorf("kept %d lines, %q … %q", len(lines), lines[0], lines[len(lines)-1])
	}
	big := w.screen("t0002-w1")
	if len(big) > MaxBytes || big != "last\n" {
		t.Errorf("a screen over %d bytes keeps its whole last lines: %d bytes %q", MaxBytes, len(big), big[:min(20, len(big))])
	}
	if _, err := os.Stat(filepath.Join(w.relay.Dir, "t0003-w1.txt")); !os.IsNotExist(err) {
		t.Errorf("a failed look wrote a screen: %v", err)
	}
	if st, err := os.Stat(w.relay.Dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("screens dir: %v %v", st, err)
	}

	// t0001 stops; leftovers that are not a running agent's screen go too,
	// but never a directory
	w.agents = w.agents[1:]
	os.WriteFile(filepath.Join(w.relay.Dir, "t0009-w1.txt.123.tmp"), nil, 0o600)
	os.WriteFile(filepath.Join(w.relay.Dir, "stray"), nil, 0o600)
	os.Mkdir(filepath.Join(w.relay.Dir, "sub"), 0o700)
	w.relay.Round(context.Background())
	if got := w.names(); !slices.Equal(got, []string{"sub", "t0002-w1.txt"}) {
		t.Errorf("after the prune: %v", got)
	}
	// a run whose rounds fail the same way logs it once
	log := &logRec{}
	w.relay.Log = log
	w.relay.Agents = func(context.Context) ([]Agent, error) { return nil, errors.New("no board") }
	w.watched(w.clock.Now())
	w.relay.Run(context.Background())
	if !slices.Equal(log.lines, []string{"screens: list the agents: no board"}) {
		t.Errorf("logged %q", log.lines)
	}
	// a board that cannot be listed prunes nothing
	w.relay.Agents = func(context.Context) ([]Agent, error) { return nil, errors.New("no board") }
	if err := w.relay.Round(context.Background()); err == nil {
		t.Errorf("a failed list is not an error")
	}
	if got := w.names(); !slices.Equal(got, []string{"sub", "t0002-w1.txt"}) {
		t.Errorf("a failed list pruned: %v", got)
	}
}

// At most Slots looks run at once, each with its own deadline.
func TestLooksAreBoundedAndTimed(t *testing.T) {
	w := newRelayWorld(t)
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5"} {
		w.agents = append(w.agents, Agent{Name: n, Dir: "/srv"})
		w.scion.screens[n] = n
	}
	w.scion.hold = 20 * time.Millisecond
	w.relay.Round(context.Background())
	if w.scion.most < 1 || w.scion.most > Slots {
		t.Errorf("%d looks at once (at most %d)", w.scion.most, Slots)
	}
	if slices.Contains(w.scion.ctxs, false) || len(w.scion.ctxs) != 5 {
		t.Errorf("a look without a deadline: %v", w.scion.ctxs)
	}
}

// The relay never follows a link: not the screens directory, not a screen
// a link stands in for, and a name that could be a path is never looked at.
func TestTheRelayNeverFollowsLinks(t *testing.T) {
	w := newRelayWorld(t)
	w.agents = []Agent{{Name: "t0001-w1", Dir: "/srv"}, {Name: "../evil", Dir: "/srv"}, {Name: ".hidden", Dir: "/srv"}, {Name: "-n", Dir: "/srv"}}
	w.scion.screens["t0001-w1"] = "screen\n"
	target := filepath.Join(w.root, "config.toml")
	os.WriteFile(target, []byte("keep\n"), 0o600)

	// a link where a screen goes is replaced, the file it points to untouched
	os.MkdirAll(w.relay.Dir, 0o700)
	os.Symlink(target, filepath.Join(w.relay.Dir, "t0001-w1.txt"))
	if err := w.relay.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := w.scion.take(); !slices.Equal(calls, []string{"/srv look t0001-w1 --plain -n 400"}) {
		t.Errorf("looked at %v", calls)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Errorf("wrote through a link: %q", b)
	}
	if st, err := os.Lstat(filepath.Join(w.relay.Dir, "t0001-w1.txt")); err != nil || !st.Mode().IsRegular() {
		t.Errorf("the screen is not a regular file: %v %v", st, err)
	}
	// a linked stale screen is removed as a link
	os.Symlink(target, filepath.Join(w.relay.Dir, "t0002-w1.txt"))
	w.relay.Round(context.Background())
	if b, err := os.ReadFile(target); err != nil || string(b) != "keep\n" {
		t.Errorf("the prune followed a link: %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(w.relay.Dir, "t0002-w1.txt")); !os.IsNotExist(err) {
		t.Errorf("the linked stale screen is still there: %v", err)
	}

	// a linked screens directory: nothing is written or removed through it
	os.RemoveAll(w.relay.Dir)
	elsewhere := filepath.Join(w.root, "elsewhere")
	os.Mkdir(elsewhere, 0o700)
	os.WriteFile(filepath.Join(elsewhere, "old.txt"), nil, 0o600)
	os.Symlink(elsewhere, w.relay.Dir)
	if err := w.relay.Round(context.Background()); err == nil {
		t.Errorf("a linked screens directory is not an error")
	}
	if es, _ := os.ReadDir(elsewhere); len(es) != 1 || es[0].Name() != "old.txt" {
		t.Errorf("wrote or pruned through a linked directory: %v", es)
	}

	// a linked marker is not a watch
	os.Remove(w.relay.Dir)
	os.Remove(w.relay.Marker)
	fresh := filepath.Join(w.root, "fresh")
	os.WriteFile(fresh, nil, 0o600)
	os.Symlink(fresh, w.relay.Marker)
	if w.relay.Watched() {
		t.Errorf("a linked marker counts as a watch")
	}
}

// Read is Foreman's side: a valid name's screen and when it was written,
// never through a link, never over the cap.
func TestReadTheRelayedScreen(t *testing.T) {
	dir := t.TempDir()
	r := Reader{Dir: dir}
	if _, _, err := r.Read("t0001-w1"); err == nil {
		t.Errorf("a missing screen read")
	}
	path := filepath.Join(dir, "t0001-w1.txt")
	os.WriteFile(path, []byte("screen\n"), 0o600)
	at := start.Add(-3 * time.Second)
	os.Chtimes(path, at, at)
	text, when, err := r.Read("t0001-w1")
	if err != nil || text != "screen\n" || !when.Equal(at) {
		t.Errorf("read %q %v %v", text, when, err)
	}
	for _, bad := range []string{"", "../t0001-w1", ".x", "-n", "a/b", strings.Repeat("a", 200)} {
		if _, _, err := r.Read(bad); err == nil {
			t.Errorf("read the name %q", bad)
		}
	}
	os.WriteFile(filepath.Join(dir, "secret"), []byte("s"), 0o600)
	os.Symlink(filepath.Join(dir, "secret"), filepath.Join(dir, "t0002-w1.txt"))
	if _, _, err := r.Read("t0002-w1"); err == nil {
		t.Errorf("read through a link")
	}
	os.WriteFile(filepath.Join(dir, "t0003-w1.txt"), make([]byte, MaxBytes+1), 0o600)
	if _, _, err := r.Read("t0003-w1"); err == nil {
		t.Errorf("read a screen over the cap")
	}
}
