// Package textx holds the Python-compatible text helpers of the factory and
// the shared regex classes every ported pattern uses (ARCHITECTURE.md §2.3,
// §8.5).
//
// Python counts str lengths in code points and splits, strips and lowers by
// Unicode rules that differ from Go's strings package. Every helper here is
// checked against goldens written by Python (testdata/py, from
// tools/goldens.py). Lengths are in code points (runes); an invalid UTF-8
// byte counts as one, as utf8.RuneCountInString does. Python strings never
// hold invalid UTF-8, so that case only arises for Go-made input.
package textx

import (
	"strings"
	"unicode/utf8"
)

// ClipDefault is clip()'s default length.
const ClipDefault = 500

// Ellipsis ends a clipped text.
const Ellipsis = "…"

// Fold is Python's " ".join(s.split()): runs of Python whitespace (IsSpace,
// which includes \x1c-\x1f, NEL, LS and PS) become one space, and the ends
// are trimmed.
func Fold(s string) string {
	return strings.Join(strings.FieldsFunc(s, IsSpace), " ")
}

// OneLine is one_line() (F:3580): the same fold, so that every line break
// becomes a space.
func OneLine(s string) string { return Fold(s) }

// Clip is clip(text, n) (F:360): fold the text, and if it is longer than n
// code points, cut it to n-1, strip trailing whitespace and add "…". Python
// slices with n-1 even when n is 0 or negative (text[:-1]), and so does Clip.
func Clip(s string, n int) string {
	s = Fold(s)
	l := RuneLen(s)
	if l <= n {
		return s
	}
	return RStrip(runePrefix(s, l, n-1)) + Ellipsis
}

// ClipDefaultN is clip(text) with the default length.
func ClipDefaultN(s string) string { return Clip(s, ClipDefault) }

// runePrefix is Python's s[:k] for a string of l code points, with negative
// k counting from the end.
func runePrefix(s string, l, k int) string {
	if k < 0 {
		k += l
		if k < 0 {
			k = 0
		}
	}
	if k >= l {
		return s
	}
	i := 0
	for n := 0; n < k; n++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// Sentence is sentence(text, n) (F:365): the first n sentences of the
// folded text, clipped to ClipDefault, ending in a full stop unless it
// already ends in . ? ! or ….
func Sentence(s string, n int) string {
	parts := SentenceSplit(Fold(s))
	parts = pySliceHead(parts, n)
	out := ClipDefaultN(strings.Join(parts, " "))
	if out == "" {
		return out
	}
	r, _ := utf8.DecodeLastRuneInString(out)
	switch r {
	case '.', '?', '!', '…':
		return out
	}
	return out + "."
}

// pySliceHead is Python's xs[:n], negative n included.
func pySliceHead(xs []string, n int) []string {
	if n < 0 {
		n += len(xs)
		if n < 0 {
			n = 0
		}
	}
	if n > len(xs) {
		n = len(xs)
	}
	return xs[:n]
}

// SentenceSplit is re.split(r"(?<=[.?!])\s+", s), written by hand because
// RE2 has no lookbehind: s is cut at every run of Python whitespace that
// directly follows ".", "?" or "!".
func SentenceSplit(s string) []string {
	var out []string
	start := 0
	var prev rune = -1
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !IsSpace(r) || prev != '.' && prev != '?' && prev != '!' {
			prev = r
			i += size
			continue
		}
		// A run of whitespace after a stop: cut, and go on after the run.
		out = append(out, s[start:i])
		for i < len(s) {
			r, size = utf8.DecodeRuneInString(s[i:])
			if !IsSpace(r) {
				break
			}
			i += size
		}
		start, prev = i, -1
	}
	return append(out, s[start:])
}

// Strip is Python's str.strip(): Python whitespace removed at both ends.
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// LStrip is Python's str.lstrip().
func LStrip(s string) string { return strings.TrimLeftFunc(s, IsSpace) }

// RStrip is Python's str.rstrip().
func RStrip(s string) string { return strings.TrimRightFunc(s, IsSpace) }

// RuneLen is Python's len(s): the number of code points.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// Prefix is Python's s[:n] in code points, for n >= 0 (a byte that is not
// UTF-8 counts as one). A negative n is s[:0], "".
func Prefix(s string, n int) string {
	i := 0
	for j := range s {
		if i >= n {
			return s[:j]
		}
		i++
	}
	return s
}

// IsLineBreak reports a Markdown line break, LINE_BREAK in bin/factory
// (F:3577): CR, LF, VT, FF, NEL, LS and PS. A lone CR is a line ending in
// CommonMark.
func IsLineBreak(r rune) bool {
	switch r {
	case '\r', '\n', '\v', '\f', 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// HasLineBreak is LINE_BREAK.search(s) is not None.
func HasLineBreak(s string) bool { return strings.IndexFunc(s, IsLineBreak) >= 0 }

// isSplitLinesBreak is the set str.splitlines() splits on: the Markdown
// breaks plus \x1c, \x1d and \x1e (but not \x1f).
func isSplitLinesBreak(r rune) bool {
	return IsLineBreak(r) || r == 0x1c || r == 0x1d || r == 0x1e
}

// SplitLines is Python's str.splitlines(): lines without their endings,
// where "\r\n" is one ending and a final ending adds no empty line.
func SplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !isSplitLinesBreak(r) {
			i += size
			continue
		}
		out = append(out, s[start:i])
		i += size
		if r == '\r' && i < len(s) && s[i] == '\n' {
			i++
		}
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Slugify is slugify(title) (F:568): the title lowered as Python does, every
// run of characters outside [a-z0-9] turned into "-", hyphens trimmed, cut
// to 48 characters, trailing hyphens trimmed again, and "change" when
// nothing is left.
func Slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range Lower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 48 { // all ASCII now: bytes are code points
		s = s[:48]
	}
	if s = strings.TrimRight(s, "-"); s == "" {
		return "change"
	}
	return s
}
