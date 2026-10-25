package gate

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

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gocache"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// earlyRig is a rig with a branch b one commit ahead of main, its ticket
// and the two heads.
func earlyRig(t *testing.T) (r *rig, tk *ticket.Ticket, head, base string) {
	t.Helper()
	r = newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "branch\n"})
	tk = gateTicket(t, `{"work": {"status": "done", "summary": "s", "category": "docs"},
		"review": {"verdict": "pass", "runtime_behaviour": "no", "summary": "fine"}}`)
	return r, tk, r.git(r.repo.Dir, "rev-parse", "b"), r.git(r.repo.Dir, "rev-parse", "main")
}

func encoded(t *testing.T, tk *ticket.Ticket) string { return string(ticket.Encode(tk)) }

// The early checks are Gate 1's checks of the head the work stage left,
// on a fresh checkout of their own, and their result is kept for the
// ticket. The gate uses it once, as checks that ran elsewhere: nothing
// runs again, and the decision is still Gate 1's. Neither touches the
// ticket.
func TestEarlyChecksAreKeptAndUsedOnce(t *testing.T) {
	r, tk, head, base := earlyRig(t)
	cfg, p := r.config("", "[gate]\nallowed_paths = [\"*.md\"]\n")
	r.podman.out = "checks output"
	before := encoded(t, tk)
	wt := filepath.Join(r.paths.Gate, "early-T0001")
	r.podman.inspect = func(podman.Gate) {
		if got := r.git(wt, "rev-parse", "HEAD"); got != head {
			t.Errorf("the early checks ran on %s, not %s", got, head)
		}
	}
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.runs) != 1 || r.podman.runs[0].Name != "factory-gate-early-T0001" || r.podman.runs[0].Workdir != wt {
		t.Fatalf("runs %+v", r.podman.runs)
	}
	if exists(wt) {
		t.Error("the early checkout is still there")
	}
	file := filepath.Join(r.paths.EarlyChecks(), "T0001.json")
	fi, err := os.Lstat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("kept result: %v %v", fi, err)
	}

	kept, ok, err := r.runner.Kept(t.Context(), cfg, p, tk)
	if err != nil || !ok {
		t.Fatalf("Kept = %v %v", ok, err)
	}
	if kept.Head != head || kept.Base != base || !kept.ChecksPass || kept.Host != "" || !strings.HasSuffix(kept.Output, "checks output") {
		t.Errorf("kept %+v", kept)
	}
	if !strings.Contains(kept.Output, "ran early") {
		t.Errorf("the output does not say the checks ran early: %q", kept.Output)
	}
	if exists(file) {
		t.Error("a used result is still kept")
	}
	if _, ok, _ := r.runner.Kept(t.Context(), cfg, p, tk); ok {
		t.Error("a result was used twice")
	}
	g, err := r.runner.Decide(t.Context(), cfg, p, tk, kept)
	if err != nil || !g.ChecksPass || !g.AutoEligible || g.Head != head || g.Base != base {
		t.Fatalf("Decide = %+v %v", g, err)
	}
	if len(r.podman.runs) != 1 {
		t.Errorf("the gate ran the checks again: %d runs", len(r.podman.runs))
	}
	if got := encoded(t, tk); got != before {
		t.Errorf("the early checks changed the ticket:\n%s\n%s", before, got)
	}
}

// A kept result is used only when everything the checks' outcome depends
// on is as it was: the branch's head, the base's head, the image (its id,
// not its tag), the commands, the [gate] env and mounts, the gate memory,
// the pinned files and the project. Anything else, a file that does not
// read, or one that is a link, runs the checks as before; the result is
// dropped either way.
func TestKeptNeedsTheWholeKey(t *testing.T) {
	const pinned = "pinned_untracked = [\"vendor.txt\"]\n"
	for _, c := range []struct {
		name   string
		change func(r *rig) (*config.Config, config.Project)
		ticket func(tk *ticket.Ticket)
	}{
		{name: "nothing"},
		{name: "head", change: func(r *rig) (*config.Config, config.Project) {
			r.git(r.repo.Dir, "checkout", "-q", "b")
			r.repo.Write("lib/late.rb", "late\n")
			r.repo.Commit("late")
			r.git(r.repo.Dir, "checkout", "-q", "main")
			return r.config(pinned, "")
		}},
		{name: "base", change: func(r *rig) (*config.Config, config.Project) {
			r.repo.Write("lib/other.rb", "other\n")
			r.repo.Commit("other")
			return r.config(pinned, "")
		}},
		{name: "image", change: func(r *rig) (*config.Config, config.Project) {
			r.podman.imageID = func(string) (string, error) { return strings.Repeat("9", 64), nil }
			return r.config(pinned, "")
		}},
		{name: "image unknown", change: func(r *rig) (*config.Config, config.Project) {
			r.podman.imageID = func(string) (string, error) { return "", errors.New("no such image") }
			return r.config(pinned, "")
		}},
		{name: "image tag", change: func(r *rig) (*config.Config, config.Project) {
			cfg, p := r.config(pinned, "")
			cfg.Image = "localhost/gate:other"
			return cfg, p
		}},
		{name: "commands", change: func(r *rig) (*config.Config, config.Project) {
			cfg, p := r.config(pinned, "")
			p.Test = "rake test TESTOPTS=-v"
			return cfg, p
		}},
		{name: "env", change: func(r *rig) (*config.Config, config.Project) {
			return r.config(pinned, "[gate]\nenv = { GOFLAGS = \"-mod=mod\" }\n")
		}},
		{name: "mounts", change: func(r *rig) (*config.Config, config.Project) {
			return r.config(pinned, "[gate]\nmounts = [\"/srv/shared:/srv/shared:ro\"]\n")
		}},
		{name: "memory", change: func(r *rig) (*config.Config, config.Project) {
			return r.config(pinned+"gate_memory = \"1100m\"\n", "")
		}},
		{name: "pinned", change: func(r *rig) (*config.Config, config.Project) {
			r.repo.Write("vendor.txt", "vendored, changed\n")
			return r.config(pinned, "")
		}},
		{name: "project", ticket: func(tk *ticket.Ticket) { tk.Project = "other" }},
		{name: "not json", change: func(r *rig) (*config.Config, config.Project) {
			os.WriteFile(filepath.Join(r.paths.EarlyChecks(), "T0001.json"), []byte("{not json"), 0o600)
			return r.config(pinned, "")
		}},
		{name: "a link", change: func(r *rig) (*config.Config, config.Project) {
			file := filepath.Join(r.paths.EarlyChecks(), "T0001.json")
			moved := filepath.Join(r.t.TempDir(), "kept.json")
			if err := os.Rename(file, moved); err != nil {
				r.t.Fatal(err)
			}
			if err := os.Symlink(moved, file); err != nil {
				r.t.Fatal(err)
			}
			return r.config(pinned, "")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, tk, _, _ := earlyRig(t)
			r.repo.Write("vendor.txt", "vendored\n")
			cfg, p := r.config(pinned, "")
			if err := r.runner.RunEarly(t.Context(), cfg, p, tk, r.git(r.repo.Dir, "rev-parse", "b")); err != nil {
				t.Fatal(err)
			}
			if c.change != nil {
				cfg, p = c.change(r)
			}
			if c.ticket != nil {
				c.ticket(tk)
				p.Key = tk.Project
			}
			_, ok, err := r.runner.Kept(t.Context(), cfg, p, tk)
			if err != nil {
				t.Fatal(err)
			}
			if want := c.name == "nothing"; ok != want {
				t.Errorf("used = %v, want %v", ok, want)
			}
			if _, err := os.Lstat(filepath.Join(r.paths.EarlyChecks(), "T0001.json")); err == nil {
				t.Error("the result is still kept after the gate looked")
			}
		})
	}
}

// The gate removes only the result it read: early checks that keep a
// newer one while it looks (a later head's) keep it for the next gate.
func TestKeptRemovesOnlyWhatItRead(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(r.paths.EarlyChecks(), "T0001.json")
	newer := []byte(`{"newer": true}`)
	r.podman.imageID = func(string) (string, error) {
		if err := os.WriteFile(file, newer, 0o600); err != nil {
			t.Error(err)
		}
		return fakeImage, nil
	}
	if _, ok, err := r.runner.Kept(t.Context(), cfg, p, tk); !ok || err != nil {
		t.Fatalf("Kept = %v %v", ok, err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != string(newer) {
		t.Errorf("the newer result: %q %v", got, err)
	}
	if entries, _ := os.ReadDir(r.paths.EarlyChecks()); len(entries) != 2 {
		t.Errorf("left %v, want the lock and the newer result", entries)
	}
}

// A ticket with no early result, and a factory with no results at all,
// simply has none.
func TestKeptWithNoneKept(t *testing.T) {
	r, tk, _, _ := earlyRig(t)
	cfg, p := r.config("", "")
	if _, ok, err := r.runner.Kept(t.Context(), cfg, p, tk); ok || err != nil {
		t.Errorf("Kept = %v %v", ok, err)
	}
}

// Early checks that do not finish (podman fails, or they time out) keep
// nothing, and neither do checks that fail: they ran beside the agents, so
// a timing test or the memory limit may have failed them, and the gate
// runs the checks itself.
func TestEarlyChecksThatDoNotPassKeepNothing(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	r.podman.err = errors.New("timed out")
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err == nil {
		t.Fatal("no error")
	}
	if _, ok, _ := r.runner.Kept(t.Context(), cfg, p, tk); ok {
		t.Fatal("a check that did not finish was kept")
	}
	r.podman.err, r.podman.ok, r.podman.out = nil, false, "1 failure"
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(r.paths.EarlyChecks(), "T0001.json")) {
		t.Error("a failed early check was kept")
	}
	if !r.log.has("T0001: the early checks of " + head[:12] + " failed: nothing kept") {
		t.Errorf("log %q", r.log.lines)
	}
}

// A failed result that is kept anyway (by an older factory) is not used:
// the gate runs the checks itself.
func TestAKeptFailureIsNotUsed(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(r.paths.EarlyChecks(), "T0001.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	failed := strings.Replace(string(raw), `"pass":true`, `"pass":false`, 1)
	if failed == string(raw) {
		t.Fatalf("no pass in %s", raw)
	}
	if err := os.WriteFile(file, []byte(failed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.runner.Kept(t.Context(), cfg, p, tk); ok || err != nil {
		t.Errorf("Kept = %v %v", ok, err)
	}
	if exists(file) {
		t.Error("the failure is still kept")
	}
}

// An image that a rebuild moves while the early checks run is not the one
// they ran in for certain: nothing is kept.
func TestEarlyChecksKeepNothingWhenTheImageMoves(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	n := 0
	r.podman.imageID = func(string) (string, error) {
		n++
		if n > 1 {
			return strings.Repeat("9", 64), nil
		}
		return fakeImage, nil
	}
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(r.paths.EarlyChecks(), "T0001.json")) {
		t.Error("kept a result whose image moved during the checks")
	}
	r.podman.imageID = func(string) (string, error) { return "", errors.New("no such image") }
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err == nil || len(r.podman.runs) != 1 {
		t.Errorf("an image with no id: %v, %d runs", err, len(r.podman.runs))
	}
}

// At most one early check runs at a time on this host: one that finds
// another running does nothing and says so; that ticket's checks run at
// the gate.
func TestOneEarlyCheckAtATime(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	if err := os.MkdirAll(r.paths.EarlyChecks(), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := fsx.TryLock(filepath.Join(r.paths.EarlyChecks(), ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.runs) != 0 || exists(filepath.Join(r.paths.EarlyChecks(), "T0001.json")) {
		t.Fatalf("ran beside another: %d runs", len(r.podman.runs))
	}
	if !r.log.has("another early check runs") {
		t.Errorf("log %q", r.log.lines)
	}
}

// While early checks run they say so, for the tick: their ticket and
// head, while they hold the lock. A marker whose checks are gone (killed),
// or one past the unit's time, is not early checks that run.
func TestEarlyRunningWhileTheChecksRun(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	if _, _, ok := EarlyRunning(r.paths); ok {
		t.Fatal("running before any started")
	}
	seen := false
	r.podman.inspect = func(podman.Gate) {
		id, h, ok := EarlyRunning(r.paths)
		seen = ok && id == tk.ID && h == head
	}
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Error("the checks did not say they run")
	}
	marker := filepath.Join(r.paths.EarlyChecks(), ".running")
	if _, _, ok := EarlyRunning(r.paths); ok || exists(marker) {
		t.Errorf("still running after the checks: marker %v", exists(marker))
	}

	// a killed run's marker, with nothing holding the lock
	run := []byte(`{"ticket": "T0001", "head": "` + head + `"}`)
	if err := os.WriteFile(marker, run, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := EarlyRunning(r.paths); ok || exists(marker) {
		t.Errorf("a killed run's marker: kept %v", exists(marker))
	}
	// the lock held, but the marker older than the unit may run
	held, err := fsx.TryLock(filepath.Join(r.paths.EarlyChecks(), ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	if err := os.WriteFile(marker, run, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := EarlyRunning(r.paths); !ok {
		t.Error("a fresh marker under the lock is not running")
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(marker, old, old)
	if _, _, ok := EarlyRunning(r.paths); ok {
		t.Error("a marker past the unit's time is running")
	}
	for _, bad := range []string{`{"ticket": "../x", "head": "` + head + `"}`, `{"ticket": "T0001", "head": "main"}`, `not json`} {
		if err := os.WriteFile(marker, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := EarlyRunning(r.paths); ok {
			t.Errorf("%s: running", bad)
		}
	}
}

// A head the branch is no longer at, or one that is not a commit id, is
// not checked: the gate would never use the result.
func TestEarlyChecksOfAHeadTheBranchLeft(t *testing.T) {
	r, tk, head, base := earlyRig(t)
	cfg, p := r.config("", "")
	for _, h := range []string{base, "main", "-x", strings.ToUpper(head), ""} {
		if err := r.runner.RunEarly(t.Context(), cfg, p, tk, h); h == base && err != nil || h != base && err == nil {
			t.Errorf("%q: %v", h, err)
		}
	}
	if len(r.podman.runs) != 0 || exists(filepath.Join(r.paths.EarlyChecks(), "T0001.json")) {
		t.Errorf("checked a head the branch is not at: %d runs", len(r.podman.runs))
	}
}

// A go project's early checks are lent the trusted build cache exactly as
// Gate 1's are: warmed from the base head, mounted only as an overlay.
func TestEarlyChecksLendTheCacheOnlyAsAnOverlay(t *testing.T) {
	r, tk, head, base := earlyRig(t)
	cfg, p := r.config(goProject, "")
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.warms) != 1 || len(r.podman.runs) != 1 {
		t.Fatalf("%d warms, %d runs", len(r.podman.warms), len(r.podman.runs))
	}
	gen := filepath.Join(gocache.Root(r.paths.Root), "gem", base[:12])
	g := r.podman.runs[0]
	if got := cacheMounts(r.paths.Root, g); !slices.Equal(got, []string{gocache.Mount(gen)}) {
		t.Errorf("early cache mounts %q", got)
	}
	for _, m := range g.Mounts {
		if gocache.Reaches(r.paths.Root, m) && !strings.HasSuffix(m, ":O") {
			t.Errorf("a writable mount of the trusted cache: %q", m)
		}
	}
	if gocache.Reaches(r.paths.Root, g.Workdir) || strings.HasPrefix(r.paths.EarlyChecks(), g.Workdir) {
		t.Errorf("the checkout %s reaches the cache or the kept results", g.Workdir)
	}
}

// Results nobody used (a ticket dropped before its gate) are removed after
// a week, when the next early check keeps its own.
func TestOldEarlyResultsArePruned(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	dir := r.paths.EarlyChecks()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, fresh := filepath.Join(dir, "T0090.json"), filepath.Join(dir, "T0091.json")
	for _, f := range []string{old, fresh} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	week := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(old, week, week)
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if exists(old) || !exists(fresh) || !exists(filepath.Join(dir, "T0001.json")) {
		t.Errorf("old %v fresh %v new %v", exists(old), exists(fresh), exists(filepath.Join(dir, "T0001.json")))
	}
}

// userBus is a directory whose bus is a socket, as the user manager's
// XDG_RUNTIME_DIR is (short, for the socket's path limit).
func userBus(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "bus"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return dir
}

// Start runs the early checks as a transient unit of their own, so the
// tick never waits for them; systemd refusing a second unit of that name
// is the one-at-a-time rule, not an error. The unit gets the tick's PATH
// and FACTORY_* settings (not its GitHub token), and the checks count as
// running from then on, before they hold the lock. With no user bus
// nothing runs.
func TestEarlyStartIsAUnitOfItsOwn(t *testing.T) {
	head := strings.Repeat("ab", 20)
	root, bus := t.TempDir(), userBus(t)
	var calls []execx.Cmd
	answer := execx.Result{}
	e := Early{
		Paths: paths.FromEnv(map[string]string{"FACTORY_ROOT": root, "HOME": "/h"}),
		Exec: execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
			calls = append(calls, c)
			return answer, nil
		}),
		Env: map[string]string{"PATH": "/opt/podman/bin:/usr/bin", "HOME": "/h", "XDG_RUNTIME_DIR": bus,
			"FACTORY_GH_TOKEN": "secret", "FACTORY_ROOT": root, "OTHER": "x"},
		Log: &fakeLog{},
	}
	if err := e.Start(t.Context(), "T0001", head); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemd-run", "--user", "--no-block", "--collect", "--unit=" + EarlyUnit,
		"--property=RuntimeMaxSec=4200", "--setenv=FACTORY_ROOT=" + root, "--setenv=PATH=/opt/podman/bin:/usr/bin",
		root + "/bin/factory", "early-checks", "T0001", head}
	if !slices.Equal(calls[0].Argv, want) {
		t.Errorf("argv\n got %q\nwant %q", calls[0].Argv, want)
	}
	if !slices.Equal(calls[0].Env, []string{"PATH=/opt/podman/bin:/usr/bin", "HOME=/h", "XDG_RUNTIME_DIR=" + bus}) {
		t.Errorf("env %q", calls[0].Env)
	}
	if calls[0].Timeout == 0 || calls[0].Timeout > time.Minute {
		t.Errorf("timeout %v", calls[0].Timeout)
	}
	if id, h, ok := e.Running(); !ok || id != "T0001" || h != head {
		t.Errorf("just started: Running = %s %s %v", id, h, ok)
	}
	answer = execx.Result{Code: 1, Stderr: []byte("Unit factory-gate-early.service already exists.")}
	if err := e.Start(t.Context(), "T0002", head); err != nil {
		t.Errorf("a running early check: %v", err)
	}
	if l := e.Log.(*fakeLog); !l.has("T0002: another early check runs") {
		t.Errorf("log %q", l.lines)
	}
	if id, _, _ := e.Running(); id != "T0001" {
		t.Errorf("a unit that did not start counts as running: %s", id)
	}
	answer = execx.Result{Code: 1, Stderr: []byte("Failed to connect to bus")}
	if err := e.Start(t.Context(), "T0003", head); err == nil {
		t.Error("a failed systemd-run is not an error")
	}
	n := len(calls)
	for _, bad := range []struct {
		id   ticket.ID
		head string
	}{{"T0001", "main"}, {"T0001", "-" + head[1:]}, {"../x", head}, {"", head}} {
		if err := e.Start(t.Context(), bad.id, bad.head); err == nil {
			t.Errorf("%q %q: started", bad.id, bad.head)
		}
	}
	for _, dir := range []string{"", t.TempDir()} {
		e.Env["XDG_RUNTIME_DIR"] = dir
		if err := e.Start(t.Context(), "T0001", head); err == nil || !strings.Contains(err.Error(), "no user bus") {
			t.Errorf("XDG_RUNTIME_DIR %q: %v", dir, err)
		}
	}
	if len(calls) != n {
		t.Errorf("ran %d more", len(calls)-n)
	}
}

// Early checks the tick has just started count as running before they
// hold the lock, for a short while only, and not once they are done.
func TestEarlyChecksJustStartedAreRunning(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	if err := os.MkdirAll(r.paths.EarlyChecks(), 0o700); err != nil {
		t.Fatal(err)
	}
	starting := filepath.Join(r.paths.EarlyChecks(), ".starting")
	run := []byte(`{"ticket": "T0001", "head": "` + head + `"}`)
	if err := os.WriteFile(starting, run, 0o600); err != nil {
		t.Fatal(err)
	}
	if id, h, ok := EarlyRunning(r.paths); !ok || id != "T0001" || h != head {
		t.Errorf("just started: %s %s %v", id, h, ok)
	}
	old := time.Now().Add(-10 * time.Minute)
	os.Chtimes(starting, old, old)
	if _, _, ok := EarlyRunning(r.paths); ok {
		t.Error("started long ago and never held the lock: running")
	}
	if err := os.WriteFile(starting, run, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := EarlyRunning(r.paths); ok || exists(starting) {
		t.Errorf("still starting after the checks: %v", exists(starting))
	}
}

// A result that lands after a gate of its head decided (the gate ran the
// checks itself while the early checks were still going) is never used:
// the gate drops the ticket's result when it decides, and a result kept
// later for a head the ticket's gate already holds runs the checks again
// when that gate is run again.
func TestALateEarlyResultIsNotUsed(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	file := filepath.Join(r.paths.EarlyChecks(), "T0001.json")
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	r.podman.ok = false // a flaky test fails Gate 1
	g, err := r.runner.Run(t.Context(), cfg, p, tk)
	if err != nil || g.ChecksPass {
		t.Fatalf("Run = %+v %v", g, err)
	}
	if exists(file) {
		t.Error("the gate decided and kept the early result")
	}
	tk.Gate = ticket.Some(g)

	r.podman.ok = true
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if !exists(file) {
		t.Fatal("nothing kept")
	}
	if _, ok, err := r.runner.Kept(t.Context(), cfg, p, tk); ok || err != nil {
		t.Errorf("the gate run again used a result for a head a gate decided: %v %v", ok, err)
	}
	if !r.log.has("T0001: early checks not used: a gate already decided " + head[:12]) {
		t.Errorf("log %q", r.log.lines)
	}
	runs := len(r.podman.runs)
	if _, err := r.runner.Run(t.Context(), cfg, p, tk); err != nil || len(r.podman.runs) != runs+1 {
		t.Errorf("the gate run again ran no checks: %v", err)
	}

	// a gate job's decision drops it too
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil || !exists(file) {
		t.Fatalf("nothing kept: %v", err)
	}
	if _, err := r.runner.Decide(t.Context(), cfg, p, tk, Remote{Head: head, Base: g.Base, ChecksPass: true, Host: "desk"}); err != nil {
		t.Fatal(err)
	}
	if exists(file) {
		t.Error("a gate job decided and the early result is still kept")
	}
}

// Gate 1 and the early checks of one ticket running at once each have a
// checkout and a container of their own, and neither removes the other's.
func TestGateAndEarlyChecksOfOneTicketAtOnce(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("", "")
	early, gate := filepath.Join(r.paths.Gate, "early-T0001"), filepath.Join(r.paths.Gate, "T0001")
	nested := false
	r.podman.inspect = func(g podman.Gate) {
		if nested {
			if !exists(early) || !exists(gate) {
				t.Errorf("during Gate 1: early checkout %v, Gate 1's %v", exists(early), exists(gate))
			}
			return
		}
		nested = true
		if _, err := r.runner.Run(t.Context(), cfg, p, tk); err != nil {
			t.Error(err)
		}
		if !exists(early) || exists(gate) {
			t.Errorf("after Gate 1: early checkout %v, Gate 1's %v", exists(early), exists(gate))
		}
	}
	if err := r.runner.RunEarly(t.Context(), cfg, p, tk, head); err != nil {
		t.Fatal(err)
	}
	if len(r.podman.runs) != 2 {
		t.Fatalf("runs %+v", r.podman.runs)
	}
	a, b := r.podman.runs[0], r.podman.runs[1]
	if a.Name == b.Name || a.Workdir == b.Workdir || a.Workdir != early || b.Workdir != gate {
		t.Errorf("early %s in %s, Gate 1 %s in %s", a.Name, a.Workdir, b.Name, b.Workdir)
	}
}

// A heavy gate's checks are not started early (they would not fit beside
// the agents), but a pass run by hand with factory early-checks is used:
// only a pass is kept, and memory is all a heavy gate's waiting is for.
func TestAHeavyGatesEarlyPassRunByHandIsUsed(t *testing.T) {
	r, tk, head, _ := earlyRig(t)
	cfg, p := r.config("gate_memory = \"1800m\"\n", "")
	if !cfg.HeavyGate(p) {
		t.Fatal("not a heavy gate")
	}
	if err := r.runner.EarlyChecks(t.Context(), cfg, tk, head); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.runner.Kept(t.Context(), cfg, p, tk); !ok || err != nil {
		t.Errorf("Kept = %v %v", ok, err)
	}
}
