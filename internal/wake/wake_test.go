package wake

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

var at = time.Unix(1_800_000_000, 123456789).UTC()

type fakes struct {
	runs   []execx.Cmd
	spawns []execx.Cmd
	answer func(execx.Cmd) (execx.Result, error)
	spawn  error
}

func (f *fakes) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	f.runs = append(f.runs, c)
	if f.answer != nil {
		return f.answer(c)
	}
	return execx.Result{Argv: c.Argv}, nil
}

func (f *fakes) Spawn(c execx.Cmd) error {
	f.spawns = append(f.spawns, c)
	return f.spawn
}

// userBus makes $XDG_RUNTIME_DIR/bus a socket, as a running user manager has.
func userBus(t *testing.T) string {
	t.Helper()
	// a short path: unix socket names are limited to 108 bytes
	dir, err := os.MkdirTemp("", "xdg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "bus"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return dir
}

func setup(t *testing.T, env map[string]string) (Waker, *fakes, paths.Paths) {
	t.Helper()
	e := testkit.Root(t)
	full := map[string]string{}
	for k, v := range e.Vars {
		full[k] = v
	}
	for k, v := range env {
		full[k] = v
	}
	p := paths.FromEnv(full)
	f := &fakes{}
	return Waker{Paths: p, Exec: f, Spawn: f, Env: full, Clock: testkit.Fixed{T: at}, PID: 4242}, f, p
}

// port: TF WakeTest.test_a_wake_during_a_tick_makes_it_go_round_once_more
// (the wake's half: it always leaves .tick.again, which a running tick sees
// and goes round once more; the tick's half is in pipeline/tick_test.go.)
func TestAWakeLeavesTheFlagForARunningTick(t *testing.T) {
	for name, env := range map[string]map[string]string{"user bus": nil, "no bus": {"XDG_RUNTIME_DIR": ""}} {
		t.Run(name, func(t *testing.T) {
			if env == nil {
				env = map[string]string{"XDG_RUNTIME_DIR": userBus(t)}
			}
			w, _, p := setup(t, env)
			if err := w.Wake(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(p.TickAgain); err != nil {
				t.Errorf("no .tick.again: %v", err)
			}
		})
	}
}

func TestWithAUserBusTheTickStartsAsItsOwnUnit(t *testing.T) {
	xdg := userBus(t)
	w, f, p := setup(t, map[string]string{"XDG_RUNTIME_DIR": xdg, "DBUS_SESSION_BUS_ADDRESS": "unix:path=" + xdg + "/bus",
		"PATH": "/usr/bin:/bin", "FACTORY_GITHUB_API": "http://127.0.0.1:1", "SECRET_THING": "x", "ANTHROPIC_API_KEY": "k"})
	if err := w.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.spawns) != 0 || len(f.runs) != 1 {
		t.Fatalf("runs %v spawns %v", f.runs, f.spawns)
	}
	c := f.runs[0]
	want := []string{"systemd-run", "--user", "--no-block", "--collect",
		"--unit=factory-wake-1800000000123456789-4242", "--property=RuntimeMaxSec=45min",
		"--setenv=FACTORY_API_TOKENS=" + p.Tokens,
		"--setenv=FACTORY_GITHUB_API=http://127.0.0.1:1",
		"--setenv=FACTORY_ROOT=" + p.Root,
		p.Root + "/bin/factory", "tick"}
	if !slices.Equal(c.Argv, want) {
		t.Errorf("argv\n%q\nwant\n%q", c.Argv, want)
	}
	if c.Timeout <= 0 {
		t.Error("systemd-run has no timeout")
	}
	env := strings.Join(c.Env, "\n")
	for _, s := range []string{"XDG_RUNTIME_DIR=" + xdg, "DBUS_SESSION_BUS_ADDRESS=", "PATH=/usr/bin:/bin"} {
		if !strings.Contains(env, s) {
			t.Errorf("env lacks %s: %q", s, c.Env)
		}
	}
	for _, s := range []string{"SECRET_THING", "ANTHROPIC_API_KEY"} {
		if strings.Contains(env, s) || strings.Contains(strings.Join(c.Argv, " "), s) {
			t.Errorf("%s passed on", s)
		}
	}
}

// Two wakes in the same nanosecond (or a reused pid) name the same unit:
// systemd refuses the second, and that is success, because .tick.again is
// touched and a tick is starting.
func TestAUnitNameCollisionIsSuccess(t *testing.T) {
	w, f, _ := setup(t, map[string]string{"XDG_RUNTIME_DIR": userBus(t)})
	f.answer = func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Argv: c.Argv, Code: 1, Stderr: []byte("Failed to start transient service unit: Unit factory-wake-1800000000123456789-4242.service already exists.\n")}, nil
	}
	if err := w.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.spawns) != 0 {
		t.Errorf("fell back after a collision: %v", f.spawns)
	}
}

func TestFallbackStartsBinFactoryDetached(t *testing.T) {
	cases := map[string]struct {
		env    map[string]string
		answer func(execx.Cmd) (execx.Result, error)
		runs   int
	}{
		"no runtime dir":         {map[string]string{"XDG_RUNTIME_DIR": ""}, nil, 0},
		"runtime dir but no bus": {map[string]string{"XDG_RUNTIME_DIR": "/nonexistent"}, nil, 0},
		"systemd-run fails": {nil, func(c execx.Cmd) (execx.Result, error) {
			return execx.Result{Argv: c.Argv, Code: 1, Stderr: []byte("Failed to connect to bus: No medium found\n")}, nil
		}, 1},
		"systemd-run missing": {nil, func(c execx.Cmd) (execx.Result, error) {
			return execx.Result{Argv: c.Argv}, errors.New("run systemd-run: executable file not found in $PATH")
		}, 1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := c.env
			if env == nil {
				env = map[string]string{"XDG_RUNTIME_DIR": userBus(t)}
			}
			env["PATH"] = "/usr/bin:/bin"
			env["CLAUDE_CODE_OAUTH_TOKEN"] = "never-passed"
			w, f, p := setup(t, env)
			f.answer = c.answer
			if err := w.Wake(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.runs) != c.runs || len(f.spawns) != 1 {
				t.Fatalf("runs %d spawns %d", len(f.runs), len(f.spawns))
			}
			s := f.spawns[0]
			// never os.Executable(): inside Foreman that is foreman
			if !slices.Equal(s.Argv, []string{filepath.Join(p.Bin, "factory"), "tick"}) {
				t.Errorf("argv %q", s.Argv)
			}
			env2 := strings.Join(s.Env, "\n")
			if !strings.Contains(env2, "FACTORY_ROOT="+p.Root) || !strings.Contains(env2, "PATH=/usr/bin:/bin") {
				t.Errorf("env %q", s.Env)
			}
			if strings.Contains(env2, "never-passed") {
				t.Error("a credential reached the tick's environment")
			}
		})
	}
}

func TestFallbackFailureIsReported(t *testing.T) {
	w, f, p := setup(t, map[string]string{"XDG_RUNTIME_DIR": ""})
	f.spawn = errors.New("spawn: no such file")
	err := w.Wake(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err %v", err)
	}
	if _, serr := os.Stat(p.TickAgain); serr != nil {
		t.Error("flag not left")
	}
}

// The real fallback: a detached bin/factory tick, in its own session.
func TestFallbackRunsTheRealBinary(t *testing.T) {
	w, _, p := setup(t, map[string]string{"XDG_RUNTIME_DIR": "", "PATH": "/usr/bin:/bin"})
	w.Exec, w.Spawn = execx.OS{}, execx.OS{}
	out := filepath.Join(p.Root, "woke")
	if err := os.MkdirAll(p.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nread -r a b c d e sid rest < /proc/$$/stat\necho \"$* $FACTORY_ROOT $sid $$\" > \"$FACTORY_ROOT/woke.tmp\"\nmv \"$FACTORY_ROOT/woke.tmp\" \"$FACTORY_ROOT/woke\"\n"
	if err := os.WriteFile(filepath.Join(p.Bin, "factory"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	var b []byte
	var err error
	for range 250 {
		if b, err = os.ReadFile(out); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("bin/factory never ran")
	}
	f := strings.Fields(string(b))
	if len(f) != 4 || f[0] != "tick" || f[1] != p.Root || f[2] != f[3] {
		t.Errorf("child said %q (want: tick, the root, a session of its own)", b)
	}
}

// FACTORY_WAKE_LOG (the differential tests' seam, as in Python): the wake
// is only noted in that file, and nothing starts.
func TestTheWakeLogSeamOnlyNotesTheWake(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "wakes")
	w, f, p := setup(t, map[string]string{"FACTORY_WAKE_LOG": logPath, "XDG_RUNTIME_DIR": userBus(t)})
	for range 2 {
		if err := w.Wake(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(logPath)
	if err != nil || string(b) != "2027-01-15T08:00:00+00:00 wake\n2027-01-15T08:00:00+00:00 wake\n" {
		t.Errorf("wake log %q %v", b, err)
	}
	if len(f.runs)+len(f.spawns) != 0 {
		t.Error("started a tick under the seam")
	}
	if _, err := os.Stat(p.TickAgain); err == nil {
		t.Error("Python's seam leaves no .tick.again")
	}
}

func TestADeadContextStillLeavesTheFlag(t *testing.T) {
	w, f, p := setup(t, map[string]string{"XDG_RUNTIME_DIR": userBus(t)})
	f.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Argv: c.Argv}, context.Canceled }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Wake(ctx)
	if _, err := os.Stat(p.TickAgain); err != nil {
		t.Error("no flag")
	}
}

// Under the sandboxed Foreman .tick.again cannot be written. A tick that
// starts leaves the flag itself if one is running, so the wake succeeds;
// with no tick started the flag error is reported with the start error.
func TestAnUnwritableFlagIsMootOnceATickStarts(t *testing.T) {
	for name, env := range map[string]map[string]string{"user bus": nil, "no bus": {"XDG_RUNTIME_DIR": ""}} {
		t.Run(name, func(t *testing.T) {
			if env == nil {
				env = map[string]string{"XDG_RUNTIME_DIR": userBus(t)}
			}
			w, _, _ := setup(t, env)
			w.Paths.TickAgain = filepath.Join(t.TempDir(), "missing", ".tick.again")
			if err := w.Wake(context.Background()); err != nil {
				t.Fatalf("wake: %v", err)
			}
		})
	}
	t.Run("nothing started", func(t *testing.T) {
		w, f, _ := setup(t, map[string]string{"XDG_RUNTIME_DIR": ""})
		w.Paths.TickAgain = filepath.Join(t.TempDir(), "missing", ".tick.again")
		f.spawn = errors.New("no fork")
		err := w.Wake(context.Background())
		if err == nil || !strings.Contains(err.Error(), "no fork") || !strings.Contains(err.Error(), ".tick.again") {
			t.Fatalf("err %v", err)
		}
	})
}

// Foreman's sandbox hides the user manager's bus, so a Waker with a
// Trigger never asks systemd (systemd-run through the bus would run any
// command outside the sandbox) and never spawns a tick inside the sandbox:
// it writes the trigger file, which the factory-wake.path unit watches.
func TestATriggerWakesThroughTheFileOnly(t *testing.T) {
	w, f, p := setup(t, map[string]string{"XDG_RUNTIME_DIR": userBus(t)})
	w.Trigger = p.WakeTrigger()
	if err := w.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 0 || len(f.spawns) != 0 {
		t.Errorf("a trigger wake ran %v and spawned %v", f.runs, f.spawns)
	}
	b, err := os.ReadFile(p.WakeTrigger())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "2027-01-15T08:00:00+00:00 wake\n" {
		t.Errorf("trigger holds %q", b)
	}
	if _, err := os.Stat(p.TickAgain); err != nil {
		t.Errorf("no .tick.again: %v", err)
	}
}

// A trigger that cannot be written is the wake's error, and nothing else
// is tried; .tick.again being read-only (the sandbox) is not an error.
func TestAnUnwritableTriggerIsAnError(t *testing.T) {
	w, f, p := setup(t, nil)
	w.Trigger = filepath.Join(p.Root, "no-such-dir", "wake")
	if err := os.WriteFile(filepath.Join(p.Root, "no-such-dir"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := w.Wake(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wake trigger") {
		t.Errorf("err = %v", err)
	}
	if len(f.runs) != 0 || len(f.spawns) != 0 {
		t.Errorf("fell back to %v %v", f.runs, f.spawns)
	}
}
