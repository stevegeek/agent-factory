package toml

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// PyRepr is Python's repr() of the value tomllib would return: what
// `{x!r}` prints in the factory's messages, and the oracle's comparison
// form. A nil Value is None.
func PyRepr(v Value) string {
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

// PyStr is Python's str(): a string as itself, anything else as its repr.
func PyStr(v Value) string {
	if s, ok := v.(string); ok {
		return s
	}
	return PyRepr(v)
}

func writeRepr(b *strings.Builder, v Value) {
	switch x := v.(type) {
	case nil:
		b.WriteString("None")
	case string:
		b.WriteString(reprString(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(reprFloat(x))
	case bool:
		if x {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case []Value:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, e)
		}
		b.WriteByte(']')
	case *Table:
		b.WriteByte('{')
		for i, k := range x.Keys() {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(reprString(k))
			b.WriteString(": ")
			e, _ := x.Get(k)
			writeRepr(b, e)
		}
		b.WriteByte('}')
	default:
		fmt.Fprintf(b, "<%T>", v)
	}
}

// reprString is Python's repr of a str: single quotes unless the text has a
// single quote and no double quote; backslash escapes for the quote,
// backslash, \t \n \r, other ASCII controls and non-printable characters
// (str.isprintable: not Cc, Cf, Cs, Co, Cn, Zl, Zp, or Zs other than space).
func reprString(s string) string {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case !unicode.IsPrint(r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// reprFloat is Python's float repr: the shortest round-tripping digits, in
// positional form when the decimal exponent is in [-4, 16), with ".0" on an
// integral value, otherwise as d.ddde±XX.
func reprFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	if exp < -4 || exp >= 16 {
		return mant + "e" + expText[:1] + fmt.Sprintf("%02d", abs(exp))
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Truthy is Python truthiness: "", 0, 0.0, False, [], {} and None are
// false; everything else is true.
func Truthy(v Value) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case bool:
		return x
	case []Value:
		return len(x) > 0
	case *Table:
		return x.Len() > 0
	}
	return true
}

// Equal is Python ==: int, float and bool compare as numbers (1 == 1.0 ==
// True), nan equals nothing, arrays compare element by element and tables
// by content, whatever their key order.
func Equal(a, b Value) bool {
	if x, ok := a.(int64); ok {
		if y, ok := b.(int64); ok {
			return x == y
		}
	}
	if x, ok := number(a); ok {
		y, ok := number(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []Value:
		y, ok := b.([]Value)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case *Table:
		y, ok := b.(*Table)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for _, k := range x.Keys() {
			xv, _ := x.Get(k)
			yv, ok := y.Get(k)
			if !ok || !Equal(xv, yv) {
				return false
			}
		}
		return true
	}
	return false
}

// number gives int64, float64 and bool as a float64 for Equal (two int64s
// are compared exactly before it is used).
func number(v Value) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
