package cli

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
)

const deployConfig = `[deploy]
core = "example/agent-factory"
config = "example/config-repo"

[github]
installation_id = "1"
`

// deployFixture: a master with [deploy] and a user bus.
func deployFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.env.WriteConfig(t, deployConfig)
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
	f.env.Vars["XDG_RUNTIME_DIR"] = dir
	return f
}

func systemdRuns(ex *fakeExec) [][]string {
	var out [][]string
	for _, a := range ex.argv {
		if len(a) > 0 && a[0] == "systemd-run" {
			out = append(out, a)
		}
	}
	return out
}

func TestDeployStartWritesARequest(t *testing.T) {
	f := deployFixture(t)
	ex := &fakeExec{}
	code, out, errs := opsRun(t, f, opsCLI{}, ex, "deploy", "start", "--why", "a docs fix")
	if code != 0 || !strings.Contains(out, "factory deploy status") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	runs := systemdRuns(ex)
	if len(runs) != 1 || !slices.Contains(runs[0], "--unit=factory-deploy") || !slices.Equal(runs[0][len(runs[0])-2:], []string{"deploy", "run"}) {
		t.Errorf("systemd-run %q", runs)
	}
	p := paths.FromEnv(f.env.Vars)
	r, ok, err := deployrec.TakeRequest(p.DeployRequest())
	if err != nil || !ok || r.Rev != "main" || r.Why != "a docs fix" || r.Drill {
		t.Errorf("request %+v, %v, %v", r, ok, err)
	}
	// a commit and a drill, only here; the hold does not matter
	if err := deployrec.SetHold(p.DeployHold(), &deployrec.Hold{By: "maintainer", At: "now"}); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := opsRun(t, f, opsCLI{}, ex, "deploy", "start", "0123456789ab", "--drill"); code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	if r, ok, _ := deployrec.TakeRequest(p.DeployRequest()); !ok || r.Rev != "0123456789ab" || !r.Drill {
		t.Errorf("request %+v", r)
	}
	if code, _, errs := opsRun(t, f, opsCLI{}, ex, "deploy", "start", "0123456789ab", "--allow-older"); code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	if r, ok, _ := deployrec.TakeRequest(p.DeployRequest()); !ok || !r.AllowOlder || r.Drill {
		t.Errorf("request %+v", r)
	}
	for _, argv := range [][]string{{"deploy", "status", "--allow-older"}, {"deploy", "start", "--oops"}, {"deploy", "start", "-x"}, {"deploy", "start", "HEAD~1"}, {"deploy", "run", "main"}, {"deploy", "status", "--drill"}} {
		if code, _, _ := opsRun(t, f, opsCLI{}, ex, argv...); code == 0 {
			t.Errorf("%q ran", argv)
		}
	}
}

func TestDeployRefusedWithoutDeployConfig(t *testing.T) {
	f := deployFixture(t)
	f.env.WriteConfig(t, "")
	ex := &fakeExec{}
	code, _, errs := opsRun(t, f, opsCLI{}, ex, "deploy", "start")
	if code == 0 || !strings.Contains(errs, "no [deploy]") || len(systemdRuns(ex)) != 0 {
		t.Errorf("%d %q", code, errs)
	}
	if code, _, errs := opsRun(t, f, opsCLI{}, ex, "deploy", "run"); code == 0 || !strings.Contains(errs, "no [deploy]") {
		t.Errorf("run: %d %q", code, errs)
	}
	// an ask nobody can act on is not filed
	if code, out, errs := opsRun(t, f, opsCLI{}, ex, "deploy", "ask"); code == 0 || !strings.Contains(errs, "no [deploy]") || out != "" {
		t.Errorf("ask: %d %q %q", code, out, errs)
	}
}

func TestDeployStatusPrintsHoldAndLast(t *testing.T) {
	f := deployFixture(t)
	p := paths.FromEnv(f.env.Vars)
	code, out, _ := opsRun(t, f, opsCLI{}, nil, "deploy", "status")
	if code != 0 || !strings.Contains(out, "merges deploy by themselves") || !strings.Contains(out, "no deploy yet") {
		t.Fatalf("%d %q", code, out)
	}
	if err := deployrec.SetHold(p.DeployHold(), &deployrec.Hold{By: "maintainer", At: "2026-10-18T09:00:00+00:00", Note: "a demo tonight"}); err != nil {
		t.Fatal(err)
	}
	st := deployrec.State{Phase: deployrec.PhaseDone, Rev: "main", Commit: "0123456789ab", From: "ba9876543210", ConfigCommit: "aaaabbbbcccc",
		Started: "2026-10-18T08:00:00+00:00", Finished: "2026-10-18T08:10:00+00:00", Result: deployrec.ResultRolledBack,
		Tail: "deploy.sh: the check failed\n", Log: "/x/deploys/logs/a.log", Helpers: map[string]string{"helpera": "ok"}}
	if err := deployrec.WriteState(p.DeployState(), st); err != nil {
		t.Fatal(err)
	}
	if err := deployrec.WriteRequest(p.DeployRequest(), deployrec.Request{Rev: "main", By: "maintainer", At: "2026-10-18T09:30:00+00:00"}); err != nil {
		t.Fatal(err)
	}
	code, out, _ = opsRun(t, f, opsCLI{}, nil, "deploy", "status")
	for _, want := range []string{"held by maintainer since 2026-10-18T09:00:00+00:00: a demo tonight", "0123456789ab", "rolled_back",
		"from ba9876543210", "config aaaabbbbcccc", "helpera: ok", "the check failed", "waiting: main, by maintainer"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Errorf("exit %d", code)
	}
}

func TestDeployHoldThenAskFilesTheTicket(t *testing.T) {
	f := deployFixture(t)
	wakeLog(f)
	p := paths.FromEnv(f.env.Vars)
	code, out, errs := opsRun(t, f, opsCLI{}, nil, "deploy", "hold", "--note", "a demo tonight")
	if code != 0 || !strings.Contains(out, "deploys held") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if h, held, err := deployrec.ReadHold(p.DeployHold()); !held || err != nil || h.Note != "a demo tonight" {
		t.Errorf("hold %+v %v %v", h, held, err)
	}
	code, out, errs = opsRun(t, f, opsCLI{}, nil, "deploy", "ask", "--why", "PR #4 merged")
	if code != 0 || !strings.HasPrefix(out, "T") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	id := strings.TrimSpace(out)
	b, err := os.ReadFile(p.TicketFile(id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"project": "`+model.NoticeProject+`"`) || !strings.Contains(string(b), "PR #4 merged") {
		t.Errorf("ticket %s", b)
	}
	if code, out, _ := opsRun(t, f, opsCLI{}, nil, "deploy", "unhold"); code != 0 || !strings.Contains(out, "deploys released") {
		t.Errorf("%d %q", code, out)
	}
	if _, held, _ := deployrec.ReadHold(p.DeployHold()); held {
		t.Error("still held")
	}
}

func TestDeployRestoreWithNoSnapshot(t *testing.T) {
	f := deployFixture(t)
	if code, _, errs := opsRun(t, f, opsCLI{}, nil, "deploy", "restore"); code == 0 || !strings.Contains(errs, "no snapshot") {
		t.Errorf("%d %q", code, errs)
	}
}

func TestDeployRestoreRefusedWhileADeployRuns(t *testing.T) {
	f := deployFixture(t)
	p := paths.FromEnv(f.env.Vars)
	if err := os.MkdirAll(p.Deploys(), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := fsx.TryLock(p.DeployLock())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if code, _, errs := opsRun(t, f, opsCLI{}, nil, "deploy", "restore"); code == 0 || !strings.Contains(errs, "a deploy is running (factory deploy status)") {
		t.Errorf("%d %q", code, errs)
	}
}
