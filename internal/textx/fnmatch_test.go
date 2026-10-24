package textx

import (
	"slices"
	"testing"
)

// fnmatch.json: fnmatch.fnmatch(name, pattern) as classify() and gate() use
// it (POSIX: case-sensitive, * crosses /). For each pattern, the names that
// match.
func TestFnmatchMatchesPython(t *testing.T) {
	var g struct {
		Names []string `json:"names"`
		Rows  []struct {
			Pattern string   `json:"pattern"`
			Matches []string `json:"matches"`
		} `json:"rows"`
	}
	loadJSON(t, "fnmatch.json", &g)
	if len(g.Rows) < 20 || len(g.Names) < 40 {
		t.Fatalf("fnmatch.json is too small: %d rows, %d names", len(g.Rows), len(g.Names))
	}
	for _, r := range g.Rows {
		var got []string
		for _, n := range g.Names {
			if Fnmatch(n, r.Pattern) {
				got = append(got, n)
			}
		}
		if !slices.Equal(got, r.Matches) && !(len(got) == 0 && len(r.Matches) == 0) {
			t.Errorf("Fnmatch(*, %q) matched %q, Python %q", r.Pattern, got, r.Matches)
		}
	}
}

// Cases from fnmatch.translate's set handling, checked with Python 3.14.
func TestFnmatchSets(t *testing.T) {
	for _, c := range []struct {
		name, pat string
		want      bool
	}{
		{"b", "[a-c]", true},
		{"-", "[a-]", true},
		{"-", "[-a]", true},
		{"b", "[z-a]", false}, // an empty range never matches
		{"q", "[!z-a]", true}, // its negation matches any one character
		{"\n", "[!z-a]", true},
		{"-", "[a-b-c]", true}, // a range, then a literal hyphen
		{"c", "[a-b-c]", true},
		{"x", "[c-a-x]", true},
		{"-", "[c-a-x]", true},
		{"b", "[c-a-x]", false},
		{"\\", "[\\]", true},
		{"]", "[]]", true},
		{"a", "[!]]", true},
		{"]", "[!]]", false},
		{"[", "[", true},
		{"[a", "[a", true},
		{"^", "[^]", true},
		{"&", "[&&]", true},
		{"a/b/c.rb", "*.rb", true},
		{"a\nb", "a?b", true},
		{"a\nb", "a*", true},
		{"", "*", true},
		{"", "?", false},
		{"é", "?", true},
		{"😀.md", "?.md", true},
		{"abc", "a**c", true},
		{"aXbXc", "a*b*c", true},
		{"abd", "a*c", false},
		{"Ab", "a*", false},
	} {
		if got := Fnmatch(c.name, c.pat); got != c.want {
			t.Errorf("Fnmatch(%q, %q) = %v, want %v", c.name, c.pat, got, c.want)
		}
	}
}
