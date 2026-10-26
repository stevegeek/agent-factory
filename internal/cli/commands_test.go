package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pythonArgv is the argv table of the Python factory's argparse, frozen at
// the cutover (testdata/python-argv.json, read from bin/factory's main()):
// each subcommand's name and the arguments it added ("--flag" or a
// positional's name), in order.
func pythonArgv(t *testing.T) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "python-argv.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The table has every command Python's argparse had, each with the same
// positionals and flags, so every argv Foreman, the units and the docs
// use still parses.
func TestTheTableTakesPythonsArgv(t *testing.T) {
	py := pythonArgv(t)
	if len(py) < 30 {
		t.Fatalf("read only %d subcommands from testdata/python-argv.json", len(py))
	}
	ours := map[string]Command{}
	for _, c := range commands() {
		ours[c.Name] = c
	}
	for name, args := range py {
		c, ok := ours[name]
		if !ok {
			t.Errorf("no command %s", name)
			continue
		}
		var got []string
		for _, p := range c.Positionals {
			got = append(got, p.Name)
		}
		if c.Rest != "" {
			got = append(got, c.Rest)
		}
		for _, f := range c.Flags {
			got = append(got, "--"+f.Name)
		}
		want := append([]string(nil), args...)
		switch name { // the flags Go adds
		case "check":
			want = append(want, "--board", "--asks")
		case "rebuild": // automatic updates; --latest: the latest-toolchain trial
			want = append(want, "--rollback", "--unhold", "--latest")
		case "add": // the findings flow
			want = append(want, "--findings")
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: arguments %q, Python's %q", name, got, want)
		}
	}
	// The commands Go adds.
	for _, name := range []string{"answer", "render", "patch-scion", "version", "backup", "findings", "screens", "fizzy", "project", "build", "kinds", "onboard", "deploy", "usage", "early-checks", "prune-branches"} { // backup: the R2 backup; project: the maintainer's switch; build, kinds, onboard, deploy: self-hosting; usage: tokens spent; early-checks: the gate's checks begun early; prune-branches: merged bot/* branches
		if _, ok := ours[name]; !ok {
			t.Errorf("no command %s", name)
		}
	}
	if len(ours) != len(py)+16 {
		var extra []string
		for n := range ours {
			if _, ok := py[n]; !ok {
				extra = append(extra, n)
			}
		}
		sort.Strings(extra)
		t.Errorf("commands Python did not have: %q", extra)
	}
}

func TestEveryCommandRuns(t *testing.T) {
	for _, c := range commands() {
		if (c.Run == nil) == (c.Raw == nil) {
			t.Errorf("%s: wants exactly one of Run and Raw", c.Name)
		}
	}
}
