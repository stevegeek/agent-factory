package helper

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
)

// podmanFake answers a rebuild's podman calls: which images exist, and
// whether the build and the smoke test pass.
func podmanFake(have []string, buildCode int, smokeOut string) func(execx.Cmd) (execx.Result, error) {
	return func(c execx.Cmd) (execx.Result, error) {
		a := c.Argv
		switch {
		case len(a) > 3 && a[1] == "image" && a[2] == "exists":
			if slices.Contains(have, a[3]) {
				return execx.Result{}, nil
			}
			return execx.Result{Code: 1}, nil
		case len(a) > 1 && a[1] == "build":
			return execx.Result{Code: buildCode, Stdout: []byte("STEP 1/9\n")}, nil
		case len(a) > 1 && a[1] == "run":
			return execx.Result{Stdout: []byte(smokeOut)}, nil
		}
		return execx.Result{}, nil
	}
}

// `factory-remote rebuild` (and `factory rebuild` through the symlink) is
// the helper's weekly image rebuild: the pi image from [remote] image at
// the [images] pin, under the tick lock, logged to logs/rebuild-pi.log and
// factory.log, with no project checks and no notice (a helper has no board).
func TestRebuildThePiImage(t *testing.T) {
	r := newRig(t)
	r.write(t, "config.toml", `
[images]
claude = "2.1.296"
pi = "1.2.0"
ameba = "1.6.4"
[remote]
image = "localhost/factory-pi:dev"
[projects.gem]
path = "`+r.Root+`"
`)
	r.exec.answer = podmanFake([]string{"localhost/factory-pi:dev"}, 0, "ruby 3.4\npi 1.2.0\n")
	if code := r.argv(t, "rebuild"); code != 0 {
		t.Fatalf("rebuild: exit %d: %s", code, r.stderr)
	}
	argvs := r.exec.argvs()
	var build []string
	for _, a := range argvs {
		if a[0] != "podman" {
			t.Errorf("rebuild ran %v", a)
		}
		if a[1] == "build" {
			build = a
		}
		if a[1] == "image" && a[2] == "exists" && a[3] != "localhost/factory-pi:dev" {
			t.Errorf("looked for %v: the claude image has no tag here", a)
		}
	}
	want := []string{"podman", "build", "--pull=newer", "--no-cache", "--build-arg", "PI_VERSION=1.2.0",
		"--build-arg", "AMEBA_VERSION=1.6.4", "-t", "localhost/factory-pi:candidate", filepath.Join(r.Root, "images", "pi")}
	if !slices.Equal(build, want) {
		t.Errorf("build %q, want %q", build, want)
	}
	if !slices.ContainsFunc(argvs, func(a []string) bool {
		return slices.Equal(a, []string{"podman", "tag", "localhost/factory-pi:candidate", "localhost/factory-pi:dev"})
	}) {
		t.Errorf("no swap in %v", argvs)
	}
	if !onDisk(filepath.Join(r.Root, ".tick.lock")) {
		t.Error("the tick lock was not taken")
	}
	if log := readFile(t, filepath.Join(r.Root, "logs", "rebuild-pi.log")); !strings.Contains(log, "== 2026-10-18T12:00:00+00:00 rebuild pi -> localhost/factory-pi:dev") {
		t.Errorf("rebuild-pi.log:\n%s", log)
	}
	if log := readFile(t, filepath.Join(r.Root, "logs", "factory.log")); log != "2026-10-18T12:00:00+00:00 rebuild pi: ok: pi 1.2.0\n" {
		t.Errorf("factory.log %q", log)
	}
	if onDisk(filepath.Join(r.Root, "board")) {
		t.Error("the helper's rebuild touched a board")
	}
	for _, c := range r.exec.calls {
		if slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, "SECRET_TOKEN=") }) {
			t.Errorf("podman got the whole environment: %v", c.Env)
		}
	}
}

// The claude image is rebuilt too when this host has the top-level image;
// a failed build keeps the old image and still exits 0, as Python does.
func TestRebuildBothImagesAndAFailure(t *testing.T) {
	r := newRig(t)
	r.write(t, "config.toml", "image = \"localhost/factory-agent:scion\"\n[images]\nclaude = \"2.1.296\"\n")
	r.exec.answer = podmanFake([]string{"localhost/factory-agent:scion", "localhost/factory-pi:dev"}, 1, "")
	if code := r.argv(t, "rebuild"); code != 0 {
		t.Fatalf("rebuild: exit %d: %s", code, r.stderr)
	}
	log := readFile(t, filepath.Join(r.Root, "logs", "factory.log"))
	if !strings.Contains(log, "rebuild claude: FAILED: build failed\n") || !strings.Contains(log, "rebuild pi: FAILED: build failed\n") {
		t.Errorf("factory.log %q", log)
	}
	for _, a := range r.exec.argvs() {
		if a[1] == "tag" {
			t.Errorf("swapped after a failed build: %v", a)
		}
	}
}

// With no config at all the helper still rebuilds its default pi image.
func TestRebuildWithNoConfig(t *testing.T) {
	r := newRig(t)
	r.exec.answer = podmanFake(nil, 0, "")
	if code := r.argv(t, "rebuild"); code != 0 {
		t.Fatalf("rebuild: %d %s", code, r.stderr)
	}
	if got := r.exec.argvs(); len(got) != 1 || !slices.Equal(got[0], []string{"podman", "image", "exists", "localhost/factory-pi:dev"}) {
		t.Errorf("ran %v", got)
	}
	r.reset()
	r.write(t, "config.toml", "[remote\n")
	if code := r.argv(t, "rebuild"); code != 1 || len(r.exec.argvs()) != 0 {
		t.Errorf("bad config: exit %d, ran %v", code, r.exec.argvs())
	}
	if !strings.Contains(r.stderr.String(), "config.toml") {
		t.Errorf("stderr %q", r.stderr)
	}
	os.Remove(filepath.Join(r.Root, "config.toml"))
}

// §15.1 on the helper: a newer Pi release is built, checked for that exact
// version, promoted and recorded in the helper's images.json; a held one
// is not retried.
func TestRebuildUpdatesPiOnTheHelper(t *testing.T) {
	r := newRig(t)
	r.write(t, "config.toml", "[images]\npi = \"1.2.0\"\nauto_update = [\"pi\"]\n")
	r.Latest = func(_ context.Context, pkg string) (string, error) {
		if pkg != "@earendil-works/pi-coding-agent" {
			t.Errorf("asked for %s", pkg)
		}
		return "1.3.0", nil
	}
	r.exec.answer = podmanFake([]string{"localhost/factory-pi:dev"}, 0, "ruby 3.4\npi 1.3.0\n")
	if code := r.argv(t, "rebuild"); code != 0 {
		t.Fatalf("rebuild: exit %d: %s", code, r.stderr)
	}
	if !slices.ContainsFunc(r.exec.argvs(), func(a []string) bool { return slices.Contains(a, "PI_VERSION=1.3.0") }) {
		t.Errorf("not built at 1.3.0: %v", r.exec.argvs())
	}
	st := readFile(t, filepath.Join(r.Root, "images.json"))
	if !strings.Contains(st, `"version": "1.3.0"`) || !strings.Contains(st, `"previous": "1.2.0"`) {
		t.Errorf("images.json %s", st)
	}
	r.reset()
	r.write(t, "images.json", `{"pi": {"version": "1.2.0", "held": "1.3.0"}}`)
	if code := r.argv(t, "rebuild"); code != 0 {
		t.Fatalf("rebuild: exit %d: %s", code, r.stderr)
	}
	if slices.ContainsFunc(r.exec.argvs(), func(a []string) bool { return slices.Contains(a, "PI_VERSION=1.3.0") }) {
		t.Errorf("a held version was built: %v", r.exec.argvs())
	}
}

// On a host whose agent is claude, [remote] image is the claude image: it
// is rebuilt as the claude kind, and no pi image is built into its tag.
func TestRebuildOnAClaudeHost(t *testing.T) {
	r := newRig(t)
	r.Host = "helperb"
	r.write(t, "config.toml", `
image = "localhost/factory-agent:scion"
[images]
claude = "2.1.296"
pi = "1.2.0"
[remote]
image = "localhost/factory-pi:dev"
[remote.helperb]
agent = "claude"
image = "localhost/factory-agent:mac"
`)
	r.exec.answer = podmanFake([]string{"localhost/factory-agent:mac", "localhost/factory-pi:dev", "localhost/factory-agent:scion"}, 0, "ruby 3.4\n2.1.296 (Claude Code)\n")
	if code := r.argv(t, "rebuild"); code != 0 {
		t.Fatalf("rebuild: exit %d: %s", code, r.stderr)
	}
	var builds [][]string
	for _, a := range r.exec.argvs() {
		if a[1] == "build" {
			builds = append(builds, a)
		}
	}
	if len(builds) != 1 || !slices.Contains(builds[0], "CLAUDE_VERSION=2.1.296") || builds[0][len(builds[0])-1] != filepath.Join(r.Root, "images", "scion-agent") ||
		!slices.Contains(builds[0], "localhost/factory-agent:candidate") {
		t.Errorf("builds %q", builds)
	}
}
