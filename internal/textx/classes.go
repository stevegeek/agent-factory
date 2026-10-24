package textx

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsSpace is Python's str.isspace() for one character, which is also what
// re's \s and str.split() use: \t \n \v \f \r, \x1c-\x1f, space, NEL, NBSP
// and the Unicode Zs, Zl and Zp characters. Unlike unicode.IsSpace it
// includes \x1c-\x1f.
func IsSpace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r >= 0x1c && r <= ' ', r == 0x85, r == 0xa0:
		return true
	case r < 0x1680:
		return false
	}
	return unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp)
}

// IsDecimal is re's \d (str patterns): the Unicode Nd characters, which is
// also str.isdecimal().
func IsDecimal(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= '0' && r <= '9'
	}
	return unicode.Is(unicode.Nd, r)
}

// IsDigit is Python's str.isdigit() for one character: the decimals plus the
// other characters with a digit value, such as superscripts ("²") and
// circled digits. int() does not accept all of them.
func IsDigit(r rune) bool {
	return IsDecimal(r) || r >= 0x80 && unicode.Is(unicode.No, r) && hasDigitValue(r)
}

// IsAlnum is Python's str.isalnum() for one character: letters (L) and
// numbers (Nd, Nl, No).
func IsAlnum(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	}
	return unicode.In(r, unicode.L, unicode.Nd, unicode.Nl, unicode.No)
}

// IsWord is re's \w (str patterns): IsAlnum or "_". Combining marks are not
// word characters in Python.
func IsWord(r rune) bool { return r == '_' || IsAlnum(r) }

// IsDigits is Python's s.isdigit(): at least one character, and every one
// IsDigit.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !IsDigit(r) {
			return false
		}
	}
	return true
}

// IsWordBoundary is Python's \b at byte offset i of s: exactly one of the
// characters before and after i is a word character (IsWord). Use it for
// the \b that RE2 only knows in ASCII.
func IsWordBoundary(s string, i int) bool {
	before, after := false, false
	if i > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:i])
		before = IsWord(r)
	}
	if i < len(s) {
		r, _ := utf8.DecodeRuneInString(s[i:])
		after = IsWord(r)
	}
	return before != after
}

// hasDigitValue reports the No characters with Numeric_Type=Digit (Python's
// str.isdigit beyond Nd). The Go standard library has no Numeric_Type
// table, so the ranges are listed; classes.json checks them.
func hasDigitValue(r rune) bool {
	for _, rg := range digitValueRanges() {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// digitValueRanges are the No ranges with a digit value (Unicode 16):
// superscripts, subscripts, circled and parenthesized digits, and the like.
func digitValueRanges() [][2]rune {
	return [][2]rune{
		{0x00B2, 0x00B3}, {0x00B9, 0x00B9}, {0x1369, 0x1371}, {0x19DA, 0x19DA},
		{0x2070, 0x2070}, {0x2074, 0x2079}, {0x2080, 0x2089}, {0x2460, 0x2468},
		{0x2474, 0x247C}, {0x2488, 0x2490}, {0x24EA, 0x24EA}, {0x24F5, 0x24FD},
		{0x24FF, 0x24FF}, {0x2776, 0x277E}, {0x2780, 0x2788}, {0x278A, 0x2792},
		{0x10A40, 0x10A43}, {0x10E60, 0x10E68}, {0x11052, 0x1105A}, {0x1F100, 0x1F10A},
	}
}

// Lower is Python's str.lower(): the full Unicode lowercase mapping, which
// differs from strings.ToLower in two places. U+0130 (İ) becomes "i̇" (two
// code points), and a capital sigma becomes the final form ς at the end of
// a word (the Final_Sigma rule of SpecialCasing.txt).
func Lower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r < utf8.RuneSelf:
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			b.WriteByte(byte(r))
		case r == utf8.RuneError && size == 1:
			b.WriteByte(s[i]) // an invalid byte stays as it is
		case r == 0x130:
			b.WriteString("i\u0307") // i and a combining dot above
		case r == 0x3A3:
			if finalSigma(s, i, i+size) {
				b.WriteRune(0x3C2)
			} else {
				b.WriteRune(0x3C3)
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
		i += size
	}
	return b.String()
}

// finalSigma is Python's Final_Sigma test for the sigma at s[start:end]:
// preceded by a cased letter (skipping case-ignorable characters), and not
// followed by one.
func finalSigma(s string, start, end int) bool {
	return casedBefore(s[:start]) && !casedAfter(s[end:])
}

// casedBefore reports whether the last character of s that is not
// case-ignorable is cased.
func casedBefore(s string) bool {
	for j := len(s); j > 0; {
		r, size := utf8.DecodeLastRuneInString(s[:j])
		j -= size
		if !caseIgnorable(r) {
			return cased(r)
		}
	}
	return false
}

// casedAfter reports whether the first character of s that is not
// case-ignorable is cased.
func casedAfter(s string) bool {
	for _, r := range s {
		if !caseIgnorable(r) {
			return cased(r)
		}
	}
	return false
}

// cased is the Unicode Cased property: Lu, Ll, Lt, Other_Lowercase and
// Other_Uppercase.
func cased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

// caseIgnorable is the Unicode Case_Ignorable property: Mn, Me, Cf, Lm, Sk,
// and the Word_Break classes MidLetter, MidNumLet and Single_Quote (listed
// here, as Go has no Word_Break table).
func caseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', 0xB7, 0x387, 0x55F, 0x5F4, 0x2018, 0x2019, 0x2024, 0x2027,
		0xFE13, 0xFE52, 0xFE55, 0xFF07, 0xFF0E, 0xFF1A:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}
