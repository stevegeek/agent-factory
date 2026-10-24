package textx

import (
	"strings"
	"unicode/utf8"
)

// Fnmatch is Python's fnmatch.fnmatch(name, pattern) on POSIX, as
// classify() and gate() use it: case-sensitive, matched on code points,
// where * matches any run of characters including "/" and newlines, ?
// matches any one character, and [...] is a set ([!...] negated) with
// fnmatch.translate's rules: a ] first in the set is a member, a [ with no
// closing ] is a literal, a reversed range such as z-a is empty, and a
// backslash is an ordinary character.
func Fnmatch(name, pattern string) bool {
	return globMatch(compileGlob(pattern), name)
}

type globKind uint8

const (
	globLit globKind = iota
	globAny
	globStar
	globSet
	globNever // an empty set: never matches
)

type globTok struct {
	kind   globKind
	lit    rune
	negate bool
	ranges [][2]rune // inclusive
}

func compileGlob(pat string) []globTok {
	p := []rune(pat)
	var toks []globTok
	for i := 0; i < len(p); {
		c := p[i]
		i++
		switch c {
		case '*':
			for i < len(p) && p[i] == '*' {
				i++
			}
			toks = append(toks, globTok{kind: globStar})
		case '?':
			toks = append(toks, globTok{kind: globAny})
		case '[':
			tok, next, ok := parseGlobSet(p, i)
			if !ok {
				toks = append(toks, globTok{kind: globLit, lit: '['})
				continue
			}
			toks = append(toks, tok)
			i = next
		default:
			toks = append(toks, globTok{kind: globLit, lit: c})
		}
	}
	return toks
}

// parseGlobSet reads a set starting just after its "[", as
// fnmatch._translate does. It returns ok=false when there is no closing ].
func parseGlobSet(p []rune, i int) (globTok, int, bool) {
	j := i
	if j < len(p) && p[j] == '!' {
		j++
	}
	if j < len(p) && p[j] == ']' {
		j++
	}
	for j < len(p) && p[j] != ']' {
		j++
	}
	if j >= len(p) {
		return globTok{}, 0, false
	}
	// The chunks between hyphens that form ranges (translate's loop): each
	// "-" found at least two characters into the current chunk splits it.
	var chunks [][]rune
	k := i + 1
	if p[i] == '!' {
		k = i + 2
	}
	start := i
	for {
		dash := indexRune(p, '-', k, j)
		if dash < 0 {
			break
		}
		chunks = append(chunks, p[start:dash])
		start = dash + 1
		k = dash + 3
	}
	if last := p[start:j]; len(last) > 0 {
		chunks = append(chunks, last)
	} else {
		chunks[len(chunks)-1] = append(append([]rune(nil), chunks[len(chunks)-1]...), '-')
	}
	// Remove empty ranges (a-b with a > b), merging the chunks around them.
	for k := len(chunks) - 1; k > 0; k-- {
		a, b := chunks[k-1], chunks[k]
		if a[len(a)-1] > b[0] {
			chunks[k-1] = append(append([]rune(nil), a[:len(a)-1]...), b[1:]...)
			chunks = append(chunks[:k], chunks[k+1:]...)
		}
	}
	next := j + 1
	tok := globTok{kind: globSet}
	if len(chunks) == 1 && len(chunks[0]) == 0 {
		return globTok{kind: globNever}, next, true
	}
	if len(chunks[0]) > 0 && chunks[0][0] == '!' {
		tok.negate = true
		chunks[0] = chunks[0][1:]
		if len(chunks) == 1 && len(chunks[0]) == 0 {
			return globTok{kind: globAny}, next, true // [!] after removal: any character
		}
	}
	// Every character of a chunk is a member; the last of one chunk and the
	// first of the next are the ends of a range.
	for n, ch := range chunks {
		from, to := 0, len(ch)
		if n > 0 {
			from = 1
		}
		if n < len(chunks)-1 {
			to = len(ch) - 1
			tok.ranges = append(tok.ranges, [2]rune{ch[len(ch)-1], chunks[n+1][0]})
		}
		for _, r := range ch[min(from, to):to] {
			tok.ranges = append(tok.ranges, [2]rune{r, r})
		}
	}
	return tok, next, true
}

func indexRune(p []rune, r rune, from, to int) int {
	for i := from; i < to; i++ {
		if p[i] == r {
			return i
		}
	}
	return -1
}

func (t globTok) one(r rune) bool {
	switch t.kind {
	case globLit:
		return r == t.lit
	case globAny:
		return true
	case globSet:
		in := false
		for _, rg := range t.ranges {
			if r >= rg[0] && r <= rg[1] {
				in = true
				break
			}
		}
		return in != t.negate
	}
	return false
}

// globMatch matches the tokens against the whole name. Every token but a
// star takes exactly one character, so the classic single-backtrack
// algorithm is exact and linear in practice.
func globMatch(toks []globTok, name string) bool {
	s := []rune(name)
	if !utf8.ValidString(name) {
		s = []rune(strings.ToValidUTF8(name, "\ufffd"))
	}
	ti, si := 0, 0
	starT, starS := -1, 0
	for si < len(s) {
		switch {
		case ti < len(toks) && toks[ti].kind == globStar:
			starT, starS = ti, si
			ti++
		case ti < len(toks) && toks[ti].one(s[si]):
			ti++
			si++
		case starT >= 0:
			ti = starT + 1
			starS++
			si = starS
		default:
			return false
		}
	}
	for ti < len(toks) && toks[ti].kind == globStar {
		ti++
	}
	return ti == len(toks)
}
