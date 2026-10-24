package stage

import (
	"bytes"
	"strconv"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// An agent writes report.json, so any value in it can be anything JSON
// holds. The finishers read it as Python's dict accesses do: report.get(k)
// compared with a string, str() of whatever is there, `x or y` taking y
// when x is falsy, int() of a vote. Report is that reading, over the parsed
// object the ticket stores verbatim.

// ParseReport is parse_report: the report as stored on the ticket.
// Invalid JSON (or bytes that are not UTF-8), or JSON that is not an
// object, becomes a blocked, failing report that says so, so a garbage
// report always fails closed.
//
// The report is what json.loads made of the file, so it is stored as
// json.dump writes that back: a repeated key keeps its first place and its
// last value, and numbers are Python's ints and floats (2.50 is written
// 2.5, 1e2 is 100.0), not the agent's spelling.
func ParseReport(data []byte) jsonx.Value {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // json.loads(bytes) reads UTF-8 with a BOM
	if !utf8.Valid(data) {
		return failed("report.json is not valid JSON")
	}
	v, err := jsonx.Parse(data)
	if err != nil {
		return failed("report.json is not valid JSON")
	}
	if _, ok := v.Object(); !ok {
		return failed("report.json is not a JSON object")
	}
	return pythonNumbers(v)
}

// pythonNumbers rewrites every number in v as Python writes the int or
// float json.loads made of it.
func pythonNumbers(v jsonx.Value) jsonx.Value {
	switch v.Kind() {
	case jsonx.KindNumber:
		if v.IsInt() {
			if n, ok := v.Int64(); ok {
				return jsonx.Int(n)
			}
			return v // beyond int64: Python prints the same digits
		}
		f, _ := v.Float64()
		return jsonx.Float(f)
	case jsonx.KindArray:
		a, _ := v.Array()
		out := make([]jsonx.Value, len(a))
		for i, e := range a {
			out[i] = pythonNumbers(e)
		}
		return jsonx.ArrayOf(out...)
	case jsonx.KindObject:
		o, _ := v.Object()
		out := make(jsonx.Object, len(o))
		for i, m := range o {
			out[i] = jsonx.Member{Key: m.Key, Value: pythonNumbers(m.Value)}
		}
		return out.Value()
	}
	return v
}

func failed(summary string) jsonx.Value {
	return jsonx.ObjectOf(jsonx.Member{Key: "status", Value: jsonx.String("blocked")},
		jsonx.Member{Key: "verdict", Value: jsonx.String("fail")},
		jsonx.Member{Key: "summary", Value: jsonx.String(summary)})
}

// Report is a stage's report.json as the finishers read it. Its methods
// are Python's dict accesses on the report object; a report that is not an
// object reads as empty.
type Report struct{ o jsonx.Object }

// ReadReport is the reading of a parsed report.
func ReadReport(v jsonx.Value) Report {
	o, _ := v.Object()
	return Report{o: o}
}

// Value is report.get(key) and whether the key is there.
func (r Report) Value(key string) (jsonx.Value, bool) { return r.o.Lookup(key) }

// Is is report.get(key) == want, for a string want.
func (r Report) Is(key, want string) bool {
	v, _ := r.o.Lookup(key)
	s, ok := v.Str()
	return ok && s == want
}

// Str is str(report.get(key, "")).
func (r Report) Str(key string) string {
	v, ok := r.o.Lookup(key)
	if !ok {
		return ""
	}
	return jsonx.PyStr(v)
}

// Or is str(report.get(key) or fallback).
func (r Report) Or(key, fallback string) string {
	v, _ := r.o.Lookup(key)
	if !jsonx.Truthy(v) {
		return fallback
	}
	return jsonx.PyStr(v)
}

// Truthy is bool(report.get(key)).
func (r Report) Truthy(key string) bool {
	v, _ := r.o.Lookup(key)
	return jsonx.Truthy(v)
}

// Items is iterating report.get(key) or []: a list's elements; a string's
// characters and an object's keys, as Python iterates them. Anything else
// would raise in Python, and reads as nothing.
func (r Report) Items(key string) []jsonx.Value {
	v, _ := r.o.Lookup(key)
	if !jsonx.Truthy(v) {
		return nil
	}
	return pyIter(v)
}

// List is report.get(key) when it is a list (isinstance(x, list)), else
// nothing.
func (r Report) List(key string) []jsonx.Value {
	v, _ := r.o.Lookup(key)
	a, _ := v.Array()
	return a
}

func pyIter(v jsonx.Value) []jsonx.Value {
	switch v.Kind() {
	case jsonx.KindArray:
		a, _ := v.Array()
		return a
	case jsonx.KindString:
		s, _ := v.Str()
		var out []jsonx.Value
		for _, r := range s {
			out = append(out, jsonx.String(string(r)))
		}
		return out
	case jsonx.KindObject:
		o, _ := v.Object()
		out := make([]jsonx.Value, len(o))
		for i, m := range o {
			out[i] = jsonx.String(m.Key)
		}
		return out
	}
	return nil
}

func hex(r rune, n int) string {
	s := strconv.FormatInt(int64(r), 16)
	for len(s) < n {
		s = "0" + s
	}
	return s
}

// pyInt is Python's int(v) for a value json.loads made, and false where
// int() raises KeyError, TypeError or ValueError (finish_vote skips those):
// booleans are 0 and 1, floats are cut toward zero, strings are read as
// int() reads them (Python whitespace, a sign, Unicode decimal digits and
// single underscores between them). A value beyond int64 is false too: no
// vote index or score is that large.
func pyInt(v jsonx.Value) (int64, bool) {
	switch v.Kind() {
	case jsonx.KindBool:
		if b, _ := v.Bool(); b {
			return 1, true
		}
		return 0, true
	case jsonx.KindNumber:
		if v.IsInt() {
			return v.Int64()
		}
		f, ok := v.Float64()
		if !ok || f != f || f >= 1<<63 || f <= -(1<<63) {
			return 0, false
		}
		return int64(f), true
	case jsonx.KindString:
		s, _ := v.Str()
		return pyIntString(textx.Strip(s))
	}
	return 0, false
}

func pyIntString(s string) (int64, bool) {
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	if s == "" {
		return 0, false
	}
	var n int64
	prevDigit := false
	for _, r := range s {
		if r == '_' {
			if !prevDigit {
				return 0, false
			}
			prevDigit = false
			continue
		}
		d, ok := decimalValue(r)
		if !ok {
			return 0, false
		}
		if n > (1<<62)/5 {
			return 0, false
		}
		n = n*10 + int64(d)
		prevDigit = true
	}
	if !prevDigit {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// decimalValue is the value of a Unicode decimal digit (Nd): its distance
// from the start of its run of ten.
func decimalValue(r rune) (int, bool) {
	if !textx.IsDecimal(r) {
		return 0, false
	}
	start := r
	for start > 0 && textx.IsDecimal(start-1) {
		start--
	}
	return int(r-start) % 10, true
}
