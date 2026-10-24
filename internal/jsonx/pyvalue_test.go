package jsonx

import (
	"strings"
	"testing"
)

// pyCases are JSON documents whose str(), repr() and bool() Python gives.
// "~u" stands for a JSON \u escape (so surrogates can be written).
func pyCases() []string {
	return []string{
		`null`, `true`, `false`, `0`, `-0`, `1`, `1.0`, `1e5`, `2.5e-7`, `1e400`, `-1e400`, `-1.5`, `12345678901234567890`, `0.0`,
		`""`, `"plain"`, `"it's"`, `"say \"hi\""`, `"both '\""`, `"tab\t nl\n cr\r bs\\"`,
		`"~u00e9 ~u00a0 ~u200b ~u0007 ~u007f ~u0085 ~ud83d~ude00 ~u2028 ~u00ad ~ue000 ~ud83f~udffe"`,
		`"~ud800 lone"`, `"日本語"`,
		`[]`, `{}`, `[0]`, `[1, "a", null, [true, {}]]`,
		`{"b": 1, "a": [1.5, "x"], "c": {"d": null}}`,
		`{"a": 1, "b": 2, "a": 3}`,
		`[{"message": "Could not resolve", "type": "NOT_FOUND", "path": ["x"], "locations": [{"line": 1, "column": 2}]}]`,
	}
}

func esc(s string) string { return strings.ReplaceAll(s, "~u", `\u`) }

// Known answers, printed by python3 (3.14) from the same JSON.
func TestPyStrReprTruthyKnownAnswers(t *testing.T) {
	cases := []struct{ json, str, repr string }{
		{`[{"message": "Could not resolve", "type": "NOT_FOUND", "path": ["x"], "locations": [{"line": 1, "column": 2}]}]`,
			`[{'message': 'Could not resolve', 'type': 'NOT_FOUND', 'path': ['x'], 'locations': [{'line': 1, 'column': 2}]}]`, ""},
		{`["it's", "say \"hi\"", "both '\"", "tab\t nl\n cr\r bs\\", "~u00e9 ~u00a0 ~u200b ~u0007 ~u007f ~u0085 ~ud83d~ude00 ~u2028 ~u00ad ~ue000 ~ud83f~udffe"]`,
			`["it's", 'say "hi"', 'both \'"', 'tab\t nl\n cr\r bs\\', 'é \xa0 ~u200b \x07 \x7f \x85 😀 ~u2028 \xad ~ue000 \U0001fffe']`, ""},
		{`{"a": true, "b": false, "c": null, "d": 1.0, "e": 1e5, "f": -0, "g": 12345678901234567890, "h": 2.5e-7, "i": {}, "j": [], "k": 1e400, "l": -1.5}`,
			`{'a': True, 'b': False, 'c': None, 'd': 1.0, 'e': 100000.0, 'f': 0, 'g': 12345678901234567890, 'h': 2.5e-07, 'i': {}, 'j': [], 'k': inf, 'l': -1.5}`, ""},
		{`"plain"`, `plain`, `'plain'`},
	}
	for _, c := range cases {
		v, err := Parse([]byte(esc(c.json)))
		if err != nil {
			t.Fatalf("parse %s: %v", c.json, err)
		}
		if got := PyStr(v); got != esc(c.str) {
			t.Errorf("PyStr(%s)\n got %s\nwant %s", c.json, got, c.str)
		}
		if c.repr != "" && PyRepr(v) != c.repr {
			t.Errorf("PyRepr(%s) = %s", c.json, PyRepr(v))
		}
	}
	for doc, want := range map[string]bool{`null`: false, `0`: false, `0.0`: false, `""`: false, `[]`: false, `{}`: false,
		`false`: false, `true`: true, `-1`: true, `" "`: true, `[0]`: true, `{"a": null}`: true} {
		v, _ := Parse([]byte(doc))
		if Truthy(v) != want {
			t.Errorf("Truthy(%s) = %v", doc, !want)
		}
	}
}
