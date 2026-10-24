package config

import (
	"reflect"
	"testing"
)

// The TT CONFIG fixture (the Python tests).
const ticketsConfig = `
[board]
people = ["alice", "bob"]

[projects.sandbox]
mode = "board"

[projects.other]
mode = "board"

[projects.fac]
path = "/nonexistent"
github = "o/fac"
`

func keys(ps []Project) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Key)
	}
	return out
}

// port: TT ModeTest.test_factory_projects_leave_board_projects_out
func TestFactoryProjectsLeaveBoardProjectsOut(t *testing.T) {
	c := mustParse(t, ticketsConfig)
	if got := keys(c.FactoryProjects()); !reflect.DeepEqual(got, []string{"fac"}) {
		t.Errorf("FactoryProjects = %v", got)
	}
	if got := keys(c.BoardProjects()); !reflect.DeepEqual(got, []string{"sandbox", "other"}) {
		t.Errorf("BoardProjects = %v (declaration order)", got)
	}
	if mode, ok := c.ProjectMode(NoticeProject); !ok || mode != ModeFactory {
		t.Errorf("ProjectMode(factory) = %q, %v: the notice project is a factory project with no entry", mode, ok)
	}
	if mode, ok := c.ProjectMode("nope"); ok || mode != "" {
		t.Errorf("ProjectMode(nope) = %q, %v: want None", mode, ok)
	}
	if !reflect.DeepEqual(c.People(), []string{"alice", "bob"}) || c.Maintainer() != "alice" {
		t.Errorf("people %v, maintainer %q", c.People(), c.Maintainer())
	}
}

// A mode that is neither factory nor board leaves the project out of both,
// as project_mode(...) == "factory" / "board" does.
func TestOddModesAreNeither(t *testing.T) {
	c := mustParse(t, "[projects.x]\nmode = \"kanban\"\n[projects.y]\nmode = 3\n[projects.z]\n")
	if got := keys(c.FactoryProjects()); !reflect.DeepEqual(got, []string{"z"}) {
		t.Errorf("FactoryProjects = %v", got)
	}
	if got := keys(c.BoardProjects()); len(got) != 0 {
		t.Errorf("BoardProjects = %v", got)
	}
	if m, ok := c.ProjectMode("y"); !ok || m != "3" {
		t.Errorf("ProjectMode(y) = %q, %v", m, ok)
	}
}

// people(cfg) is list(get("people") or DEFAULT_PEOPLE): an empty list is the
// default, and a string is a list of its characters (check says it is wrong).
func TestPeople(t *testing.T) {
	for text, want := range map[string][]string{
		"":                                {"maintainer"},
		"[board]\npeople = []\n":          {"maintainer"},
		"[board]\npeople = [\"bo\", 3]\n": {"bo", "3"},
		"[board]\npeople = \"ab\"\n":      {"a", "b"},
		"[board]\npeople = \"\"\n":        {"maintainer"},
	} {
		if got := mustParse(t, text).People(); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: People = %q, want %q", text, got, want)
		}
	}
}
