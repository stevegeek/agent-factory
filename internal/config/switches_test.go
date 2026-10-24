package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const switchConfig = `
[projects.a]

[projects.b]
enabled = false

[projects.c]
mode = "board"
`

func writeSwitches(t *testing.T, path, text string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestReadSwitches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")
	if sw, err := ReadSwitches(path); sw != nil || err != nil {
		t.Fatalf("missing file: %v %v", sw, err)
	}
	writeSwitches(t, path, `{"a": {"enabled": false, "by": "alice", "at": "2026-01-01T00:00:00+00:00"}}`)
	sw, err := ReadSwitches(path)
	if err != nil || sw["a"] != (Switch{Enabled: false, By: "alice", At: "2026-01-01T00:00:00+00:00"}) {
		t.Fatalf("%v %v", sw, err)
	}
	for _, bad := range []string{`{`, `[]`, `null`, `{"a": {"by": "alice"}}`, `{"a": {"enabled": "no"}}`} {
		writeSwitches(t, path, bad)
		if sw, err := ReadSwitches(path); err == nil {
			t.Errorf("%s read as %v", bad, sw)
		}
	}
	// a link is refused, never followed
	target := filepath.Join(dir, "elsewhere.json")
	writeSwitches(t, target, `{}`)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSwitches(link); err == nil {
		t.Error("a link was followed")
	}
}

func TestWithSwitchesOverridesFactoryProjectsOnly(t *testing.T) {
	c := mustParse(t, switchConfig)
	sw := Switches{
		"a": {Enabled: false, By: "alice", At: "s1"},
		"b": {Enabled: true, By: "alice", At: "s2"},
		"c": {Enabled: false}, // a board project: ignored
		"z": {Enabled: false}, // not in config: ignored
	}
	n := c.WithSwitches(sw)
	if n == c {
		t.Fatal("nothing switched")
	}
	if n.Enabled("a") || !n.Enabled("b") || !n.Enabled("c") || !n.Enabled("z") {
		t.Errorf("enabled a=%v b=%v c=%v z=%v", n.Enabled("a"), n.Enabled("b"), n.Enabled("c"), n.Enabled("z"))
	}
	a, _ := n.Project("a")
	if a.Switch == nil || !a.ConfigEnabled || a.SwitchNote() != "paused here by alice at s1" || a.ConfigState() != "enabled" {
		t.Errorf("a: %+v %q", a.Switch, a.SwitchNote())
	}
	b, _ := n.Project("b")
	if b.SwitchNote() != "switched on here by alice at s2" || b.ConfigState() != "paused" {
		t.Errorf("b: %q %q", b.SwitchNote(), b.ConfigState())
	}
	if cp, _ := n.Project("c"); cp.Switch != nil {
		t.Error("a board project was switched")
	}
	// the shared config is untouched
	if !c.Enabled("a") || c.Enabled("b") || c.Projects[0].Switch != nil {
		t.Error("WithSwitches changed its receiver")
	}
	if got := c.WithSwitches(Switches{"z": {}}); got != c {
		t.Error("nothing to switch made a copy")
	}
	if got := c.WithSwitches(nil); got != c {
		t.Error("no switches made a copy")
	}
}

func TestSwitchedFallsBackToConfigOnABrokenFile(t *testing.T) {
	c := mustParse(t, switchConfig)
	path := filepath.Join(t.TempDir(), "projects.json")
	log := &recLog{}
	if got := c.Switched(path, log); got != c || log.count() != 0 {
		t.Fatal("a missing file switched something or was logged")
	}
	writeSwitches(t, path, `{"a": {"enabled": false}}`)
	if got := c.Switched(path, log); got.Enabled("a") {
		t.Fatal("a not switched off")
	}
	writeSwitches(t, path, `{"a": {"enabled": fals`)
	if got := c.Switched(path, log); got != c {
		t.Fatal("a broken file switched something")
	}
	if log.count() != 1 || !strings.Contains(log.lines[0], "following config.toml") {
		t.Fatalf("log %q", log.lines)
	}
}

func TestUpdateSwitches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreman-state", "projects.json")
	set := func(k string, s *Switch) {
		t.Helper()
		err := UpdateSwitches(t.Context(), path, func(sw Switches) error {
			if s == nil {
				delete(sw, k)
			} else {
				sw[k] = *s
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	set("a", &Switch{Enabled: false, By: "alice", At: "s1"})
	set("b", &Switch{Enabled: true, By: "alice", At: "s2"})
	sw, err := ReadSwitches(path)
	if err != nil || len(sw) != 2 || sw["a"].Enabled || !sw["b"].Enabled {
		t.Fatalf("%v %v", sw, err)
	}
	set("a", nil)
	if sw, _ := ReadSwitches(path); len(sw) != 1 {
		t.Fatalf("follow left %v", sw)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}
	// a broken file is replaced: it switched nothing anyway
	writeSwitches(t, path, `garbage`)
	set("a", &Switch{Enabled: true})
	if sw, err := ReadSwitches(path); err != nil || len(sw) != 1 || !sw["a"].Enabled {
		t.Fatalf("%v %v", sw, err)
	}
}

func TestSourceAppliesSwitches(t *testing.T) {
	src, _, log := newSource(t, switchConfig)
	src.Switches = filepath.Join(t.TempDir(), "projects.json")
	c1, err := src.Current()
	if err != nil || !c1.Enabled("a") {
		t.Fatalf("%v %v", c1, err)
	}
	writeSwitches(t, src.Switches, `{"a": {"enabled": false, "by": "alice"}}`)
	c2, _ := src.Current()
	if c2.Enabled("a") {
		t.Fatal("the switch was not seen")
	}
	if c3, _ := src.Current(); c3 != c2 {
		t.Error("an unchanged switch file made a new config")
	}
	// broken: config.toml alone, logged once for this state of the file
	writeSwitches(t, src.Switches, `{`)
	c4, err := src.Current()
	if err != nil || !c4.Enabled("a") {
		t.Fatalf("a broken file: %v", err)
	}
	src.Current()
	if log.count() != 1 {
		t.Fatalf("log %q", log.lines)
	}
	// removed: follows config again
	if err := os.Remove(src.Switches); err != nil {
		t.Fatal(err)
	}
	if c5, _ := src.Current(); !c5.Enabled("a") || c5.Enabled("b") {
		t.Fatal("a removed file still switches")
	}
}
