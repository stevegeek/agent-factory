package textx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"unicode/utf8"
)

// The text golden is written by tools/goldens.py from the Python helpers in
// bin/factory: clip, sentence, " ".join(s.split()), one_line, str.lower,
// slugify, str.splitlines, len and the UTF-8 length.
type textRow struct {
	In         string            `json:"in"`
	Clip       map[string]string `json:"clip"`
	Sentence   map[string]string `json:"sentence"`
	Fold       *string           `json:"fold"`
	OneLine    *string           `json:"one_line"`
	Lower      *string           `json:"lower"`
	Slugify    *string           `json:"slugify"`
	SplitLines []string          `json:"splitlines"`
	Len        *int              `json:"len"`
	UTF8Len    *int              `json:"utf8_len"`
}

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "py", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func textRows(t *testing.T) []textRow {
	var g struct {
		ClipDefault int       `json:"clip_default_n"`
		Rows        []textRow `json:"rows"`
	}
	loadJSON(t, "text.json", &g)
	if g.ClipDefault != ClipDefault {
		t.Fatalf("clip default: golden %d, ClipDefault %d", g.ClipDefault, ClipDefault)
	}
	if len(g.Rows) < 40 {
		t.Fatalf("only %d rows in text.json", len(g.Rows))
	}
	return g.Rows
}

func TestClipMatchesPython(t *testing.T) {
	for _, r := range textRows(t) {
		for ns, want := range r.Clip {
			n, _ := strconv.Atoi(ns)
			if got := Clip(r.In, n); got != want {
				t.Errorf("Clip(%q, %d) = %q, want %q", r.In, n, got, want)
			}
		}
	}
}

func TestSentenceMatchesPython(t *testing.T) {
	for _, r := range textRows(t) {
		for ns, want := range r.Sentence {
			n, _ := strconv.Atoi(ns)
			if got := Sentence(r.In, n); got != want {
				t.Errorf("Sentence(%q, %d) = %q, want %q", r.In, n, got, want)
			}
		}
	}
}

func TestFoldAndOneLineMatchPython(t *testing.T) {
	for _, r := range textRows(t) {
		if r.Fold != nil {
			if got := Fold(r.In); got != *r.Fold {
				t.Errorf("Fold(%q) = %q, want %q", r.In, got, *r.Fold)
			}
		}
		if r.OneLine != nil {
			if got := OneLine(r.In); got != *r.OneLine {
				t.Errorf("OneLine(%q) = %q, want %q", r.In, got, *r.OneLine)
			}
		}
	}
}

func TestLowerAndSlugifyMatchPython(t *testing.T) {
	for _, r := range textRows(t) {
		if r.Lower != nil {
			if got := Lower(r.In); got != *r.Lower {
				t.Errorf("Lower(%q) = %q, want %q", r.In, got, *r.Lower)
			}
		}
		if r.Slugify != nil {
			if got := Slugify(r.In); got != *r.Slugify {
				t.Errorf("Slugify(%q) = %q, want %q", r.In, got, *r.Slugify)
			}
		}
	}
}

func TestSplitLinesAndLengthsMatchPython(t *testing.T) {
	for _, r := range textRows(t) {
		if r.Len == nil {
			continue
		}
		if got := SplitLines(r.In); !slices.Equal(got, r.SplitLines) && !(len(got) == 0 && len(r.SplitLines) == 0) {
			t.Errorf("SplitLines(%q) = %q, want %q", r.In, got, r.SplitLines)
		}
		if got := RuneLen(r.In); got != *r.Len {
			t.Errorf("RuneLen(%q) = %d, want %d", r.In, got, *r.Len)
		}
		if got := len(r.In); got != *r.UTF8Len {
			t.Errorf("len(%q) = %d, want UTF-8 length %d", r.In, got, *r.UTF8Len)
		}
	}
}

func TestClipEdges(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 0, "ab…"}, // Python text[:-1]: n - 1 = -1 drops the last code point
		{"abc", -1, "a…"},
		{"abcdef", -10, "…"},
		{"", 0, ""}, // len("") <= 0 keeps it
		{"ab", 2, "ab"},
		{"a b c", 4, "a b…"},
		{"a  b c", 3, "a…"}, // folded first, then rstripped before the ellipsis
		{"\xff\xfe x", 10, "\xff\xfe x"},
	} {
		if got := Clip(c.in, c.n); got != c.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	if got := ClipDefaultN("x"); got != "x" {
		t.Errorf("ClipDefaultN = %q", got)
	}
}

func TestSentenceSplitMatchesPythonRegexSplit(t *testing.T) {
	// The cases of the sentence_split entry in regex.json: re.split(r"(?<=[.?!])\s+", s).
	for _, p := range regexGolden(t).Patterns {
		if p.Name != "sentence_split" {
			continue
		}
		for _, c := range p.Cases {
			var want []string
			if err := json.Unmarshal(c.Out, &want); err != nil {
				t.Fatal(err)
			}
			if got := SentenceSplit(c.In); !slices.Equal(got, want) {
				t.Errorf("SentenceSplit(%q) = %q, want %q", c.In, got, want)
			}
		}
		return
	}
	t.Fatal("no sentence_split pattern in regex.json")
}

func TestStripUsesPythonWhitespace(t *testing.T) {
	for _, c := range []struct{ in, strip, lstrip, rstrip string }{
		{"  a b  ", "a b", "a b  ", "  a b"},
		{"\x1c\x1fa\u0085\u2028", "a", "a\u0085\u2028", "\x1c\x1fa"},
		{"\u200ba\u200b", "\u200ba\u200b", "\u200ba\u200b", "\u200ba\u200b"}, // ZWSP is not whitespace in Python
		{"\u3000\u00a0x\t", "x", "x\t", "\u3000\u00a0x"},
		{"", "", "", ""},
	} {
		if got := Strip(c.in); got != c.strip {
			t.Errorf("Strip(%q) = %q, want %q", c.in, got, c.strip)
		}
		if got := LStrip(c.in); got != c.lstrip {
			t.Errorf("LStrip(%q) = %q, want %q", c.in, got, c.lstrip)
		}
		if got := RStrip(c.in); got != c.rstrip {
			t.Errorf("RStrip(%q) = %q, want %q", c.in, got, c.rstrip)
		}
	}
}

func TestHasLineBreakIsMarkdownLineBreak(t *testing.T) {
	// LINE_BREAK in bin/factory: [\r\n\x0b\x0c\x85\u2028\u2029]; \x1c-\x1e are
	// str.splitlines breaks but not Markdown ones.
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"one line", false}, {"a\rb", true}, {"a\nb", true}, {"a\vb", true}, {"a\fb", true},
		{"a\u0085b", true}, {"a\u2028b", true}, {"a\u2029b", true}, {"a\x1cb", false}, {"a\tb", false}, {"", false},
	} {
		if got := HasLineBreak(c.in); got != c.want {
			t.Errorf("HasLineBreak(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRuneLenCountsInvalidBytesOneEach(t *testing.T) {
	if got := RuneLen("a\xffb"); got != utf8.RuneCountInString("a\xffb") || got != 3 {
		t.Errorf("RuneLen = %d", got)
	}
}

func TestLowerFinalSigma(t *testing.T) {
	// Python str.lower applies the Final_Sigma rule of SpecialCasing.txt.
	for _, c := range []struct{ in, want string }{
		{"ΣΑΣ", "σας"},
		{"Σ", "σ"},
		{"ΑΣ.", "ας."},
		{"ΑΣ'Β", "ασ'β"}, // ' is case-ignorable, then a cased letter follows: not final
		{"ΑΣ'", "ας'"},   // case-ignorable tail, then the end: final
		{"Α'Σ", "α'ς"},   // case-ignorable before: still preceded by a cased letter
		{"1Σ", "1σ"},     // a digit is not cased
		{"ΑΣ1", "ας1"},   // a digit after is not cased, so final
		{"ΑΣ́", "ας́"},   // a combining mark is case-ignorable
		{"İ", "i̇"},
		{"ǅ", "ǆ"},
		{"K", "k"},
	} {
		if got := Lower(c.in); got != c.want {
			t.Errorf("Lower(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
