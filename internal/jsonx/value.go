// Package jsonx is JSON that matches Python's json module byte for byte
// (ARCHITECTURE.md §2.3, §3.1, §4.2): an ordered tree that keeps key order
// at every depth, a parser that accepts what json.loads accepts, an encoder
// that writes what json.dumps writes (ensure_ascii, Python separators,
// Python's float repr, no HTML escaping), the canonical form used for the
// idempotency digest. Writing a typed view back into the tree it was read
// from (keeping unknown keys and their order) is ticket's codec (§3.1).
//
// Strings are Go strings. Python strings may hold lone surrogates (a JSON
// escape of a surrogate that is not half of a pair); jsonx keeps them as
// their generalized UTF-8 bytes (WTF-8: ED A0..BF xx), so that a file read
// and written again keeps them. No other invalid UTF-8 is ever produced by
// Parse; the encoder writes an invalid byte of a Go-made string as U+FFFD.
package jsonx

import (
	"math"
	"slices"
	"strconv"
)

// Kind is the kind of a Value. The zero Value is null.
type Kind uint8

// The kinds of JSON values.
const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindArray:
		return "array"
	case KindObject:
		return "object"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Value is one JSON value. Numbers keep the literal text they were read
// with (an integer of any size stays exact); the encoder writes them as
// Python writes the number json.loads makes of that text.
type Value struct {
	kind Kind
	b    bool
	s    string // a string value, or a number's literal
	arr  []Value
	obj  Object
}

// Member is one key and value of an object.
type Member struct {
	Key   string
	Value Value
}

// Object is an ordered JSON object. Keys are unique: Parse keeps a repeated
// key at its first position with its last value, as a Python dict does.
type Object []Member

// Null is the JSON null (the zero Value).
func Null() Value { return Value{} }

// Bool is a JSON boolean.
func Bool(b bool) Value { return Value{kind: KindBool, b: b} }

// String is a JSON string.
func String(s string) Value { return Value{kind: KindString, s: s} }

// Int is a JSON integer.
func Int(i int64) Value { return Value{kind: KindNumber, s: strconv.FormatInt(i, 10)} }

// Float is a Python float: it encodes as Python's repr (4.0, 1e-05, 1e+16),
// and NaN and the infinities as NaN, Infinity and -Infinity, as json.dumps
// does by default.
func Float(f float64) Value { return Value{kind: KindNumber, s: pyFloat(f)} }

// Number is a number from its literal: JSON number syntax, or NaN,
// Infinity and -Infinity, which json.loads also accepts.
func Number(lit string) (Value, error) {
	if !validNumber(lit) {
		return Value{}, &SyntaxError{Msg: "not a number: " + strconv.Quote(lit)}
	}
	return Value{kind: KindNumber, s: lit}, nil
}

// ArrayOf is a JSON array of vs.
func ArrayOf(vs ...Value) Value { return Value{kind: KindArray, arr: vs} }

// ObjectOf is a JSON object of members, in order.
func ObjectOf(ms ...Member) Value { return Value{kind: KindObject, obj: ms} }

// Value is o as a Value.
func (o Object) Value() Value { return Value{kind: KindObject, obj: o} }

// Kind is v's kind.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports a null.
func (v Value) IsNull() bool { return v.kind == KindNull }

// Bool is v's boolean, and whether v is one.
func (v Value) Bool() (bool, bool) { return v.b, v.kind == KindBool }

// Str is v's string, and whether v is one.
func (v Value) Str() (string, bool) {
	if v.kind != KindString {
		return "", false
	}
	return v.s, true
}

// Number is v's literal text, and whether v is a number.
func (v Value) Number() (string, bool) {
	if v.kind != KindNumber {
		return "", false
	}
	return v.s, true
}

// IsInt reports a number written as an integer (Python reads it as int,
// not float).
func (v Value) IsInt() bool { return v.kind == KindNumber && isIntLiteral(v.s) }

// Int64 is v as an int64, when v is an integer literal that fits. A float
// such as 4.0 is not an int, as in Python.
func (v Value) Int64() (int64, bool) {
	if !v.IsInt() {
		return 0, false
	}
	i, err := strconv.ParseInt(v.s, 10, 64)
	return i, err == nil
}

// Float64 is v as a float64, for any number (Python's float(v)).
func (v Value) Float64() (float64, bool) {
	if v.kind != KindNumber {
		return 0, false
	}
	switch v.s {
	case "NaN":
		return math.NaN(), true
	case "Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	f, err := strconv.ParseFloat(v.s, 64)
	if err != nil && !isRangeErr(err) {
		return 0, false
	}
	return f, true
}

// Array is v's elements, and whether v is an array. The slice is v's own.
func (v Value) Array() ([]Value, bool) { return v.arr, v.kind == KindArray }

// Object is v's members, and whether v is an object. The slice is v's own.
func (v Value) Object() (Object, bool) { return v.obj, v.kind == KindObject }

// Get is the value of key when v is an object holding it, else null. Use
// Object().Lookup to tell a missing key from a null.
func (v Value) Get(key string) Value {
	if v.kind != KindObject {
		return Value{}
	}
	m, _ := v.obj.Lookup(key)
	return m
}

// Clone is a deep copy of v.
func (v Value) Clone() Value {
	switch v.kind {
	case KindArray:
		if v.arr == nil {
			return v
		}
		out := make([]Value, len(v.arr))
		for i, e := range v.arr {
			out[i] = e.Clone()
		}
		v.arr = out
	case KindObject:
		v.obj = v.obj.Clone()
	}
	return v
}

// Clone is a deep copy of o.
func (o Object) Clone() Object {
	if o == nil {
		return nil
	}
	out := make(Object, len(o))
	for i, m := range o {
		out[i] = Member{Key: m.Key, Value: m.Value.Clone()}
	}
	return out
}

// Index is the position of key, or -1.
func (o Object) Index(key string) int {
	return slices.IndexFunc(o, func(m Member) bool { return m.Key == key })
}

// Lookup is the value of key and whether o holds it.
func (o Object) Lookup(key string) (Value, bool) {
	if i := o.Index(key); i >= 0 {
		return o[i].Value, true
	}
	return Value{}, false
}

// Get is the value of key, or null.
func (o Object) Get(key string) Value {
	v, _ := o.Lookup(key)
	return v
}

// Keys are o's keys in order.
func (o Object) Keys() []string {
	out := make([]string, len(o))
	for i, m := range o {
		out[i] = m.Key
	}
	return out
}

// Set gives key the value v: in place when o holds it, appended otherwise
// (Python's d[key] = v).
func (o *Object) Set(key string, v Value) {
	if i := o.Index(key); i >= 0 {
		(*o)[i].Value = v
		return
	}
	*o = append(*o, Member{Key: key, Value: v})
}

// Delete removes key (Python's d.pop(key, None)) and reports whether it was
// there.
func (o *Object) Delete(key string) bool {
	i := o.Index(key)
	if i < 0 {
		return false
	}
	*o = slices.Delete(*o, i, i+1)
	return true
}

// Equal reports whether a and b encode to the same bytes: the same kinds,
// numbers that Python writes the same way (1.0 and 1.00, but not 1 and
// 1.0), the same strings, and objects with the same members in the same
// order. The ticket codec uses it to keep an unchanged value as it was.
func Equal(a, b Value) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case KindNull:
		return true
	case KindBool:
		return a.b == b.b
	case KindString:
		return a.s == b.s
	case KindNumber:
		return a.s == b.s || pyNumber(a.s) == pyNumber(b.s)
	case KindArray:
		return slices.EqualFunc(a.arr, b.arr, Equal)
	case KindObject:
		return slices.EqualFunc(a.obj, b.obj, func(x, y Member) bool { return x.Key == y.Key && Equal(x.Value, y.Value) })
	}
	return false
}
