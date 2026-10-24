package github

import (
	"testing"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
)

func fixedNow() clock.Clock { return clock.Fixed{T: testNow} }

// pyStr is str(json.loads(s)) in Python: set_pr_draft puts str(r["errors"])
// in its error, and so in the ticket's history. The wants were printed by
// python3 (3.14) from the same JSON.
func TestPyStrIsPythonsStrOfParsedJSON(t *testing.T) {
	cases := []struct{ json, want string }{
		{`[{"message": "Could not resolve", "type": "NOT_FOUND", "path": ["x"], "locations": [{"line": 1, "column": 2}]}]`,
			`[{'message': 'Could not resolve', 'type': 'NOT_FOUND', 'path': ['x'], 'locations': [{'line': 1, 'column': 2}]}]`},
		{`["it's", "say \"hi\"", "both '\"", "tab\t nl\n cr\r bs\\", "~u00e9 ~u00a0 ~u200b ~u0007 ~u007f ~u0085 ~ud83d~ude00 ~u2028 ~u00ad ~ue000 ~ud83f~udffe"]`,
			`["it's", 'say "hi"', 'both \'"', 'tab\t nl\n cr\r bs\\', 'é \xa0 ~u200b \x07 \x7f \x85 😀 ~u2028 \xad ~ue000 ~U0001fffe']`},
		{`{"a": true, "b": false, "c": null, "d": 1.0, "e": 1e5, "f": -0, "g": 12345678901234567890, "h": 2.5e-7, "i": {}, "j": [], "k": 1e400, "l": -1.5}`,
			`{'a': True, 'b': False, 'c': None, 'd': 1.0, 'e': 100000.0, 'f': 0, 'g': 12345678901234567890, 'h': 2.5e-07, 'i': {}, 'j': [], 'k': inf, 'l': -1.5}`},
		{`"plain"`, `plain`},
	}
	for _, tc := range cases {
		tc.json, tc.want = esc(tc.json), esc(tc.want)
		v, err := jsonx.Parse([]byte(tc.json))
		if err != nil {
			t.Fatalf("parse %s: %v", tc.json, err)
		}
		if got := jsonx.PyStr(v); got != tc.want {
			t.Errorf("jsonx.PyStr(%s)\n got %s\nwant %s", tc.json, got, tc.want)
		}
	}
}
