package textx

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Python 3 re on str patterns is Unicode-aware; RE2 is ASCII by default.
// Every pattern ported from Python follows this checklist (ARCHITECTURE.md
// §8.5), and carries a "// py: <original pattern>" comment so that archtest
// finds its golden in testdata/py/regex.json. regex_test.go applies the
// checklist to every pattern the Python code uses; the owning slices copy
// those translations.
//
//	Python                      Go (RE2)
//	\w                          PyWord (letters and numbers, not marks), \W PyNotWord
//	\d                          PyDigit (Unicode Nd), \D PyNotDigit
//	\s                          PySpace (incl. \x1c-\x1f, NEL, NBSP), \S PyNotSpace
//	[...\s...], [^...\s...]     the *Set forms inside a class: "[^" + PySpaceSet + "<>]"
//	\b                          no Unicode \b in RE2: IsWordBoundary; for a leading \b,
//	                            SearchAtBoundary
//	$ (no re.M), re.fullmatch   \z
//	$ (no re.M), match/search   \z, and AtEnd: Python's $ also matches before a final "\n"
//	re.match                    a leading ^ (or a check that the match starts at 0)
//	re.fullmatch                \A(?:...)\z
//	re.I                        (?i) plus FoldI: Python also folds ASCII i with İ and ı
//	re.S                        (?s)
//	(?=x) after the match       an extra group for x, removed by Lookahead
//	(?<=...), backreferences    hand-written code (SentenceSplit)
//	re.sub templates            Sub (\1, \g<name>, escapes), not ReplaceAllString's $1
//	re.split with groups        Split
//
// Within a character class, Python's re.I also matches İ (whose simple
// lowercase is i) for a range that holds i; FoldI does not rewrite classes,
// so a ported re.I pattern must not rely on a class containing i.

// The classes, as the inside of a bracket expression (to combine) and as a
// whole one.
const (
	// PyWordSet is Python's \w: str.isalnum() (L, Nd, Nl, No) or "_".
	PyWordSet = `\p{L}\p{Nd}\p{Nl}\p{No}_`
	// PyDigitSet is Python's \d: the Unicode decimal digits.
	PyDigitSet = `\p{Nd}`
	// PySpaceSet is Python's \s and str.isspace(), listed rather than
	// \p{Z} so that it does not move with Go's Unicode version.
	PySpaceSet = `\t-\r\x{1c}-\x{20}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

	PyWord     = `[` + PyWordSet + `]`
	PyNotWord  = `[^` + PyWordSet + `]`
	PyDigit    = PyDigitSet
	PyNotDigit = `\P{Nd}`
	PySpace    = `[` + PySpaceSet + `]`
	PyNotSpace = `[^` + PySpaceSet + `]`
)

// SearchAtBoundary is re.search for a Python pattern that starts with \b:
// re is the rest of the pattern compiled behind \A (`\A(?:rest)`), and it
// is tried at each code point position of s in order where IsWordBoundary
// holds. The result is FindStringSubmatchIndex's, with offsets into s.
func SearchAtBoundary(re *regexp.Regexp, s string) []int {
	for i := 0; i <= len(s); {
		if IsWordBoundary(s, i) {
			if loc := re.FindStringSubmatchIndex(s[i:]); loc != nil {
				for k := range loc {
					if loc[k] >= 0 {
						loc[k] += i
					}
				}
				return loc
			}
		}
		if i == len(s) {
			break
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return nil
}

// AtEnd is re.match or re.search for a Python pattern ending in $ without
// re.M: re is compiled with \z for the $. Python's $ also matches just
// before a final "\n", so when re finds nothing in s, AtEnd tries s without
// that newline. (A match that needed the newline itself is found by the
// first try.)
func AtEnd(re *regexp.Regexp, s string) []int {
	if loc := re.FindStringSubmatchIndex(s); loc != nil {
		return loc
	}
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		return re.FindStringSubmatchIndex(t)
	}
	return nil
}

// Lookahead turns a match that consumed a lookahead into Python's: group g
// stands for a lookahead at the end of the match (Python's \b or (?=x)
// written as "(x)|\z"). When it took part, every group ending past its start
// is cut back to that start; group g itself is removed from the result.
func Lookahead(loc []int, g int) []int {
	if loc == nil {
		return nil
	}
	cut := loc[2*g]
	out := make([]int, 0, len(loc)-2)
	for i := 0; i < len(loc)/2; i++ {
		if i == g {
			continue
		}
		start, end := loc[2*i], loc[2*i+1]
		if cut >= 0 && end > cut {
			end = cut
		}
		out = append(out, start, end)
	}
	return out
}

// FoldI rewrites a pattern meant for (?i) so that it folds case as Python's
// re.I does for ASCII patterns: Python also matches U+0130 (İ, whose simple
// lowercase is i) and U+0131 (ı) for an i, which RE2's case folding does
// not. Every literal i or I outside a bracket expression, an escape and a
// (?flags) group becomes [iIİı]. The other ASCII letters fold the same in
// both (k with the Kelvin sign, s with ſ).
func FoldI(pattern string) string {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '\\' && i+1 < len(pattern):
			// An escape: copy it whole, including \p{..}, \x{..} and \Q..\E.
			j := i + 2
			switch pattern[i+1] {
			case 'p', 'P', 'x':
				if j < len(pattern) && pattern[j] == '{' {
					if k := strings.IndexByte(pattern[j:], '}'); k >= 0 {
						j += k + 1
					}
				}
			case 'Q':
				if k := strings.Index(pattern[j:], `\E`); k >= 0 {
					j += k + 2
				} else {
					j = len(pattern)
				}
			}
			b.WriteString(pattern[i:j])
			i = j - 1
		case inClass:
			if c == ']' {
				inClass = false
			}
			b.WriteByte(c)
		case c == '[':
			inClass = true
			b.WriteByte(c)
			// A ] right after [ or [^ is a literal member.
			for _, lead := range []string{"^]", "]", "^"} {
				if strings.HasPrefix(pattern[i+1:], lead) {
					b.WriteString(lead)
					i += len(lead)
					break
				}
			}
		case c == '(' && strings.HasPrefix(pattern[i:], "(?"):
			// A flag group or a named group: copy up to its ':' or ')'.
			j := i + 2
			if strings.HasPrefix(pattern[j:], "P<") {
				if k := strings.IndexByte(pattern[j:], '>'); k >= 0 {
					j += k + 1
				}
			} else {
				for j < len(pattern) && pattern[j] != ':' && pattern[j] != ')' {
					j++
				}
			}
			b.WriteString(pattern[i:j])
			i = j - 1
		case c == 'i' || c == 'I':
			b.WriteString("[iIİı]")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Split is re.split(pattern, s): the pieces between matches, and after each
// match the text of every group in it ("" for a group that did not take
// part, where Python gives None).
func Split(re *regexp.Regexp, s string) []string {
	var out []string
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, s[last:loc[0]])
		for g := 1; g < len(loc)/2; g++ {
			if loc[2*g] >= 0 {
				out = append(out, s[loc[2*g]:loc[2*g+1]])
			} else {
				out = append(out, "")
			}
		}
		last = loc[1]
	}
	return append(out, s[last:])
}

// ErrTemplate is returned for a re.sub template Python refuses to compile.
var ErrTemplate = errors.New("bad re.sub template")

// Sub is re.sub(pattern, repl, s) with a Python replacement template: \1 to
// \99 and \g<n> or \g<name> for groups (empty when the group did not take
// part), \g<0> for the whole match, \a \b \f \n \r \t \v \\ and octal \0,
// \0oo and \ooo escapes; a backslash before any other non-letter stays as
// written. A template Python would refuse (an unknown group, a bad escape)
// is an error wrapping ErrTemplate. Empty matches follow Go's rule (one
// right after a match is skipped), which only differs from Python's for
// patterns that can match the empty string.
func Sub(re *regexp.Regexp, s, repl string) (string, error) {
	parts, err := parseTemplate(re, repl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:loc[0]])
		for _, p := range parts {
			if p.group < 0 {
				b.WriteString(p.text)
			} else if loc[2*p.group] >= 0 {
				b.WriteString(s[loc[2*p.group]:loc[2*p.group+1]])
			}
		}
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

type templatePart struct {
	text  string
	group int // -1: literal text
}

func parseTemplate(re *regexp.Regexp, repl string) ([]templatePart, error) {
	var parts []templatePart
	var lit strings.Builder
	group := func(n int, at int) error {
		if n > re.NumSubexp() {
			return fmt.Errorf("%w: invalid group reference %d at position %d", ErrTemplate, n, at)
		}
		if lit.Len() > 0 {
			parts = append(parts, templatePart{text: lit.String(), group: -1})
			lit.Reset()
		}
		parts = append(parts, templatePart{group: n})
		return nil
	}
	isOct := func(c byte) bool { return c >= '0' && c <= '7' }
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		if c != '\\' {
			lit.WriteByte(c)
			continue
		}
		if i+1 >= len(repl) {
			return nil, fmt.Errorf("%w: bad escape (end of pattern) at position %d", ErrTemplate, i)
		}
		at := i
		i++
		c = repl[i]
		switch {
		case c == 'g':
			if i+1 >= len(repl) || repl[i+1] != '<' {
				return nil, fmt.Errorf("%w: missing < at position %d", ErrTemplate, i+1)
			}
			end := strings.IndexByte(repl[i+2:], '>')
			if end < 0 {
				return nil, fmt.Errorf("%w: missing >, unterminated name at position %d", ErrTemplate, i+2)
			}
			name := repl[i+2 : i+2+end]
			i += 2 + end
			n, err := strconv.Atoi(name)
			if err != nil || n < 0 || strings.HasPrefix(name, "+") {
				n = re.SubexpIndex(name)
				if name == "" || n < 0 {
					return nil, fmt.Errorf("%w: unknown group name %q", ErrTemplate, name)
				}
			}
			if err := group(n, at); err != nil {
				return nil, err
			}
		case c == '0':
			v := 0
			for k := 0; k < 2 && i+1 < len(repl) && isOct(repl[i+1]); k++ {
				i++
				v = v*8 + int(repl[i]-'0')
			}
			lit.WriteRune(rune(v))
		case isDigit(c):
			if i+1 < len(repl) && isDigit(repl[i+1]) {
				if isOct(c) && isOct(repl[i+1]) && i+2 < len(repl) && isOct(repl[i+2]) {
					v := int(c-'0')*64 + int(repl[i+1]-'0')*8 + int(repl[i+2]-'0')
					if v > 0o377 {
						return nil, fmt.Errorf("%w: octal escape value \\%s outside of range 0-0o377", ErrTemplate, repl[i:i+3])
					}
					lit.WriteRune(rune(v))
					i += 2
					continue
				}
				n := int(c-'0')*10 + int(repl[i+1]-'0')
				i++
				if err := group(n, at); err != nil {
					return nil, err
				}
				continue
			}
			if err := group(int(c-'0'), at); err != nil {
				return nil, err
			}
		default:
			if e, ok := templateEscape(c); ok {
				lit.WriteString(e)
			} else if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
				return nil, fmt.Errorf("%w: bad escape \\%c at position %d", ErrTemplate, c, at)
			} else {
				lit.WriteByte('\\')
				lit.WriteByte(c)
			}
		}
	}
	if lit.Len() > 0 {
		parts = append(parts, templatePart{text: lit.String(), group: -1})
	}
	return parts, nil
}

func templateEscape(c byte) (string, bool) {
	switch c {
	case 'a':
		return "\a", true
	case 'b':
		return "\b", true
	case 'f':
		return "\f", true
	case 'n':
		return "\n", true
	case 'r':
		return "\r", true
	case 't':
		return "\t", true
	case 'v':
		return "\v", true
	case '\\':
		return "\\", true
	}
	return "", false
}
