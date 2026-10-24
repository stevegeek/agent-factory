package prflow

import (
	"strconv"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// Python's view of values read from tickets and GitHub's answers: len()
// and dict access of a json.loads result (str() and bool() are jsonx.PyStr
// and jsonx.Truthy). The PR body and the events quote such values, so the
// text must be Python's.

// pyLen is len(v) for a str, list or dict (0 for anything else, where Python
// would have raised).
func pyLen(v jsonx.Value) int {
	switch v.Kind() {
	case jsonx.KindString:
		s, _ := v.Str()
		return utf8.RuneCountInString(s)
	case jsonx.KindArray:
		a, _ := v.Array()
		return len(a)
	case jsonx.KindObject:
		o, _ := v.Object()
		return len(o)
	}
	return 0
}

// dictGet is d.get(key, def) where d is a dict, and def when d is not one.
func dictGet(d jsonx.Value, key string, def jsonx.Value) jsonx.Value {
	o, ok := d.Object()
	if !ok {
		return def
	}
	if v, ok := o.Lookup(key); ok {
		return v
	}
	return def
}

// dictOr is (d.get(key) or {}): the value when it is a non-empty dict,
// else an empty one.
func dictOr(d jsonx.Value, key string) jsonx.Value {
	v := dictGet(d, key, jsonx.Null())
	if v.Kind() != jsonx.KindObject {
		return jsonx.ObjectOf()
	}
	return v
}

// lastRunes is s[-n:] in code points.
func lastRunes(s string, n int) string {
	l := utf8.RuneCountInString(s)
	if l <= n {
		return s
	}
	skip := l - n
	for j := range s {
		if skip == 0 {
			return s[j:]
		}
		skip--
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }
