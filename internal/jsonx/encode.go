package jsonx

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Format is a json.dumps call. The zero Format is json.dumps(v) with
// indent=0, which is rarely wanted: use EncodePython or set Indent to -1.
type Format struct {
	// Indent is the indent= argument: spaces per level, or -1 for None
	// (everything on one line). 0 puts every item on its own line with no
	// indentation, as Python does.
	Indent int
	// SortKeys is sort_keys=True: object keys in code point order.
	SortKeys bool
	// Compact is separators=(",", ":"). Otherwise the separators are
	// Python's defaults: ", " and ": " on one line, "," and ": " with an
	// indent.
	Compact bool
}

// EncodePython is json.dumps(v, indent=indent) (indent -1 for None), with
// ensure_ascii as Python's default: json.dump(t, f, indent=2) writes a
// ticket, json.dumps(x) a CLI answer.
func EncodePython(v Value, indent int) []byte {
	return Format{Indent: indent}.Encode(v)
}

// Canonical is json.dumps(v, sort_keys=True, separators=(",", ":")): the
// form whose sha256 is request_digest's idempotency digest.
func Canonical(v Value) []byte {
	return Format{Indent: -1, SortKeys: true, Compact: true}.Encode(v)
}

// Encode writes v as json.dumps does with these arguments.
func (f Format) Encode(v Value) []byte {
	e := encoder{f: f}
	if f.Indent >= 0 {
		e.ind = strings.Repeat(" ", f.Indent)
	}
	switch {
	case f.Compact:
		e.item, e.key = ",", ":"
	case f.Indent >= 0:
		e.item, e.key = ",", ": "
	default:
		e.item, e.key = ", ", ": "
	}
	e.value(v, 0)
	return e.b
}

type encoder struct {
	f         Format
	b         []byte
	ind       string
	item, key string
}

func (e *encoder) newline(level int) {
	if e.f.Indent < 0 {
		return
	}
	e.b = append(e.b, '\n')
	for range level {
		e.b = append(e.b, e.ind...)
	}
}

func (e *encoder) value(v Value, level int) {
	switch v.kind {
	case KindNull:
		e.b = append(e.b, "null"...)
	case KindBool:
		if v.b {
			e.b = append(e.b, "true"...)
		} else {
			e.b = append(e.b, "false"...)
		}
	case KindNumber:
		e.b = append(e.b, pyNumber(v.s)...)
	case KindString:
		e.b = appendString(e.b, v.s)
	case KindArray:
		if len(v.arr) == 0 {
			e.b = append(e.b, "[]"...)
			return
		}
		e.b = append(e.b, '[')
		for i, x := range v.arr {
			if i > 0 {
				e.b = append(e.b, e.item...)
			}
			e.newline(level + 1)
			e.value(x, level+1)
		}
		e.newline(level)
		e.b = append(e.b, ']')
	case KindObject:
		if len(v.obj) == 0 {
			e.b = append(e.b, "{}"...)
			return
		}
		ms := v.obj
		if e.f.SortKeys {
			ms = slices.Clone(ms)
			slices.SortStableFunc(ms, func(a, b Member) int { return strings.Compare(a.Key, b.Key) })
		}
		e.b = append(e.b, '{')
		for i, m := range ms {
			if i > 0 {
				e.b = append(e.b, e.item...)
			}
			e.newline(level + 1)
			e.b = appendString(e.b, m.Key)
			e.b = append(e.b, e.key...)
			e.value(m.Value, level+1)
		}
		e.newline(level)
		e.b = append(e.b, '}')
	}
}

const hexDigits = "0123456789abcdef"

func appendU(b []byte, r rune) []byte {
	return append(b, '\\', 'u', hexDigits[r>>12&0xF], hexDigits[r>>8&0xF], hexDigits[r>>4&0xF], hexDigits[r&0xF])
}

// appendString writes s as json.dumps does with ensure_ascii: printable
// ASCII as itself except " and \, the short escapes \" \\ \n \r \t \b \f,
// and everything else as a lowercase \u escape, with astral characters as a
// surrogate pair. "/" and "<>&" are not escaped. A WTF-8 lone surrogate is
// written as its own escape; any other invalid byte as U+FFFD.
func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c <= 0x7e {
			if c == '"' || c == '\\' {
				b = append(b, '\\')
			}
			b = append(b, c)
			i++
			continue
		}
		if c < utf8.RuneSelf {
			switch c {
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			case '\b':
				b = append(b, '\\', 'b')
			case '\f':
				b = append(b, '\\', 'f')
			default:
				b = appendU(b, rune(c))
			}
			i++
			continue
		}
		if sr, ok := surrogateAt(s[i:]); ok {
			b = appendU(b, sr)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r >= 0x10000 {
			r -= 0x10000
			b = appendU(b, 0xD800+r>>10)
			b = appendU(b, 0xDC00+r&0x3FF)
			continue
		}
		b = appendU(b, r) // RuneError for an invalid byte
	}
	return append(b, '"')
}
