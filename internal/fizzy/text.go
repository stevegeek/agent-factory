package fizzy

import (
	"strings"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/textx"
)

// cutMark ends a text the import had to shorten.
const cutMark = "\n\n[cut on import]"

// clean is untrusted text as a ticket keeps it: valid UTF-8, CRLF and CR
// as LF, U+2028 and U+2029 as LF, and no other control character but tab
// and LF (C0, DEL, C1, and the bidi overrides and isolates, which can make
// text read other than it is), with the ends trimmed.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\r' || r == 0x2028 || r == 0x2029:
			b.WriteByte('\n')
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r < 0x20 || r >= 0x7f && r < 0xa0:
		case r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069:
		default:
			b.WriteRune(r)
		}
	}
	return textx.Strip(b.String())
}

// line is untrusted text as one line: cleaned and folded.
func line(s string) string { return textx.Fold(clean(s)) }

// cutRunes cuts a line to at most n code points.
func cutRunes(s string, n int) string {
	if textx.RuneLen(s) <= n {
		return s
	}
	return textx.RStrip(textx.Prefix(s, n-1)) + textx.Ellipsis
}

// cutBytes cuts a text to at most n UTF-8 bytes, on a code point, ending
// it with cutMark when it had to.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	keep := n - len(cutMark)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return textx.RStrip(s[:keep]) + cutMark
}

// person is how the factory names a Fizzy user: their name there, marked
// as from Fizzy ("Ana Example (Fizzy)"). It is never a person of [board]
// people or a token's name (a token's name has no space or parenthesis),
// so it grants nothing.
func person(p *Person) string {
	name := ""
	if p != nil {
		name = cutRunes(line(p.Name), maxPersonLen)
	}
	if name == "" {
		name = "someone"
	}
	return name + " (Fizzy)"
}

// Label is a Fizzy tag as a label (^[a-z0-9][a-z0-9-]{0,31}$): lower
// case, every run of other characters one dash, the ends trimmed of
// dashes, at most 32. "" when nothing is left.
func Label(tag string) string {
	var b strings.Builder
	dash := false
	for _, r := range textx.Lower(tag) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	s := b.String()
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	return s
}

// validURL is the ticket API's link URL: http(s), with no space, <, > or ".
func validURL(s string) bool {
	rest, ok := strings.CutPrefix(s, "https://")
	if !ok {
		if rest, ok = strings.CutPrefix(s, "http://"); !ok {
			return false
		}
	}
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if textx.IsSpace(r) || r == '<' || r == '>' || r == '"' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
