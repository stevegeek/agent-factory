package ops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type pyRegex struct {
	Patterns []struct {
		Name  string `json:"name"`
		Cases []struct {
			In  string          `json:"in"`
			Out json.RawMessage `json:"out"`
		} `json:"cases"`
	} `json:"patterns"`
}

// readPyRegex is the regex golden tools/goldens.py wrote from Python: each
// pattern's inputs and what its op returned on them, null for no match.
func readPyRegex(t *testing.T) pyRegex {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "textx", "testdata", "py", "regex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g pyRegex
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// SCION_NOISE, written by hand, against Python's re.match over the regex
// corpus (non-ASCII digits included).
func TestScionNoiseMatchesPython(t *testing.T) {
	n := 0
	for _, p := range readPyRegex(t).Patterns {
		if p.Name != "scion_noise" {
			continue
		}
		for _, c := range p.Cases {
			n++
			want := string(c.Out) != "null"
			if got := scionNoise(c.In); got != want {
				t.Errorf("%q: %v, want %v", c.In, got, want)
			}
		}
	}
	if n == 0 {
		t.Fatal("no scion_noise cases")
	}
}

// MOVE_DETAIL against Python's fullmatch with re.S, groups included.
func TestMoveDetailMatchesPython(t *testing.T) {
	n := 0
	for _, p := range readPyRegex(t).Patterns {
		if p.Name != "move_detail" {
			continue
		}
		for _, c := range p.Cases {
			n++
			var want []*string
			if err := json.Unmarshal(c.Out, &want); err != nil {
				t.Fatal(err)
			}
			m := moveDetail.FindStringSubmatchIndex(c.In)
			if (m == nil) != (want == nil) {
				t.Errorf("%q: match %v, want %v", c.In, m != nil, want != nil)
				continue
			}
			for i, w := range want {
				var got *string
				if m[2*i] >= 0 {
					s := c.In[m[2*i]:m[2*i+1]]
					got = &s
				}
				if (got == nil) != (w == nil) || got != nil && *got != *w {
					t.Errorf("%q group %d: %v, want %v", c.In, i, got, w)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no move_detail cases")
	}
}

// Python's errors="replace": one U+FFFD per maximal ill-formed subpart
// (the expectations are Python 3's own decodes).
func TestDecodeReplace(t *testing.T) {
	for in, want := range map[string]string{
		"ok":               "ok",
		"a\xe2\x82b":       "a�b",
		"\xff\xfe":         "��",
		"\xed\xa0\x80":     "���",
		"\xf0\x9f\x98":     "�",
		"\xf0\x9f\x98\x80": "\U0001F600",
		"\xc0\xaf":         "��",
		"\xe0\x80\x80":     "���",
	} {
		if got := decodeReplace([]byte(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
