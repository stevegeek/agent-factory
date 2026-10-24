package cli

import (
	"bytes"
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

// buildFactory builds cmd/factory into a temp dir: the exit codes are the
// binary's, so they are checked on the binary (§10.2).
func buildFactory(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	out := filepath.Join(t.TempDir(), "factory")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, gobin, "build", "-o", out, "./cmd/factory")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func runBinary(t *testing.T, bin string, f *fixture, stdin string, argv ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = f.env.Environ()
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, out.String(), errb.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), out.String(), errb.String()
	}
	t.Fatal(err)
	return 0, "", ""
}

// port: TT VerbTest.test_bad_input_is_bad_request_and_a_crash_is_not_a_refusal
// Ported here: exit codes 0/1/2/3 of the binary.
func TestTheBinarysExitCodes(t *testing.T) {
	bin := buildFactory(t)
	f := ticketFixture(t)
	code, out, _ := runBinary(t, bin, f, `{"project": "sandbox", "title": "t"}`, "ticket", "create", "--json")
	if code != 0 || !strings.HasPrefix(out, `{"ok": true, "created": true, "ticket": {`) {
		t.Errorf("create: %d %q", code, out)
	}
	code, out, _ = runBinary(t, bin, f, "not json", "ticket", "get", "--json")
	if code != 1 || !strings.Contains(out, `"bad_request"`) {
		t.Errorf("bad json: %d %q", code, out)
	}
	code, out, errs := runBinary(t, bin, f, "", "ticket", "nope", "--json")
	if code != 2 || out != "" || !strings.Contains(errs, "invalid choice") {
		t.Errorf("bad verb: %d %q %q", code, out, errs)
	}
	code, out, _ = runBinary(t, bin, f, "", "version")
	if code != 0 || out != "dev dev\n" {
		t.Errorf("version: %d %q", code, out)
	}
	code, _, errs = runBinary(t, bin, f, "")
	if code != 2 || !strings.HasPrefix(errs, "usage: factory") {
		t.Errorf("no command: %d %q", code, errs)
	}
	os.Remove(f.env.Path("config.toml"))
	code, out, errs = runBinary(t, bin, f, "{}", "ticket", "whoami", "--json")
	if code != 3 || out != "" || errs == "" {
		t.Errorf("no config: %d %q %q", code, out, errs)
	}
	code, out, errs = runBinary(t, bin, f, "", "ls")
	if code != 1 || out != "" || !strings.HasPrefix(errs, "factory: ") {
		t.Errorf("ls with no config: %d %q %q", code, out, errs)
	}
}

// A command killed by a signal exits 128+n (§7.2): the lock wait of
// `ticket --json` is cut short by SIGTERM.
func TestASignalExits128PlusN(t *testing.T) {
	bin := buildFactory(t)
	f := ticketFixture(t)
	if code, _, errs := runBinary(t, bin, f, `{"project": "sandbox", "title": "t"}`, "ticket", "create", "--json"); code != 0 {
		t.Fatal(errs)
	}
	lockDir := f.env.Path("board", "tickets", ".locks")
	os.MkdirAll(lockDir, 0o755)
	lf, err := os.OpenFile(filepath.Join(lockDir, "T0001"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "ticket", "note", "--json", "--wait", "30")
	cmd.Env = f.env.Environ()
	cmd.Stdin = strings.NewReader(`{"id": "T0001", "text": "x"}`)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 128+int(syscall.SIGTERM) {
			t.Errorf("exit: %v (stdout %q)", err, out.String())
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("SIGTERM did not end the wait")
	}
}
