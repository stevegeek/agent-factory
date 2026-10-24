package hostrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/usage"
)

func listAnswer(byDir map[string]execx.Result) func(c execx.Cmd) (execx.Result, error) {
	return func(c execx.Cmd) (execx.Result, error) {
		if r, ok := byDir[c.Dir]; ok {
			return r, nil
		}
		return execx.Result{Stdout: []byte("[]")}, nil
	}
}

// port: TF MemoryTest.test_a_failed_scion_list_is_not_an_empty_one
func TestAFailedScionListIsNotAnEmptyOne(t *testing.T) {
	f := newFixture(t)
	cfg := cfgOf(t, "[projects.p]\npath = \"/x/p\"\n")
	f.exec.answer = listAnswer(map[string]execx.Result{"/x/p": {Code: 1, Stderr: []byte("boom")}})
	got, err := f.local().Running(context.Background(), cfg)
	if !errors.Is(err, ErrUnknown) || got != nil {
		t.Errorf("failed list: %v %v", got, err)
	}
	f.exec.answer = listAnswer(map[string]execx.Result{"/x/p": {Stdout: []byte(`[{"name": "a"}]`)}})
	got, err = f.local().Running(context.Background(), cfg)
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"a": true}) {
		t.Errorf("list: %v %v", got, err)
	}
	f.exec.answer = listAnswer(map[string]execx.Result{"/x/p": {Stdout: []byte(`{"name": "a"}`)}})
	if _, err := f.local().Running(context.Background(), cfg); !errors.Is(err, ErrUnknown) {
		t.Errorf("not a list: %v", err)
	}
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, os.ErrNotExist }
	if _, err := f.local().Running(context.Background(), cfg); !errors.Is(err, ErrUnknown) {
		t.Errorf("scion missing: %v", err)
	}
}

// The manager's agents run in its own workspace and count as running here
// (so they count in busy); board projects are never listed; every
// factory project is.
func TestTheManagersAgentsCountAsRunningHere(t *testing.T) {
	f := newFixture(t)
	cfg := cfgOf(t, "[projects.a]\npath = \"/x/a\"\n[projects.b]\npath = \"/x/b\"\nmode = \"board\"\n[projects.c]\npath = \"/x/c\"\n")
	f.exec.answer = listAnswer(map[string]execx.Result{
		"/x/a":          {Stdout: []byte(`[{"name": "t0001-w1"}]`)},
		"/x/c":          {Stdout: []byte(`[{"name": "t0002-r1"}, {"name": "t0001-w1"}]`)},
		f.paths.Manager: {Stdout: []byte(`[{"name": "t0009-m1"}]`)},
	})
	got, _ := f.local().Running(context.Background(), cfg)
	if !reflect.DeepEqual(got, map[string]bool{"t0001-w1": true, "t0002-r1": true}) {
		t.Errorf("without a manager workspace: %v", got)
	}
	if err := os.MkdirAll(filepath.Join(f.paths.Manager, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.exec.calls = nil
	got, err := f.local().Running(context.Background(), cfg)
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"t0001-w1": true, "t0002-r1": true, "t0009-m1": true}) {
		t.Errorf("with one: %v %v", got, err)
	}
	var dirs []string
	for _, c := range f.exec.calls {
		dirs = append(dirs, c.Dir)
		if strings.Join(c.Argv[1:], " ") != "list --format json" {
			t.Errorf("argv %q", c.Argv)
		}
		for _, kv := range c.Env {
			if strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=") {
				t.Error("scion list got the Claude token")
			}
		}
	}
	if !reflect.DeepEqual(dirs, []string{"/x/a", "/x/c", f.paths.Manager}) {
		t.Errorf("dirs %q", dirs)
	}
}

// a project the repository guard refuses is skipped by the
// count, not a failed list, so the other projects' agents stay known and
// new ones can still start elsewhere.
func TestARefusedProjectDoesNotMakeTheCountUnknown(t *testing.T) {
	f := newFixture(t)
	cfg := cfgOf(t, "[projects.a]\npath = \"/x/a\"\n[projects.b]\npath = \"/x/b\"\n")
	f.exec.answer = listAnswer(map[string]execx.Result{
		"/x/a": {Stdout: []byte(`[{"name": "t0001-w1"}]`)},
		"/x/b": {Stdout: []byte(`[{"name": "t0002-w1"}]`)},
	})
	l := f.local()
	l.Scion.Guard = func(_ context.Context, dir string) error {
		if dir == "/x/b" {
			return errors.New("/x/b: unexpected .git file: the factory only runs git in a plain clone")
		}
		return nil
	}
	got, err := l.Running(context.Background(), cfg)
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"t0001-w1": true}) {
		t.Fatalf("running %v, %v", got, err)
	}
	for _, c := range f.exec.calls {
		if c.Dir == "/x/b" {
			t.Errorf("scion ran in the refused project: %q", c.Argv)
		}
	}
}

// port: TF LookTest.test_look_reads_the_local_screen_or_the_helper_hosts_tail
func TestLookReadsTheLocalScreen(t *testing.T) {
	f := newFixture(t)
	l := f.local()
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Stdout: []byte("working…\n")}, nil }
	got, err := l.Look(context.Background(), "/x", "t0001-w1", 120)
	if err != nil || got != "working…" {
		t.Errorf("%q %v", got, err)
	}
	if a := f.exec.args(); len(a) != 1 || a[0] != "look t0001-w1 --plain -n 120" || f.exec.calls[0].Dir != "/x" {
		t.Errorf("argv %q", a)
	}
	// a blank screen or a failed look falls back to the end of agent.log
	home := f.agentHome(t, "t0001-w1")
	log := strings.Repeat("x\n", 40_000) + "last line\n"
	os.WriteFile(filepath.Join(home, "agent.log"), []byte(log), 0o644)
	for _, ans := range []execx.Result{{Stdout: []byte(" \n")}, {Code: 1, Stderr: []byte("no such agent")}} {
		f.exec.answer = func(c execx.Cmd) (execx.Result, error) { return ans, nil }
		got, err = l.Look(context.Background(), "/x", "t0001-w1", 2)
		if err != nil || got != "x\nlast line" {
			t.Errorf("fallback: %q %v", got, err)
		}
	}
	// no checkout: straight to the log; no home: nothing
	if got, _ := l.Look(context.Background(), "", "t0001-w1", 1); got != "last line" {
		t.Errorf("no dir: %q", got)
	}
	if got, err := l.Look(context.Background(), "", "t0002-w1", 1); got != "" || err != nil {
		t.Errorf("no home: %q %v", got, err)
	}
	// a link planted in the home is not read
	os.Remove(filepath.Join(home, "agent.log"))
	secret := filepath.Join(f.env.Home, "secret")
	os.WriteFile(secret, []byte("token\n"), 0o600)
	os.Symlink(secret, filepath.Join(home, "agent.log"))
	if got, _ := l.Look(context.Background(), "", "t0001-w1", 5); got != "" {
		t.Errorf("read through a link: %q", got)
	}
	// a scion that cannot run is an error, not a blank screen
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, os.ErrNotExist }
	if _, err := l.Look(context.Background(), "/x", "t0001-w1", 5); err == nil {
		t.Error("no error")
	}
}

// The local usage-limit path: only an agent whose activity log
// has been quiet for QUIET_MINUTES is looked at, over 40 lines.
func TestQuietAndTheLimitLook(t *testing.T) {
	f := newFixture(t)
	l := f.local()
	if l.Quiet("t0001-w1", 4*time.Minute) {
		t.Error("no home is quiet")
	}
	home := f.agentHome(t, "t0001-w1")
	if l.Quiet("t0001-w1", 4*time.Minute) {
		t.Error("no log is quiet")
	}
	log := filepath.Join(home, "agent.log")
	os.WriteFile(log, []byte("x"), 0o644)
	os.Chtimes(log, now, now.Add(-3*time.Minute))
	if l.Quiet("t0001-w1", 4*time.Minute) {
		t.Error("3 minutes is not quiet")
	}
	os.Chtimes(log, now, now.Add(-5*time.Minute))
	if !l.Quiet("t0001-w1", 4*time.Minute) {
		t.Error("5 minutes is quiet")
	}
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: []byte("⎿  Claude usage limit reached. Your limit will reset at 5pm\n")}, nil
	}
	got, err := l.Look(context.Background(), "/x", "t0001-w1", 40)
	if err != nil || !strings.Contains(got, "usage limit reached") || f.exec.args()[0] != "look t0001-w1 --plain -n 40" {
		t.Errorf("%q %v %q", got, err, f.exec.args())
	}
}

func TestReportIsReadWithoutFollowingLinks(t *testing.T) {
	f := newFixture(t)
	l := f.local()
	if _, ok, err := l.Report("t0001-w1"); ok || err != nil {
		t.Errorf("no home: %v %v", ok, err)
	}
	home := f.agentHome(t, "t0001-w1")
	if _, ok, _ := l.Report("t0001-w1"); ok {
		t.Error("no report")
	}
	os.WriteFile(filepath.Join(home, "report.json"), []byte(`{"status": "done"}`), 0o644)
	if b, ok, err := l.Report("t0001-w1"); !ok || err != nil || string(b) != `{"status": "done"}` {
		t.Errorf("%q %v %v", b, ok, err)
	}
	os.Remove(filepath.Join(home, "report.json"))
	secret := filepath.Join(f.env.Home, "secret.json")
	os.WriteFile(secret, []byte(`{"status": "done", "secret": 1}`), 0o600)
	os.Symlink(secret, filepath.Join(home, "report.json"))
	if b, ok, err := l.Report("t0001-w1"); !ok || err != nil || len(b) != 0 {
		t.Errorf("a linked report: %q %v %v", b, ok, err)
	}
}

// remove_agent: the screen and the run's files kept in logs/, the run
// config dropped, then scion stop and delete.
func TestRemoveKeepsTheScreenArchivesAndDeletes(t *testing.T) {
	f := newFixture(t)
	l := f.local()
	home := f.agentHome(t, "t0003-w1")
	os.WriteFile(filepath.Join(home, "report.json"), []byte(`{"status": "done"}`), 0o640)
	os.WriteFile(filepath.Join(home, "agent.log"), []byte("log\n"), 0o644)
	old := now.Add(-time.Hour)
	os.Chtimes(filepath.Join(home, "agent.log"), old, old)
	secret := filepath.Join(f.env.Home, "secret")
	os.WriteFile(secret, []byte("token"), 0o600)
	os.Symlink(secret, filepath.Join(home, "prompt.md"))
	os.MkdirAll(f.paths.Run, 0o755)
	os.WriteFile(filepath.Join(f.paths.Run, "t0003-w1.yaml"), []byte("x"), 0o644)
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		if c.Argv[1] == "look" {
			return execx.Result{Stdout: []byte("2026/10/16 20:12:08 WARN x\nlast screen  \n\n")}, nil
		}
		return execx.Result{Code: 1}, nil // stop and delete failing is not an error
	}
	if err := l.Remove(context.Background(), "/x/p", "T0003", "t0003-w1", true); err != nil {
		t.Fatal(err)
	}
	want := []string{"look t0003-w1 --plain -n 200", "stop t0003-w1", "delete t0003-w1 -b -y"}
	if got := f.exec.args(); !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q", got)
	}
	dest := filepath.Join(f.paths.Logs, "T0003", "t0003-w1")
	if b, _ := os.ReadFile(filepath.Join(dest, "screen.txt")); string(b) != "last screen" {
		t.Errorf("screen %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "report.json")); string(b) != `{"status": "done"}` {
		t.Errorf("report %q", b)
	}
	if st, err := os.Stat(filepath.Join(dest, "report.json")); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Join(dest, "agent.log")); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("mtime %v %v", st, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "prompt.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a linked prompt was archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.paths.Run, "t0003-w1.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Error("run config kept")
	}
	// a no-branch stage: no -b; a blank screen writes no screen.txt
	f2 := newFixture(t)
	f2.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, nil }
	if err := f2.local().Remove(context.Background(), "/x/p", "T0004", "t0004-t1", false); err != nil {
		t.Fatal(err)
	}
	if got := f2.exec.args(); got[2] != "delete t0004-t1 -y" {
		t.Errorf("argv %q", got)
	}
	if _, err := os.Stat(filepath.Join(f2.paths.Logs, "T0004", "t0004-t1", "screen.txt")); err == nil {
		t.Error("blank screen kept")
	}
	// a scion that cannot run stops the removal
	f2.exec.calls = nil
	f2.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, context.DeadlineExceeded }
	if err := f2.local().Remove(context.Background(), "/x/p", "T0004", "t0004-t1", false); err == nil || len(f2.exec.calls) != 1 {
		t.Errorf("%v after %d calls", err, len(f2.exec.calls))
	}
}

func projectAt(t *testing.T, dir string) *config.Project {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".scion", "project-id"), []byte("p"), 0o644)
	return &config.Project{Key: "p", Path: dir, Base: "main"}
}

// The full local scion start argv: -t, message mode, --config only with
// memory, -b only for branch stages, --model only when set, the prompt
// last.
func TestLocalStartArgv(t *testing.T) {
	f := newFixture(t)
	p := projectAt(t, t.TempDir())
	tk := ticketOf(t, `{"id": "T0003", "branch": "bot/x"}`)
	for _, c := range []struct {
		job  Job
		want string
	}{
		{Job{Ticket: tk, Stage: model.StageWork, Name: "t0003-w1", Prompt: "do it", Template: "worker", Project: p,
			RunConfig: "/r/t0003-w1.yaml", Model: "m"},
			"start t0003-w1 -t worker --message-mode none --config /r/t0003-w1.yaml -b bot/x --model m do it"},
		{Job{Ticket: tk, Stage: model.StageTriage, Name: "t0003-t1", Prompt: "triage", Template: "triage", Project: p},
			"start t0003-t1 -t triage --message-mode none triage"},
	} {
		f.exec.calls = nil
		if err := f.local().Start(context.Background(), c.job); err != nil {
			t.Fatal(err)
		}
		args := f.exec.args()
		if len(args) != 1 || args[0] != c.want || f.exec.calls[0].Dir != p.Path {
			t.Errorf("argv %q", args)
		}
		hasToken := false
		for _, kv := range f.exec.calls[0].Env {
			hasToken = hasToken || kv == "CLAUDE_CODE_OAUTH_TOKEN=tok"
		}
		if !hasToken {
			t.Error("scion start without the token")
		}
	}
	if err := f.local().Start(context.Background(), Job{Ticket: tk, Stage: model.StageTask, Name: "t0003-k1", Template: "task"}); err == nil {
		t.Error("a local start with no checkout")
	}
	noBranch := ticketOf(t, `{"id": "T0005"}`)
	if err := f.local().Start(context.Background(), Job{Ticket: noBranch, Stage: model.StageReview, Name: "t0005-r1", Template: "reviewer", Project: p}); err == nil {
		t.Error("a review with no branch")
	}
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Code: 1, Stderr: []byte("bad template")}, nil
	}
	err := f.local().Start(context.Background(), Job{Ticket: tk, Stage: model.StageTriage, Name: "t0003-t2", Template: "triage", Project: p})
	if err == nil || !strings.Contains(err.Error(), "bad template") {
		t.Errorf("failed start: %v", err)
	}
}

// Removing a local agent keeps what it spent: its home's Claude Code
// transcript, read before Scion deletes the home, as logs/<id>/<run>/
// usage.json. A home with no transcript (Pi) leaves none.
func TestRemoveKeepsTheRunsUsage(t *testing.T) {
	f := newFixture(t)
	home := f.agentHome(t, "t0003-w1")
	dir := filepath.Join(home, ".claude", "projects", "-repo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(
		`{"type":"assistant","message":{"id":"m1","model":"claude-opus","usage":{"input_tokens":7,"output_tokens":8}}}`+"\n"), 0o644)
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, nil }
	if err := f.local().Remove(context.Background(), "/x/p", "T0003", "t0003-w1", true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.paths.Logs, "T0003", "t0003-w1", "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := usage.Parse(b)
	if err != nil || u.Models["claude-opus"] != (usage.Tokens{Input: 7, Output: 8, Messages: 1}) {
		t.Errorf("usage %s %v", b, err)
	}
	f.agentHome(t, "t0004-w1")
	if err := f.local().Remove(context.Background(), "/x/p", "T0004", "t0004-w1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.paths.Logs, "T0004", "t0004-w1", "usage.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("usage.json for a run with no transcript: %v", err)
	}
}

// The usage is read once `scion stop` has stopped the agent, so what it
// spent up to then counts, and before `scion delete` takes its home.
func TestRemoveReadsTheUsageAfterTheStop(t *testing.T) {
	f := newFixture(t)
	home := f.agentHome(t, "t0003-w1")
	dir := filepath.Join(home, ".claude", "projects", "-repo")
	os.MkdirAll(dir, 0o755)
	transcript := filepath.Join(dir, "s.jsonl")
	os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"id":"m1","model":"claude-opus","usage":{"input_tokens":7}}}`+"\n"), 0o644)
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		switch {
		case slices.Contains(c.Argv, "stop"):
			// the agent's last message, written while it stops
			fh, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
			fh.WriteString(`{"type":"assistant","message":{"id":"m2","model":"claude-opus","usage":{"input_tokens":5}}}` + "\n")
			fh.Close()
		case slices.Contains(c.Argv, "delete"):
			os.RemoveAll(home)
		}
		return execx.Result{}, nil
	}
	if err := f.local().Remove(context.Background(), "/x/p", "T0003", "t0003-w1", true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.paths.Logs, "T0003", "t0003-w1", "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if u, err := usage.Parse(b); err != nil || u.Models["claude-opus"] != (usage.Tokens{Input: 12, Messages: 2}) {
		t.Errorf("usage %s %v", b, err)
	}
}

// A usage read that fails (here a home the walk cannot list) is logged,
// and the agent is still stopped and deleted: the usage is only a record.
func TestRemoveGoesOnWhenTheUsageReadFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root lists a directory it may not read")
	}
	f := newFixture(t)
	home := f.agentHome(t, "t0003-w1")
	os.MkdirAll(filepath.Join(home, ".claude", "projects", "-repo"), 0o755)
	if err := os.Chmod(home, 0o100); err != nil { // enter, not list
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(home, 0o755) })
	f.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, nil }
	var logged []string
	l := f.local()
	l.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if err := l.Remove(context.Background(), "/x/p", "T0003", "t0003-w1", true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := f.exec.args(); len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "delete t0003-w1") {
		t.Errorf("not deleted: %q", got)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "usage of t0003-w1 not kept") {
		t.Errorf("logged %q", logged)
	}
}
