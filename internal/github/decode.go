package github

import (
	"fmt"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// The typed answers decode leniently, as Python's dict.get did: a missing or
// wrongly typed field is its zero value, and Raw keeps the whole answer for
// anything the types leave out. Only the outer shape is checked (an object
// where Python indexed a dict, a list of objects where it iterated dicts).

// str is v[key] when it is a string, else "".
func str(v jsonx.Value, key string) string {
	s, _ := v.Get(key).Str()
	return s
}

// num is v[key] when it is an integer, else 0.
func num(v jsonx.Value, key string) int64 {
	n, _ := v.Get(key).Int64()
	return n
}

func object(what string, v jsonx.Value) error {
	if v.Kind() != jsonx.KindObject {
		return fmt.Errorf("github: %s: expected an object, got %s", what, v.Kind())
	}
	return nil
}

// objects is v as a list of objects.
func objects(what string, v jsonx.Value) ([]jsonx.Value, error) {
	items, ok := v.Array()
	if !ok {
		return nil, fmt.Errorf("github: %s: expected a list, got %s", what, v.Kind())
	}
	for i, it := range items {
		if it.Kind() != jsonx.KindObject {
			return nil, fmt.Errorf("github: %s: item %d is %s, not an object", what, i, it.Kind())
		}
	}
	return items, nil
}
