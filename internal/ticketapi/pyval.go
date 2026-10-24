package ticketapi

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// Python's view of the JSON values the verbs read: the verbs port code that
// reads a ticket as a dict (t.get(k), `or []`, `is None`), and the view
// they answer with carries the stored values as they are. These helpers say
// what Python says of a jsonx.Value.

// getOr is d.get(key, def): the stored value, even null, when the key is
// there.
func getOr(v jsonx.Value, key string, def jsonx.Value) jsonx.Value {
	o, _ := v.Object()
	if x, ok := o.Lookup(key); ok {
		return x
	}
	return def
}

// has is `key in d`.
func has(v jsonx.Value, key string) bool {
	o, _ := v.Object()
	_, ok := o.Lookup(key)
	return ok
}

// or is `a or b`.
func or(a, b jsonx.Value) jsonx.Value {
	if jsonx.Truthy(a) {
		return a
	}
	return b
}

func jstr(s string) jsonx.Value { return jsonx.String(s) }

func strs(ss []string) jsonx.Value {
	vs := make([]jsonx.Value, len(ss))
	for i, s := range ss {
		vs[i] = jsonx.String(s)
	}
	return jsonx.ArrayOf(vs...)
}

func emptyList() jsonx.Value { return jsonx.ArrayOf() }

// list is the elements of an array (none for anything else).
func list(v jsonx.Value) []jsonx.Value {
	a, _ := v.Array()
	return a
}

// sval is v's string, "" when it is not one.
func sval(v jsonx.Value) string {
	s, _ := v.Str()
	return s
}

// isStr reports v is the string s.
func isStr(v jsonx.Value, s string) bool {
	x, ok := v.Str()
	return ok && x == s
}

// pyEq is Python's == on two JSON values as json.load reads them: numbers
// by value (1 == 1.0, and True == 1), lists element by element, and dicts
// by their items, in any order.
func pyEq(a, b jsonx.Value) bool {
	num := func(v jsonx.Value) (float64, bool) {
		if bv, ok := v.Bool(); ok {
			if bv {
				return 1, true
			}
			return 0, true
		}
		return v.Float64()
	}
	if x, ok := num(a); ok {
		y, ok := num(b)
		return ok && x == y
	}
	switch a.Kind() {
	case jsonx.KindNull:
		return b.IsNull()
	case jsonx.KindString:
		return isStr(b, sval(a))
	case jsonx.KindArray:
		x, y := list(a), list(b)
		if b.Kind() != jsonx.KindArray || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEq(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	x, _ := a.Object()
	y, ok := b.Object()
	if !ok || len(x) != len(y) {
		return false
	}
	for _, m := range x {
		v, ok := y.Lookup(m.Key)
		if !ok || !pyEq(m.Value, v) {
			return false
		}
	}
	return true
}

// pyIn is `s in v`: a list's equal element, a string's substring, a dict's
// key.
func pyIn(s string, v jsonx.Value) bool {
	switch v.Kind() {
	case jsonx.KindArray:
		for _, e := range list(v) {
			if isStr(e, s) {
				return true
			}
		}
	case jsonx.KindString:
		return strings.Contains(sval(v), s)
	case jsonx.KindObject:
		return has(v, s)
	}
	return false
}

// pyKey is a dict key made of a JSON value: equal values give equal keys
// (1 and 1.0 alike).
func pyKey(v jsonx.Value) string {
	if f, ok := v.Float64(); ok {
		return "n" + strconv.FormatFloat(f, 'g', -1, 64)
	}
	if b, ok := v.Bool(); ok {
		if b {
			return "n1"
		}
		return "n0"
	}
	return string(jsonx.Canonical(v))
}

// repr is Python's repr() of a string, as a message quotes a key or label.
func repr(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(q):
			b.WriteByte('\\')
			b.WriteByte(q)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && !unicode.IsPrint(r)):
			switch {
			case r < 0x100:
				b.WriteString(`\x` + hexPad(int(r), 2))
			case r < 0x10000:
				b.WriteString(`\u` + hexPad(int(r), 4))
			default:
				b.WriteString(`\U` + hexPad(int(r), 8))
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

func hexPad(n, width int) string {
	s := strconv.FormatInt(int64(n), 16)
	return strings.Repeat("0", max(0, width-len(s))) + s
}

// runes is len(s) in Python: code points.
func runes(s string) int { return utf8.RuneCountInString(s) }
