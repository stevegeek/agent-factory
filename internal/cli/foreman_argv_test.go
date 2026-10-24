package cli

import (
	"reflect"
	"strings"
	"testing"
)

// Every argv the old Foreman (core/foreman) sent the factory, from its
// Plan (actions.go), its new-ticket form (server.go), its live look
// (live.go), its recipes list and its API runner (api.go): the Go router
// must take each one and read the same arguments out of it as argparse did
// (§10.2). The table outlives core/foreman, deleted at the cutover, so
// every argv the old units, docs and scripts use keeps parsing.

func TestForemanArgvIsAccepted(t *testing.T) {
	type want struct {
		pos   map[string]string
		flags map[string]string
		rest  []string
	}
	cases := []struct {
		argv []string
		want want
	}{
		// Plan's answers (TestActions' table)
		{[]string{"note", "T0001", "split it"}, want{pos: map[string]string{"ticket": "T0001"}, rest: []string{"split it"}}},
		{[]string{"note", "T0001", "--", "-v but why"}, want{pos: map[string]string{"ticket": "T0001"}, rest: []string{"-v but why"}}},
		{[]string{"note", "T0001", "--", "--note"}, want{pos: map[string]string{"ticket": "T0001"}, rest: []string{"--note"}}},
		{[]string{"move", "T0001", "ready"}, want{pos: map[string]string{"ticket": "T0001", "column": "ready"}}},
		{[]string{"move", "T0007", "gate"}, want{pos: map[string]string{"ticket": "T0007", "column": "gate"}}},
		{[]string{"approve", "T0004", "--only", "1,3"}, want{pos: map[string]string{"ticket": "T0004"}, flags: map[string]string{"only": "1,3"}}},
		{[]string{"approve", "T0004"}, want{pos: map[string]string{"ticket": "T0004"}}},
		{[]string{"redirect", "T0004", "--note", "smaller please"}, want{pos: map[string]string{"ticket": "T0004"}, flags: map[string]string{"note": "smaller please"}}},
		{[]string{"redirect", "T0004", "--note=-v smaller"}, want{pos: map[string]string{"ticket": "T0004"}, flags: map[string]string{"note": "-v smaller"}}},
		{[]string{"reply", "T0009", "--", "-yes, keep it"}, want{pos: map[string]string{"ticket": "T0009"}, rest: []string{"-yes, keep it"}}},
		{[]string{"reply", "T0009", "--", "plain"}, want{pos: map[string]string{"ticket": "T0009"}, rest: []string{"plain"}}},
		{[]string{"ask", "T0001", "--", "-does it pass?"}, want{pos: map[string]string{"ticket": "T0001"}, rest: []string{"-does it pass?"}}},
		{[]string{"merge", "T0005"}, want{pos: map[string]string{"ticket": "T0005"}}},
		{[]string{"pr", "T0005"}, want{pos: map[string]string{"ticket": "T0005"}}},
		// the new-ticket form: options first, then "--"
		{[]string{"add", "--body=- a body that starts with a dash", "--", "gem", "-A title"},
			want{pos: map[string]string{"project": "gem", "title": "-A title"}, flags: map[string]string{"body": "- a body that starts with a dash"}}},
		{[]string{"add", "--body=", "--intent", "--", "gem", "An idea"},
			want{pos: map[string]string{"project": "gem", "title": "An idea"}, flags: map[string]string{"body": "", "intent": "true"}}},
		{[]string{"add", "--body=x", "--task", "--", "factory", "Why?"},
			want{pos: map[string]string{"project": "factory", "title": "Why?"}, flags: map[string]string{"body": "x", "task": "true"}}},
		{[]string{"add", "--body=x", "--manage", "--", "factory", "Add a host"},
			want{pos: map[string]string{"project": "factory", "title": "Add a host"}, flags: map[string]string{"body": "x", "manage": "true"}}},
		// the live page and the recipes list
		{[]string{"look", "T0008", "--lines", "150"}, want{pos: map[string]string{"ticket": "T0008"}, flags: map[string]string{"lines": "150"}}},
		{[]string{"recipes", "--json"}, want{flags: map[string]string{"json": "true"}}},
		// the API runner
		{[]string{"ticket", "create", "--as", "example-agent", "--wait", "10", "--json"},
			want{pos: map[string]string{"verb": "create"}, flags: map[string]string{"as": "example-agent", "wait": "10", "json": "true"}}},
	}
	cmds := commands()
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			var cmd *Command
			for i := range cmds {
				if cmds[i].Name == tc.argv[0] {
					cmd = &cmds[i]
				}
			}
			if cmd == nil {
				t.Fatalf("no command %s", tc.argv[0])
			}
			a, help, err := parse(cmd, tc.argv[1:])
			if err != nil || help {
				t.Fatalf("parse: %v (help %v)", err, help)
			}
			for k, v := range tc.want.pos {
				if a.Pos(k) != v {
					t.Errorf("%s = %q, want %q", k, a.Pos(k), v)
				}
			}
			for k, v := range tc.want.flags {
				if !a.Has(k) || a.Str(k) != v {
					t.Errorf("--%s = %q (given %v), want %q", k, a.Str(k), a.Has(k), v)
				}
			}
			if !reflect.DeepEqual(a.Rest(), tc.want.rest) && (len(a.Rest()) > 0 || len(tc.want.rest) > 0) {
				t.Errorf("rest %q, want %q", a.Rest(), tc.want.rest)
			}
			if cmd.Run == nil {
				t.Fatal("no runner")
			}
		})
	}
}

// Every command Foreman sends has a runner.
func TestForemanCommandsAreBuilt(t *testing.T) {
	built := map[string]bool{}
	for _, c := range commands() {
		built[c.Name] = c.Run != nil || c.Raw != nil
	}
	for _, name := range []string{"add", "move", "note", "reply", "ask", "approve", "redirect", "merge", "pr", "host",
		"migrate", "wake", "answer", "look", "audit", "memory", "github", "gate-workflow"} {
		if !built[name] {
			t.Errorf("%s has no runner", name)
		}
	}
}
