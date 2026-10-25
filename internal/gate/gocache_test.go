package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gocache"
	"github.com/stevegeek/agent-factory/internal/podman"
)

const goProject = "kind = \"go-module\"\n"

// cacheMounts are the mounts of a gate run that name the trusted caches.
func cacheMounts(root string, g podman.Gate) []string {
	var out []string
	for _, m := range g.Mounts {
		if strings.Contains(m, gocache.Root(root)) {
			out = append(out, m)
		}
	}
	return out
}

// A go project's gate first warms this host's cache from the base head:
// a fresh clone of that commit, read-only at /work where the checks will
// run, in a warm container that writes only a scratch cache. The checks
// then get the generation as an overlay, never writable.
func TestGoGateWarmsFromTheBaseAndLendsAnOverlay(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "branch\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config(goProject, "[gate]\nenv = { GOFLAGS = \"-mod=readonly\", SECRET = \"s\" }\n")
	r.podman.warm = func(w podman.Warm) {
		if got := r.git(w.Source, "rev-parse", "HEAD"); got != base {
			t.Errorf("the warm's source is at %s, not the base %s", got, base)
		}
		if b, _ := os.ReadFile(filepath.Join(w.Source, "README.md")); string(b) != "hello\n" {
			t.Errorf("the warm's source has %q", b)
		}
	}
	ok, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b", Base: base})
	if err != nil || !ok {
		t.Fatalf("Checks = %v %v", ok, err)
	}
	if len(r.podman.warms) != 1 {
		t.Fatalf("warmed %d times", len(r.podman.warms))
	}
	w := r.podman.warms[0]
	dir := filepath.Join(r.paths.Root, "cache", "go", "gem")
	if w.Cache != filepath.Join(dir, ".warm-"+base[:12]) || w.Target != gocache.Target || w.SourceAt != "/work" ||
		w.Script != gocache.Compile || w.Image != "localhost/gate:test" || w.Memory != "900m" || w.Name != "factory-warm-gem" {
		t.Errorf("warm %+v", w)
	}
	if !slices.Equal(w.Env, []string{"GOFLAGS=-mod=readonly"}) {
		t.Errorf("warm env %q", w.Env)
	}
	if exists(w.Source) {
		t.Error("the warm's checkout is still there")
	}
	g := r.podman.runs[0]
	gen := filepath.Join(dir, base[:12])
	if got := cacheMounts(r.paths.Root, g); !slices.Equal(got, []string{gocache.Mount(gen)}) {
		t.Errorf("gate cache mounts %q, want only the overlay of %s", got, gen)
	}
	if !slices.Contains(g.Env, gocache.Env()) {
		t.Errorf("gate env %q", g.Env)
	}

	// the next gate on that base is lent it without a warm
	if _, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0010", Branch: "b", Base: base}); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.warms) != 1 || !slices.Equal(cacheMounts(r.paths.Root, r.podman.runs[1]), []string{gocache.Mount(gen)}) {
		t.Errorf("second gate: %d warms, mounts %q", len(r.podman.warms), r.podman.runs[1].Mounts)
	}
	// a check with no base (the merge queue's) is lent the cache as it is,
	// and warms nothing
	if _, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "merge-gem", Ref: base}); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.warms) != 1 || !slices.Equal(cacheMounts(r.paths.Root, r.podman.runs[2]), []string{gocache.Mount(gen)}) {
		t.Errorf("no-base check: %d warms, mounts %q", len(r.podman.warms), r.podman.runs[2].Mounts)
	}
}

// Gate 1 itself warms from the base head it took, and its real podman argv
// mounts the generation with :O.
func TestGateRunLendsTheCacheWarmedFromItsBase(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"lib/thing.rb": "changed\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config(goProject, "")
	var gateArgv []string
	var warmed []string
	r.runner.Podman = podman.Client{Exec: execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
		switch name := c.Argv[slices.Index(c.Argv, "--name")+1]; {
		case strings.HasPrefix(name, "factory-warm-"):
			warmed = append(warmed, c.Argv[len(c.Argv)-1])
		case strings.HasPrefix(name, "factory-gate-"):
			gateArgv = c.Argv
		}
		return execx.Result{Argv: c.Argv}, nil
	})}
	tk := gateTicket(t, `{"work": {"status": "done", "summary": "s", "category": "fix"}}`)
	if _, err := r.runner.Run(t.Context(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if len(warmed) != 1 || warmed[0] != gocache.Compile {
		t.Fatalf("warmed %q", warmed)
	}
	gen := filepath.Join(r.paths.Root, "cache", "go", "gem", base[:12])
	i := slices.Index(gateArgv, gocache.Mount(gen))
	if i < 1 || gateArgv[i-1] != "-v" || !slices.Contains(gateArgv, gocache.Env()) {
		t.Errorf("gate argv %q lacks the overlay of %s", gateArgv, gen)
	}
	for _, a := range gateArgv {
		if strings.Contains(a, gocache.Root(r.paths.Root)) && !strings.HasSuffix(a, ":O") {
			t.Errorf("the gate mounts the trusted cache writable: %q", a)
		}
	}
}

// A warm that fails is a cold gate, never a failed or broken one.
func TestGoGateWhoseWarmFailsRunsCold(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config(goProject, "")
	for _, c := range []struct {
		name string
		ok   bool
		err  error
	}{{"exit", false, nil}, {"error", true, errors.New("podman went away")}} {
		r.podman.warmOK, r.podman.warmErr = c.ok, c.err
		r.podman.runs = nil
		ok, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b", Base: base})
		if err != nil || !ok {
			t.Fatalf("%s: Checks = %v %v", c.name, ok, err)
		}
		if got := cacheMounts(r.paths.Root, r.podman.runs[0]); len(got) != 0 || slices.Contains(r.podman.runs[0].Env, gocache.Env()) {
			t.Errorf("%s: a cold gate got %q", c.name, got)
		}
	}
	if !slices.ContainsFunc(r.log.lines, func(l string) bool { return strings.Contains(l, "running cold") }) {
		t.Errorf("log %q", r.log.lines)
	}
}

// While another run warms this project's cache (it holds the lock), a
// gate does not wait and does not warm: with no generation it runs cold,
// and once there is one it is lent that one as it is, still only as an
// overlay. Neither is a failed or broken gate.
func TestGoGateWhoseWarmIsLockedRunsCold(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config(goProject, "")
	dir := filepath.Join(gocache.Root(r.paths.Root), "gem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := fsx.TryLock(filepath.Join(dir, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	ok, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b", Base: base})
	if err != nil || !ok || len(r.podman.warms) != 0 {
		t.Fatalf("locked, no generation: %v %v, %d warms", ok, err, len(r.podman.warms))
	}
	if got := cacheMounts(r.paths.Root, r.podman.runs[0]); len(got) != 0 || slices.Contains(r.podman.runs[0].Env, gocache.Env()) {
		t.Errorf("a cold gate got %q", got)
	}
	held.Unlock()
	if _, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0010", Branch: "b", Base: base}); err != nil {
		t.Fatal(err)
	}
	gen := filepath.Join(dir, base[:12])
	r.repo.Write("lib/next.rb", "next\n")
	r.repo.Commit("next")
	next := r.git(r.repo.Dir, "rev-parse", "main")
	if held, err = fsx.TryLock(filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	ok, _, err = r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0011", Branch: "b", Base: next})
	if err != nil || !ok || len(r.podman.warms) != 1 {
		t.Fatalf("locked over a generation: %v %v, %d warms", ok, err, len(r.podman.warms))
	}
	if got := cacheMounts(r.paths.Root, r.podman.runs[2]); !slices.Equal(got, []string{gocache.Mount(gen)}) {
		t.Errorf("locked over %s: mounts %q", gen, got)
	}
}

// A project that is not go never warms or names the caches.
func TestOtherGatesHaveNoCache(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config("", "")
	if _, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b", Base: base}); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.warms) != 0 || len(cacheMounts(r.paths.Root, r.podman.runs[0])) != 0 || exists(filepath.Join(r.paths.Root, "cache", "go")) {
		t.Errorf("warms %d, mounts %q", len(r.podman.warms), r.podman.runs[0].Mounts)
	}
}

// A [gate] mount that reaches the trusted caches, or the early checks'
// kept results, is refused: no gate runs with a writable view of them, so
// no branch's tests can forge a result another gate would use.
func TestGateRefusesAMountOfTheTrustedCache(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	for _, m := range []string{"{root}/cache/go:/x", "{root}/cache:/x", "{root}/cache/go/gem/current:/x:O",
		"{root}/gate-early:/x", "{root}/gate-early/T0001.json:/x:ro"} {
		cfg, p := r.config("", "[gate]\nmounts = [\""+m+"\"]\n")
		r.podman.runs = nil
		if _, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b"}); err == nil || len(r.podman.runs) != 0 {
			t.Errorf("%s: err %v, ran %d", m, err, len(r.podman.runs))
		}
	}
}

// The checks hold the generation's lease while their container runs, so
// no prune removes their overlay's lower layer, and give it up after.
func TestGoGateHoldsTheLeaseWhileTheChecksRun(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config(goProject, "")
	lease := filepath.Join(r.paths.Root, "cache", "go", "gem", ".lease-"+base[:12])
	r.podman.inspect = func(podman.Gate) {
		if l, err := fsx.TryLock(lease); !errors.Is(err, fsx.ErrBusy) {
			if err == nil {
				l.Unlock()
			}
			t.Errorf("the lease is not held while the checks run: %v", err)
		}
	}
	if _, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b", Base: base}); err != nil {
		t.Fatal(err)
	}
	l, err := fsx.TryLock(lease)
	if err != nil {
		t.Fatalf("the lease is still held after the checks: %v", err)
	}
	l.Unlock()
}

// Under the tick's deadline the warm gets only what leaves the checks
// their whole GateTimeout and a margin; with less than a minute of that,
// nothing is warmed and the cache is lent as it was.
func TestGoGateWarmsWithinTheTicksDeadline(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config(goProject, "")
	var left time.Duration
	r.podman.warmCtx = func(ctx context.Context) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("the warm has no deadline")
		}
		left = time.Until(dl)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()
	if _, _, err := r.runner.Checks(ctx, CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b", Base: base}); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.warms) != 1 || left <= 0 || left > 40*time.Minute-podman.GateTimeout-warmMargin {
		t.Errorf("%d warms, with %v left of a tick with 40 minutes", len(r.podman.warms), left)
	}
	gen := filepath.Join(r.paths.Root, "cache", "go", "gem", base[:12])

	// a new base, with 31 minutes left: no warm, the cache as it was
	other := strings.Repeat("b", 40)
	ctx, cancel = context.WithTimeout(t.Context(), 31*time.Minute)
	defer cancel()
	if _, _, err := r.runner.Checks(ctx, CheckIn{Config: cfg, Project: p, ID: "T0010", Branch: "b", Base: other}); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.warms) != 1 || !slices.Equal(cacheMounts(r.paths.Root, r.podman.runs[1]), []string{gocache.Mount(gen)}) {
		t.Errorf("%d warms, mounts %q", len(r.podman.warms), r.podman.runs[1].Mounts)
	}
	if !slices.ContainsFunc(r.log.lines, func(l string) bool { return strings.Contains(l, "too little time") }) {
		t.Errorf("log %q", r.log.lines)
	}
}

// A check in a candidate image (the rebuild's, the trial's) runs cold:
// compile output the live image made could hide what the candidate's
// toolchain breaks.
func TestGoCheckInACandidateImageRunsCold(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	base := r.git(r.repo.Dir, "rev-parse", "main")
	cfg, p := r.config(goProject, "")
	if _, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "T0009", Branch: "b", Base: base}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.runner.Checks(t.Context(), CheckIn{Config: cfg, Project: p, ID: "rebuild-gem", Ref: base, Image: "localhost/gate:candidate"}); err != nil {
		t.Fatal(err)
	}
	g := r.podman.runs[1]
	if got := cacheMounts(r.paths.Root, g); len(got) != 0 || slices.Contains(g.Env, gocache.Env()) || len(r.podman.warms) != 1 {
		t.Errorf("a candidate's check got %q, %d warms", got, len(r.podman.warms))
	}
}
