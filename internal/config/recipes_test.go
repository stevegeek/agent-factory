package config

import (
	"reflect"
	"testing"
)

// port: TF ManagerTest.test_a_recipe_files_with_its_title_body_and_kind
// (the recipes(cfg) part; the add is ops/add_test.go)
func TestARecipeFilesWithItsTitleBodyAndKind(t *testing.T) {
	c := mustParse(t, `
[projects.gem]
path = "/x"
base = "main"
github = "acme/gem"

[[schedules]]
name = "bug-hunt"
kind = "task"
title = "Bug hunt"
body = "Look in {github}."
every_days = 30

[[recipes]]
name = "docs-pass"
title = "Tidy the docs"
body = "Read the README."
projects = ["other"]
`)
	rs := c.Recipes()
	want := []Recipe{
		{Name: "bug-hunt", Title: "Bug hunt", Body: "Look in {github}.", Kind: "task", Projects: []string{}, About: "", Scheduled: true},
		{Name: "docs-pass", Title: "Tidy the docs", Body: "Read the README.", Kind: "change", Projects: []string{"other"}, About: "", Scheduled: false},
	}
	if !reflect.DeepEqual(rs, want) {
		t.Fatalf("Recipes =\n%+v\nwant\n%+v", rs, want)
	}
	r, ok := c.Recipe("bug-hunt")
	if !ok || r.Kind != "task" {
		t.Errorf("Recipe(bug-hunt) = %+v, %v", r, ok)
	}
	if _, ok := c.Recipe("nope"); ok {
		t.Error("Recipe(nope)")
	}
	// the slice is the caller's
	rs[0].Name = "changed"
	if c.Recipes()[0].Name != "bug-hunt" {
		t.Error("Recipes shares its slice")
	}
}

// Kinds: kind, else intent when intent is true, else change; any other kind
// is left out. A recipe with a schedule's name takes its place in the
// order (a Python dict keeps a key's first position).
func TestRecipeKindsAndOrder(t *testing.T) {
	c := mustParse(t, `
[[schedules]]
name = "a"
every_days = 1
title = "A"
body = "a"

[[schedules]]
name = "b"
every_days = 1
title = "B"
body = "b"
kind = "prospect"
about = "Prospecting."

[[recipes]]
name = "c"
title = "C"
body = "c"
intent = true

[[recipes]]
name = "a"
title = "A again"
body = "a2"
kind = "intent"

[[recipes]]
name = "d"
title = "D"
body = "d"
kind = "watch"

[[recipes]]
name = "e"
title = "E"
body = "e"
kind = ""
intent = false
`)
	var got [][3]any
	for _, r := range c.Recipes() {
		got = append(got, [3]any{r.Name, r.Kind, r.Scheduled})
	}
	want := [][3]any{{"a", "intent", false}, {"b", "prospect", true}, {"c", "intent", false}, {"e", "change", false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Recipes = %v, want %v", got, want)
	}
	if r, _ := c.Recipe("b"); r.About != "Prospecting." {
		t.Errorf("about: %q", r.About)
	}
}

// scheduled is `r in cfg["schedules"]`: a recipe equal to a schedule (by
// value, as Python compares dicts) counts as scheduled.
func TestRecipeEqualToAScheduleIsScheduled(t *testing.T) {
	c := mustParse(t, `
[[schedules]]
name = "x"
title = "X"
body = "x"
every_days = 7

[[recipes]]
body = "x"
title = "X"
name = "x"
every_days = 7.0
`)
	rs := c.Recipes()
	if len(rs) != 1 || !rs[0].Scheduled {
		t.Errorf("Recipes = %+v", rs)
	}
}
