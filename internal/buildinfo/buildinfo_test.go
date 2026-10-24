package buildinfo

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreDev(t *testing.T) {
	if Version != "dev" || GoVersion != "dev" {
		t.Fatalf("Version, GoVersion = %q, %q; want dev, dev in a plain test build", Version, GoVersion)
	}
	if got := String(); got != "dev dev" {
		t.Fatalf("String() = %q", got)
	}
}

// The linker flags build.sh passes reach `factory version`.
func TestLinkerSetsTheVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out := filepath.Join(t.TempDir(), "factory")
	pkg := "github.com/stevegeek/agent-factory/internal/buildinfo"
	build := exec.CommandContext(ctx, gobin, "build",
		"-ldflags=-X "+pkg+".Version=abc1234 -X "+pkg+".GoVersion=go1.27.1",
		"-o", out, "github.com/stevegeek/agent-factory/cmd/factory")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	got, err := exec.CommandContext(ctx, out, "version").Output()
	if err != nil {
		t.Fatalf("factory version: %v", err)
	}
	if strings.TrimSpace(string(got)) != "abc1234 go1.27.1" {
		t.Fatalf("factory version = %q", got)
	}
}
