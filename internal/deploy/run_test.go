package deploy

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/deployrec"
)

func TestRunNotOnMainInstallsNothing(t *testing.T) {
	f := newFixture(t)
	before := f.read("bin/factory")
	st := f.runRequest(deployrec.Request{Rev: "0123456789ab", By: "maintainer"})
	if st.Phase != deployrec.PhaseDone || st.Result != deployrec.ResultNothingInstalled || !strings.Contains(st.Tail, "0123456789ab is not on core main") {
		t.Errorf("state %+v", st)
	}
	if f.read("bin/factory") != before {
		t.Error("bin/factory changed")
	}
	if !slices.Equal(f.notices.titles, []string{"Deploy of 0123456789ab failed: nothing installed"}) {
		t.Errorf("notices %q", f.notices.titles)
	}
}

func TestRunHappyPathUpdatesHelpers(t *testing.T) {
	f := newFixture(t)
	f.ssh.fail["helper2"] = "ssh: connect to host helper2: No route to host"
	st := f.runRequest(deployrec.Request{Rev: "main", By: "maintainer"})
	if st.Result != deployrec.ResultOK || st.Commit != f.b || st.From != f.a || st.Phase != deployrec.PhaseDone || !st.Snapshotted {
		t.Fatalf("state %+v\n%s", st, logOf(t, st))
	}
	if got, want := f.read("deploys/before/SNAPSHOT"), "for "+f.b+" from "+f.a+"\n"; got != want {
		t.Errorf("SNAPSHOT %q, want %q", got, want)
	}
	if want := f.conf.Git("rev-parse", "--short=12", "HEAD"); st.ConfigCommit != want {
		t.Errorf("config commit %q, want %q", st.ConfigCommit, want)
	}
	if got, want := strings.TrimSpace(f.read("deploys/config-commit")), f.conf.Git("rev-parse", "HEAD"); got != want {
		t.Errorf("deploys/config-commit %q, want %q", got, want)
	}
	if st.Helpers["helper1"] != "ok" || !strings.Contains(st.Helpers["helper2"], "No route to host") || len(st.Helpers) != 2 {
		t.Errorf("helpers %v", st.Helpers)
	}
	if want := []string{"helper1 install " + f.b, "helper2 install " + f.b}; !slices.Equal(f.ssh.calls, want) {
		t.Errorf("ssh calls %q, want %q", f.ssh.calls, want)
	}
	if !strings.Contains(f.read("bin/BUILD"), "commit "+f.b+" ") {
		t.Errorf("BUILD %q", f.read("bin/BUILD"))
	}
	// staged in its own directory; the deploy.sh of the new commit, as self
	log := logOf(t, st)
	if !strings.Contains(log, "fake deploy.sh: "+f.b) || !strings.Contains(log, "--host self --build helper") {
		t.Errorf("log %q", log)
	}
	if _, err := os.Stat(filepath.Join(f.p.Root, ".deploy")); !os.IsNotExist(err) {
		t.Error("made ~/factory/.deploy")
	}
	if len(f.notices.titles) != 0 {
		t.Errorf("notices %q", f.notices.titles)
	}
	if _, err := os.Stat(f.p.DeployRequest()); !os.IsNotExist(err) {
		t.Error("the request is still there")
	}
}

func logOf(t *testing.T, st deployrec.State) string {
	t.Helper()
	if st.Log == "" {
		return ""
	}
	b, err := os.ReadFile(st.Log)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunFailedCheckRestoresTheSnapshot(t *testing.T) {
	t.Parallel() // it waits out a check
	f := newFixture(t)
	f.conf.Write("config/config.toml", f.configToml("# broken\n"))
	f.conf.Commit("a bad config")
	oldConfig, oldEnv := f.read("config.toml"), f.read("foreman.env")
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultRolledBack {
		t.Fatalf("state %+v\n%s", st, logOf(t, st))
	}
	if !strings.Contains(f.read("bin/BUILD"), "commit "+f.a+" ") || !strings.Contains(f.read("bin/factory"), f.a) {
		t.Errorf("the old build is not back: %q", f.read("bin/BUILD"))
	}
	if f.read("config.toml") != oldConfig || f.read("foreman.env") != oldEnv {
		t.Error("the config or foreman.env is not back")
	}
	if want := []string{"Deploy of " + f.b + " failed its check: " + f.a + " is back"}; !slices.Equal(f.notices.titles, want) {
		t.Errorf("notices %q, want %q", f.notices.titles, want)
	}
	if len(f.ssh.calls) != 0 {
		t.Errorf("helpers were sent %q", f.ssh.calls)
	}
	if _, err := os.Stat(f.p.DeployConfigCommit()); !os.IsNotExist(err) {
		t.Error("a failed deploy wrote config-commit")
	}
}

func TestRunConfigOnlyRollbackKeepsTheBinaries(t *testing.T) {
	t.Parallel() // it waits out a check
	f := newFixture(t)
	// the master runs b already, with .prev an older build: only config changes
	f.install(f.b)
	f.write("bin/factory.prev", "an older build\n")
	binaries := map[string]os.FileInfo{}
	for _, b := range []string{"factory", "foreman", "factory-remote"} {
		st, err := os.Stat(filepath.Join(f.p.Bin, b))
		if err != nil {
			t.Fatal(err)
		}
		binaries[b] = st
	}
	f.conf.Write("config/config.toml", f.configToml("# broken\n"))
	f.conf.Commit("a bad config")
	oldConfig := f.read("config.toml")
	f.setEnv("FAKE_DEPLOY", "config-only")
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultRolledBack {
		t.Fatalf("state %+v\n%s", st, logOf(t, st))
	}
	for b, was := range binaries {
		now, err := os.Stat(filepath.Join(f.p.Bin, b))
		if err != nil || !os.SameFile(was, now) {
			t.Errorf("bin/%s was replaced", b)
		}
	}
	if f.read("config.toml") != oldConfig || !strings.Contains(f.read("foreman.env"), "--old") {
		t.Error("config.toml and foreman.env are not back")
	}
	if f.read("bin/factory.prev") != "an older build\n" {
		t.Error("bin/factory.prev was touched")
	}
}

func TestRunNothingInstalledWhenDeployShFailsEarly(t *testing.T) {
	f := newFixture(t)
	f.setEnv("FAKE_DEPLOY", "early")
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultNothingInstalled || !strings.Contains(st.Tail, "the builder is down") {
		t.Fatalf("state %+v", st)
	}
	if want := []string{"Deploy of " + f.b + " failed: nothing installed"}; !slices.Equal(f.notices.titles, want) {
		t.Errorf("notices %q", f.notices.titles)
	}
	if !strings.Contains(f.notices.bodies[0], "the builder is down") {
		t.Errorf("notice body %q lacks the log's tail", f.notices.bodies[0])
	}
	// the next run is not blocked
	f.setEnv("FAKE_DEPLOY", "ok")
	if st := f.runRequest(deployrec.Request{Rev: "main"}); st.Result != deployrec.ResultOK {
		t.Errorf("the next run: %+v", st)
	}
}

func TestRunFailedLateRestores(t *testing.T) {
	f := newFixture(t)
	f.setEnv("FAKE_DEPLOY", "late")
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultRolledBack || !strings.Contains(f.read("bin/BUILD"), "commit "+f.a+" ") {
		t.Fatalf("state %+v, BUILD %q", st, f.read("bin/BUILD"))
	}
}

func TestRunRefusesAFreshDesktopDotDeploy(t *testing.T) {
	f := newFixture(t)
	dot := filepath.Join(f.p.Root, ".deploy")
	if err := os.Mkdir(dot, 0o700); err != nil {
		t.Fatal(err)
	}
	// the fixture's clock is fixed: a .deploy made now is young
	now := f.s.now()
	if err := os.Chtimes(dot, now, now); err != nil {
		t.Fatal(err)
	}
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultNothingInstalled || !strings.Contains(st.Tail, "a desktop deploy is running, or one died: remove ~/factory/.deploy") {
		t.Fatalf("state %+v", st)
	}
	old := now.Add(-3 * time.Hour)
	if err := os.Chtimes(dot, old, old); err != nil {
		t.Fatal(err)
	}
	if st := f.runRequest(deployrec.Request{Rev: "main"}); st.Result != deployrec.ResultOK {
		t.Errorf("an old .deploy: %+v", st)
	}
}

func TestRunDrillRollsBack(t *testing.T) {
	f := newFixture(t)
	st := f.runRequest(deployrec.Request{Rev: "main", Drill: true})
	if st.Result != deployrec.ResultRolledBack || !strings.Contains(f.read("bin/BUILD"), "commit "+f.a+" ") {
		t.Fatalf("state %+v", st)
	}
	if len(f.notices.titles) != 1 || len(f.ssh.calls) != 0 {
		t.Errorf("notices %q, ssh %q", f.notices.titles, f.ssh.calls)
	}
}

func TestRunDrillRefusedOnTheRunningCommit(t *testing.T) {
	f := newFixture(t)
	st := f.runRequest(deployrec.Request{Rev: f.a, Drill: true})
	if st.Result != deployrec.ResultNothingInstalled || !strings.Contains(st.Tail, "a drill needs a commit other than the running one") {
		t.Fatalf("state %+v", st)
	}
	if st.Log != "" || st.Snapshotted {
		t.Error("deploy.sh ran, or the snapshot was taken")
	}
}

func TestRunTakesAnOlderCommitOnMain(t *testing.T) {
	f := newFixture(t)
	f.install("000000000000")
	st := f.runRequest(deployrec.Request{Rev: f.a})
	if st.Result != deployrec.ResultOK || st.Commit != f.a {
		t.Fatalf("state %+v", st)
	}
}

// deploy.sh refuses a core older than the running one unless the request
// allows it; a drill always restores, so it allows it too.
func TestRunPassesAllowOlder(t *testing.T) {
	for _, tc := range []struct {
		req  deployrec.Request
		want bool
	}{
		{deployrec.Request{Rev: "main"}, false},
		{deployrec.Request{Rev: "main", AllowOlder: true}, true},
		{deployrec.Request{Rev: "main", Drill: true}, true},
	} {
		f := newFixture(t)
		st := f.runRequest(tc.req)
		if got := strings.Contains(logOf(t, st), "--allow-older"); got != tc.want {
			t.Errorf("%+v: --allow-older passed %v, want %v\n%s", tc.req, got, tc.want, logOf(t, st))
		}
	}
}

func TestRunReturnsWhenAnotherRunnerHoldsTheLock(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.p.Deploys(), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := lockFile(f.p.DeployLock())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if err := deployrec.WriteRequest(f.p.DeployRequest(), deployrec.Request{Rev: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Run(context.Background(), f.cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.p.DeployRequest()); err != nil {
		t.Error("the request was taken under another runner's lock")
	}
}

// TestRunWaitsOutAMomentaryLock: the tick's look for an interrupted
// deploy holds the lock a moment; a runner starting then still runs.
func TestRunWaitsOutAMomentaryLock(t *testing.T) {
	f := newFixture(t)
	f.s.RunLockWait = 5 * time.Second
	if err := os.MkdirAll(f.p.Deploys(), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := lockFile(f.p.DeployLock())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		l.Unlock()
	}()
	if st := f.runRequest(deployrec.Request{Rev: "main"}); st.Result != deployrec.ResultOK {
		t.Fatalf("state %+v", st)
	}
}

// TestRunTakesARequestWrittenAsItFinished: a start that came after the
// runner's last take found the unit still there; the runner runs it once
// it has let the lock go.
func TestRunTakesARequestWrittenAsItFinished(t *testing.T) {
	f := newFixture(t)
	late := true
	f.s.afterLock = func() {
		if late {
			late = false
			if err := deployrec.WriteRequest(f.p.DeployRequest(), deployrec.Request{Rev: f.a, By: "late"}); err != nil {
				t.Error(err)
			}
		}
	}
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Commit != f.a || st.By != "late" || st.Result != deployrec.ResultOK {
		t.Fatalf("state %+v", st)
	}
	if _, err := os.Stat(f.p.DeployRequest()); err == nil {
		t.Error("the late request still waits")
	}
}

// TestRunStopsOnATakeThatKeepsFailing: a taken request that cannot be
// removed fails every take; the run ends instead of spinning.
func TestRunStopsOnATakeThatKeepsFailing(t *testing.T) {
	f := newFixture(t)
	f.write("deploys/request.json.taken/stuck", "x")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := f.s.Run(ctx, f.cfg)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("Run = %v (ctx %v)", err, ctx.Err())
	}
	if n := len(f.s.Log.(*memLog).lines); n > 3 {
		t.Errorf("logged %d lines: %q", n, f.s.Log.(*memLog).lines)
	}
}

func TestRootStepsNotice(t *testing.T) {
	f := newFixture(t)
	f.core.Write("host/foreman-system.sh", "root, changed\n")
	f.core.Write("systemd/factory-tick.timer", "[Timer]\n")
	f.core.Write("systemd/foreman.service", "user unit\n")
	f.core.Commit("C")
	c := f.core.Git("rev-parse", "--short=12", "HEAD")
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultOK {
		t.Fatalf("state %+v", st)
	}
	if want := []string{"Deployed " + c + ": root steps wait for you"}; !slices.Equal(f.notices.titles, want) {
		t.Fatalf("notices %q", f.notices.titles)
	}
	body := f.notices.bodies[0]
	if !strings.Contains(body, "host/foreman-system.sh") || !strings.Contains(body, "systemd/factory-tick.timer") || strings.Contains(body, "foreman.service") {
		t.Errorf("body %q", body)
	}
}

func TestRootStepsNoticeWithUnknownBase(t *testing.T) {
	f := newFixture(t)
	f.install("fedcba987654") // a deploy by hand put a commit off main
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultOK {
		t.Fatalf("state %+v", st)
	}
	if want := []string{"Deployed " + f.b + ": root steps wait for you"}; !slices.Equal(f.notices.titles, want) {
		t.Fatalf("notices %q", f.notices.titles)
	}
	if !strings.Contains(f.notices.bodies[0], "unknown base: check host/ and systemd/") {
		t.Errorf("body %q", f.notices.bodies[0])
	}
}

func TestRunKeepsTenLogs(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.p.DeployLogs(), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		name := filepath.Join(f.p.DeployLogs(), "20250101T0000"+string(rune('a'+i))+"Z-old.log")
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st := f.runRequest(deployrec.Request{Rev: "main"})
	entries, err := os.ReadDir(f.p.DeployLogs())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != KeepLogs {
		t.Errorf("%d logs, want %d", len(entries), KeepLogs)
	}
	if _, err := os.Stat(st.Log); err != nil {
		t.Errorf("the newest log went: %v", err)
	}
}

// TestRunRefusesAConfigThatMovesDeploy: config main decides what the
// master fetches and runs next; a config commit that points [deploy] at
// other repos (or another directory) needs a desktop deploy.
func TestRunRefusesAConfigThatMovesDeploy(t *testing.T) {
	for _, c := range []struct{ name, from, to string }{
		{"core", `core = "example/agent-factory"`, `core = "someone/else"`},
		{"config", `config = "example/config-repo"`, `config = "someone/config"`},
		{"config_dir", `config = "example/config-repo"`, "config = \"example/config-repo\"\nconfig_dir = \"elsewhere\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.conf.Write("config/config.toml", strings.Replace(f.configToml(""), c.from, c.to, 1))
			f.conf.Commit("move [deploy]")
			before := f.read("bin/BUILD")
			st := f.runRequest(deployrec.Request{Rev: "main"})
			if st.Result != deployrec.ResultNothingInstalled || !strings.Contains(st.Tail, "[deploy] "+c.name) || !strings.Contains(st.Tail, "a desktop deploy") {
				t.Fatalf("state %+v", st)
			}
			if st.Log != "" || st.Snapshotted || f.read("bin/BUILD") != before {
				t.Error("deploy.sh ran")
			}
			if want := []string{"Deploy of " + f.b + " failed: nothing installed"}; !slices.Equal(f.notices.titles, want) {
				t.Errorf("notices %q", f.notices.titles)
			}
		})
	}
}

// TestRunSendsHelpersTheNewConfigsHosts: the helpers updated are those of
// the config just installed, not the one the run started with.
func TestRunSendsHelpersTheNewConfigsHosts(t *testing.T) {
	f := newFixture(t)
	f.conf.Write("config/config.toml", f.configToml("\n[hosts.helper3]\nssh = \"factory@helper3\"\n"))
	f.conf.Commit("a third helper")
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultOK {
		t.Fatalf("state %+v", st)
	}
	if want := []string{"helper1 install " + f.b, "helper2 install " + f.b, "helper3 install " + f.b}; !slices.Equal(f.ssh.calls, want) {
		t.Errorf("ssh calls %q, want %q", f.ssh.calls, want)
	}
}

// TestRunReadsTheInstalledConfigPerRequest: each request runs with the
// config.toml installed then, not the one the runner started with (a
// deploy before it may have installed another).
func TestRunReadsTheInstalledConfigPerRequest(t *testing.T) {
	f := newFixture(t)
	f.cfg = parseConfig(t, strings.Replace(f.configToml(""), `core = "example/agent-factory"`, `core = "example/stale"`, 1))
	if st := f.runRequest(deployrec.Request{Rev: "main"}); st.Result != deployrec.ResultOK {
		t.Fatalf("state %+v", st)
	}
}

// TestRunSameCommitWaitsForANewForeman: a deploy of the running commit
// (config only) cannot tell the new Foreman by its version; the check
// waits for one started after deploy.sh began, so the old Foreman,
// still up, does not pass it.
func TestRunSameCommitWaitsForANewForeman(t *testing.T) {
	t.Parallel() // it waits out a check
	f := newFixture(t)
	f.install(f.b)
	f.conf.Write("config/config.toml", f.configToml("# a new setting\n"))
	f.conf.Commit("config only")
	f.setEnv("FAKE_DEPLOY", "config-only")
	f.stays.Store(true)
	st := f.runRequest(deployrec.Request{Rev: "main"})
	if st.Result != deployrec.ResultRolledBack || len(f.notices.bodies) != 1 || !strings.Contains(f.notices.bodies[0], "started after") {
		t.Fatalf("state %+v, notices %q", st, f.notices.bodies)
	}
	// a Foreman that restarts passes
	f.stays.Store(false)
	f.notices.titles = nil
	if st := f.runRequest(deployrec.Request{Rev: "main"}); st.Result != deployrec.ResultOK {
		t.Fatalf("a restarted Foreman: state %+v", st)
	}
}
