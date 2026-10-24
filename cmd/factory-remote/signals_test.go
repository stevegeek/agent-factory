package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildRemote builds this command into a temp dir: the signal handling is
// main()'s, so it is checked on the binary.
func buildRemote(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	out := filepath.Join(t.TempDir(), "factory-remote")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, gobin, "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// §5.4: `factory-remote run <job>` ignores SIGHUP; on SIGTERM (systemd
// stopping the job's unit) it TERMs podman's client, removes the container
// with `podman rm -f --time 5` (which stops it first), writes what the run
// printed to run.log and 255 to exit, and exits 1.
func TestRunSignals(t *testing.T) {
	bin := buildRemote(t)
	base := t.TempDir()
	home, root, fake := filepath.Join(base, "home"), filepath.Join(base, "factory"), filepath.Join(base, "bin")
	job := filepath.Join(root, "jobs", "t0001-w1")
	podmanLog := filepath.Join(base, "podman.log")
	for _, d := range []string{filepath.Join(home, ".config", "factory"), filepath.Join(job, "work"), filepath.Join(job, "home"), fake} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(home, ".config", "factory", "anthropic-api-key"): "k\n",
		filepath.Join(job, "meta.json"):                                `{"branch": "bot/t0001", "start": "main", "model": "m", "bundle": false, "timeout_minutes": 30}`,
		// podman run prints and then runs until it is signalled; every call is logged
		filepath.Join(fake, "podman"): "#!/bin/sh\necho \"$*\" >> " + podmanLog + "\n" +
			"if [ \"$1\" = run ]; then echo the agent started; exec sleep 60; fi\nexit 0\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bin, "run", "t0001-w1")
	cmd.Env = []string{"PATH=" + fake + ":/usr/bin:/bin", "HOME=" + home, "FACTORY_ROOT=" + root}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	logged := func(prefix string) bool {
		b, _ := os.ReadFile(podmanLog)
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, prefix) {
				return true
			}
		}
		return false
	}
	waitFor(t, "podman run", func() bool { return logged("run --rm --name factory-job-t0001-w1") })

	cmd.Process.Signal(syscall.SIGHUP)
	select {
	case err := <-done:
		t.Fatalf("SIGHUP ended the run: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	cmd.Process.Signal(syscall.SIGTERM)
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("SIGTERM did not end the run")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Errorf("exit: %v", err)
	}
	if !logged("rm -f --time 5 factory-job-t0001-w1") {
		b, _ := os.ReadFile(podmanLog)
		t.Errorf("the container was not removed; podman calls:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(job, "exit")); string(b) != "255" {
		t.Errorf("exit file %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(job, "run.log")); !strings.Contains(string(b), "the agent started") {
		t.Errorf("run.log %q", b)
	}
}
