package execx

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sh(script string, args ...string) []string {
	return append([]string{"/bin/sh", "-c", script, "sh"}, args...)
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	r, err := OS{}.Run(context.Background(), Cmd{Argv: sh(`echo out; echo err >&2; exit 3`)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Code != 3 || string(r.Stdout) != "out\n" || string(r.Stderr) != "err\n" {
		t.Errorf("got %+v", r)
	}
	if r.Out() != "out\n" {
		t.Errorf("Out %q", r.Out())
	}
}

func TestRunDirEnvStdin(t *testing.T) {
	dir := t.TempDir()
	r, err := OS{}.Run(context.Background(), Cmd{
		Argv:  sh(`pwd; echo "$ONLY-${HOME:-nohome}"; cat`),
		Dir:   dir,
		Env:   []string{"ONLY=yes", "PATH=/usr/bin:/bin"},
		Stdin: strings.NewReader("from stdin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if want := real + "\nyes-nohome\nfrom stdin"; string(r.Stdout) != want {
		t.Errorf("stdout %q, want %q", r.Stdout, want)
	}
}

func TestRunWithNoStdinReadsNothing(t *testing.T) {
	r, err := OS{}.Run(context.Background(), Cmd{Argv: sh(`cat; echo done`)})
	if err != nil || string(r.Stdout) != "done\n" {
		t.Errorf("%q %v", r.Stdout, err)
	}
}

func TestRunAMissingProgramIsAnError(t *testing.T) {
	_, err := OS{}.Run(context.Background(), Cmd{Argv: []string{"/nonexistent/prog"}})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err %v", err)
	}
	if _, err := (OS{}).Run(context.Background(), Cmd{}); err == nil {
		t.Error("empty argv ran")
	}
}

// A signal-killed process reports -signum, as Python's returncode does.
func TestRunSignalExitCodeIsPythons(t *testing.T) {
	r, err := OS{}.Run(context.Background(), Cmd{Argv: sh(`kill -9 $$`)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Code != -9 {
		t.Errorf("code %d, want -9", r.Code)
	}
}

func TestRunKeepsTheTailWhenOutputIsCapped(t *testing.T) {
	r, err := OS{}.Run(context.Background(), Cmd{Argv: sh(`i=0; while [ $i -lt 2000 ]; do echo "line $i"; i=$((i+1)); done; echo tail >&2`), MaxOut: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Stdout) != 100 || !strings.HasSuffix(string(r.Stdout), "line 1999\n") || !r.Truncated {
		t.Errorf("stdout %d bytes %q truncated=%v", len(r.Stdout), r.Stdout, r.Truncated)
	}
	if string(r.Stderr) != "tail\n" {
		t.Errorf("stderr %q", r.Stderr)
	}
}

func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// a zombie waiting for its reaper is dead for our purpose
	fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

// A timeout kills the whole process group: TERM, then KILL after WaitDelay
// for anything that ignores TERM. No child is left behind.
func TestTimeoutKillsTheWholeGroupAndLeavesNoChild(t *testing.T) {
	start := time.Now()
	r, err := OS{WaitDelay: 200 * time.Millisecond}.Run(context.Background(), Cmd{
		Argv:    sh(`trap "" TERM; (trap "" TERM; exec sleep 60) & echo $!; wait`),
		Timeout: 300 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v, want ErrTimeout", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v", took)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(r.Stdout)))
	if perr != nil {
		t.Fatalf("child pid %q", r.Stdout)
	}
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("child %d survived the timeout", pid)
	}
}

func TestTermIsEnoughForAWellBehavedGroup(t *testing.T) {
	start := time.Now()
	_, err := OS{WaitDelay: 10 * time.Second}.Run(context.Background(), Cmd{Argv: sh(`sleep 60 & wait`), Timeout: 100 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("waited for the KILL (%v) though TERM sufficed", took)
	}
}

func TestCancelEndsTheRunAndCallsCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cleaned := make(chan bool, 1)
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := OS{WaitDelay: time.Second}.Run(ctx, Cmd{Argv: sh(`exec sleep 60`), Cleanup: func() { cleaned <- true }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	select {
	case <-cleaned:
	default:
		t.Error("cleanup not called after a cancelled run")
	}
	if _, err := (OS{}).Run(context.Background(), Cmd{Argv: sh(`true`), Cleanup: func() { cleaned <- true }}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleaned:
		t.Error("cleanup called after a run that finished")
	default:
	}
}

func TestADeadContextStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := OS{}.Run(ctx, Cmd{Argv: sh(`touch "$1"`, marker)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("ran under a dead context")
	}
}

func TestCheckErrIsPythonsRunMessage(t *testing.T) {
	long := strings.Repeat("é", 500)
	r := Result{Argv: []string{"git", "-C", "/repo", "fetch", "origin", "main"}, Code: 128, Stderr: []byte("\n  " + long + "end \n\x1c")}
	err := CheckErr(r)
	want := "git -C /repo fetch failed (128): " + strings.Repeat("é", 397) + "end"
	if err == nil || err.Error() != want {
		t.Fatalf("got %q\nwant %q", err, want)
	}
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 128 {
		t.Errorf("not an *ExitError: %v", err)
	}
	if CheckErr(Result{Argv: []string{"true"}}) != nil {
		t.Error("rc 0 is no error")
	}
	if got := CheckErr(Result{Argv: []string{"x"}, Code: 1}).Error(); got != "x failed (1): " {
		t.Errorf("empty stderr: %q", got)
	}
}

func TestFuncIsAnExecer(t *testing.T) {
	var e Execer = Func(func(_ context.Context, c Cmd) (Result, error) {
		return Result{Argv: c.Argv, Stdout: []byte("fake")}, nil
	})
	r, err := e.Run(context.Background(), Cmd{Argv: []string{"scion", "list"}})
	if err != nil || r.Out() != "fake" {
		t.Errorf("%v %v", r, err)
	}
}

func TestSpawnDetachesIntoItsOwnSession(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	err := OS{}.Spawn(Cmd{Argv: sh(`read -r a b c d e sid rest < /proc/$$/stat; echo "$$ $sid" > "$1.tmp"; ls -l /proc/$$/fd/0 /proc/$$/fd/1 >> "$1.tmp" 2>&1; mv "$1.tmp" "$1"`, out), Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var b []byte
	for range 250 {
		if b, err = os.ReadFile(out); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("spawned process never ran")
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if f := strings.Fields(lines[0]); len(f) != 2 || f[0] != f[1] {
		t.Errorf("not a session leader: %q", lines[0])
	}
	if !strings.Contains(string(b), "/dev/null") {
		t.Errorf("stdio not /dev/null: %s", b)
	}
}
