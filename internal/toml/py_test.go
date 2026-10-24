package toml

import (
	"math"
	"testing"
)

func TestPyReprAndStr(t *testing.T) {
	tab := &Table{}
	tab.set("k", "v")
	tab.set("n", int64(3))
	cases := []struct {
		v         Value
		repr, str string
	}{
		{"plain", "'plain'", "plain"},
		{"it's", `"it's"`, "it's"},
		{`say "hi"`, `'say "hi"'`, `say "hi"`},
		{`it's "x"`, `'it\'s "x"'`, `it's "x"`},
		{"a\\b", `'a\\b'`, "a\\b"},
		{"\x00\x1f\x7f\n\r\t", `'\x00\x1f\x7f\n\r\t'`, "\x00\x1f\x7f\n\r\t"},
		{"é ü 中 😀", "'é ü 中 😀'", "é ü 中 😀"},
		{"\u0085\u00a0\u00ad\u2028\u2029\ufeff", `'\x85\xa0\xad\u2028\u2029\ufeff'`, "\u0085\u00a0\u00ad\u2028\u2029\ufeff"},
		{"\U000E0001\U0010FFFF", `'\U000e0001\U0010ffff'`, "\U000E0001\U0010FFFF"},
		{"", "''", ""},
		{int64(-12), "-12", "-12"},
		{true, "True", "True"},
		{false, "False", "False"},
		{1.0, "1.0", "1.0"},
		{math.Copysign(0, -1), "-0.0", "-0.0"},
		{0.1, "0.1", "0.1"},
		{1e16, "1e+16", "1e+16"},
		{1e15, "1000000000000000.0", "1000000000000000.0"},
		{123456789012345678.0, "1.2345678901234568e+17", "1.2345678901234568e+17"},
		{0.0001, "0.0001", "0.0001"},
		{0.00001, "1e-05", "1e-05"},
		{1.5e-7, "1.5e-07", "1.5e-07"},
		{5e-324, "5e-324", "5e-324"},
		{1.7976931348623157e308, "1.7976931348623157e+308", "1.7976931348623157e+308"},
		{math.Inf(1), "inf", "inf"},
		{math.Inf(-1), "-inf", "-inf"},
		{math.NaN(), "nan", "nan"},
		{[]Value{}, "[]", "[]"},
		{[]Value{"a", int64(1), []Value{true}}, "['a', 1, [True]]", "['a', 1, [True]]"},
		{tab, "{'k': 'v', 'n': 3}", "{'k': 'v', 'n': 3}"},
		{&Table{}, "{}", "{}"},
	}
	for _, c := range cases {
		if got := PyRepr(c.v); got != c.repr {
			t.Errorf("PyRepr(%#v) = %s, want %s", c.v, got, c.repr)
		}
		if got := PyStr(c.v); got != c.str {
			t.Errorf("PyStr(%#v) = %s, want %s", c.v, got, c.str)
		}
	}
}

func TestTruthy(t *testing.T) {
	empty := &Table{}
	full := &Table{}
	full.set("a", int64(1))
	for _, c := range []struct {
		v    Value
		want bool
	}{
		{nil, false}, {"", false}, {"x", true}, {int64(0), false}, {int64(-1), true},
		{0.0, false}, {math.Copysign(0, -1), false}, {0.5, true}, {math.NaN(), true},
		{true, true}, {false, false}, {[]Value{}, false}, {[]Value{false}, true},
		{empty, false}, {full, true},
	} {
		if got := Truthy(c.v); got != c.want {
			t.Errorf("Truthy(%#v) = %v", c.v, got)
		}
	}
}

// Equal is Python ==: numbers compare across int, float and bool; a dict
// compares by content whatever its order; nan is never equal.
func TestEqual(t *testing.T) {
	a := &Table{}
	a.set("x", int64(1))
	a.set("y", "s")
	b := &Table{}
	b.set("y", "s")
	b.set("x", 1.0)
	c := &Table{}
	c.set("x", int64(1))
	for _, tc := range []struct {
		x, y Value
		want bool
	}{
		{"a", "a", true}, {"a", "b", false}, {int64(1), 1.0, true}, {int64(1), true, true},
		{int64(0), false, true}, {1.5, int64(1), false}, {"1", int64(1), false},
		{math.NaN(), math.NaN(), false}, {[]Value{int64(1)}, []Value{1.0}, true},
		{[]Value{int64(1)}, []Value{int64(1), int64(2)}, false}, {a, b, true}, {a, c, false},
		{a, []Value{}, false}, {nil, nil, true}, {nil, "", false},
	} {
		if got := Equal(tc.x, tc.y); got != tc.want {
			t.Errorf("Equal(%s, %s) = %v", PyRepr(tc.x), PyRepr(tc.y), got)
		}
	}
}
