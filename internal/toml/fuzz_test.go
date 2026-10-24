package toml

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// FuzzParse: Parse never panics, and whatever it accepts survives a round
// trip through a plain re-encoding (every value written inline).
func FuzzParse(f *testing.F) {
	for _, c := range specCases() {
		f.Add(c.in)
	}
	f.Add("a = [{b = '''x'''}, \"\\u00e9\"]\n[[t]]\n[t.u]\nv = -inf\n")
	f.Fuzz(func(t *testing.T, doc string) {
		tab, err := Parse([]byte(doc))
		if err != nil {
			return
		}
		again, err := Parse([]byte(encode(tab)))
		if err != nil {
			t.Fatalf("re-encoding of %q does not parse: %v\n%s", doc, err, encode(tab))
		}
		if !sameValue(tab, again) {
			t.Fatalf("round trip of %q changed it:\n%s\n%s", doc, PyRepr(tab), PyRepr(again))
		}
	})
}

func TestRoundTripSpec(t *testing.T) {
	for _, c := range specCases() {
		tab, err := Parse([]byte(c.in))
		if err != nil {
			continue
		}
		again, err := Parse([]byte(encode(tab)))
		if err != nil || !sameValue(tab, again) {
			t.Errorf("%s: round trip: %v\n%s", c.name, err, encode(tab))
		}
	}
}

// encode writes a table as root key/value lines with inline values.
func encode(t *Table) string {
	var b strings.Builder
	for _, k := range t.Keys() {
		v, _ := t.Get(k)
		b.WriteString(encKey(k) + " = " + encValue(v) + "\n")
	}
	return b.String()
}

func encKey(k string) string { return encString(k) }

func encString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func encValue(v Value) string {
	switch x := v.(type) {
	case string:
		return encString(x)
	case int64:
		return fmt.Sprint(x)
	case float64:
		switch {
		case math.IsNaN(x):
			return "nan"
		case math.IsInf(x, 1):
			return "inf"
		case math.IsInf(x, -1):
			return "-inf"
		}
		return PyRepr(x)
	case bool:
		return fmt.Sprint(x)
	case []Value:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = encValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Table:
		parts := make([]string, 0, x.Len())
		for _, k := range x.Keys() {
			e, _ := x.Get(k)
			parts = append(parts, encKey(k)+" = "+encValue(e))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	panic(fmt.Sprintf("unexpected value %T", v))
}

// sameValue is structural identity: same types, same order, nan equal to
// nan and -0.0 distinct from 0.0.
func sameValue(a, b Value) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && (math.Float64bits(x) == math.Float64bits(y) || math.IsNaN(x) && math.IsNaN(y))
	case []Value:
		y, ok := b.([]Value)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case *Table:
		y, ok := b.(*Table)
		if !ok || x.Len() != y.Len() {
			return false
		}
		yk := y.Keys()
		for i, k := range x.Keys() {
			if yk[i] != k {
				return false
			}
			xv, _ := x.Get(k)
			yv, _ := y.Get(k)
			if !sameValue(xv, yv) {
				return false
			}
		}
		return true
	}
	return a == b
}
