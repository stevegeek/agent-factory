// Package wake starts a tick now (ARCHITECTURE.md §5.5).
//
// Python's wake() Popen'd a detached tick, which inherited the caller's
// cgroup: a wake from Foreman ran the tick inside Foreman's 64 MB limit. Wake
// instead leaves .tick.again and asks the user's systemd for a transient
// unit, so the tick runs in the factory user's own slice. Without a user
// bus it falls back to a detached bin/factory tick. A wake while a tick
// runs only matters through .tick.again, which the running tick sees.
//
// Foreman never reaches the bus (§6.2): its Waker has a Trigger file, which
// the factory-wake.path user unit watches.
package wake

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
)

// SystemdRunTimeout bounds the systemd-run call (it does not wait for the
// tick: --no-block).
const SystemdRunTimeout = 30 * time.Second

// Clock is the time source (the unit name and the wake-log stamp).
type Clock = clock.Clock

// Spawner starts a detached process (execx.OS does).
type Spawner interface {
	Spawn(c execx.Cmd) error
}

// Waker starts ticks. Env is the caller's environment, from which only the
// variables named in passEnv and the FACTORY_* settings reach the tick.
type Waker struct {
	Paths paths.Paths
	Exec  execx.Execer // runs systemd-run
	Spawn Spawner      // the fallback
	Env   map[string]string
	Clock Clock
	PID   int // in the unit name; zero means os.Getpid()
	// Trigger, when set, is the only way this Waker wakes a tick: it writes
	// the file (Paths.WakeTrigger), and the factory-wake.path user unit
	// starts the tick outside the caller's sandbox. Foreman sets it: its
	// sandbox hides the user manager's bus, since any process that reaches
	// the bus can run any command outside the sandbox (§6.2). It never
	// asks systemd and never spawns a tick itself.
	Trigger string
}

// passEnv are the variables passed to a fallback tick by name (§5.3), besides
// FACTORY_*. Credentials are never among them: the tick reads its token files.
func passEnv() []string {
	return []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}
}

// Wake starts a tick. With FACTORY_WAKE_LOG set (the differential tests'
// seam, as in Python) it only appends "<stamp> wake" to that file.
func (w Waker) Wake(ctx context.Context) error {
	if logPath := w.Env["FACTORY_WAKE_LOG"]; logPath != "" {
		return w.note(logPath)
	}
	// the flag first: whatever happens next, a running tick goes round again.
	// A failed flag matters only when no tick starts: a tick that starts and
	// finds the lock held leaves the flag itself. That is the sandboxed
	// Foreman's case, where .tick.again is read-only and the transient unit
	// runs outside the sandbox.
	flagErr := fsx.Touch(w.Paths.TickAgain)
	if flagErr != nil {
		flagErr = fmt.Errorf("wake: %w", flagErr)
	}
	if w.Trigger != "" {
		return w.trigger()
	}
	var unitErr error
	if w.userBus() {
		if unitErr = w.startUnit(ctx); unitErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(flagErr, unitErr)
		}
	}
	argv := []string{w.Paths.FactoryBin(), "tick"}
	if err := w.Spawn.Spawn(execx.Cmd{Argv: argv, Env: w.tickEnv()}); err != nil {
		return errors.Join(flagErr, unitErr, fmt.Errorf("wake: %w", err))
	}
	return nil
}

// trigger rewrites the trigger file with the wake's stamp. Writing and
// closing it is the change factory-wake.path watches for (PathModified=).
// The flag a running tick needs is the woken tick's to leave: it finds the
// tick lock held and leaves .tick.again itself.
func (w Waker) trigger() error {
	if err := os.MkdirAll(filepath.Dir(w.Trigger), 0o700); err != nil {
		return fmt.Errorf("wake trigger: %w", err)
	}
	if err := os.WriteFile(w.Trigger, []byte(clock.Stamp(w.Clock.Now())+" wake\n"), 0o600); err != nil {
		return fmt.Errorf("wake trigger: %w", err)
	}
	return nil
}

func (w Waker) note(logPath string) error {
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return fmt.Errorf("wake log: %w", err)
	}
	_, err = f.WriteString(clock.Stamp(w.Clock.Now()) + " wake\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("wake log: %w", err)
	}
	return nil
}

// userBus reports whether XDG_RUNTIME_DIR holds the user manager's bus.
func (w Waker) userBus() bool {
	dir := w.Env["XDG_RUNTIME_DIR"]
	if dir == "" {
		return false
	}
	st, err := os.Lstat(filepath.Join(dir, "bus"))
	return err == nil && st.Mode()&fs.ModeSocket != 0
}

// UnitName is the transient unit for one wake.
func (w Waker) UnitName() string {
	pid := w.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	return "factory-wake-" + strconv.FormatInt(w.Clock.Now().UnixNano(), 10) + "-" + strconv.Itoa(pid)
}

func (w Waker) startUnit(ctx context.Context) error {
	argv := []string{"systemd-run", "--user", "--no-block", "--collect",
		"--unit=" + w.UnitName(), "--property=RuntimeMaxSec=45min"}
	// the user manager's environment is not ours: the root and the other
	// FACTORY_* settings go with the unit (none is a secret)
	for _, kv := range w.factoryEnv() {
		argv = append(argv, "--setenv="+kv)
	}
	argv = append(argv, w.Paths.FactoryBin(), "tick")
	var env []string
	for _, k := range []string{"PATH", "HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := w.Env[k]; ok {
			env = append(env, k+"="+v)
		}
	}
	r, err := w.Exec.Run(ctx, execx.Cmd{Argv: argv, Env: env, Timeout: SystemdRunTimeout})
	if err != nil {
		return fmt.Errorf("wake: %w", err)
	}
	if r.Code == 0 || alreadyExists(r.Stderr) {
		return nil
	}
	return fmt.Errorf("wake: %w", execx.CheckErr(r))
}

// alreadyExists recognises systemd refusing a unit name that is taken: a
// wake in the same nanosecond, which is starting a tick already.
func alreadyExists(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "already exists") || strings.Contains(s, "already loaded")
}

// factoryEnv is FACTORY_ROOT and FACTORY_API_TOKENS from Paths, plus every
// other FACTORY_* variable of the caller, sorted.
func (w Waker) factoryEnv() []string {
	vars := map[string]string{}
	for k, v := range w.Env {
		if strings.HasPrefix(k, "FACTORY_") && v != "" {
			vars[k] = v
		}
	}
	vars["FACTORY_ROOT"] = w.Paths.Root
	vars["FACTORY_API_TOKENS"] = w.Paths.Tokens
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (w Waker) tickEnv() []string {
	env := w.factoryEnv()
	for _, k := range passEnv() {
		if v, ok := w.Env[k]; ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
