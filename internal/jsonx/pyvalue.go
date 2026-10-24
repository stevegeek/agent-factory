package jsonx

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Python's view of a value json.loads made: str(), repr() and bool(), the
// one copy every package uses (ARCHITECTURE.md §2.2, layer 0). Agents and
// people write the files, so a value can be anything JSON holds; the
// factory formats and tests them as Python did.

// Truthy is Python's bool(v): false for null, false, 0, "", [] and {}.
func Truthy(v Value) bool {
	switch v.Kind() {
	case KindNull:
		return false
	case KindBool:
		b, _ := v.Bool()
		return b
	case KindNumber:
		f, _ := v.Float64()
		return f != 0
	case KindString:
		s, _ := v.Str()
		return s != ""
	case KindArray:
		a, _ := v.Array()
		return len(a) > 0
	}
	o, _ := v.Object()
	return len(o) > 0
}

// PyStr is Python's str(v): a string as itself, anything else as repr().
func PyStr(v Value) string {
	if s, ok := v.Str(); ok {
		return s
	}
	return PyRepr(v)
}

// PyRepr is Python's repr(v): None, True, 4, 4.5, 'text', [1, 'a'],
// {'k': 'v'}, in the object's own key order.
func PyRepr(v Value) string {
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

func writeRepr(b *strings.Builder, v Value) {
	switch v.Kind() {
	case KindNull:
		b.WriteString("None")
	case KindBool:
		if x, _ := v.Bool(); x {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case KindNumber:
		b.WriteString(PyNumber(v))
	case KindString:
		s, _ := v.Str()
		b.WriteString(PyReprString(s))
	case KindArray:
		a, _ := v.Array()
		b.WriteByte('[')
		for i, e := range a {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, e)
		}
		b.WriteByte(']')
	default:
		o, _ := v.Object()
		b.WriteByte('{')
		// a repeated key keeps its first place and its last value, as in
		// the dict json.loads makes
		var seen map[string]bool
		first := true
		for i, m := range o {
			if seen[m.Key] {
				continue
			}
			val := m.Value
			for _, later := range o[i+1:] {
				if later.Key == m.Key {
					if seen == nil {
						seen = map[string]bool{}
					}
					seen[m.Key] = true
					val = later.Value
				}
			}
			if !first {
				b.WriteString(", ")
			}
			first = false
			b.WriteString(PyReprString(m.Key))
			b.WriteString(": ")
			writeRepr(b, val)
		}
		b.WriteByte('}')
	}
}

// PyNumber is str() of the int or float json.loads made of a number: an
// integer literal as written, a float as Python's repr (4.0, 1e-05, inf).
func PyNumber(v Value) string {
	if v.IsInt() {
		if n, ok := v.Int64(); ok {
			return strconv.FormatInt(n, 10)
		}
		lit, _ := v.Number()
		return lit
	}
	f, _ := v.Float64()
	s := string(EncodePython(Float(f), -1))
	switch s {
	case "Infinity":
		return "inf"
	case "-Infinity":
		return "-inf"
	case "NaN":
		return "nan"
	}
	return s
}

// PyReprString is Python's repr of a str: single quotes unless the text
// holds a single quote and no double one; \\, the quote, \t \n \r and
// unprintable characters escaped; a lone surrogate (kept as WTF-8) as
// \udXXX.
func PyReprString(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if t := s[i:]; len(t) >= 3 && t[0] == 0xED && t[1] >= 0xA0 && t[1] <= 0xBF && t[2] >= 0x80 && t[2] <= 0xBF {
				cp := rune(s[i]&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
				b.WriteString(`\u` + strconv.FormatInt(int64(cp), 16))
				i += 3
				continue
			}
			b.WriteString(`\x` + strconv.FormatInt(int64(s[i]), 16))
			i++
			continue
		}
		i += size
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(q):
			b.WriteByte('\\')
			b.WriteByte(q)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x` + pad0(strconv.FormatInt(int64(r), 16), 2))
		case r >= 0x80 && !PyPrintable(r):
			switch {
			case r <= 0xff:
				b.WriteString(`\x` + pad0(strconv.FormatInt(int64(r), 16), 2))
			case r <= 0xffff:
				b.WriteString(`\u` + pad0(strconv.FormatInt(int64(r), 16), 4))
			default:
				b.WriteString(`\U` + pad0(strconv.FormatInt(int64(r), 16), 8))
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

// PyPrintable is str.isprintable() for one non-ASCII character: letters,
// marks, numbers, punctuation and symbols; not separators (Z*), controls,
// format, private-use, surrogate or unassigned characters (C*).
func PyPrintable(r rune) bool {
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S)
}

func pad0(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}
