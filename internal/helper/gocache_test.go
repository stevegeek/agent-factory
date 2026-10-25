package helper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gocache"
)

var goBase = strings.Repeat("a", 40)

const goConfig = `
[remote]
image = "localhost/factory-agent:test"
memory = "1g"
cpus = 3
env = { GOFLAGS = "-mod=readonly", ANTHROPIC_AUTH_TOKEN = "never", OTHER_SECRET = "s3cret" }
`

func goMeta(project, toolchain, base string) string {
	return `{"branch": "bot/t0001", "start": "main", "model": "m", "bundle": true, "timeout_minutes": 30, ` +
		`"project": "` + project + `", "toolchain": "` + toolchain + `", "base": "` + base + `"}`
}

// goJob is a job dir for a go project with its bundle, on a host whose
// config is goConfig.
func (r *rig) goJob(t *testing.T, job, meta string) string {
	t.Helper()
	r.write(t, "config.toml", goConfig)
	d := r.job(t, job, meta)
	r.write(t, "jobs/"+job+"/work/repo.bundle", "bundle")
	r.key(t, "sk-test-key")
	return d
}

func warmName(c execx.Cmd) string {
	i := slices.Index(c.Argv, "--name")
	if i < 0 {
		return ""
	}
	return c.Argv[i+1]
}

// trustedCacheMounts are the -v values of a run that name anything under
// {root}/cache/go.
func trustedCacheMounts(root string, argv []string) []string {
	var out []string
	for i, a := range argv {
		if a == "-v" && i+1 < len(argv) && strings.Contains(argv[i+1], filepath.Join(root, "cache", "go")) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// A go project's first job on a host warms the cache for the base head in
// its own container (no credential, no network, the bundle read-only, only
// the scratch dir writable), then the agent gets the generation as an
// overlay: its only view of the trusted cache, whose writes never land.
func TestRunGoJobWarmsThenLendsTheCache(t *testing.T) {
	r := newRig(t)
	d := r.goJob(t, "t0001-w1", goMeta("example-lib", "go", goBase))
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	calls := r.exec.calls
	if len(calls) != 2 || warmName(calls[0]) != "factory-warm-t0001-w1" || warmName(calls[1]) != "factory-job-t0001-w1" {
		t.Fatalf("ran %q", r.exec.argvs())
	}
	cacheDir := filepath.Join(r.Root, "cache", "go", "example-lib")
	warm := calls[0]
	for _, pair := range [][2]string{{"--network", "none"}, {"--memory", "1g"}, {"--cpus", "3"}, {"-w", "/src"}} {
		if i := slices.Index(warm.Argv, pair[0]); i < 0 || warm.Argv[i+1] != pair[1] {
			t.Errorf("warm %s: want %s in %q", pair[0], pair[1], warm.Argv)
		}
	}
	if got := trustedCacheMounts(r.Root, warm.Argv); !slices.Equal(got, []string{cacheDir + "/.warm-aaaaaaaaaaaa:" + gocache.Target}) {
		t.Errorf("warm cache mounts %q: want only the scratch dir", got)
	}
	if !slices.Contains(warm.Argv, d+"/work:/src:ro") || !slices.Contains(warm.Argv, d+"/warm:/job") {
		t.Errorf("the warm does not read the job's input read-only, or has no /job: %q", warm.Argv)
	}
	if onDisk(filepath.Join(d, "warm")) {
		t.Error("the warm's /job is left behind")
	}
	// the warm compiles where the job's checkout will be
	if !strings.Contains(jobSetup, "cd /job\n") || !strings.Contains(jobSetup, "clone -q --no-checkout repo.bundle repo && cd repo") ||
		!strings.Contains(warm.Argv[len(warm.Argv)-1], "repo.bundle "+jobCheckout+"; cd "+jobCheckout+";") {
		t.Errorf("the warm's checkout is not the job's %s", jobCheckout)
	}
	for _, a := range append(slices.Clone(warm.Argv), warm.Env...) {
		for _, secret := range []string{"sk-test-key", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "never", "s3cret", "SECRET_TOKEN"} {
			if strings.Contains(a, secret) {
				t.Errorf("the warm got %q (%s)", a, secret)
			}
		}
	}
	if !slices.Contains(warm.Argv, "GOFLAGS=-mod=readonly") || !strings.Contains(warm.Argv[len(warm.Argv)-1], "checkout -q --detach "+goBase) {
		t.Errorf("warm argv %q", warm.Argv)
	}
	gen := filepath.Join(cacheDir, "aaaaaaaaaaaa")
	job := calls[1]
	if got := trustedCacheMounts(r.Root, job.Argv); !slices.Equal(got, []string{gocache.Mount(gen)}) {
		t.Errorf("job cache mounts %q, want only the overlay of %s", got, gen)
	}
	if !slices.Contains(job.Argv, gocache.Env()) {
		t.Errorf("job env lacks %s: %q", gocache.Env(), job.Argv)
	}
	// every mount of the whole job, by any flag: only the overlay reaches
	// the caches, and it is the read-only lower layer of a throwaway one
	for i, arg := range job.Argv {
		if strings.HasPrefix(arg, "--mount") || strings.HasPrefix(arg, "--volume") || strings.HasPrefix(arg, "-v=") {
			t.Errorf("the job mounts with %q", arg)
		}
		if arg == "-v" && gocache.Reaches(r.Root, job.Argv[i+1]) && job.Argv[i+1] != gen+":"+gocache.Target+":O" {
			t.Errorf("the job mounts %q", job.Argv[i+1])
		}
	}
	if fi, err := os.Lstat(filepath.Join(d, "home", ".cache", "go-build")); err != nil || !fi.IsDir() {
		t.Errorf("the mount point is not made beforehand, as the factory user: %v", err)
	}
	if got := readFile(t, filepath.Join(d, "run.log")); !strings.HasPrefix(got, "build cache warmed for aaaaaaaaaaaa\n") {
		t.Errorf("run.log = %q", got)
	}

	// the next job for that base lends it without a warm
	r.reset()
	r.goJob(t, "t0002-r1", goMeta("example-lib", "go", goBase))
	if code := r.argv(t, "run", "t0002-r1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	if calls := r.exec.calls; len(calls) != 1 || !slices.Equal(trustedCacheMounts(r.Root, calls[0].Argv), []string{gocache.Mount(gen)}) {
		t.Errorf("second job ran %q", r.exec.argvs())
	}
}

// The job holds the generation's lease while its container runs, so no
// prune removes its overlay's lower layer, and gives it up after.
func TestRunGoJobHoldsTheLeaseWhileItRuns(t *testing.T) {
	r := newRig(t)
	r.goJob(t, "t0001-w1", goMeta("example-lib", "go", goBase))
	lease := filepath.Join(r.Root, "cache", "go", "example-lib", ".lease-"+goBase[:12])
	held := false
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		if warmName(c) == "factory-job-t0001-w1" {
			l, err := fsx.TryLock(lease)
			if err == nil {
				l.Unlock()
			}
			held = errors.Is(err, fsx.ErrBusy)
		}
		return execx.Result{}, nil
	}
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	if !held {
		t.Error("the lease is not held while the job runs")
	}
	l, err := fsx.TryLock(lease)
	if err != nil {
		t.Fatalf("the lease is still held after the job: %v", err)
	}
	l.Unlock()
}

// A warm that fails is not the job's failure: the job runs cold (with no
// generation yet) and its own exit is what counts.
func TestRunGoJobWhoseWarmFailsRunsCold(t *testing.T) {
	r := newRig(t)
	d := r.goJob(t, "t0001-w1", goMeta("example-lib", "go", goBase))
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		if strings.HasPrefix(warmName(c), "factory-warm-") {
			return execx.Result{Code: 1, Stderr: []byte("vet: broken")}, nil
		}
		return execx.Result{Code: 0, Stdout: []byte("agent ran\n")}, nil
	}
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	calls := r.exec.calls
	if len(calls) != 2 {
		t.Fatalf("ran %q", r.exec.argvs())
	}
	if got := trustedCacheMounts(r.Root, calls[1].Argv); len(got) != 0 || slices.Contains(calls[1].Argv, gocache.Env()) {
		t.Errorf("a cold job got %q", got)
	}
	if got := readFile(t, filepath.Join(d, "exit")); got != "0" {
		t.Errorf("exit = %q", got)
	}
	log := readFile(t, filepath.Join(d, "run.log"))
	if !strings.Contains(log, "running cold") || !strings.HasSuffix(log, "agent ran\n") {
		t.Errorf("run.log = %q", log)
	}
	if got := readFile(t, filepath.Join(d, "warm.log")); !strings.Contains(got, "vet: broken") {
		t.Errorf("warm.log = %q", got)
	}
}

// While another job on this host warms the project's cache (it holds the
// lock), a job neither waits nor warms: it runs cold, and its exit is its
// own.
func TestRunGoJobWhoseWarmIsLockedRunsCold(t *testing.T) {
	r := newRig(t)
	d := r.goJob(t, "t0001-w1", goMeta("example-lib", "go", goBase))
	dir := filepath.Join(gocache.Root(r.Root), "example-lib")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := fsx.TryLock(filepath.Join(dir, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Code: 0, Stdout: []byte("agent ran\n")}, nil
	}
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	if len(r.exec.calls) != 1 || strings.HasPrefix(warmName(r.exec.calls[0]), "factory-warm-") {
		t.Fatalf("ran %q", r.exec.argvs())
	}
	if got := trustedCacheMounts(r.Root, r.exec.calls[0].Argv); len(got) != 0 || slices.Contains(r.exec.calls[0].Argv, gocache.Env()) {
		t.Errorf("a cold job got %q", got)
	}
	if got := readFile(t, filepath.Join(d, "exit")); got != "0" {
		t.Errorf("exit = %q", got)
	}
	if log := readFile(t, filepath.Join(d, "run.log")); !strings.Contains(log, "running cold") {
		t.Errorf("run.log = %q", log)
	}
}

// What an older dispatcher sends (no project, toolchain or base), another
// toolchain, a job with no bundle: no warm, and the run never names the
// cache, exactly as before.
func TestRunJobWithoutAGoProjectIsCold(t *testing.T) {
	for _, c := range []struct{ name, meta string }{
		{"old dispatcher", goodMeta},
		{"ruby", goMeta("example-gem", "ruby", goBase)},
		{"no base", `{"branch": "bot/t0001", "start": "main", "project": "example-lib", "toolchain": "go"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			d := r.goJob(t, "t0001-w1", c.meta)
			if code := r.argv(t, "run", "t0001-w1"); code != 0 {
				t.Fatalf("run: exit %d: %s", code, r.stderr)
			}
			if len(r.exec.calls) != 1 {
				t.Fatalf("ran %q", r.exec.argvs())
			}
			for _, a := range r.exec.calls[0].Argv {
				if strings.Contains(a, "cache/go") || strings.Contains(a, "GOCACHE") {
					t.Errorf("a cold job names the cache: %q", a)
				}
			}
			if onDisk(filepath.Join(r.Root, "cache", "go")) || onDisk(filepath.Join(d, "warm.log")) {
				t.Error("a cold job made cache files")
			}
		})
	}
	r := newRig(t)
	r.goJob(t, "t0001-w1", goMeta("example-lib", "go", goBase))
	os.Remove(filepath.Join(r.jobDir("t0001-w1"), "work", "repo.bundle"))
	if code := r.argv(t, "run", "t0001-w1"); code != 0 || len(r.exec.calls) != 1 {
		t.Errorf("no bundle: exit %d, ran %q", code, r.exec.argvs())
	}
}

// A go job's warm comes out of the job's own time, which the dispatcher
// counts from the start: the warm gets at most a quarter of it, the
// agent's container what the warm left, and the unit no more than any
// job's.
func TestAGoJobsWarmAndAgentKeepToItsTimeout(t *testing.T) {
	r := newRig(t)
	if code := r.start(t, "t0001-w1", jobInput(t, goMeta("example-lib", "go", goBase))); code != 0 {
		t.Fatalf("start: %d %s", code, r.stderr)
	}
	if got := r.exec.argvs()[0][5]; got != "--property=RuntimeMaxSec=2100" {
		t.Errorf("unit limit %s, want 30*60+300", got)
	}

	r = newRig(t)
	clk := clock.NewStep(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), 0)
	r.Clock = clk
	d := r.goJob(t, "t0001-w1", goMeta("example-lib", "go", goBase))
	var warmLeft time.Duration
	fake := r.exec
	r.Exec = execx.Func(func(ctx context.Context, c execx.Cmd) (execx.Result, error) {
		if warmName(c) == "factory-warm-t0001-w1" {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Error("the warm has no deadline")
			}
			warmLeft = time.Until(dl)
			clk.Advance(7 * time.Minute) // the warm took 7 of the job's 30 minutes
		}
		return fake.Run(ctx, c)
	})
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	if warmLeft <= 0 || warmLeft > 30*time.Minute/4 {
		t.Errorf("the warm may take %v of a 30-minute job", warmLeft)
	}
	calls := fake.calls
	if len(calls) != 2 {
		t.Fatalf("ran %q", fake.argvs())
	}
	timeout := func(c execx.Cmd) string {
		if j := slices.Index(c.Argv, "--timeout"); j >= 0 {
			return c.Argv[j+1]
		}
		return ""
	}
	if got, err := strconv.Atoi(timeout(calls[0])); err != nil || got < 1 || got > 450 {
		t.Errorf("the warm's podman --timeout %q, want at most a quarter of 30 minutes", timeout(calls[0]))
	}
	if got := timeout(calls[1]); got != "1380" {
		t.Errorf("the agent's --timeout %q, want 1380 (30 minutes less the warm's 7)", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(d, "run.log")), "build cache warmed") {
		t.Error("the warm did not lend")
	}
}

// A warm cannot leave the agent less than a minute, whatever it took.
func TestAgentTimeout(t *testing.T) {
	for _, c := range []struct {
		minutes int
		warmed  time.Duration
		want    int
	}{{45, 0, 2700}, {45, 12 * time.Minute, 1980}, {1, 5 * time.Minute, 60}} {
		if got := agentTimeout(c.minutes, c.warmed); got != c.want {
			t.Errorf("agentTimeout(%d, %v) = %d, want %d", c.minutes, c.warmed, got, c.want)
		}
	}
	if got := warmBudget(120); got != 30*time.Minute {
		t.Errorf("warmBudget(120) = %v", got)
	}
	if got := warmBudget(45); got != 45*time.Minute/4 {
		t.Errorf("warmBudget(45) = %v", got)
	}
}

// What a killed earlier attempt left in the job's warm dir is removed
// first, so the warm still runs.
func TestRunGoJobWarmsOverALeftWarmDir(t *testing.T) {
	r := newRig(t)
	d := r.goJob(t, "t0001-w1", goMeta("example-lib", "go", goBase))
	r.write(t, "jobs/t0001-w1/warm/repo/stale", "x")
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	if got := readFile(t, filepath.Join(d, "run.log")); !strings.HasPrefix(got, "build cache warmed") {
		t.Errorf("run.log = %q", got)
	}
}

// A [remote] mount that reaches the trusted caches (the dir, a parent, or
// anything below) would let a job write them: the job is refused.
func TestRunRefusesAMountOfTheTrustedCache(t *testing.T) {
	for _, m := range []string{"{root}/cache/go:/x", "{root}/cache:/x", "{root}:/x", "{root}/cache/go/example-lib/current:/x:O", "{root}/./cache//go:/x"} {
		r := newRig(t)
		r.write(t, "config.toml", "[remote]\nmounts = [\""+m+"\"]\n")
		d := r.job(t, "t0001-w1", goodMeta)
		r.key(t, "k")
		if code := r.argv(t, "run", "t0001-w1"); code == 0 || len(r.exec.calls) != 0 {
			t.Errorf("%s: exit %d, ran %q", m, code, r.exec.argvs())
		}
		if got := readFile(t, filepath.Join(d, "exit")); got != "255" {
			t.Errorf("%s: exit file %q", m, got)
		}
	}
	r := newRig(t)
	r.write(t, "config.toml", "[remote]\nmounts = [\"{root}/cache/bundle:/home/scion/.bundle-cache\", \"{root}/cache/gopher:/x\"]\n")
	r.job(t, "t0001-w1", goodMeta)
	r.key(t, "k")
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Errorf("a mount beside the caches was refused: %s", r.stderr)
	}
}
