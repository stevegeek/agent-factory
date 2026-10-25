package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The early checks: when the work stage leaves a head, Gate 1's checks of
// it start at once, in a unit of their own (Early.Start, which runs
// `factory early-checks`, RunEarly), while review, security and public
// review run. Their result is kept for the ticket under everything its
// outcome depends on (earlyKey). At the gate column Gate 1 uses it (Kept)
// only when that whole key is what it is now, and otherwise runs the
// checks itself; the changed files, walls, hygiene, the decision, the
// audit and every write stay with Gate 1 there. The early checks read the
// ticket's file (its project and branch) but never change it, a result is
// used at most once, and never for a head a gate already decided.
const (
	// EarlyUnit is the early checks' transient unit: one name, so systemd
	// runs at most one at a time.
	EarlyUnit = "factory-gate-early"
	// earlyMaxRuntime is above a warm's and the checks' own timeouts.
	earlyMaxRuntime = 70 * time.Minute
	earlyKeepFor    = 7 * 24 * time.Hour
	earlyMaxBytes   = 64 << 10
	earlyLock       = ".lock"
	// earlyLockWait is how long early checks wait for the lock: a tick
	// asking whether they run (EarlyRunning) holds it for an instant, and
	// other early checks for minutes.
	earlyLockWait = 2 * time.Second
	// earlyMarker names the early checks that hold the lock: their ticket
	// and head, while they run.
	earlyMarker = ".running"
	// earlyStarting names the early checks the tick has just started, from
	// systemd-run until they hold the lock (or for earlyStartGrace, if
	// they never do).
	earlyStarting     = ".starting"
	earlyStartGrace   = 2 * time.Minute
	earlyUsed         = ".used.json" // <ticket>.used.json: a result the gate is reading
	earlyPrefix       = "early-"
	systemdRunTimeout = 30 * time.Second
	// earlyTimeout bounds EarlyChecks: a warm and the checks, each at most
	// their own 30 minutes, inside the unit's earlyMaxRuntime.
	earlyTimeout = podman.WarmTimeout + podman.GateTimeout + 5*time.Minute
)

// earlyKey is what a kept result is for. Image is the image's id, not its
// tag, which a rebuild moves; Setup is a digest of the rest of what the
// container gets (the tag, the gate memory, [gate] env and mounts, the
// pinned files as they are in the checkout).
type earlyKey struct {
	Project string `json:"project"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	Image   string `json:"image"`
	Script  string `json:"script"`
	Setup   string `json:"setup"`
}

// earlyResult is one kept result, gate-early/<ticket>.json.
type earlyResult struct {
	Key    earlyKey `json:"key"`
	Pass   bool     `json:"pass"`
	Output string   `json:"output"`
	At     string   `json:"at"`
}

// earlyFile is where the ticket's kept result is, "" for an id that is not
// a ticket's.
func (r Runner) earlyFile(id ticket.ID) string {
	if got, ok := ticket.ParseID(string(id)); !ok || got != id {
		return ""
	}
	return filepath.Join(r.Paths.EarlyChecks(), string(id)+".json")
}

// EarlyChecks is `factory early-checks`: RunEarly for the ticket's project
// under earlyTimeout, with any error also in the log. A paused project's
// ticket, a board project's or one config does not have is not checked. A
// heavy gate's is, when asked by hand (the tick never starts those): only
// a pass is kept, and a heavy gate waits only for memory.
func (r Runner) EarlyChecks(ctx context.Context, cfg *config.Config, t *ticket.Ticket, head string) error {
	p, ok := cfg.Project(t.Project)
	mode, _ := cfg.ProjectMode(t.Project)
	if !ok || !cfg.Enabled(t.Project) || mode == config.ModeBoard {
		r.logf("%s: not checked early: project %s is paused, a board project or not in config", t.ID, t.Project)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, earlyTimeout)
	defer cancel()
	err := r.RunEarly(ctx, cfg, *p, t, head)
	if err != nil {
		r.logf("%s: early checks: %v", t.ID, err)
	}
	return err
}

// RunEarly is Gate 1's checks of head, the ticket's branch's head, on a
// fresh checkout of their own (gate/early-<id>, lent the build cache as
// Gate 1's checks are), and the result kept for the ticket. One at a time
// on this host: when another holds the lock it does nothing. A branch no
// longer at head is not checked. Only checks that pass are kept: checks
// that fail (perhaps only because they ran beside the agents) or do not
// finish keep nothing, and neither do checks during which their key
// changed (a rebuild moved the image, a pinned file changed).
func (r Runner) RunEarly(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, head string) error {
	file := r.earlyFile(t.ID)
	if file == "" {
		return fmt.Errorf("early checks: bad ticket %q", t.ID)
	}
	if !commitID(head) {
		return fmt.Errorf("early checks %s: bad head %q", t.ID, textx.Prefix(head, 80))
	}
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("early checks %s: %w", t.ID, err)
	}
	lock, err := fsx.Lock(ctx, filepath.Join(dir, earlyLock), fsx.Wait(earlyLockWait))
	if errors.Is(err, fsx.ErrBusy) {
		r.logf("%s: another early check runs; this one's checks run at the gate", t.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("early checks %s: %w", t.ID, err)
	}
	defer lock.Unlock()
	defer os.Remove(filepath.Join(dir, earlyStarting)) // they run now, or never
	repo := r.Repo(p)
	branch := t.Branch.Or("")
	now, err := repo.Head(ctx, branch)
	if err != nil {
		return fmt.Errorf("early checks %s: %w", t.ID, err)
	}
	if now != head {
		r.logf("%s: %s is at %s, not %s: not checked early", t.ID, branch, textx.Prefix(now, 12), head[:12])
		return nil
	}
	// the gate of this head waits while the marker names it, and the tick
	// counts the checks as one of this host's slots (EarlyRunning)
	marker := filepath.Join(dir, earlyMarker)
	b, err := json.Marshal(earlyRun{Ticket: t.ID, Head: head})
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(marker, b, 0o600); err != nil {
		return fmt.Errorf("early checks %s: %w", t.ID, err)
	}
	defer os.Remove(marker) // before the lock is let go
	base, err := repo.Head(ctx, p.Base)
	if err != nil {
		return fmt.Errorf("early checks %s: %w", t.ID, err)
	}
	key, err := r.earlyKey(ctx, cfg, p, head, base)
	if err != nil {
		return fmt.Errorf("early checks %s: %w", t.ID, err)
	}
	ok, tail, err := r.Checks(ctx, CheckIn{Config: cfg, Project: p, ID: earlyPrefix + string(t.ID), Ref: head, Base: base})
	if err != nil {
		return err
	}
	after, err := r.earlyKey(ctx, cfg, p, head, base)
	if err != nil {
		r.logf("%s: early checks: %v: nothing kept", t.ID, err)
		return nil
	}
	if after != key {
		r.logf("%s: the %s changed while the early checks ran: nothing kept", t.ID, differs(key, after))
		return nil
	}
	if !ok {
		// they ran beside the agents, in a busier host than Gate 1's: a
		// failure may be the crowding's (a timing test, the memory
		// limit), so Gate 1 runs the checks itself
		r.logf("%s: the early checks of %s failed: nothing kept, the gate runs them itself", t.ID, head[:12])
		return nil
	}
	b, err = json.Marshal(earlyResult{Key: key, Pass: true, Output: tail, At: clock.Stamp(r.now())})
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(file, b, 0o600); err != nil {
		return fmt.Errorf("early checks %s: %w", t.ID, err)
	}
	r.pruneEarly(dir)
	r.logf("%s: early checks of %s passed; kept for the gate", t.ID, head[:12])
	return nil
}

// earlyRun is the marker of the early checks that run now.
type earlyRun struct {
	Ticket ticket.ID `json:"ticket"`
	Head   string    `json:"head"`
}

// EarlyRunning is the ticket and head of the early checks that run on
// this host now: what their marker names while they hold the lock, for at
// most earlyMaxRuntime, or else what the tick started less than
// earlyStartGrace ago (Early.Start) and has not yet taken the lock. A
// marker whose checks are gone (killed) is removed. The tick asks it to
// count them as one of this host's slots and to hold that head's gate
// until they are done.
func EarlyRunning(p paths.Paths) (ticket.ID, string, bool) {
	dir := p.EarlyChecks()
	marker := filepath.Join(dir, earlyMarker)
	if run, ok := readRun(marker, earlyMaxRuntime); ok {
		lock, err := fsx.TryLock(filepath.Join(dir, earlyLock))
		if err == nil {
			os.Remove(marker) // nothing holds the lock: the checks are gone
			lock.Unlock()
		} else if errors.Is(err, fsx.ErrBusy) {
			return run.Ticket, run.Head, true
		}
	}
	if run, ok := readRun(filepath.Join(dir, earlyStarting), earlyStartGrace); ok {
		return run.Ticket, run.Head, true
	}
	return "", "", false
}

// readRun reads a marker no older than maxAge that names a ticket and a
// commit; anything else is none.
func readRun(file string, maxAge time.Duration) (earlyRun, bool) {
	raw, err := fsx.ReadNoFollow(file, 1<<10)
	if err != nil {
		return earlyRun{}, false
	}
	fi, err := os.Lstat(file)
	if err != nil || fi.ModTime().Before(clock.System{}.Now().Add(-maxAge)) {
		return earlyRun{}, false
	}
	var run earlyRun
	if json.Unmarshal(raw, &run) != nil || !commitID(run.Head) {
		return earlyRun{}, false
	}
	if id, ok := ticket.ParseID(string(run.Ticket)); !ok || id != run.Ticket {
		return earlyRun{}, false
	}
	return run, true
}

// Kept is the early checks' result for the ticket as checks that ran
// elsewhere (Decide's input, with no host), when its whole key is the
// ticket's as it is now: the branch's and the base's heads, the image's
// id, the commands and the rest of the container's setup, and only when
// they passed (an older factory kept failures too) and no gate of the
// ticket has decided this head yet: a result that lands after its gate
// ran (a gate run again is run again). The result is removed whether it
// is used or not, so it is used at most once. A result that does not
// read, or is a link, is none; the error is only for git.
func (r Runner) Kept(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (Remote, bool, error) {
	file := r.earlyFile(t.ID)
	if file == "" {
		return Remote{}, false, nil
	}
	// claimed by a rename, so early checks keeping a newer result
	// meanwhile keep it for the next gate: only what was read is removed
	used := strings.TrimSuffix(file, ".json") + earlyUsed
	if err := os.Rename(file, used); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.logf("%s: the early checks' result: %v; the gate runs the checks", t.ID, err)
		}
		return Remote{}, false, nil
	}
	defer os.Remove(used)
	raw, err := fsx.ReadNoFollow(used, earlyMaxBytes)
	var res earlyResult
	if err == nil {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		err = dec.Decode(&res)
	}
	if err != nil {
		r.logf("%s: the early checks' result does not read (%v); the gate runs the checks", t.ID, err)
		return Remote{}, false, nil
	}
	if !res.Pass {
		r.logf("%s: early checks not used: they failed, and the gate runs them itself", t.ID)
		return Remote{}, false, nil
	}
	repo := r.Repo(p)
	head, err := repo.Head(ctx, t.Branch.Or(""))
	if err != nil {
		return Remote{}, false, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	if g, ok := t.Gate.Get(); ok && g.Head == head {
		r.logf("%s: early checks not used: a gate already decided %s, and this one runs the checks again", t.ID, head[:12])
		return Remote{}, false, nil
	}
	base, err := repo.Head(ctx, p.Base)
	if err != nil {
		return Remote{}, false, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	key, err := r.earlyKey(ctx, cfg, p, head, base)
	if err != nil {
		r.logf("%s: early checks not used: %v", t.ID, err)
		return Remote{}, false, nil
	}
	if res.Key != key {
		r.logf("%s: early checks not used: the %s changed since", t.ID, differs(res.Key, key))
		return Remote{}, false, nil
	}
	r.logf("%s: the gate uses the early checks of %s (%s)", t.ID, head[:12], res.At)
	note := fmt.Sprintf("(these checks ran early, at %s, on %s with this base and image)\n", res.At, head[:12])
	return Remote{Head: head, Base: base, ChecksPass: res.Pass, Output: note + res.Output}, true, nil
}

// dropEarly removes the ticket's kept result once a gate has decided:
// checks of its head that finished after a gate ran it are not that gate's.
func (r Runner) dropEarly(id ticket.ID) {
	if file := r.earlyFile(id); file != "" {
		os.Remove(file)
	}
}

// earlyKey is the key of checks of head on base as they would run now.
func (r Runner) earlyKey(ctx context.Context, cfg *config.Config, p config.Project, head, base string) (earlyKey, error) {
	script, err := gateScript(p)
	if err != nil {
		return earlyKey{}, err
	}
	if cfg.Image == "" {
		return earlyKey{}, errors.New("no image to run the checks in")
	}
	id, err := r.Podman.ImageID(ctx, cfg.Image)
	if err != nil {
		return earlyKey{}, err
	}
	pinned, err := pinnedPaths(p)
	if err != nil {
		return earlyKey{}, err
	}
	lines := []string{"tag " + cfg.Image, "memory " + cfg.GateMemory(p)}
	for _, e := range cfg.Gate.Env {
		lines = append(lines, "env "+e.Name+"="+e.Value)
	}
	for _, m := range cfg.Gate.Mounts {
		lines = append(lines, "mount "+strings.ReplaceAll(m, "{root}", r.Paths.Root))
	}
	files, err := pinnedState(p.Path, pinned)
	if err != nil {
		return earlyKey{}, err
	}
	sum := sha256.Sum256([]byte(strings.Join(append(lines, files...), "\n")))
	return earlyKey{Project: p.Key, Head: head, Base: base, Image: id, Script: script, Setup: hex.EncodeToString(sum[:])}, nil
}

// pinnedState is one line per entry at or under the pinned paths in the
// project (path, mode, size, modification time, a link's target), read
// without following any link out of the project; a missing one is a line
// too.
func pinnedState(project string, pinned []string) ([]string, error) {
	if len(pinned) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, fmt.Errorf("pinned_untracked: %w", err)
	}
	defer root.Close()
	var out []string
	line := func(name string, fi fs.FileInfo) {
		l := fmt.Sprintf("pinned %q %v %d %d", name, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if fi.Mode()&fs.ModeSymlink != 0 {
			to, _ := root.Readlink(name)
			l += " -> " + to
		}
		out = append(out, l)
	}
	for _, f := range pinned {
		name := filepath.ToSlash(f)
		fi, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			out = append(out, fmt.Sprintf("pinned %q absent", name))
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("pinned_untracked %s: %w", f, err)
		}
		if !fi.IsDir() {
			line(name, fi)
			continue
		}
		err = fs.WalkDir(root.FS(), name, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			line(path, info)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("pinned_untracked %s: %w", f, err)
		}
	}
	return out, nil
}

// differs names the first part of the key that is not the same.
func differs(a, b earlyKey) string {
	for _, d := range []struct {
		what string
		same bool
	}{
		{"project", a.Project == b.Project}, {"branch's head", a.Head == b.Head}, {"base's head", a.Base == b.Base},
		{"image", a.Image == b.Image}, {"commands", a.Script == b.Script},
	} {
		if !d.same {
			return d.what
		}
	}
	return "gate setup (tag, memory, env, mounts or pinned files)"
}

// pruneEarly removes kept results older than earlyKeepFor (a ticket
// dropped before its gate) by their modification time, which is the
// system's; nothing here fails the early checks.
func (r Runner) pruneEarly(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	old := clock.System{}.Now().Add(-earlyKeepFor)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || !e.Type().IsRegular() {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(old) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// commitID is a full lower-case hex commit id.
func commitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

// Early starts the early checks (Start).
type Early struct {
	Paths paths.Paths
	Exec  execx.Execer // systemd-run
	// Env is the tick's environment: systemd-run gets its PATH, HOME and
	// user bus, the unit its PATH and FACTORY_* settings but the GitHub
	// token.
	Env map[string]string
	Log Logger // nil logs nothing
}

// Start starts `factory early-checks <id> <head>` as the transient unit
// EarlyUnit and returns: the tick never waits for the checks. A unit that
// is already there (another early check runs) counts as started, and that
// ticket's checks run at the gate. Without the user manager's bus nothing
// starts. Once the unit is queued it leaves the earlyStarting marker, so
// EarlyRunning counts the checks before they hold the lock.
func (e Early) Start(ctx context.Context, id ticket.ID, head string) error {
	if got, ok := ticket.ParseID(string(id)); !ok || got != id {
		return fmt.Errorf("early checks: bad ticket %q", id)
	}
	if !commitID(head) {
		return fmt.Errorf("early checks %s: bad head %q", id, textx.Prefix(head, 80))
	}
	if !e.userBus() {
		return fmt.Errorf("early checks %s: no user bus to start %s", id, EarlyUnit)
	}
	argv := []string{"systemd-run", "--user", "--no-block", "--collect", "--unit=" + EarlyUnit,
		"--property=RuntimeMaxSec=" + strconv.Itoa(int(earlyMaxRuntime.Seconds()))}
	for _, kv := range e.unitEnv() {
		argv = append(argv, "--setenv="+kv)
	}
	argv = append(argv, e.Paths.FactoryBin(), "early-checks", string(id), head)
	var env []string
	for _, k := range []string{"PATH", "HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := e.Env[k]; ok {
			env = append(env, k+"="+v)
		}
	}
	res, err := e.Exec.Run(ctx, execx.Cmd{Argv: argv, Env: env, Timeout: systemdRunTimeout})
	if err != nil {
		return fmt.Errorf("early checks %s: %w", id, err)
	}
	if res.Code != 0 {
		if s := strings.ToLower(string(res.Stderr)); strings.Contains(s, "already exists") || strings.Contains(s, "already loaded") {
			e.logf("%s: another early check runs; this one's checks run at the gate", id)
			return nil
		}
		return fmt.Errorf("early checks %s: %w", id, execx.CheckErr(res))
	}
	e.logf("%s: early checks of %s started as %s", id, head[:12], EarlyUnit)
	if err := e.starting(id, head); err != nil {
		e.logf("%s: early checks: %v", id, err) // counted once they hold the lock
	}
	return nil
}

// starting leaves the earlyStarting marker for the checks just started.
func (e Early) starting(id ticket.ID, head string) error {
	dir := e.Paths.EarlyChecks()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(earlyRun{Ticket: id, Head: head})
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(filepath.Join(dir, earlyStarting), b, 0o600)
}

// userBus reports whether XDG_RUNTIME_DIR holds the user manager's bus.
func (e Early) userBus() bool {
	dir := e.Env["XDG_RUNTIME_DIR"]
	if dir == "" {
		return false
	}
	st, err := os.Lstat(filepath.Join(dir, "bus"))
	return err == nil && st.Mode()&fs.ModeSocket != 0
}

// unitEnv is the tick's PATH (podman and git are found as the tick finds
// them), FACTORY_ROOT from Paths and every other FACTORY_* setting of Env
// but the GitHub token, sorted: the user manager's environment is not the
// tick's.
func (e Early) unitEnv() []string {
	vars := map[string]string{}
	for k, v := range e.Env {
		if strings.HasPrefix(k, "FACTORY_") && v != "" && k != "FACTORY_GH_TOKEN" {
			vars[k] = v
		}
	}
	vars["FACTORY_ROOT"] = e.Paths.Root
	if v := e.Env["PATH"]; v != "" {
		vars["PATH"] = v
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Running is EarlyRunning on this host.
func (e Early) Running() (ticket.ID, string, bool) { return EarlyRunning(e.Paths) }

func (e Early) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	}
}
