package podman

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
)

type recorder struct {
	calls []execx.Cmd
	// answer, when set, gives the result of a call
	answer func(c execx.Cmd) (execx.Result, error)
}

func (r *recorder) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	r.calls = append(r.calls, c)
	if r.answer != nil {
		res, err := r.answer(c)
		res.Argv = c.Argv
		return res, err
	}
	return execx.Result{Argv: c.Argv}, nil
}

func (r *recorder) argvs() [][]string {
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.Argv
	}
	return out
}

func TestImageCommandsArgv(t *testing.T) {
	r := &recorder{}
	p := Client{Exec: r, Dir: "/home/f"}
	ctx := context.Background()
	if _, err := p.Build(ctx, Build{Tag: "localhost/a:candidate", Context: "/f/images/scion-agent",
		Args: []string{"CLAUDE_VERSION=2.1.296", "CRYSTAL_VERSION=1.20.1"}}); err != nil {
		t.Fatal(err)
	}
	p.Tag(ctx, "localhost/a:scion", "localhost/a:previous")
	p.Rmi(ctx, "localhost/a:candidate")
	p.Prune(ctx)
	want := [][]string{
		{"podman", "build", "--pull=newer", "--no-cache", "--build-arg", "CLAUDE_VERSION=2.1.296", "--build-arg", "CRYSTAL_VERSION=1.20.1",
			"-t", "localhost/a:candidate", "/f/images/scion-agent"},
		{"podman", "tag", "localhost/a:scion", "localhost/a:previous"},
		{"podman", "rmi", "localhost/a:candidate"},
		{"podman", "image", "prune", "-f"},
	}
	if !reflect.DeepEqual(r.argvs(), want) {
		t.Fatalf("argv\n got %q\nwant %q", r.argvs(), want)
	}
	if r.calls[0].Timeout != BuildTimeout || r.calls[0].Dir != "/home/f" {
		t.Errorf("build: timeout %v dir %q", r.calls[0].Timeout, r.calls[0].Dir)
	}
	if r.calls[1].Dir != "" {
		t.Errorf("tag runs in no directory, as Python: %q", r.calls[1].Dir)
	}
}

func TestImageExists(t *testing.T) {
	r := &recorder{answer: func(c execx.Cmd) (execx.Result, error) {
		if c.Argv[3] == "there" {
			return execx.Result{}, nil
		}
		return execx.Result{Code: 1}, nil
	}}
	p := Client{Exec: r}
	ctx := context.Background()
	if ok, err := p.ImageExists(ctx, "there"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, err := p.ImageExists(ctx, "gone"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if !reflect.DeepEqual(r.calls[0].Argv, []string{"podman", "image", "exists", "there"}) {
		t.Fatalf("%q", r.calls[0].Argv)
	}
	if _, err := (Client{Exec: &recorder{answer: func(execx.Cmd) (execx.Result, error) {
		return execx.Result{}, errors.New("no podman")
	}}}).ImageExists(ctx, "x"); err == nil {
		t.Fatal("a podman that did not run is an error, not a missing image")
	}
}

// rebuild_image's smoke test, with the container named so a
// cancelled run can be removed.
func TestSmokeArgv(t *testing.T) {
	r := &recorder{answer: func(execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: []byte("2.1.296 (Claude Code)\n"), Stderr: []byte("warn\n")}, nil
	}}
	p := Client{Exec: r, Dir: "/home/f"}
	res, err := p.RunSmoke(context.Background(), Smoke{Name: "factory-smoke-claude", Image: "localhost/a:candidate", Script: "ruby -v && claude --version"})
	if err != nil || res.Out() != "2.1.296 (Claude Code)\n" {
		t.Fatalf("%q %v", res.Out(), err)
	}
	want := []string{"podman", "run", "--rm", "--name", "factory-smoke-claude", "--network", "none", "--userns=keep-id:uid=1000,gid=1000", "--user", "1000:1000",
		"--entrypoint", "/bin/sh", "localhost/a:candidate", "-c", "ruby -v && claude --version"}
	c := r.calls[0]
	if !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", c.Argv, want)
	}
	if c.Timeout != SmokeTimeout || c.Dir != "/home/f" || c.Cleanup == nil {
		t.Errorf("timeout %v dir %q cleanup %v", c.Timeout, c.Dir, c.Cleanup != nil)
	}
}

// checks_on_fresh_checkout's container: no network, keep-id user,
// the memory limit, the gate's env and mounts in order, the clone at /work.
func TestGateArgv(t *testing.T) {
	r := &recorder{}
	p := Client{Exec: r}
	ok, _, err := p.RunGate(context.Background(), Gate{
		Name: "factory-gate-T0001", Image: "localhost/a:scion", Memory: "900m",
		Env: []string{"BUNDLE_PATH=/cache", "CI=1"}, Mounts: []string{"/f/cache/bundle:/cache:ro"},
		Workdir: "/f/gate/T0001", Script: "set -e; bundle install; rake; rubocop"})
	if !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	want := []string{"podman", "run", "--rm", "--name", "factory-gate-T0001", "--entrypoint", "", "--network", "none",
		"--userns=keep-id:uid=1000,gid=1000", "--user", "1000:1000", "--memory", "900m",
		"-e", "BUNDLE_PATH=/cache", "-e", "CI=1", "-v", "/f/cache/bundle:/cache:ro",
		"-v", "/f/gate/T0001:/work", "-w", "/work", "localhost/a:scion", "sh", "-c", "set -e; bundle install; rake; rubocop"}
	c := r.calls[0]
	if !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", c.Argv, want)
	}
	if c.Timeout != GateTimeout || c.Cleanup == nil {
		t.Errorf("timeout %v cleanup %v", c.Timeout, c.Cleanup != nil)
	}
}

func TestGateOutcomeAndTail(t *testing.T) {
	long := strings.Repeat("é", 5000)
	r := &recorder{answer: func(execx.Cmd) (execx.Result, error) {
		return execx.Result{Code: 2, Stdout: []byte(long), Stderr: []byte("rake aborted")}, nil
	}}
	ok, tail, err := Client{Exec: r}.RunGate(context.Background(), Gate{Name: "factory-gate-T1", Image: "i", Memory: "1g", Workdir: "/w", Script: "x"})
	if ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	// (r.stdout + r.stderr)[-4000:]: code points, not bytes
	if want := strings.Repeat("é", 4000-len("rake aborted")) + "rake aborted"; tail != want {
		t.Fatalf("tail has %d runes", len([]rune(tail)))
	}
}

func TestGateRefusesBadValues(t *testing.T) {
	for _, g := range []Gate{
		{Name: "", Image: "i", Memory: "1g", Workdir: "/w", Script: "x"},
		{Name: "-x", Image: "i", Memory: "1g", Workdir: "/w", Script: "x"},
		{Name: "a b", Image: "i", Memory: "1g", Workdir: "/w", Script: "x"},
		{Name: "n", Image: "-i", Memory: "1g", Workdir: "/w", Script: "x"},
		{Name: "n", Image: "", Memory: "1g", Workdir: "/w", Script: "x"},
		{Name: "n", Image: "i", Memory: "-1", Workdir: "/w", Script: "x"},
		{Name: "n", Image: "i", Memory: "1g", Workdir: "", Script: "x"},
		{Name: "n", Image: "i", Memory: "1g", Workdir: "/w:/etc", Script: "x"},
	} {
		r := &recorder{}
		if _, _, err := (Client{Exec: r}).RunGate(context.Background(), g); err == nil || len(r.calls) != 0 {
			t.Errorf("%+v ran", g)
		}
	}
}

// A warm's container: no network, no capability, the keep-id user, the
// source read-only, and the scratch cache the only writable mount; nothing
// in its environment but HOME and what the caller passes.
func TestWarmArgv(t *testing.T) {
	r := &recorder{}
	p := Client{Exec: r, Env: []string{"PATH=/usr/bin"}}
	ok, _, err := p.RunWarm(context.Background(), Warm{
		Name: "factory-warm-example", Image: "localhost/a:scion", Memory: "900m", CPUs: "2",
		Env: []string{"GOFLAGS=-mod=readonly"}, Cache: "/f/cache/go/example/.warm-aaaaaaaaaaaa", Target: "/home/scion/.cache/go-build",
		Source: "/f/jobs/j/work", SourceAt: "/src", Work: "/f/jobs/j/warm", WorkAt: "/job", Script: "go build ./..."})
	if !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	want := []string{"podman", "run", "--rm", "--name", "factory-warm-example", "--network", "none",
		"--cap-drop=all", "--security-opt=no-new-privileges", "--userns=keep-id:uid=1000,gid=1000", "--user", "1000:1000",
		"--memory", "900m", "--pids-limit", "512", "--cpus", "2", "--timeout", "1800",
		"-e", "HOME=/tmp", "-e", "GOCACHE=/home/scion/.cache/go-build", "-e", "GOFLAGS=-mod=readonly",
		"-v", "/f/cache/go/example/.warm-aaaaaaaaaaaa:/home/scion/.cache/go-build",
		"-v", "/f/jobs/j/work:/src:ro", "-v", "/f/jobs/j/warm:/job", "-w", "/src",
		"--entrypoint", "/bin/sh", "localhost/a:scion", "-c", "go build ./..."}
	c := r.calls[0]
	if !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", c.Argv, want)
	}
	if c.Timeout != WarmTimeout || c.Cleanup == nil || !reflect.DeepEqual(c.Env, []string{"PATH=/usr/bin"}) {
		t.Errorf("timeout %v cleanup %v env %q", c.Timeout, c.Cleanup != nil, c.Env)
	}
	r.answer = func(execx.Cmd) (execx.Result, error) { return execx.Result{Code: 1, Stderr: []byte("vet failed")}, nil }
	ok, tail, err := p.RunWarm(context.Background(), Warm{Name: "n", Image: "i", Memory: "1g", Cache: "/c", Target: "/t", Source: "/s", SourceAt: "/work", Script: "x"})
	if ok || err != nil || tail != "vet failed" {
		t.Errorf("a failed warm: %v %q %v", ok, tail, err)
	}
	if a := r.calls[1].Argv; slices.Contains(a, "--cpus") || slices.Contains(a, "/job") || a[slices.Index(a, "-w")+1] != "/work" {
		t.Errorf("no cpus or work dir asked, got %q", a)
	}
}

// A warm under a context that ends sooner than WarmTimeout gets what is
// left, as podman's --timeout too: a warm outlives no caller's deadline.
func TestWarmTimeoutFollowsTheContext(t *testing.T) {
	r := &recorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute+500*time.Millisecond)
	defer cancel()
	w := Warm{Name: "n", Image: "i", Memory: "1g", Cache: "/c", Target: "/t", Source: "/s", SourceAt: "/src", Script: "x"}
	if _, _, err := (Client{Exec: r}).RunWarm(ctx, w); err != nil {
		t.Fatal(err)
	}
	c := r.calls[0]
	if i := slices.Index(c.Argv, "--timeout"); i < 0 || c.Argv[i+1] != "600" || c.Timeout != 600*time.Second {
		t.Errorf("timeout %v, argv %q", c.Timeout, c.Argv)
	}
}

func TestWarmRefusesBadValues(t *testing.T) {
	good := Warm{Name: "n", Image: "i", Memory: "1g", Cache: "/c", Target: "/t", Source: "/s", SourceAt: "/src", Script: "x"}
	for _, change := range []func(w *Warm){
		func(w *Warm) { w.Name = "-n" },
		func(w *Warm) { w.Image = "" },
		func(w *Warm) { w.Memory = "-1" },
		func(w *Warm) { w.CPUs = "-2" },
		func(w *Warm) { w.Cache = "" },
		func(w *Warm) { w.Cache = "/c:/etc" },
		func(w *Warm) { w.Cache = "/c,O" },
		func(w *Warm) { w.Cache = "relative" },
		func(w *Warm) { w.Target = "t" },
		func(w *Warm) { w.Source = "/s:/x" },
		func(w *Warm) { w.Source = "" },
		func(w *Warm) { w.SourceAt = "" },
		func(w *Warm) { w.Work = "/w" },
		func(w *Warm) { w.Work, w.WorkAt = "/w:/x", "/job" },
		func(w *Warm) { w.Env = []string{"NOEQUALS"} },
	} {
		w := good
		change(&w)
		r := &recorder{}
		if _, _, err := (Client{Exec: r}).RunWarm(context.Background(), w); err == nil || len(r.calls) != 0 {
			t.Errorf("%+v ran", w)
		}
	}
}

// a cancelled or timed-out run leaves no container behind: Cleanup
// removes it by name, bounded by its own timeout.
func TestCleanupRemovesTheNamedContainer(t *testing.T) {
	r := &recorder{answer: func(c execx.Cmd) (execx.Result, error) {
		if c.Argv[1] == "run" {
			return execx.Result{}, context.Canceled
		}
		return execx.Result{}, nil
	}}
	p := Client{Exec: r}
	_, _, err := p.RunGate(context.Background(), Gate{Name: "factory-gate-T1", Image: "i", Memory: "1g", Workdir: "/w", Script: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	r.calls[0].Cleanup() // what execx.OS does after a cancelled run
	got := r.calls[len(r.calls)-1]
	if !reflect.DeepEqual(got.Argv, []string{"podman", "rm", "-f", "--time", "5", "factory-gate-T1"}) {
		t.Fatalf("cleanup argv %q", got.Argv)
	}
	if got.Timeout <= 0 || got.Timeout > time.Minute {
		t.Fatalf("cleanup timeout %v", got.Timeout)
	}
}

func TestRunning(t *testing.T) {
	r := &recorder{answer: func(execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: []byte("factory-gate-T1\nt0001-w1\n\nfactory-smoke-pi\n")}, nil
	}}
	got, err := Client{Exec: r}.Running(context.Background(), "factory-")
	if err != nil || !reflect.DeepEqual(got, []string{"factory-gate-T1", "factory-smoke-pi"}) {
		t.Fatalf("%q %v", got, err)
	}
	if !reflect.DeepEqual(r.calls[0].Argv, []string{"podman", "ps", "--format", "{{.Names}}"}) {
		t.Fatalf("%q", r.calls[0].Argv)
	}
	all, _ := Client{Exec: r}.Running(context.Background(), "")
	if len(all) != 3 {
		t.Fatalf("%q", all)
	}
	r.answer = func(execx.Cmd) (execx.Result, error) { return execx.Result{Code: 125, Stderr: []byte("no")}, nil }
	if _, err := (Client{Exec: r}).Running(context.Background(), ""); err == nil {
		t.Fatal("a failed ps is an error")
	}
}

func TestPassEnvKeepsOnlyWhatPodmanNeeds(t *testing.T) {
	got := PassEnv([]string{"PATH=/bin", "HOME=/h", "XDG_RUNTIME_DIR=/run/user/1", "DBUS_SESSION_BUS_ADDRESS=unix:x",
		"CLAUDE_CODE_OAUTH_TOKEN=t", "ANTHROPIC_API_KEY=k", "FACTORY_GH_TOKEN=g", "TMPDIR=/tmp/x"})
	want := []string{"PATH=/bin", "HOME=/h", "XDG_RUNTIME_DIR=/run/user/1", "DBUS_SESSION_BUS_ADDRESS=unix:x", "TMPDIR=/tmp/x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestEnvIsPassedToEveryCall(t *testing.T) {
	r := &recorder{}
	p := Client{Exec: r, Env: []string{"HOME=/h"}}
	ctx := context.Background()
	p.Tag(ctx, "a", "b")
	p.RunGate(ctx, Gate{Name: "n", Image: "i", Memory: "1g", Workdir: "/w", Script: "x"})
	for _, c := range r.calls {
		if !reflect.DeepEqual(c.Env, []string{"HOME=/h"}) {
			t.Errorf("%q env %q", c.Argv, c.Env)
		}
	}
}

func TestBinOverride(t *testing.T) {
	r := &recorder{}
	Client{Exec: r, Bin: "/usr/bin/podman"}.Prune(context.Background())
	if r.calls[0].Argv[0] != "/usr/bin/podman" {
		t.Fatalf("%q", r.calls[0].Argv)
	}
}

// ImageID is the image a tag names now, so a result can be tied to the
// image itself rather than to a tag a rebuild moves; anything but one
// plain id is an error, and podman gets a short timeout: a tick asks.
func TestImageID(t *testing.T) {
	id := strings.Repeat("ab", 32)
	r := &recorder{answer: func(c execx.Cmd) (execx.Result, error) {
		switch c.Argv[len(c.Argv)-1] {
		case "there":
			return execx.Result{Stdout: []byte(id + "\n")}, nil
		case "odd":
			return execx.Result{Stdout: []byte("not an id\n")}, nil
		}
		return execx.Result{Code: 125, Stderr: []byte("no such image")}, nil
	}}
	p := Client{Exec: r}
	ctx := context.Background()
	if got, err := p.ImageID(ctx, "there"); got != id || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	if !reflect.DeepEqual(r.calls[0].Argv, []string{"podman", "image", "inspect", "--format", "{{.Id}}", "there"}) {
		t.Fatalf("%q", r.calls[0].Argv)
	}
	if c := r.calls[0].Timeout; c == 0 || c > time.Minute {
		t.Errorf("timeout %v", c)
	}
	for _, tag := range []string{"gone", "odd", "-x", ""} {
		if got, err := p.ImageID(ctx, tag); err == nil {
			t.Errorf("%q: %q", tag, got)
		}
	}
}
