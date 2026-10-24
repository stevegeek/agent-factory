package config

import (
	"fmt"

	"github.com/stevegeek/agent-factory/internal/toml"
)

// recipeKinds is RECIPE_KINDS.
func recipeKinds() []string { return []string{"change", "task", "intent", "prospect"} }

// buildRecipes is recipes(cfg): every schedule then every recipe, keyed by
// name. A later entry with a name already seen replaces the earlier one in
// its place (a Python dict keeps a key's first position). The kind is
// `kind`, or "intent" when `intent` is true, else "change"; other kinds are
// left out. scheduled is `r in cfg["schedules"]`, compared by value.
//
// Python fails on an entry without name, title or body; the port leaves it
// out (a schedule's own decode already said why; a recipe says it here).
func buildRecipes(c *Config, schedules, recipes []*section) []Recipe {
	var out []Recipe
	at := map[string]int{}
	for i, s := range append(append([]*section{}, schedules...), recipes...) {
		t := s.t
		kind := "change"
		if k, _ := t.Get("kind"); toml.Truthy(k) {
			kind = toml.PyRepr(k) // never a kind unless it is a string
			if x, ok := k.(string); ok {
				kind = x
			}
		} else if in, _ := t.Get("intent"); toml.Truthy(in) {
			kind = "intent"
		}
		if !oneOf(kind, recipeKinds()) {
			continue
		}
		var fields [3]string
		missing := ""
		for j, key := range []string{"name", "title", "body"} {
			v, _ := t.Get(key)
			x, ok := v.(string)
			if !ok && missing == "" {
				missing = key
			}
			fields[j] = x
		}
		if missing != "" {
			if i >= len(schedules) {
				c.skipped = append(c.skipped, fmt.Sprintf("%s: no %s; the recipe is skipped", s.label, missing))
			}
			continue
		}
		r := Recipe{Name: fields[0], Title: fields[1], Body: fields[2], Kind: kind, Projects: []string{}}
		if v, _ := t.Get("findings"); toml.Truthy(v) && kind == "task" {
			r.Findings = true
		}
		if v, _ := t.Get("projects"); toml.Truthy(v) {
			if arr, ok := v.([]toml.Value); ok {
				for _, e := range arr {
					if x, ok := e.(string); ok {
						r.Projects = append(r.Projects, x)
					}
				}
			}
		}
		if v, ok := t.Get("about"); ok {
			r.About, _ = v.(string)
		}
		for _, sc := range schedules {
			if toml.Equal(t, sc.t) {
				r.Scheduled = true
				break
			}
		}
		if j, ok := at[r.Name]; ok {
			out[j] = r
		} else {
			at[r.Name] = len(out)
			out = append(out, r)
		}
	}
	return out
}

func oneOf(s string, set []string) bool {
	for _, x := range set {
		if s == x {
			return true
		}
	}
	return false
}
