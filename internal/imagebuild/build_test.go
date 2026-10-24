package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/logx"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

var at = time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)

// fakePodman answers like UpkeepTest.rebuild's fake_run: build rc from
// results["build"], the smoke run's stdout from results["smoke"], 0 and
// nothing for the rest; "exists" lists the tags that exist.
type fakePodman struct {
	calls   [][]string
	cmds    []execx.Cmd
	results map[string]string
	build   int
	exists  map[string]bool
	tagFail bool
}

func (f *fakePodman) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	f.calls = append(f.calls, c.Argv)
	f.cmds = append(f.cmds, c)
	a := c.Argv[1:]
	switch {
	case a[0] == "build":
		return execx.Result{Argv: c.Argv, Code: f.build, Stdout: []byte("STEP 1/9\n"), Stderr: []byte("built\n")}, nil
	case a[0] == "run":
		smoke, ok := f.results["smoke"]
		if !ok {
			smoke = "ruby 3.3\n2.1.296 (Claude Code)\n"
		}
		return execx.Result{Argv: c.Argv, Stdout: []byte(smoke)}, nil
	case a[0] == "image" && a[1] == "exists":
		if f.exists[a[2]] {
			return execx.Result{Argv: c.Argv}, nil
		}
		return execx.Result{Argv: c.Argv, Code: 1}, nil
	case a[0] == "tag" && f.tagFail && strings.HasSuffix(a[1], ":candidate"):
		return execx.Result{Argv: c.Argv, Code: 125, Stderr: []byte("tag refused")}, nil
	}
	return execx.Result{Argv: c.Argv}, nil
}

func (f *fakePodman) has(argv ...string) bool {
	return slices.ContainsFunc(f.calls, func(c []string) bool { return reflect.DeepEqual(c, argv) })
}

func claudeSpec(pins map[string]string) Spec {
	return SpecFor(Kinds()[0], "/f/images", "localhost/factory-agent:scion", pins)
}

func runner(t *testing.T, f *fakePodman) (Runner, string) {
	t.Helper()
	env := testkit.Root(t)
	logs := filepath.Join(env.Root, "logs")
	return Runner{
		Podman: podman.Client{Exec: f, Dir: env.Home},
		Clock:  clock.Fixed{T: at},
		Log:    &logx.Log{Dir: logs, Clock: clock.Fixed{T: at}},
		LogDir: logs,
		Lock:   filepath.Join(env.Root, ".tick.lock"),
	}, logs
}

// UpkeepTest.rebuild: the config of the Python test, both images present.
func rebuildAll(t *testing.T, f *fakePodman) ([]Result, string) {
	t.Helper()
	f.exists = map[string]bool{"localhost/factory-agent:scion": true, "localhost/factory-pi:dev": true}
	pins := map[string]string{"claude": "2.1.296"}
	r, logs := runner(t, f)
	specs := []Spec{
		SpecFor(Kinds()[0], "/f/images", "localhost/factory-agent:scion", pins),
		SpecFor(Kinds()[1], "/f/images", "localhost/factory-pi:dev", pins),
	}
	res, err := r.All(context.Background(), specs)
	if err != nil {
		t.Fatal(err)
	}
	return res, logs
}

// port: TF UpkeepTest.test_rebuild_swaps_the_tag_only_after_checks_pass
func TestRebuildSwapsTheTagOnlyAfterChecksPass(t *testing.T) {
	f := &fakePodman{}
	res, _ := rebuildAll(t, f)
	build := f.calls[slices.IndexFunc(f.calls, func(c []string) bool { return c[1] == "build" })]
	if !slices.Contains(build, "CLAUDE_VERSION=2.1.296") {
		t.Fatalf("build %q", build)
	}
	if !f.has("podman", "tag", "localhost/factory-agent:candidate", "localhost/factory-agent:scion") {
		t.Fatalf("no swap: %q", f.calls)
	}
	if !res[0].OK || res[0].Summary != "2.1.296 (Claude Code)" {
		t.Fatalf("%+v", res[0])
	}
	// the old image is tagged :previous before the swap, for a manual rollback
	prev := slices.IndexFunc(f.calls, func(c []string) bool {
		return reflect.DeepEqual(c, []string{"podman", "tag", "localhost/factory-agent:scion", "localhost/factory-agent:previous"})
	})
	swap := slices.IndexFunc(f.calls, func(c []string) bool {
		return reflect.DeepEqual(c, []string{"podman", "tag", "localhost/factory-agent:candidate", "localhost/factory-agent:scion"})
	})
	if prev < 0 || prev > swap {
		t.Fatalf(":previous at %d, swap at %d", prev, swap)
	}
}

// port: TF UpkeepTest.test_rebuild_keeps_the_old_image_when_the_version_is_wrong
func TestRebuildKeepsTheOldImageWhenTheVersionIsWrong(t *testing.T) {
	f := &fakePodman{results: map[string]string{"smoke": "2.1.999 (Claude Code)\n"}}
	res, _ := rebuildAll(t, f)
	if f.has("podman", "tag", "localhost/factory-agent:candidate", "localhost/factory-agent:scion") {
		t.Fatal("swapped to a candidate with the wrong version")
	}
	if !f.has("podman", "rmi", "localhost/factory-agent:candidate") {
		t.Fatal("candidate not removed")
	}
	if res[0].OK || res[0].Summary != "smoke test failed (expected Claude Code 2.1.296)" {
		t.Fatalf("%+v", res[0])
	}
}

// The full argv sequence of one good claude rebuild (F:2868-2925), and the
// pi image (unpinned) taken the same way.
func TestRebuildArgvSequence(t *testing.T) {
	f := &fakePodman{}
	rebuildAll(t, f)
	script := "ruby -v && bundle -v && crystal --version && ameba --version && git --version && "
	want := [][]string{
		{"podman", "image", "exists", "localhost/factory-agent:scion"},
		{"podman", "build", "--pull=newer", "--no-cache", "--build-arg", "CLAUDE_VERSION=2.1.296", "-t", "localhost/factory-agent:candidate", "/f/images/scion-agent"},
		{"podman", "run", "--rm", "--name", "factory-smoke-claude", "--network", "none", "--userns=keep-id:uid=1000,gid=1000", "--user", "1000:1000",
			"--entrypoint", "/bin/sh", "localhost/factory-agent:candidate", "-c", script + "claude --version"},
		{"podman", "tag", "localhost/factory-agent:scion", "localhost/factory-agent:previous"},
		{"podman", "tag", "localhost/factory-agent:candidate", "localhost/factory-agent:scion"},
		{"podman", "rmi", "localhost/factory-agent:candidate"},
		{"podman", "image", "prune", "-f"},
		{"podman", "image", "exists", "localhost/factory-pi:dev"},
		{"podman", "build", "--pull=newer", "--no-cache", "-t", "localhost/factory-pi:candidate", "/f/images/pi"},
		{"podman", "run", "--rm", "--name", "factory-smoke-pi", "--network", "none", "--userns=keep-id:uid=1000,gid=1000", "--user", "1000:1000",
			"--entrypoint", "/bin/sh", "localhost/factory-pi:candidate", "-c", script + "pi --version"},
		{"podman", "tag", "localhost/factory-pi:dev", "localhost/factory-pi:previous"},
		{"podman", "tag", "localhost/factory-pi:candidate", "localhost/factory-pi:dev"},
		{"podman", "rmi", "localhost/factory-pi:candidate"},
		{"podman", "image", "prune", "-f"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls\n%s", testkit.Diff(fmtCalls(want), fmtCalls(f.calls)))
	}
}

func fmtCalls(cs [][]string) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(strings.Join(c, " ") + "\n")
	}
	return b.String()
}

// §10.7 S7b: an image this host does not have is not built.
func TestAbsentImageIsSkipped(t *testing.T) {
	f := &fakePodman{exists: map[string]bool{}}
	r, logs := runner(t, f)
	res, err := r.All(context.Background(), []Spec{claudeSpec(nil), SpecFor(Kinds()[1], "/f/images", "", nil)})
	if err != nil || len(res) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// an empty tag (no image configured) runs nothing at all
	if want := [][]string{{"podman", "image", "exists", "localhost/factory-agent:scion"}}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("%q", f.calls)
	}
	if _, err := os.Stat(filepath.Join(logs, "rebuild-claude.log")); !os.IsNotExist(err) {
		t.Fatal("a log for a skipped image")
	}
}

func TestBuildFailureKeepsTheOldImage(t *testing.T) {
	f := &fakePodman{build: 1}
	r, _ := runner(t, f)
	res := r.Rebuild(context.Background(), claudeSpec(map[string]string{"claude": "2.1.296"}))
	if res.OK || res.Summary != "build failed" {
		t.Fatalf("%+v", res)
	}
	for _, c := range f.calls {
		if c[1] == "tag" || c[1] == "run" {
			t.Fatalf("ran %q after a failed build", c)
		}
	}
	if !f.has("podman", "rmi", "localhost/factory-agent:candidate") || !f.has("podman", "image", "prune", "-f") {
		t.Fatalf("no clean-up: %q", f.calls)
	}
	if want := []string{"== 2026-10-18T03:00:00+00:00 rebuild claude -> localhost/factory-agent:scion", "STEP 1/9\nbuilt\n"}; !reflect.DeepEqual(res.Log, want) {
		t.Fatalf("log %q", res.Log)
	}
}

// §10.7 S7b: a tag that is the rebuild's own candidate or previous tag is
// refused before anything runs (and nothing is cleaned up: Python returns
// before its try).
func TestTagClash(t *testing.T) {
	for _, tag := range []string{"localhost/a:candidate", "localhost/a:previous"} {
		f := &fakePodman{}
		r, _ := runner(t, f)
		res := r.Rebuild(context.Background(), SpecFor(Kinds()[0], "/f/images", tag, nil))
		if res.OK || res.Summary != "image tag "+tag+" clashes with the rebuild's own tags" || len(f.calls) != 0 || len(res.Log) != 0 {
			t.Fatalf("%s: %+v %q", tag, res, f.calls)
		}
	}
}

// The tag's version part is after the last ":" that follows the last "/":
// a registry may carry a port.
func TestRepoOfTag(t *testing.T) {
	for tag, want := range map[string]string{
		"localhost/factory-agent:scion": "localhost/factory-agent",
		"registry:5000/agent":           "registry:5000/agent",
		"registry:5000/agent:v1":        "registry:5000/agent",
		"agent":                         "agent",
		"agent:1":                       "agent",
	} {
		if got := Repo(tag); got != want {
			t.Errorf("Repo(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestToolPinsAreBuildArgs(t *testing.T) {
	f := &fakePodman{}
	r, _ := runner(t, f)
	r.Rebuild(context.Background(), claudeSpec(map[string]string{"claude": "2.1.296", "ameba": "1.7.0", "crystal": "1.20.1"}))
	want := []string{"podman", "build", "--pull=newer", "--no-cache", "--build-arg", "CLAUDE_VERSION=2.1.296",
		"--build-arg", "CRYSTAL_VERSION=1.20.1", "--build-arg", "AMEBA_VERSION=1.7.0",
		"-t", "localhost/factory-agent:candidate", "/f/images/scion-agent"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("%q", f.calls[0])
	}
	if f.cmds[0].Dir != r.Podman.Dir || f.cmds[0].Timeout != podman.BuildTimeout {
		t.Fatalf("dir %q timeout %v", f.cmds[0].Dir, f.cmds[0].Timeout)
	}
}

// An unpinned image passes its smoke test on a zero exit alone.
func TestUnpinnedSmokeNeedsOnlyAZeroExit(t *testing.T) {
	f := &fakePodman{results: map[string]string{"smoke": ""}}
	r, _ := runner(t, f)
	res := r.Rebuild(context.Background(), claudeSpec(nil))
	if !res.OK || res.Summary != "ok" {
		t.Fatalf("%+v", res)
	}
}

// For the gate image, every project's checks run in the candidate; a
// failure that the live image does not share (a regression) keeps the old
// image (F:2894-2902, §15.1).
func TestProjectChecksRunInTheCandidate(t *testing.T) {
	f := &fakePodman{}
	r, _ := runner(t, f)
	var images []string
	spec := claudeSpec(map[string]string{"claude": "2.1.296"})
	check := func(ok bool, out string) func(context.Context, string) (bool, string, error) {
		return func(_ context.Context, image string) (bool, string, error) {
			images = append(images, image)
			return ok || image == spec.Tag, out, nil // all pass in the live image
		}
	}
	spec.Projects = []ProjectCheck{
		{Key: "a", Base: "main", Run: check(true, "2 examples, 0 failures")},
		{Key: "b", Base: "trunk", Run: check(false, "1 failure")},
		{Key: "c", Base: "main", Run: check(true, "never run")},
	}
	res := r.Rebuild(context.Background(), spec)
	if res.OK || res.Summary != "b tests fail in the new image" {
		t.Fatalf("%+v", res)
	}
	if !reflect.DeepEqual(images, []string{"localhost/factory-agent:candidate", "localhost/factory-agent:candidate", spec.Tag}) {
		t.Fatalf("checked %q", images)
	}
	if f.has("podman", "tag", "localhost/factory-agent:candidate", "localhost/factory-agent:scion") {
		t.Fatal("swapped although a project fails")
	}
	tail := res.Log[len(res.Log)-3:]
	if want := []string{"-- a on main: pass\n2 examples, 0 failures", "-- b on trunk: FAIL\n1 failure",
		"-- b on trunk in the live image: pass, so the new image broke it"}; !reflect.DeepEqual(tail, want) {
		t.Fatalf("log tail %q", tail)
	}
}

// §15.1: a project whose base already fails in the live image does not
// block the new one; the log and the summary name it.
func TestAProjectAlreadyFailingDoesNotBlock(t *testing.T) {
	f := &fakePodman{}
	r, _ := runner(t, f)
	spec := claudeSpec(map[string]string{"claude": "2.1.296"})
	runs := 0
	spec.Projects = []ProjectCheck{
		{Key: "typed", Base: "main", Run: func(context.Context, string) (bool, string, error) { runs++; return false, "lint: 2 offences", nil }},
		{Key: "ok", Base: "main", Run: func(context.Context, string) (bool, string, error) { return true, "fine", nil }},
	}
	res := r.Rebuild(context.Background(), spec)
	if !res.OK || !reflect.DeepEqual(res.AlreadyFailing, []string{"typed"}) || runs != 2 ||
		!strings.HasSuffix(res.Summary, "(already failing on main: typed)") {
		t.Fatalf("%+v", res)
	}
	if !f.has("podman", "tag", "localhost/factory-agent:candidate", "localhost/factory-agent:scion") {
		t.Fatal("not swapped")
	}
	if !strings.Contains(strings.Join(res.Log, "\n"), "-- typed on main in the live image: FAIL too (already failing; not blocking)") {
		t.Errorf("log %q", res.Log)
	}
}

// §15.1: the smoke test checks the exact version: 2.1.30 is not 2.1.302.
func TestSmokeChecksTheExactVersion(t *testing.T) {
	for out, want := range map[string]bool{
		"2.1.302 (Claude Code)\n": true, "v2.1.302\n": true, "2.1.3020\n": false, "2.1.30\n": false, "": false,
	} {
		if got := hasVersion(out, "2.1.302"); got != want {
			t.Errorf("hasVersion(%q) = %v", out, got)
		}
	}
}

// An exception inside rebuild_image (here: the swap's checked tag fails)
// becomes "error: ..." with the error alone as the log, and the candidate
// is still removed.
func TestErrorPath(t *testing.T) {
	f := &fakePodman{tagFail: true}
	r, _ := runner(t, f)
	res := r.Rebuild(context.Background(), claudeSpec(nil))
	if res.OK || !strings.HasPrefix(res.Summary, "error: ") || !strings.Contains(res.Summary, "tag refused") || len(res.Log) != 1 || res.Err == nil {
		t.Fatalf("%+v", res)
	}
	if !f.has("podman", "rmi", "localhost/factory-agent:candidate") {
		t.Fatal("candidate kept")
	}
	if len([]rune(res.Summary)) > len("error: ")+200 {
		t.Fatalf("summary not cut: %d", len(res.Summary))
	}
}

// A cancelled rebuild still removes its candidate, on a context of its own.
func TestCancelledRebuildStillCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var cleanupCtxErr []error
	ex := execx.Func(func(c context.Context, cmd execx.Cmd) (execx.Result, error) {
		switch cmd.Argv[1] {
		case "build":
			cancel()
			return execx.Result{Argv: cmd.Argv}, context.Canceled
		case "rmi", "image":
			cleanupCtxErr = append(cleanupCtxErr, c.Err())
		}
		return execx.Result{Argv: cmd.Argv}, nil
	})
	r, _ := runner(t, &fakePodman{})
	r.Podman.Exec = ex
	res := r.Rebuild(ctx, claudeSpec(nil))
	if res.OK || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("%+v", res)
	}
	if len(cleanupCtxErr) != 2 || cleanupCtxErr[0] != nil || cleanupCtxErr[1] != nil {
		t.Fatalf("clean-up contexts %v", cleanupCtxErr)
	}
}

// rebuild() (F:2928): logs/rebuild-<kind>.log holds the run's output joined
// by newlines, and factory.log one line per image.
func TestLogFileAndLogLine(t *testing.T) {
	f := &fakePodman{results: map[string]string{"smoke": "2.1.999\n"}}
	_, logs := rebuildAll(t, f)
	b, err := os.ReadFile(filepath.Join(logs, "rebuild-claude.log"))
	if err != nil {
		t.Fatal(err)
	}
	want := "== 2026-10-18T03:00:00+00:00 rebuild claude -> localhost/factory-agent:scion\nSTEP 1/9\nbuilt\n\n2.1.999\n"
	if string(b) != want {
		t.Fatalf("log file\n%q\nwant\n%q", b, want)
	}
	fl, err := os.ReadFile(filepath.Join(logs, "factory.log"))
	if err != nil {
		t.Fatal(err)
	}
	wantLog := "2026-10-18T03:00:00+00:00 rebuild claude: FAILED: smoke test failed (expected Claude Code 2.1.296)\n" +
		"2026-10-18T03:00:00+00:00 rebuild pi: ok: 2.1.999\n"
	if string(fl) != wantLog {
		t.Fatalf("factory.log\n%q\nwant\n%q", fl, wantLog)
	}
}

// The build output kept in the log is its last 6000 characters.
func TestBuildOutputIsCut(t *testing.T) {
	ex := execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
		if c.Argv[1] == "build" {
			return execx.Result{Argv: c.Argv, Code: 1, Stdout: []byte(strings.Repeat("x", 7000)), Stderr: []byte("é")}, nil
		}
		return execx.Result{Argv: c.Argv}, nil
	})
	r, _ := runner(t, &fakePodman{})
	r.Podman.Exec = ex
	res := r.Rebuild(context.Background(), claudeSpec(nil))
	if got := []rune(res.Log[1]); len(got) != 6000 || got[5999] != 'é' {
		t.Fatalf("kept %d runes", len(got))
	}
}

// upkeep.Rebuild holds the tick lock (§10.7 S16): All waits for it, so no
// agent starts meanwhile, and gives up when its context ends.
func TestAllWaitsForTheTickLock(t *testing.T) {
	f := &fakePodman{exists: map[string]bool{"localhost/factory-agent:scion": true}}
	r, _ := runner(t, f)
	held, err := fsx.TryLock(r.Lock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := r.All(ctx, []Spec{claudeSpec(nil)}); err == nil {
		t.Fatal("ran while the tick lock was held")
	}
	if len(f.calls) != 0 {
		t.Fatalf("ran %q under someone else's lock", f.calls)
	}
	held.Unlock()
	if res, err := r.All(context.Background(), []Spec{claudeSpec(nil)}); err != nil || len(res) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAllStopsOnAPodmanThatDoesNotRun(t *testing.T) {
	r, _ := runner(t, &fakePodman{})
	r.Podman.Exec = execx.Func(func(context.Context, execx.Cmd) (execx.Result, error) {
		return execx.Result{}, errors.New("podman: not found")
	})
	if _, err := r.All(context.Background(), []Spec{claudeSpec(nil)}); err == nil {
		t.Fatal("no error")
	}
}

func TestKindsTable(t *testing.T) {
	k := Kinds()
	if len(k) != 2 || k[0].Kind != "claude" || k[1].Kind != "pi" {
		t.Fatalf("%+v", k)
	}
	if k[0].Dir != "scion-agent" || k[0].Arg != "CLAUDE_VERSION" || k[0].Check != "claude --version" || k[0].NPM != "@anthropic-ai/claude-code" {
		t.Fatalf("%+v", k[0])
	}
	if k[1].Dir != "pi" || k[1].Arg != "PI_VERSION" || k[1].Check != "pi --version" || k[1].Name != "Pi" {
		t.Fatalf("%+v", k[1])
	}
	k[0].Name = "changed"
	if Kinds()[0].Name != "Claude Code" {
		t.Fatal("Kinds shares its table")
	}
}

func TestTagFor(t *testing.T) {
	if got := TagFor("claude", "localhost/a:scion", ""); got != "localhost/a:scion" {
		t.Fatal(got)
	}
	if got := TagFor("pi", "x", ""); got != "localhost/factory-pi:dev" {
		t.Fatal(got)
	}
	if got := TagFor("pi", "x", "localhost/p:1"); got != "localhost/p:1" {
		t.Fatal(got)
	}
	if got := TagFor("other", "x", "y"); got != "" {
		t.Fatal(got)
	}
}
