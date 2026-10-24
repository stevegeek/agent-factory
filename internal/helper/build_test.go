package helper

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

const buildCommit = "35956371f2ff"

const builderConfig = "[remote]\nagent = \"claude\"\nimage = \"localhost/factory-agent:scion\"\nbuild = true\n"

// buildRig is a builder host: the opt-in, the pinned toolchain, and a
// source tar on stdin.
func buildRig(t *testing.T, config string) *rig {
	t.Helper()
	r := newRig(t)
	r.write(t, "config.toml", config)
	gobin := filepath.Join(r.Home, ".local/share/factory/go/go1.27.1/bin")
	if err := os.MkdirAll(gobin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gobin, "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.Stdin = bytes.NewReader(mktar(t, file("go.mod", "module x\n\ngo 1.25\n\ntoolchain go1.27.1\n"), file("build.sh", "echo build\n")))
	return r
}

// mountOf is the host side of the -v whose container side is dest.
func mountOf(argv []string, dest string) string {
	for i, a := range argv {
		if a == "-v" && i+1 < len(argv) {
			host, rest, _ := strings.Cut(argv[i+1], ":")
			if rest == dest || strings.HasPrefix(rest, dest+":") {
				return host
			}
		}
	}
	return ""
}

// a podman run that writes build.sh's four files into the out mount
func writesTheBinaries(c execx.Cmd) (execx.Result, error) {
	if len(c.Argv) > 1 && c.Argv[0] == "podman" && c.Argv[1] == "run" {
		out := mountOf(c.Argv, "/out")
		for _, n := range jobproto.BuildFiles() {
			if err := os.WriteFile(filepath.Join(out, n), []byte("the "+n), 0o644); err != nil {
				return execx.Result{}, err
			}
		}
	}
	return execx.Result{}, nil
}

// ranBuild reports a build container started (a podman run).
func ranBuild(r *rig) bool {
	return slices.ContainsFunc(r.exec.argvs(), func(a []string) bool { return len(a) > 1 && a[0] == "podman" && a[1] == "run" })
}

func workLeft(t *testing.T, r *rig) []string {
	t.Helper()
	es, _ := os.ReadDir(filepath.Join(r.Root, "builds-work"))
	var left []string
	for _, e := range es {
		if e.Name() != ".lock" {
			left = append(left, e.Name())
		}
	}
	return left
}

func TestBuildAnswersTheBinaries(t *testing.T) {
	r := buildRig(t, builderConfig)
	r.exec.answer = writesTheBinaries
	if code := r.ssh(t, "build "+buildCommit); code != 0 {
		t.Fatalf("build: %d %q", code, r.stderr)
	}
	got, err := jobproto.UntarFlat(r.stdout.Bytes())
	if err != nil || len(got) != 4 || string(got["factory-remote"]) != "the factory-remote" || string(got["BUILD"]) != "the BUILD" {
		t.Fatalf("answer: %v %v", got, err)
	}
	var argv []string
	for _, a := range r.exec.argvs() {
		if len(a) > 1 && a[1] == "run" {
			argv = a
		}
	}
	for _, want := range []string{"--network=none", "--timeout", "1080", "--cap-drop=all", "--security-opt=no-new-privileges", "--memory", "2g",
		"localhost/factory-agent:scion", "/src/build.sh", "--commit", buildCommit, "--no-box", "--no-scope"} {
		if !slices.Contains(argv, want) {
			t.Errorf("podman run lacks %q: %v", want, argv)
		}
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{":/src:ro", "/go/go1.27.1:/go:ro", "FACTORY_GOROOT=/go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("podman run lacks %q", want)
		}
	}
	for _, leak := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "SECRET_TOKEN"} {
		if strings.Contains(joined, leak) || slices.ContainsFunc(r.exec.calls[len(r.exec.calls)-1].Env, func(kv string) bool { return strings.HasPrefix(kv, leak) }) {
			t.Errorf("the build got %s", leak)
		}
	}
	if left := workLeft(t, r); len(left) != 0 {
		t.Errorf("work left behind: %v", left)
	}
}

// What a helper killed mid-build left behind goes before the next build:
// its work dir, and its container.
func TestBuildSweepsALeftoverBuild(t *testing.T) {
	r := buildRig(t, builderConfig)
	r.exec.answer = writesTheBinaries
	stale := r.write(t, "builds-work/"+buildCommit+"-1/cache/big", "x")
	if code := r.ssh(t, "build "+buildCommit); code != 0 {
		t.Fatalf("build: %d %q", code, r.stderr)
	}
	if onDisk(filepath.Dir(filepath.Dir(stale))) {
		t.Error("the leftover work dir is still there")
	}
	argvs := r.exec.argvs()
	if len(argvs) < 2 || !slices.Contains(argvs[0], "rm") || !slices.Contains(argvs[0], "factory-build-"+buildCommit) || argvs[1][1] != "run" {
		t.Errorf("no podman rm of the stale container before the run: %v", argvs)
	}
}

func TestBuildRefusedWithoutOptIn(t *testing.T) {
	r := buildRig(t, "[remote]\nagent = \"claude\"\nimage = \"localhost/factory-agent:scion\"\n")
	if code := r.ssh(t, "build "+buildCommit); code != 2 || r.stderr.String() != "no builds on this host\n" || ranBuild(r) {
		t.Errorf("build: %d %q", code, r.stderr)
	}
}

func TestBuildRefusesABadCommit(t *testing.T) {
	r := buildRig(t, builderConfig)
	if code := r.ssh(t, "build 35956371f2"); code != 2 || !strings.Contains(r.stderr.String(), "commit") {
		t.Errorf("build: %d %q", code, r.stderr)
	}
}

func TestBuildNeedsTheToolchain(t *testing.T) {
	r := buildRig(t, builderConfig)
	os.RemoveAll(filepath.Join(r.Home, ".local/share/factory/go"))
	if code := r.ssh(t, "build "+buildCommit); code != 2 || !strings.Contains(r.stderr.String(), "no Go toolchain go1.27.1 at ") || ranBuild(r) {
		t.Errorf("build: %d %q", code, r.stderr)
	}
	if left := workLeft(t, r); len(left) != 0 {
		t.Errorf("work left behind: %v", left)
	}
}

func TestBuildFailureCarriesTheLog(t *testing.T) {
	r := buildRig(t, builderConfig)
	r.exec.answer = func(execx.Cmd) (execx.Result, error) {
		return execx.Result{Code: 1, Stdout: []byte(strings.Repeat("noise\n", 2000) + "build.sh: go vet failed\n")}, nil
	}
	if code := r.ssh(t, "build "+buildCommit); code != 1 || !strings.Contains(r.stderr.String(), "build.sh: go vet failed") || len(r.stderr.String()) > 3000 {
		t.Errorf("build: %d %q", code, r.stderr)
	}
	if r.stdout.Len() != 0 {
		t.Error("a failed build answered binaries")
	}
	if left := workLeft(t, r); len(left) != 0 {
		t.Errorf("work left behind: %v", left)
	}
}

func TestBuildWithoutItsFilesFails(t *testing.T) {
	r := buildRig(t, builderConfig)
	if code := r.ssh(t, "build "+buildCommit); code != 1 || !strings.Contains(r.stderr.String(), "BUILD") {
		t.Errorf("build: %d %q", code, r.stderr)
	}
}

func TestBuildOneAtATime(t *testing.T) {
	r := buildRig(t, builderConfig)
	if err := os.MkdirAll(filepath.Join(r.Root, "builds-work"), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := fsx.Lock(context.Background(), filepath.Join(r.Root, "builds-work", ".lock"), fsx.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if code := r.ssh(t, "build "+buildCommit); code != 2 || r.stderr.String() != "a build is already running here\n" {
		t.Errorf("build: %d %q", code, r.stderr)
	}
}

func TestBuildInputTooLarge(t *testing.T) {
	r := buildRig(t, builderConfig)
	r.Stdin = bytes.NewReader(make([]byte, jobproto.MaxBuildInput+1))
	if code := r.ssh(t, "build "+buildCommit); code != 2 || !strings.Contains(r.stderr.String(), "too large") || ranBuild(r) {
		t.Errorf("build: %d %q", code, r.stderr)
	}
}

// build_memory replaces the 2g default; a host's own table can opt in.
func TestBuildConfig(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "none.toml"), "")
	if err != nil || cfg.Remote.Build || cfg.Remote.BuildMemory != "2g" {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("[remote]\nagent = \"claude\"\nimage = \"i\"\n[remote.helpera]\nbuild = true\nbuild_memory = \"3g\"\n"), 0o644)
	cfg, err = LoadConfig(p, "helpera")
	if err != nil || !cfg.Remote.Build || cfg.Remote.BuildMemory != "3g" {
		t.Fatalf("helpera: %+v %v", cfg, err)
	}
	if cfg, _ := LoadConfig(p, "other"); cfg.Remote.Build {
		t.Error("another host builds")
	}
	os.WriteFile(p, []byte("[remote]\nbuild_memory = \"\"\n"), 0o644)
	if _, err := LoadConfig(p, ""); err == nil {
		t.Error("an empty build_memory accepted")
	}
	os.WriteFile(p, []byte("[remote]\nbuild = \"yes\"\n"), 0o644)
	if _, err := LoadConfig(p, ""); err == nil {
		t.Error("build = \"yes\" accepted")
	}
}
