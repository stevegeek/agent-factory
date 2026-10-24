package cli

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// doorsExec records every command and answers podman's image checks with
// "absent" (exit 1), so a rebuild finds nothing to build.
type doorsExec struct {
	mu   sync.Mutex
	argv [][]string
	dirs []string
}

func (d *doorsExec) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	if slices.Equal(c.Argv, []string{"git", "--version"}) {
		return execx.Result{Argv: c.Argv, Stdout: []byte("git version 2.50.1\n")}, nil // the git version check, not recorded
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.argv = append(d.argv, c.Argv)
	d.dirs = append(d.dirs, c.Dir)
	r := execx.Result{Argv: c.Argv}
	if len(c.Argv) > 2 && c.Argv[0] == "podman" && c.Argv[1] == "image" && c.Argv[2] == "exists" {
		r.Code = 1
	}
	return r, nil
}

func doorsEnv(t *testing.T, cfg string) (*Env, *testkit.Env, *doorsExec, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := testkit.Root(t)
	root.WriteConfig(t, cfg)
	ex := &doorsExec{}
	var stdout, stderr bytes.Buffer
	vars := map[string]string{}
	for k, v := range root.Vars {
		vars[k] = v
	}
	// URLs that cannot be requested: the sweep's and the version check's
	// requests fail at once, and the log says which URL each used
	vars["FACTORY_GITHUB_API"] = "http://github seam/gh"
	vars["FACTORY_NPM_REGISTRY"] = "http://npm seam/npm"
	e := &Env{Stdout: &stdout, Stderr: &stderr, Environ: vars, Exec: ex, Clock: testkit.Fixed{T: testkit.Stamp()}}
	e.init()
	return e, root, ex, &stdout, &stderr
}

func TestClockCommandFilesAndWritesState(t *testing.T) {
	e, root, _, stdout, _ := doorsEnv(t, `
[projects.g]
github = "o/g"

[[schedules]]
name = "s"
every_days = 1
kind = "task"
title = "Check"
body = "look at {github}"
`)
	if err := runClock(t.Context(), e, Args{}); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout)
	}
	ts, _ := e.Board().Reader.List()
	if len(ts) != 1 || ts[0].KindName() != "task" || ts[0].Body != "look at o/g" {
		t.Fatalf("tickets %+v", ts)
	}
	data, err := os.ReadFile(root.Path("state.json"))
	if err != nil || !strings.Contains(string(data), `"versions": "2026-01-02T03:04:05+00:00"`) {
		t.Fatalf("state.json %s %v", data, err)
	}
	log, _ := os.ReadFile(root.Path("logs", "factory.log"))
	if !strings.Contains(string(log), "T0001 created door=clock column=ready") {
		t.Fatalf("factory.log %s", log)
	}
}

func TestSweepCommandReadsTheGitHubSeam(t *testing.T) {
	e, root, _, _, _ := doorsEnv(t, "[projects.g]\ngithub = \"o/g\"\n")
	if err := runSweep(t.Context(), e, Args{}); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(root.Path("logs", "factory.log"))
	if !strings.Contains(string(log), "sweep o/g: ") || !strings.Contains(string(log), "github seam") {
		t.Fatalf("factory.log %s", log)
	}
}

func TestVersionsCommandReadsTheNPMSeam(t *testing.T) {
	e, root, _, _, _ := doorsEnv(t, "[images]\nclaude = \"1.0.0\"\n")
	if err := runVersions(t.Context(), e, Args{}); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(root.Path("logs", "factory.log"))
	if !strings.Contains(string(log), "versions Claude Code: ") || !strings.Contains(string(log), "npm seam") {
		t.Fatalf("factory.log %s", log)
	}
}

func TestRebuildCommandAsksPodmanForEachImage(t *testing.T) {
	e, root, ex, _, _ := doorsEnv(t, "image = \"localhost/factory-agent:scion\"\n")
	if err := runRebuild(t.Context(), e, Args{}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"podman", "image", "exists", "localhost/factory-agent:scion"},
		{"podman", "image", "exists", "localhost/factory-pi:dev"},
	}
	if !slices.EqualFunc(ex.argv, want, slices.Equal[[]string]) {
		t.Fatalf("argv %q", ex.argv)
	}
	if _, err := os.Stat(root.Path(".tick.lock")); err != nil {
		t.Fatalf("the rebuild took no tick lock: %v", err)
	}
}

func rebuildArgs(t *testing.T, argv ...string) Args {
	t.Helper()
	cmds := commands()
	for i := range cmds {
		if cmds[i].Name == "rebuild" {
			a, _, err := parse(&cmds[i], argv)
			if err != nil {
				t.Fatal(err)
			}
			return a
		}
	}
	t.Fatal("no rebuild command")
	return Args{}
}

// `factory rebuild --latest` runs the latest-toolchain trial alone, not
// the weekly rebuild: here the release lookup fails (the seam), so the log
// says so and podman is not asked for anything.
func TestRebuildLatestRunsOnlyTheTrial(t *testing.T) {
	e, root, ex, _, _ := doorsEnv(t, "image = \"localhost/factory-agent:scion\"\n[projects.s]\npath = \""+t.TempDir()+"\"\nbase = \"main\"\nkind = \"crystal-shard\"\n")
	if err := runRebuild(t.Context(), e, rebuildArgs(t, "--latest")); err != nil {
		t.Fatal(err)
	}
	if len(ex.argv) != 0 {
		t.Fatalf("argv %q", ex.argv)
	}
	log, _ := os.ReadFile(root.Path("logs", "factory.log"))
	if !strings.Contains(string(log), "latest Crystal: ") || !strings.Contains(string(log), "github seam") {
		t.Fatalf("factory.log %s", log)
	}
}

func TestRebuildLatestIsOneAtATime(t *testing.T) {
	e, _, _, _, _ := doorsEnv(t, "")
	err := runRebuild(t.Context(), e, rebuildArgs(t, "--latest", "--unhold", "claude"))
	if err == nil || !strings.Contains(err.Error(), "one at a time") {
		t.Fatalf("err %v", err)
	}
}
