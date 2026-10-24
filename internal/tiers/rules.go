package tiers

import (
	"fmt"
	"math"
	"path"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/toml"
)

// Change is one config place a draft changes. Added and Removed say the
// place is only in the new or only in the old config; a place in both
// whose value changed has neither.
type Change struct {
	Place          string
	Old, New       toml.Value
	Added, Removed bool

	seg     []string // the place's parts: section names, then the key
	entry   bool     // seg[1] names a [[schedules]]/[[recipes]]/[[watches]] entry
	byIndex bool     // the entries could not be matched by name
	whole   bool     // part of a table added or removed whole
}

// entryArrays are the top-level arrays of tables whose entries are places.
func entryArrays() []string { return []string{"schedules", "recipes", "watches"} }

// ConfigPlaces lists the places that differ between two configs:
// a top-level key; section.key and section.sub.key; hosts.<n>.key and
// projects.<k>.key; schedules[<name>].key, recipes[<name>].key and
// watches[<name>].key. A table added or removed whole is a place itself and
// also gives each of its keys, so every rule runs on each. The string says
// why schedules were matched by position instead of name ("" when they
// were not).
func ConfigPlaces(old, new *toml.Table) ([]Change, string) {
	d := &differ{}
	why := ""
	for _, k := range unionKeys(old, new) {
		ov, oo := old.Get(k)
		nv, no := new.Get(k)
		if slices.Contains(entryArrays(), k) {
			if w := d.entries(k, ov, oo, nv, no); w != "" && k == "schedules" {
				why = w
			}
			continue
		}
		d.value([]string{k}, false, false, ov, oo, nv, no, false)
	}
	return d.out, why
}

type differ struct{ out []Change }

func unionKeys(a, b *toml.Table) []string {
	keys := a.Keys()
	for _, k := range b.Keys() {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

func placeOf(seg []string, entry bool) string {
	if !entry || len(seg) < 2 {
		return strings.Join(seg, ".")
	}
	p := seg[0] + "[" + seg[1] + "]"
	if len(seg) > 2 {
		p += "." + strings.Join(seg[2:], ".")
	}
	return p
}

func with(seg []string, k string) []string { return append(slices.Clip(seg), k) }

// same is strict equality for a leaf: Python's == says 1 == true, but the
// factory reads `enabled = 1` and `enabled = true` differently, so a type
// change is a change.
func same(a, b toml.Value) bool { return toml.PyRepr(a) == toml.PyRepr(b) }

// value diffs one place, recursing into tables present on both sides.
func (d *differ) value(seg []string, entry, byIndex bool, ov toml.Value, oo bool, nv toml.Value, no bool, whole bool) {
	ot, oIsT := ov.(*toml.Table)
	nt, nIsT := nv.(*toml.Table)
	oIsT, nIsT = oo && oIsT, no && nIsT
	if oIsT && nIsT {
		for _, k := range unionKeys(ot, nt) {
			a, aok := ot.Get(k)
			b, bok := nt.Get(k)
			d.value(with(seg, k), entry, byIndex, a, aok, b, bok, whole)
		}
		return
	}
	if oo && no && same(ov, nv) {
		return
	}
	c := Change{Place: placeOf(seg, entry), Added: !oo, Removed: !no, seg: seg, entry: entry, byIndex: byIndex, whole: whole || oIsT || nIsT}
	if oo {
		c.Old = ov
	}
	if no {
		c.New = nv
	}
	d.out = append(d.out, c)
	if oIsT {
		for _, k := range ot.Keys() {
			a, _ := ot.Get(k)
			d.value(with(seg, k), entry, byIndex, a, true, nil, false, true)
		}
	}
	if nIsT {
		for _, k := range nt.Keys() {
			b, _ := nt.Get(k)
			d.value(with(seg, k), entry, byIndex, nil, false, b, true, true)
		}
	}
}

// entries diffs a [[key]] array entry by entry: matched by name when every
// entry on both sides has a distinct name, else by position.
func (d *differ) entries(key string, ov toml.Value, oo bool, nv toml.Value, no bool) string {
	oa, oOK := tables(ov, oo)
	na, nOK := tables(nv, no)
	if !oOK || !nOK {
		d.value([]string{key}, false, false, ov, oo, nv, no, false)
		return key + " is not a list of tables; " + key + " cannot be matched by name, so each change needs a tap"
	}
	on, oNamed := names(oa)
	nn, nNamed := names(na)
	if oNamed && nNamed {
		for _, n := range append(slices.Clip(on), notIn(nn, on)...) {
			oi, ni := slices.Index(on, n), slices.Index(nn, n)
			var a, b toml.Value
			if oi >= 0 {
				a = oa[oi]
			}
			if ni >= 0 {
				b = na[ni]
			}
			d.value([]string{key, n}, true, false, a, oi >= 0, b, ni >= 0, false)
		}
		return ""
	}
	for i := 0; i < max(len(oa), len(na)); i++ {
		var a, b toml.Value
		if i < len(oa) {
			a = oa[i]
		}
		if i < len(na) {
			b = na[i]
		}
		d.value([]string{key, fmt.Sprintf("#%d", i+1)}, true, true, a, i < len(oa), b, i < len(na), false)
	}
	return key + " cannot be matched by name (an entry has no name, or two share one); each change needs a tap"
}

// tables is an array of tables; an absent key is an empty one.
func tables(v toml.Value, ok bool) ([]*toml.Table, bool) {
	if !ok {
		return nil, true
	}
	arr, isArr := v.([]toml.Value)
	if !isArr {
		return nil, false
	}
	var out []*toml.Table
	for _, e := range arr {
		t, isT := e.(*toml.Table)
		if !isT {
			return nil, false
		}
		out = append(out, t)
	}
	return out, true
}

// names are the entries' names, and whether each has a distinct non-empty
// string name.
func names(ts []*toml.Table) ([]string, bool) {
	var out []string
	for _, t := range ts {
		v, _ := t.Get("name")
		s, ok := v.(string)
		if !ok || s == "" || slices.Contains(out, s) {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func notIn(xs, ys []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(ys, x) {
			out = append(out, x)
		}
	}
	return out
}

// neverPlaces are the places only the maintainer changes.
// Each also covers everything under it (pattern.* and pattern[*), so a
// sub-table or a whole replaced section is caught too. Fnmatch's * crosses
// ".", which only widens what is refused.
func neverPlaces() []string {
	return []string{
		"github.app_client_id", "github.app_slug", "github.installation_id", "github.key", "github.reviewers",
		"backup", "host", "deploy", "foreman", "board", "build",
		"watches*",
		"gate.mounts", "gate.env",
		"remote*.mounts", "remote*.env", "remote*.build", "remote*.auth",
		"hosts.*.key",
	}
}

// wallPlaces are tap-tier places that touch the factory's own safety; the
// desk flags them. A project's or kind's install, test, lint and toolchain
// are commands the gate runs, so they are walls too.
func wallPlaces() []string {
	return []string{
		"image", "template_flavour", "templates",
		"gate", "github.open_prs", "triage", "findings", "hygiene", "identity",
		"remote*", "hosts.*.ssh", "hosts.*.transport",
		"projects.*.walls", "projects.*.hygiene", "projects.*.self_deploy", "projects.*.tier3_paths",
		"projects.*.text_paths", "projects.*.mode", "projects.*.github",
		"projects.*.install", "projects.*.test", "projects.*.lint",
		"kinds.*.install", "kinds.*.test", "kinds.*.lint", "kinds.*.toolchain",
	}
}

// matches is the place itself or anything under it.
func matches(place, pattern string) bool {
	return textx.Fnmatch(place, pattern) || textx.Fnmatch(place, pattern+".*") || textx.Fnmatch(place, pattern+"[*")
}

func matchesAny(place string, patterns []string) (string, bool) {
	for _, p := range patterns {
		if matches(place, p) {
			return p, true
		}
	}
	return "", false
}

// Rule is one place's tier, whether it touches a wall, and why. The first
// match wins: never, then the auto allowlist, then tap with a wall, then
// tap.
func Rule(c Change, home string) (tier model.Tier, wall bool, reason string) {
	if p, ok := matchesAny(c.Place, neverPlaces()); ok {
		return model.TierNever, false, c.Place + ": only the maintainer changes " + p
	}
	if why, ok := neverValue(c, home); ok {
		return model.TierNever, false, c.Place + ": " + why
	}
	if why, ok := auto(c); ok {
		return model.TierAuto, false, c.Place + ": " + why + "; applies by itself"
	}
	if _, ok := matchesAny(c.Place, wallPlaces()); ok {
		return model.TierTap, true, c.Place + ": touches the factory's walls; a tap"
	}
	return model.TierTap, false, c.Place + ": a tap"
}

// projectKey is the key a place sets in a [projects.<k>] (or, with
// section "kinds", a [kinds.<k>]) table; "" for any other place.
func projectKey(c Change, section string) string {
	if c.entry || len(c.seg) != 3 || c.seg[0] != section {
		return ""
	}
	return c.seg[2]
}

// neverValue refuses the places whose tier hangs on the value: a project
// path that is not the one onboarding gives, and an untracked pin that
// leaves the checkout.
func neverValue(c Change, home string) (string, bool) {
	if projectKey(c, "projects") == "path" {
		switch {
		case c.Removed && c.whole:
			return "", false // the project goes with it
		case c.Removed:
			return "a project's path removed; only the maintainer moves a project", true
		case !c.Added:
			return "a project's path changed; only the maintainer moves a project", true
		}
		want, ok := projectPath(home, c.seg[1])
		if s, isStr := c.New.(string); !ok || !isStr || s != want {
			return "a new project's path must be <factory_home>/projects/<key>", true
		}
		return "", false
	}
	if (projectKey(c, "projects") == "pinned_untracked" || projectKey(c, "kinds") == "pinned_untracked") && !c.Removed {
		arr, _ := c.New.([]toml.Value)
		for _, e := range arr {
			if s, ok := e.(string); ok && (strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.Contains(s, "..")) {
				return fmt.Sprintf("pinned_untracked %q leaves the checkout; only the maintainer adds that", s), true
			}
		}
	}
	return "", false
}

// projectPath is <home>/projects/<key>, for a key that is one plain path
// part.
func projectPath(home, key string) (string, bool) {
	if home == "" || !strings.HasPrefix(home, "/") || key == "" || key == "." || key == ".." || strings.ContainsAny(key, "/\\") {
		return "", false
	}
	return path.Join(home, "projects", key), true
}

// auto is the allowlist: settings that only space out or
// reduce work, never removed keys.
func auto(c Change) (string, bool) {
	if c.Removed {
		return "", false
	}
	if c.entry && !c.byIndex && len(c.seg) == 3 && c.seg[0] == "schedules" && !c.Added {
		switch c.seg[2] {
		case "every_days":
			n, ok := num(c.New)
			if !ok || n <= 0 {
				return "", false
			}
			if n >= 7 {
				return "a schedule runs at most weekly", true
			}
			if o, ok := num(c.Old); ok && n >= o {
				return "a schedule runs less often", true
			}
		case "projects":
			if subset(c.New, c.Old) {
				return "a schedule runs on fewer projects", true
			}
		}
		return "", false
	}
	switch k := projectKey(c, "projects"); k {
	case "clock", "enabled":
		if b, ok := c.New.(bool); ok && !b {
			if k == "clock" {
				return "a project leaves the schedules", true
			}
			return "a project is paused", true
		}
	}
	return "", false
}

// num is a finite TOML number (a bool is not one here).
func num(v toml.Value) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	}
	return 0, false
}

// subset reports a list of strings each in the old list of strings.
func subset(newV, oldV toml.Value) bool {
	na, ok1 := newV.([]toml.Value)
	oa, ok2 := oldV.([]toml.Value)
	if !ok1 || !ok2 {
		return false
	}
	var old []string
	for _, e := range oa {
		s, ok := e.(string)
		if !ok {
			return false
		}
		old = append(old, s)
	}
	for _, e := range na {
		s, ok := e.(string)
		if !ok || !slices.Contains(old, s) {
			return false
		}
	}
	return true
}
