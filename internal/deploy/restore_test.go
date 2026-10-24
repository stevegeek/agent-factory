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
	"github.com/stevegeek/agent-factory/internal/execx"
)

func TestSnapshotReplacesAnOldSnapshot(t *testing.T) {
	f := newFixture(t)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	f.write("deploys/before/stale", "from an old snapshot\n")
	f.write("deploys/before.new/half", "a snapshot that died\n")
	f.install(f.b)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.read("deploys/before/bin/BUILD"), "commit "+f.b) {
		t.Error("the snapshot is not of what runs")
	}
	for _, rel := range []string{"deploys/before/stale", "deploys/before.new", "deploys/before.old"} {
		if _, err := os.Stat(filepath.Join(f.p.Root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s is left: %v", rel, err)
		}
	}
	// the binaries are links to the running inodes
	a, _ := os.Stat(filepath.Join(f.p.Bin, "factory"))
	b, _ := os.Stat(filepath.Join(f.p.DeployBefore(), "bin", "factory"))
	if !os.SameFile(a, b) {
		t.Error("bin/factory is not hard-linked")
	}
	for _, rel := range []string{"bin/foreman", "bin/factory-remote", "bin/pi-run", "config.toml", "foreman.env"} {
		if _, err := os.Stat(filepath.Join(f.p.DeployBefore(), rel)); err != nil {
			t.Errorf("snapshot lacks %s", rel)
		}
	}
}

func TestRestoreRefusesASnapshotThatDoesNotMatchItsBUILD(t *testing.T) {
	f := newFixture(t)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	f.install(f.b)
	f.write("config.toml", "# new\n")
	// a damaged snapshot binary (a copy, so the running one stays)
	bad := filepath.Join(f.p.DeployBefore(), "bin", "foreman")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("damaged\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	restored, err := f.s.Restore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not match its BUILD") || len(restored) != 0 {
		t.Fatalf("Restore = %q, %v", restored, err)
	}
	if !strings.Contains(f.read("bin/BUILD"), "commit "+f.b) || f.read("config.toml") != "# new\n" {
		t.Error("something changed")
	}
}

func TestRestorePutsBackOnlyWhatDiffers(t *testing.T) {
	f := newFixture(t)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	f.install(f.b)
	f.write("foreman.env", "FOREMAN_ARGS=\"--new\"\n")
	pi, _ := os.Stat(filepath.Join(f.p.Bin, "pi-run"))
	restored, err := f.s.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bin/factory", "bin/foreman", "bin/factory-remote", "bin/BUILD", "foreman.env"}
	if !slices.Equal(restored, want) {
		t.Errorf("restored %q, want %q", restored, want)
	}
	if now, _ := os.Stat(filepath.Join(f.p.Bin, "pi-run")); !os.SameFile(pi, now) {
		t.Error("an unchanged pi-run was rewritten")
	}
	if st, _ := os.Stat(filepath.Join(f.p.Bin, "factory")); st.Mode().Perm()&0o100 == 0 {
		t.Errorf("bin/factory mode %v", st.Mode())
	}
	if !strings.Contains(f.read("bin/factory"), f.a) {
		t.Error("bin/factory is not a's")
	}
}

func TestRestoreRestartsAUserUnitForeman(t *testing.T) {
	f := newFixture(t)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	f.install(f.b)
	if _, err := f.s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	ran := f.exec.ran("systemctl")
	if len(ran) != 2 || !slices.Equal(ran[1], []string{"systemctl", "--user", "restart", "foreman"}) {
		t.Errorf("systemctl ran %q", ran)
	}
	// not enabled: no restart
	f.exec = &fakeExec{results: map[string]execx.Result{"systemctl is-enabled": {Code: 1}}}
	f.s.Exec = f.exec
	f.install(f.b)
	if _, err := f.s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ran := f.exec.ran("systemctl"); len(ran) != 1 {
		t.Errorf("systemctl ran %q", ran)
	}
	// a system unit: its path unit restarts it
	f.exec = &fakeExec{}
	f.s.Exec = f.exec
	f.write("system-unit", "")
	f.s.SystemUnit = filepath.Join(f.p.Root, "system-unit")
	f.install(f.b)
	if _, err := f.s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ran := f.exec.ran("systemctl"); len(ran) != 0 {
		t.Errorf("systemctl ran %q", ran)
	}
}

func TestRestoreGoesOnPastAStuckTick(t *testing.T) {
	t.Parallel() // it waits out the tick lock
	f := newFixture(t)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	f.install(f.b)
	l, err := lockFile(f.p.TickLock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	restored, err := f.s.Restore(context.Background())
	if err != nil || len(restored) == 0 {
		t.Fatalf("Restore = %q, %v", restored, err)
	}
	if !slices.ContainsFunc(f.s.Log.(*memLog).lines, func(l string) bool { return strings.Contains(l, "restoring anyway") }) {
		t.Errorf("log %q", f.s.Log.(*memLog).lines)
	}
}

// TestRestoreByHandRefusesWhileADeployRuns: a restore by hand under a
// runner's lock would mix the trees; it waits for the runner.
func TestRestoreByHandRefusesWhileADeployRuns(t *testing.T) {
	f := newFixture(t)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	f.install(f.b)
	f.deployed(deployrec.ResultOK)
	l, err := lockFile(f.p.DeployLock())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RestoreByHand(context.Background(), false); err == nil || !strings.Contains(err.Error(), "a deploy is running") {
		t.Fatalf("RestoreByHand = %v", err)
	}
	if got := buildCommit(f.p); got != f.b {
		t.Fatalf("restored under the lock: %s", got)
	}
	l.Unlock()
	if restored, err := f.s.RestoreByHand(context.Background(), false); err != nil || len(restored) == 0 {
		t.Fatalf("RestoreByHand = %q, %v", restored, err)
	}
	if got := buildCommit(f.p); got != f.a {
		t.Errorf("BUILD names %s", got)
	}
}

// TestRestoreByHandRefusesWhileADesktopDeployRuns: a young
// ~/factory/.deploy is the desktop's deploy.sh at work; a restore then
// would mix its install with the snapshot.
func TestRestoreByHandRefusesWhileADesktopDeployRuns(t *testing.T) {
	f := newFixture(t)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	f.install(f.b)
	f.deployed(deployrec.ResultOK)
	dot := filepath.Join(f.p.Root, ".deploy")
	if err := os.Mkdir(dot, 0o700); err != nil {
		t.Fatal(err)
	}
	now := f.s.now()
	if err := os.Chtimes(dot, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RestoreByHand(context.Background(), true); err == nil || !strings.Contains(err.Error(), "a desktop deploy is running, or one died: remove ~/factory/.deploy") {
		t.Fatalf("RestoreByHand = %v", err)
	}
	if got := buildCommit(f.p); got != f.b {
		t.Fatalf("restored during a desktop deploy: %s", got)
	}
	old := now.Add(-3 * time.Hour)
	if err := os.Chtimes(dot, old, old); err != nil {
		t.Fatal(err)
	}
	if restored, err := f.s.RestoreByHand(context.Background(), false); err != nil || len(restored) == 0 {
		t.Fatalf("an old .deploy: RestoreByHand = %q, %v", restored, err)
	}
}

func TestSnapshotRecordsItsDeploy(t *testing.T) {
	f := newFixture(t)
	if err := Snapshot(f.p, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	if got, want := f.read("deploys/before/SNAPSHOT"), "for "+f.b+" from "+f.a+"\n"; got != want {
		t.Errorf("SNAPSHOT %q, want %q", got, want)
	}
}

// deployed records the last deploy as of b from a, past its snapshot,
// ended with result.
func (f *fixture) deployed(result string) {
	f.t.Helper()
	st := deployrec.State{Phase: deployrec.PhaseDone, Rev: "main", Commit: f.b, From: f.a, Result: result, Snapshotted: true}
	if err := deployrec.WriteState(f.p.DeployState(), st); err != nil {
		f.t.Fatal(err)
	}
}

// TestRestoreByHandOnlyPutsBackItsOwnDeploysSnapshot: deploys/before is
// the snapshot of one deploy; a restore by hand after anything else
// (a later deploy cut off before its snapshot, a desktop deploy) would
// put back an older build, so it is refused unless forced.
func TestRestoreByHandOnlyPutsBackItsOwnDeploysSnapshot(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		setup func(f *fixture)
		why   string // "" restores
	}{
		{"after a good deploy", func(f *fixture) { f.deployed(deployrec.ResultOK) }, ""},
		{"interrupted after its snapshot", func(f *fixture) {
			f.install(f.a) // deploy.sh had not installed yet
			f.write("config.toml", "# new\n")
			f.deployed(deployrec.ResultInterrupted)
		}, ""},
		{"rollback failed", func(f *fixture) { f.deployed(deployrec.ResultRollbackFailed) }, ""},
		{"a later deploy cut off before its snapshot", func(f *fixture) {
			st := deployrec.State{Phase: deployrec.PhaseDone, Step: "fetching", Rev: "main", From: f.b, Result: deployrec.ResultInterrupted}
			if err := deployrec.WriteState(f.p.DeployState(), st); err != nil {
				t.Fatal(err)
			}
		}, "the snapshot is of the deploy of <b>"},
		{"the same commit again, stopped before its snapshot", func(f *fixture) {
			st := deployrec.State{Phase: deployrec.PhaseDone, Rev: "main", Commit: f.b, From: f.b, Result: deployrec.ResultNothingInstalled}
			if err := deployrec.WriteState(f.p.DeployState(), st); err != nil {
				t.Fatal(err)
			}
		}, "stopped before its snapshot"},
		{"a desktop deploy since", func(f *fixture) {
			f.deployed(deployrec.ResultOK)
			f.install("fedcba987654")
		}, "bin/BUILD names fedcba987654"},
		{"rolled back already", func(f *fixture) {
			f.deployed(deployrec.ResultRolledBack)
			f.install(f.a)
		}, "bin/BUILD names <a>"},
		{"no record in the snapshot", func(f *fixture) {
			f.deployed(deployrec.ResultOK)
			if err := os.Remove(filepath.Join(f.p.DeployBefore(), "SNAPSHOT")); err != nil {
				t.Fatal(err)
			}
		}, "records no deploy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			if err := Snapshot(f.p, f.b, f.a); err != nil {
				t.Fatal(err)
			}
			f.install(f.b)
			c.setup(f)
			why := strings.NewReplacer("<a>", f.a, "<b>", f.b).Replace(c.why)
			was := f.read("bin/BUILD")
			restored, err := f.s.RestoreByHand(ctx, false)
			if why == "" {
				if err != nil || len(restored) == 0 || buildCommit(f.p) != f.a {
					t.Fatalf("RestoreByHand = %q, %v; BUILD names %s", restored, err, buildCommit(f.p))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), why) || !strings.Contains(err.Error(), "--force") || len(restored) != 0 {
				t.Fatalf("RestoreByHand = %q, %v; want %q and --force", restored, err, why)
			}
			if f.read("bin/BUILD") != was {
				t.Fatal("a refused restore changed bin/BUILD")
			}
			if _, err := f.s.RestoreByHand(ctx, true); err != nil {
				t.Fatalf("--force: %v", err)
			}
			if buildCommit(f.p) != f.a {
				t.Errorf("--force: BUILD names %s", buildCommit(f.p))
			}
		})
	}
}
