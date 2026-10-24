package archtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imageArg is the default of `ARG name=value` in a Containerfile, or "".
func imageArg(src, name string) string {
	for _, line := range strings.Split(src, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ARG "+name+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// The agent images (Claude Code and Pi) and the Lever worker image carry the toolchain build.sh
// pins: GO_VERSION is go.mod's toolchain and GO_SHA256 the linux-amd64 line
// of build/go.sha256, so a pin change has to touch all four files.
func TestImagesPinTheBuildToolchain(t *testing.T) {
	root := moduleRoot(t)
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	version := ""
	for _, line := range strings.Split(read("go.mod"), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "toolchain go"); ok {
			version = strings.TrimSpace(v)
		}
	}
	if version == "" {
		t.Fatal("go.mod has no toolchain line")
	}
	tarball := "go" + version + ".linux-amd64.tar.gz"
	sum := ""
	for _, line := range strings.Split(read("build/go.sha256"), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == tarball {
			sum = f[0]
		}
	}
	if sum == "" {
		t.Fatalf("build/go.sha256 has no line for %s", tarball)
	}
	for _, image := range []string{"images/claude/Containerfile", "images/lever/Dockerfile", "images/pi/Containerfile"} {
		src := read(image)
		if got := imageArg(src, "GO_VERSION"); got != version {
			t.Errorf("%s: GO_VERSION = %q, want %q (go.mod's toolchain)", image, got, version)
		}
		if got := imageArg(src, "GO_SHA256"); got != sum {
			t.Errorf("%s: GO_SHA256 = %q, want %q (build/go.sha256)", image, got, sum)
		}
		for _, want := range []string{"go${GO_VERSION}.linux-amd64.tar.gz", `echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c -`, "/usr/local/go/bin", "GOTOOLCHAIN=local", "GOPROXY=off"} {
			if !strings.Contains(src, want) {
				t.Errorf("%s does not contain %q", image, want)
			}
		}
	}
}
