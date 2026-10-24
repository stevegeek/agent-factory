package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// testCommands is a small table with every kind of flag and positional
// the real table uses.
func testCommands(got *Args) []Command {
	keep := func(_ context.Context, _ *Env, a Args) error { *got = a; return nil }
	return []Command{
		{Name: "move", Positionals: []Positional{{Name: "ticket"}, {Name: "column"}},
			Flags: []Flag{{Name: "note"}}, Run: keep},
		{Name: "note", Positionals: []Positional{{Name: "ticket"}}, Rest: "text", RestMin: 1, Run: keep},
		{Name: "ticket", Positionals: []Positional{{Name: "verb", Choices: []string{"get", "list"}}}, Rest: "args",
			Flags: []Flag{{Name: "as", Metavar: "NAME"}, {Name: "wait", Kind: FloatFlag, Default: "60"}, {Name: "json", Kind: BoolFlag}},
			Run:   keep},
		{Name: "add", Positionals: []Positional{{Name: "project"}, {Name: "title", Optional: true}},
			Flags: []Flag{{Name: "door", Default: "human", Choices: []string{"human", "clock"}}, {Name: "timeout", Kind: IntFlag},
				{Name: "task", Kind: BoolFlag}, {Name: "body"}},
			Run: keep},
		{Name: "tokens", Positionals: []Positional{{Name: "verb"}, {Name: "name", Optional: true}},
			Flags: []Flag{{Name: "project", Kind: RepeatFlag}}, Run: keep},
		{Name: "redirect", Positionals: []Positional{{Name: "ticket"}}, Flags: []Flag{{Name: "note", Required: true}}, Run: keep},
		{Name: "look", Positionals: []Positional{{Name: "ticket"}}, Flags: []Flag{{Name: "lines", Kind: IntFlag, Default: "120"}}, Run: keep},
	}
}

func route(t *testing.T, argv ...string) (Args, int, string, string) {
	t.Helper()
	var got Args
	var out, errb bytes.Buffer
	e := &Env{Stdout: &out, Stderr: &errb}
	code := dispatch(context.Background(), e, testCommands(&got), argv)
	return got, code, out.String(), errb.String()
}

func TestFlagsMayComeAnywhere(t *testing.T) {
	for _, argv := range [][]string{
		{"move", "T0001", "ready", "--note", "x y"},
		{"move", "--note", "x y", "T0001", "ready"},
		{"move", "T0001", "--note=x y", "ready"},
	} {
		a, code, _, errs := route(t, argv...)
		if code != 0 || a.Pos("ticket") != "T0001" || a.Pos("column") != "ready" || a.Str("note") != "x y" {
			t.Errorf("%q: code %d, %+v, %q", argv, code, a, errs)
		}
	}
}

func TestDoubleDashEndsOptions(t *testing.T) {
	a, code, _, _ := route(t, "note", "T0001", "--", "-starts", "with", "--a-dash")
	if code != 0 || a.Text() != "-starts with --a-dash" {
		t.Errorf("code %d text %q", code, a.Text())
	}
	a, code, _, _ = route(t, "redirect", "T0001", "--note=-TEXT")
	if code != 0 || a.Str("note") != "-TEXT" {
		t.Errorf("--note=-TEXT: code %d %q", code, a.Str("note"))
	}
	// Foreman's add: flags, then -- and the positionals.
	a, code, _, _ = route(t, "add", "--body=-b", "--task", "--", "gem", "-A title")
	if code != 0 || a.Pos("project") != "gem" || a.Pos("title") != "-A title" || a.Str("body") != "-b" || !a.Bool("task") {
		t.Errorf("add: code %d %+v", code, a)
	}
}

func TestAnArgumentWithASpaceIsAPositional(t *testing.T) {
	// argparse reads "-x y" as a positional unless it starts with a known flag.
	a, code, _, errs := route(t, "note", "T0001", "-a note with spaces")
	if code != 0 || a.Text() != "-a note with spaces" {
		t.Errorf("code %d %q %q", code, a.Text(), errs)
	}
	a, code, _, _ = route(t, "move", "T1", "c", "--note=a b")
	if code != 0 || a.Str("note") != "a b" {
		t.Errorf("--note=a b: %d %q", code, a.Str("note"))
	}
}

func TestNegativeNumbersAreValues(t *testing.T) {
	a, code, _, errs := route(t, "add", "gem", "--timeout", "-5")
	if code != 0 || a.Int("timeout") != -5 {
		t.Errorf("code %d timeout %d %q", code, a.Int("timeout"), errs)
	}
	a, code, _, _ = route(t, "note", "T1", "-3")
	if code != 0 || a.Text() != "-3" {
		t.Errorf("note -3: %d %q", code, a.Text())
	}
}

func TestDefaultsTypesAndRepeats(t *testing.T) {
	a, code, _, _ := route(t, "ticket", "get", "T0001", "--json")
	if code != 0 || a.Float("wait") != 60 || !a.Bool("json") || a.Has("as") || !reflect.DeepEqual(a.Rest(), []string{"T0001"}) {
		t.Errorf("ticket: %d %+v", code, a)
	}
	a, _, _, _ = route(t, "ticket", "list", "--wait", "0.5", "--as", "bot")
	if a.Float("wait") != 0.5 || a.Str("as") != "bot" || len(a.Rest()) != 0 {
		t.Errorf("ticket list: %+v", a)
	}
	a, _, _, _ = route(t, "tokens", "add", "x", "--project", "a", "--project=b", "--project", "a")
	if !reflect.DeepEqual(a.List("project"), []string{"a", "b", "a"}) {
		t.Errorf("repeat: %q", a.List("project"))
	}
	a, _, _, _ = route(t, "tokens", "list")
	if a.Has("name") || a.Pos("name") != "" || a.List("project") != nil {
		t.Errorf("optional absent: %+v", a)
	}
	// The last of a repeated single flag wins, as in argparse.
	a, _, _, _ = route(t, "move", "T1", "c", "--note", "one", "--note", "two")
	if a.Str("note") != "two" {
		t.Errorf("last wins: %q", a.Str("note"))
	}
	a, _, _, _ = route(t, "add", "gem", "A title")
	if a.Str("door") != "human" || a.Pos("title") != "A title" || a.Has("timeout") {
		t.Errorf("add defaults: %+v", a)
	}
	a, _, _, _ = route(t, "look", "T1")
	if a.Int("lines") != 120 {
		t.Errorf("look default: %d", a.Int("lines"))
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{nil, "the following arguments are required: command"},
		{[]string{"nope"}, "argument command: invalid choice: 'nope'"},
		{[]string{"move", "T1"}, "the following arguments are required: column"},
		{[]string{"move"}, "the following arguments are required: ticket, column"},
		{[]string{"note", "T1"}, "the following arguments are required: text"},
		{[]string{"move", "T1", "c", "extra"}, "unrecognized arguments: extra"},
		{[]string{"move", "T1", "c", "--bogus"}, "unrecognized arguments: --bogus"},
		{[]string{"move", "T1", "c", "--no", "x"}, "unrecognized arguments: --no"}, // no prefix abbreviation
		{[]string{"move", "T1", "c", "--note"}, "argument --note: expected one argument"},
		{[]string{"move", "T1", "c", "--note", "--json"}, "argument --note: expected one argument"},
		{[]string{"ticket", "get", "--json=yes"}, "argument --json: ignored explicit argument 'yes'"},
		{[]string{"ticket", "nope"}, "argument verb: invalid choice: 'nope' (choose from 'get', 'list')"},
		{[]string{"ticket", "get", "--wait", "soon"}, "argument --wait: invalid float value: 'soon'"},
		{[]string{"add", "gem", "--timeout", "x"}, "argument --timeout: invalid int value: 'x'"},
		{[]string{"add", "gem", "--door", "peer"}, "argument --door: invalid choice: 'peer' (choose from 'human', 'clock')"},
		{[]string{"redirect", "T1"}, "the following arguments are required: --note"},
		{[]string{"-x"}, "unrecognized arguments: -x"},
	} {
		_, code, out, errs := route(t, c.argv...)
		if code != 2 || out != "" || !strings.HasPrefix(errs, "usage: factory") || !strings.Contains(errs, "error: "+c.want) {
			t.Errorf("%q: code %d stdout %q stderr %q, want %q", c.argv, code, out, errs, c.want)
		}
	}
}

func TestHelp(t *testing.T) {
	for _, argv := range [][]string{{"-h"}, {"--help"}} {
		_, code, out, errs := route(t, argv...)
		if code != 0 || errs != "" || !strings.HasPrefix(out, "usage: factory") || !strings.Contains(out, "  move ") {
			t.Errorf("%q: %d %q %q", argv, code, out, errs)
		}
		if !strings.Contains(out, "abbreviat") {
			t.Errorf("top-level help should say flags are not abbreviated: %q", out)
		}
	}
	_, code, out, _ := route(t, "move", "T1", "--help")
	if code != 0 || !strings.HasPrefix(out, "usage: factory move [-h] [--note NOTE] ticket column\n") {
		t.Errorf("move --help: %d %q", code, out)
	}
	_, _, out, _ = route(t, "note", "-h")
	if !strings.HasPrefix(out, "usage: factory note [-h] ticket text [text ...]\n") {
		t.Errorf("note -h: %q", out)
	}
	_, _, out, _ = route(t, "ticket", "-h")
	if !strings.HasPrefix(out, "usage: factory ticket [-h] [--as NAME] [--wait WAIT] [--json] {get,list} [args ...]\n") {
		t.Errorf("ticket -h: %q", out)
	}
	_, _, out, _ = route(t, "redirect", "-h")
	if !strings.HasPrefix(out, "usage: factory redirect [-h] --note NOTE ticket\n") {
		t.Errorf("redirect -h: %q", out)
	}
	// After --, -h is text.
	a, code, _, _ := route(t, "note", "T1", "--", "-h")
	if code != 0 || a.Text() != "-h" {
		t.Errorf("note -- -h: %d %q", code, a.Text())
	}
}

func TestErrorsMapToExitCodes(t *testing.T) {
	for _, c := range []struct {
		err       error
		code      int
		out, errs string
	}{
		{nil, 0, "", ""},
		{Refuse("T0001 has no design yet"), 1, "", "T0001 has no design yet\n"},
		{Exit(1), 1, "", ""},
		{Exit(3), 3, "", ""},
		{errors.New("boom"), 1, "", "factory: boom\n"},
		{Usage("give a name"), 2, "", "usage: factory move [-h] [--note NOTE] ticket column\nfactory move: error: give a name\n"},
	} {
		var out, errb bytes.Buffer
		e := &Env{Stdout: &out, Stderr: &errb}
		cmds := []Command{{Name: "move", Positionals: []Positional{{Name: "ticket"}, {Name: "column"}}, Flags: []Flag{{Name: "note"}},
			Run: func(context.Context, *Env, Args) error { return c.err }}}
		code := dispatch(context.Background(), e, cmds, []string{"move", "a", "b"})
		if code != c.code || out.String() != c.out || errb.String() != c.errs {
			t.Errorf("%v: %d %q %q", c.err, code, out.String(), errb.String())
		}
	}
}

func TestRawCommandsTakeTheirArgv(t *testing.T) {
	var got []string
	cmds := []Command{{Name: "render", Raw: func(_ context.Context, _ *Env, args []string) int { got = args; return 7 }}}
	var out, errb bytes.Buffer
	code := dispatch(context.Background(), &Env{Stdout: &out, Stderr: &errb}, cmds, []string{"render", "--x", "a", "--", "b"})
	if code != 7 || !reflect.DeepEqual(got, []string{"--x", "a", "--", "b"}) {
		t.Errorf("raw: %d %q", code, got)
	}
}
