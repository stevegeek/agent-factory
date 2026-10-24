package textx

import (
	"regexp"
	"strconv"
	"testing"
	"unicode"
)

// classes.json holds Python's re \w, \d, \s (str patterns) and
// str.isspace/isalnum/isdigit as inclusive code point ranges, plus every
// code point str.lower changes, from the Python that wrote the goldens.
type classesGolden struct {
	Python  string                `json:"python"`
	Unicode string                `json:"unicode"`
	Classes map[string][][2]int32 `json:"classes"`
	Lower   [][2]any              `json:"lower"`
}

func loadClasses(t *testing.T) classesGolden {
	var g classesGolden
	loadJSON(t, "classes.json", &g)
	for _, name := range []string{"word", "digit", "space", "isspace", "isalnum", "isdigit"} {
		if len(g.Classes[name]) == 0 {
			t.Fatalf("classes.json has no %s ranges", name)
		}
	}
	return g
}

func inRanges(ranges [][2]int32) func(rune) bool {
	return func(r rune) bool {
		lo, hi := 0, len(ranges)
		for lo < hi {
			m := (lo + hi) / 2
			switch {
			case r < ranges[m][0]:
				hi = m
			case r > ranges[m][1]:
				lo = m + 1
			default:
				return true
			}
		}
		return false
	}
}

// versions tells apart the differences that come from the two Unicode
// versions (Go's tables and the Python that wrote the goldens) from real
// ones. A code point one side does not assign yet has no properties there:
// in no class, and lowered to itself. Such a difference is allowed and
// counted; any other fails.
type versions struct {
	pyBlank func(rune) bool // Python treats r like an unassigned code point
}

func newVersions(g classesGolden) versions {
	word, space, isdigit := inRanges(g.Classes["word"]), inRanges(g.Classes["space"]), inRanges(g.Classes["isdigit"])
	lowered := map[rune]bool{}
	for _, row := range g.Lower {
		if cp, ok := row[0].(float64); ok {
			lowered[rune(cp)] = true
		}
	}
	return versions{pyBlank: func(r rune) bool {
		return !word(r) && !space(r) && !isdigit(r) && !lowered[r]
	}}
}

func unassignedInGo(r rune) bool {
	for _, tab := range unicode.Categories {
		if unicode.Is(tab, r) {
			return false
		}
	}
	return true
}

// allowed reports a difference at r that only the Unicode versions explain.
func (v versions) allowed(r rune) bool { return v.pyBlank(r) || unassignedInGo(r) }

// maxVersionSkew bounds the allowed differences, so that a wrong class
// cannot hide behind them (Unicode 16 to 17 adds a few thousand letters).
const maxVersionSkew = 6000

func checkClass(t *testing.T, v versions, name string, want, got func(rune) bool) {
	t.Helper()
	var bad []string
	skipped := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogates cannot occur in a Go string
		}
		if want(r) == got(r) {
			continue
		}
		if v.allowed(r) {
			skipped++
			continue
		}
		if len(bad) < 20 {
			bad = append(bad, strconv.QuoteRuneToASCII(r))
		}
	}
	if len(bad) > 0 {
		t.Errorf("%s differs from Python at %v", name, bad)
	}
	if skipped > maxVersionSkew {
		t.Errorf("%s: %d differences put down to Unicode versions, more than %d", name, skipped, maxVersionSkew)
	}
	if skipped > 0 {
		t.Logf("%s: %d code points assigned in only one Unicode version", name, skipped)
	}
}

func TestPredicatesMatchPythonClasses(t *testing.T) {
	g := loadClasses(t)
	v := newVersions(g)
	t.Logf("golden from Python %s, Unicode %s; Go Unicode %s", g.Python, g.Unicode, unicode.Version)
	checkClass(t, v, "IsWord", inRanges(g.Classes["word"]), IsWord)
	checkClass(t, v, "IsDecimal", inRanges(g.Classes["digit"]), IsDecimal)
	checkClass(t, v, "IsSpace", inRanges(g.Classes["space"]), IsSpace)
	checkClass(t, v, "IsSpace (str.isspace)", inRanges(g.Classes["isspace"]), IsSpace)
	checkClass(t, v, "IsAlnum", inRanges(g.Classes["isalnum"]), IsAlnum)
	checkClass(t, v, "IsDigit", inRanges(g.Classes["isdigit"]), IsDigit)
}

func TestRegexClassesMatchPythonClasses(t *testing.T) {
	g := loadClasses(t)
	v := newVersions(g)
	not := func(f func(rune) bool) func(rune) bool { return func(r rune) bool { return !f(r) } }
	word, digit, space := inRanges(g.Classes["word"]), inRanges(g.Classes["digit"]), inRanges(g.Classes["space"])
	for _, c := range []struct {
		name, class string
		want        func(rune) bool
	}{
		{"PyWord", PyWord, word},
		{"PyDigit", PyDigit, digit},
		{"PySpace", PySpace, space},
		{"PyNotWord", PyNotWord, not(word)},
		{"PyNotDigit", PyNotDigit, not(digit)},
		{"PyNotSpace", PyNotSpace, not(space)},
		{"[PyWordSet]", "[" + PyWordSet + "]", word},
		{"[^PySpaceSet<>]", "[^" + PySpaceSet + "<>]", func(r rune) bool { return !space(r) && r != '<' && r != '>' }},
	} {
		re := regexp.MustCompile(`\A` + c.class + `\z`)
		checkClass(t, v, c.name, c.want, func(r rune) bool { return re.MatchString(string(r)) })
	}
}

func TestLowerMatchesPythonForEveryCodePoint(t *testing.T) {
	g := loadClasses(t)
	v := newVersions(g)
	changed := map[rune]string{}
	for _, row := range g.Lower {
		cp, ok1 := row[0].(float64)
		s, ok2 := row[1].(string)
		if !ok1 || !ok2 {
			t.Fatalf("bad lower row %v", row)
		}
		changed[rune(cp)] = s
	}
	var bad []string
	skipped := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		want, ok := changed[r]
		if !ok {
			want = string(r)
		}
		if got := Lower(string(r)); got != want {
			if v.allowed(r) {
				skipped++
				continue
			}
			if len(bad) < 20 {
				bad = append(bad, strconv.QuoteRuneToASCII(r)+"->"+strconv.QuoteToASCII(got)+" want "+strconv.QuoteToASCII(want))
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("Lower differs from str.lower: %v", bad)
	}
	if skipped > 0 {
		t.Logf("Lower: %d code points assigned in only one Unicode version", skipped)
	}
}

func TestIsWordBoundary(t *testing.T) {
	for _, c := range []struct {
		s    string
		i    int
		want bool
	}{
		{"", 0, false},
		{"a", 0, true},
		{"a", 1, true},
		{"ab", 1, false},
		{"a b", 1, true},
		{"\xc3\xa9a", 2, false}, // e-acute is a word character in Python
		{"x.y", 1, true},
		{"x.y", 2, true},
		{"..", 1, false},
		{"\xef\xbc\x93s", 3, false}, // a full-width digit 3 is a word character
		{"a\xcc\x81", 1, true},      // a combining acute is not
	} {
		if got := IsWordBoundary(c.s, c.i); got != c.want {
			t.Errorf("IsWordBoundary(%q, %d) = %v, want %v", c.s, c.i, got, c.want)
		}
	}
}

func TestIsDigits(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"", false}, {"12", true}, {"1a", false}, {"\xc2\xb2", true}, {"\xd9\xa3", true}, {"-1", false}, {"\xc2\xbd", false},
	} {
		if got := IsDigits(c.s); got != c.want {
			t.Errorf("IsDigits(%q) = %v", c.s, got)
		}
	}
}
