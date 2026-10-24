package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// lsLine is the line `factory ls` printed for id, "" when none.
func lsLine(out string, id ticket.ID) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, string(id)+" ") {
			return l
		}
	}
	return ""
}

// `factory project` is the maintainer's switch on Foreman's Boards, over
// ssh: it writes foreman-state/projects.json, logs and wakes, and every
// command after it (ls, check, the tick) reads the project as switched.
func TestProjectSwitchesAFactoryProject(t *testing.T) {
	f := goldenRoot(t)
	wl := wakeLog(f)
	tk := file(t, f, "gem", "ready", nil)
	path := f.env.Path("foreman-state", "projects.json")

	code, out, errs := f.run("", "project", "disable", "gem")
	if code != 0 || !strings.HasPrefix(out, "gem paused") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	sw, err := config.ReadSwitches(path)
	if err != nil || sw["gem"] != (config.Switch{Enabled: false, By: "alice", At: "2026-10-18T10:00:00+00:00"}) {
		t.Fatalf("%v %v", sw, err)
	}
	if n := wakes(t, wl); n != 1 {
		t.Fatalf("%d wakes", n)
	}
	if b, _ := os.ReadFile(f.env.Path("logs", "factory.log")); !strings.Contains(string(b), "project gem paused by alice") {
		t.Fatalf("factory.log %q", b)
	}
	// ls and check read it as switched
	if _, out, _ := f.run("", "ls"); !strings.Contains(lsLine(out, tk.ID), "(paused)") {
		t.Fatalf("ls %q", out)
	}
	if _, out, _ := f.run("", "check"); !strings.Contains(out, "note: [projects.gem] paused here by alice at 2026-10-18T10:00:00+00:00, over config's enabled") {
		t.Fatalf("check %q", out)
	}

	// a project paused in config is switched on
	if code, out, errs := f.run("", "project", "enable", "paused"); code != 0 || !strings.HasPrefix(out, "paused switched on") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if _, out, _ := f.run("", "check"); !strings.Contains(out, "note: [projects.paused] switched on here by alice") {
		t.Fatalf("check %q", out)
	}

	// back to config: the note goes, ls no longer says paused
	if code, out, errs := f.run("", "project", "follow", "gem"); code != 0 || out != "gem follows config.toml (enabled) again\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if _, out, _ := f.run("", "check"); strings.Contains(out, "note: [projects.gem]") {
		t.Fatalf("check %q", out)
	}
	if _, out, _ := f.run("", "ls"); strings.Contains(lsLine(out, tk.ID), "(paused)") {
		t.Fatalf("ls %q", out)
	}
	if code, out, _ := f.run("", "project", "follow", "gem"); code != 0 || !strings.Contains(out, "already follows") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if n := wakes(t, wl); n != 3 {
		t.Fatalf("%d wakes", n)
	}
}

// Only a factory project config names can be switched.
func TestProjectRefusesABoardOrUnknownProject(t *testing.T) {
	f := goldenRoot(t)
	for _, name := range []string{"notes", "nope"} {
		if code, out, errs := f.run("", "project", "disable", name); code != 1 || out != "" || errs == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
	if _, err := os.Stat(f.env.Path("foreman-state", "projects.json")); err == nil {
		t.Error("a refused switch wrote the file")
	}
}

// A broken switch file stops nothing: the commands follow config.toml and
// check says why; one that names a board project or an unknown one is
// warned of and otherwise ignored.
func TestABrokenOrStraySwitchFileIsWarnedOf(t *testing.T) {
	f := goldenRoot(t)
	if err := os.MkdirAll(f.env.Path("foreman-state"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := f.env.Path("foreman-state", "projects.json")
	if err := os.WriteFile(path, []byte(`{"gem": `), 0o600); err != nil {
		t.Fatal(err)
	}
	tk := file(t, f, "gem", "ready", nil)
	if code, out, _ := f.run("", "ls"); code != 0 || lsLine(out, tk.ID) == "" || strings.Contains(lsLine(out, tk.ID), "(paused)") {
		t.Fatalf("ls: exit %d, %q", code, out)
	}
	if _, out, _ := f.run("", "check"); !strings.Contains(out, "warning: project switches") {
		t.Fatalf("check %q", out)
	}
	if err := os.WriteFile(path, []byte(`{"notes": {"enabled": false}, "nope": {"enabled": false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := f.run("", "check")
	if !strings.Contains(out, "warning: projects.json switches nope, which is not a factory project: ignored") ||
		!strings.Contains(out, "switches notes") {
		t.Fatalf("check %q", out)
	}
}
