package jsonx

import (
	"errors"
	"strings"
	"testing"
)

// Each case was checked with Python 3.14's json.loads (and json.dumps of
// the result, which is what a save writes).
func TestParseAcceptsWhatPythonAccepts(t *testing.T) {
	for _, c := range []struct{ in, out string }{
		{"NaN", "NaN"},
		{"Infinity", "Infinity"},
		{"-Infinity", "-Infinity"},
		{"\"\x7f\"", `"\u007f"`},
		{" \t\n\r1\n ", "1"},
		{`"\ud800\u0041"`, `"\ud800A"`},
		{`"\uD83D\uDE00"`, `"\ud83d\ude00"`},
		{"1e5", "100000.0"},
		{"1E+2", "100.0"},
		{"-0.0", "-0.0"},
		{"-0", "0"},
		{"1e400", "Infinity"},
		{"-1e400", "-Infinity"},
		{`"\/"`, `"/"`},
		{`"\u00e9"`, `"\u00e9"`},
		{`"é"`, `"\u00e9"`}, // raw UTF-8 is read too
		{`"\ud83d"`, `"\ud83d"`},
		{`"\ude00\ud83d"`, `"\ude00\ud83d"`},
		{`"\ud83d\ud83d\ude00"`, `"\ud83d\ud83d\ude00"`},
		{`"\b\f\n\r\t\"\\"`, `"\b\f\n\r\t\"\\"`},
		{`"\u0000"`, `"\u0000"`},
		{`{"a": 1, "a": 2, "b": 3}`, `{"a": 2, "b": 3}`},
		{`[1.5e300, 0.10, 12345678901234567890]`, `[1.5e+300, 0.1, 12345678901234567890]`},
		{"true", "true"},
		{"null", "null"},
	} {
		v, err := Parse([]byte(c.in))
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got := string(EncodePython(v, -1)); got != c.out {
			t.Errorf("Parse(%q) encodes as %s, Python %s", c.in, got, c.out)
		}
	}
}

func TestParseRefusesWhatPythonRefuses(t *testing.T) {
	for _, in := range []string{
		"", " ", "01", "1.", ".5", "+1", "-", "-NaN", "nan", "[1,]", `{"a":1,}`, "'a'", `"\x"`, `"\u12"`, "\"a\tb\"",
		"\"a\nb\"", "\x0c1", "1 2", "[]x", "\ufeff1", "tru", "nul", "[1 2]", `{"a" 1}`, "{1:2}", `"abc`, "[", "{", `{"a"`,
		"\"\xff\"", "\xff", `"\uDZ00"`, "1e", "1e+", "--1", "[,1]", "{,}", "\"\x00\"",
	} {
		if v, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) = %s, want an error", in, EncodePython(v, -1))
		} else {
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Errorf("Parse(%q) error %T is not a *SyntaxError", in, err)
			}
		}
	}
}

func TestParseDepthIsBounded(t *testing.T) {
	deep := strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1)
	if _, err := Parse([]byte(deep)); err == nil {
		t.Error("a document nested past MaxDepth parsed")
	}
	ok := strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Errorf("MaxDepth nesting: %v", err)
	}
}

// FuzzParse: Parse never panics, and whatever it accepts encodes to a
// document that parses to an equal value and encodes the same again.
func FuzzParse(f *testing.F) {
	for _, s := range []string{`{"a": [1, 2.5, "x"]}`, `"\ud800"`, `[1e400, -0, NaN]`, `{"a": 1, "a": 2}`, "\"\xc3\xa9\""} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := Parse(b)
		if err != nil {
			return
		}
		for _, indent := range []int{-1, 2} {
			out := EncodePython(v, indent)
			w, err := Parse(out)
			if err != nil {
				t.Fatalf("encoded %q does not parse: %v", out, err)
			}
			if !Equal(v, w) || string(EncodePython(w, indent)) != string(out) {
				t.Fatalf("round trip of %q is not stable: %q", b, out)
			}
		}
		_ = Canonical(v)
	})
}

func TestSyntaxErrorHasOffset(t *testing.T) {
	_, err := Parse([]byte(`{"a": tru}`))
	var se *SyntaxError
	if !errors.As(err, &se) || se.Offset != 6 {
		t.Fatalf("err = %v (%#v)", err, se)
	}
	if !strings.Contains(err.Error(), "offset 6") {
		t.Errorf("message %q", err.Error())
	}
}

func TestAccessors(t *testing.T) {
	v, err := Parse([]byte(`{"s": "x", "n": 4.0, "i": 7, "b": true, "z": null, "a": [1, "2"], "o": {"k": "v"}}`))
	if err != nil {
		t.Fatal(err)
	}
	o, ok := v.Object()
	if !ok || v.Kind() != KindObject || len(o) != 7 {
		t.Fatalf("object: %v %v %d", ok, v.Kind(), len(o))
	}
	if got := strings.Join(o.Keys(), ","); got != "s,n,i,b,z,a,o" {
		t.Errorf("keys %s", got)
	}
	if s, ok := v.Get("s").Str(); !ok || s != "x" {
		t.Errorf("s = %q %v", s, ok)
	}
	if _, ok := v.Get("n").Int64(); ok {
		t.Error("4.0 read as an int: Python keeps it a float")
	}
	if f, ok := v.Get("n").Float64(); !ok || f != 4 {
		t.Errorf("n = %v %v", f, ok)
	}
	if lit, ok := v.Get("n").Number(); !ok || lit != "4.0" {
		t.Errorf("n literal %q", lit)
	}
	if i, ok := v.Get("i").Int64(); !ok || i != 7 {
		t.Errorf("i = %v %v", i, ok)
	}
	if b, ok := v.Get("b").Bool(); !ok || !b {
		t.Errorf("b = %v %v", b, ok)
	}
	if !v.Get("z").IsNull() || v.Get("z").Kind() != KindNull {
		t.Error("z is not null")
	}
	if _, ok := o.Lookup("missing"); ok {
		t.Error("missing key found")
	}
	if !v.Get("missing").IsNull() {
		t.Error("Get of a missing key is not the zero (null) value")
	}
	if a, ok := v.Get("a").Array(); !ok || len(a) != 2 || a[1].Kind() != KindString {
		t.Errorf("a = %v", a)
	}
	if _, ok := v.Get("s").Float64(); ok {
		t.Error("a string read as a number")
	}
	if got := v.Get("o").Get("k"); got.Kind() != KindString {
		t.Errorf("o.k kind %v", got.Kind())
	}
	if (Value{}).Kind() != KindNull || string(EncodePython(Value{}, -1)) != "null" {
		t.Error("the zero Value is not null")
	}
}

func TestObjectEdits(t *testing.T) {
	var o Object
	o.Set("a", Int(1))
	o.Set("b", Int(2))
	o.Set("a", String("x")) // keeps its place, as a Python dict does
	o.Set("c", Bool(false))
	if got := string(EncodePython(o.Value(), -1)); got != `{"a": "x", "b": 2, "c": false}` {
		t.Errorf("after sets: %s", got)
	}
	if !o.Delete("b") || o.Delete("b") {
		t.Error("Delete")
	}
	if got := string(EncodePython(o.Value(), -1)); got != `{"a": "x", "c": false}` {
		t.Errorf("after delete: %s", got)
	}
	if o.Index("c") != 1 || o.Index("b") != -1 {
		t.Error("Index")
	}
}

func TestNumberValidates(t *testing.T) {
	for _, ok := range []string{"0", "-1", "1.5", "1e5", "-0.0", "12345678901234567890", "NaN", "Infinity", "-Infinity"} {
		if _, err := Number(ok); err != nil {
			t.Errorf("Number(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "01", "1.", "x", "1 ", "+1", "0x10"} {
		if _, err := Number(bad); err == nil {
			t.Errorf("Number(%q) accepted", bad)
		}
	}
}

func TestCloneIsDeep(t *testing.T) {
	v, _ := Parse([]byte(`{"a": [{"b": 1}]}`))
	c := v.Clone()
	o, _ := c.Object()
	arr, _ := o[0].Value.Array()
	inner, _ := arr[0].Object()
	inner.Set("b", Int(2))
	arr[0] = inner.Value()
	if got := string(EncodePython(v, -1)); got != `{"a": [{"b": 1}]}` {
		t.Errorf("clone shares memory with the original: %s", got)
	}
}

func TestEqualIsOrderAndTypeSensitive(t *testing.T) {
	p := func(s string) Value {
		v, err := Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{`{"a": 1, "b": 2}`, `{"a": 1, "b": 2}`, true},
		{`{"a": 1, "b": 2}`, `{"b": 2, "a": 1}`, false}, // a different file
		{`1`, `1.0`, false},
		{`1.0`, `1.00`, true}, // Python writes both as 1.0
		{`1E2`, `100.0`, true},
		{`"\u00e9"`, `"é"`, true},
		{`[1, [2]]`, `[1, [2]]`, true},
		{`null`, `false`, false},
		{`NaN`, `NaN`, true},
	} {
		if got := Equal(p(c.a), p(c.b)); got != c.want {
			t.Errorf("Equal(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
