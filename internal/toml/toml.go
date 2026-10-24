// Package toml is a strict parser for the subset of TOML 1.0 the factory's
// configs use. It reads what Python's tomllib reads, keeps the order keys
// were first defined in (Python dicts keep insertion order, and clock and
// sweep file tickets in that order), and refuses with a clear error what
// the subset leaves out: dates and times, dotted keys in a key/value pair,
// and integers beyond 64 bits.
//
// A parsed document is a *Table. Values are string, int64, float64, bool,
// []Value (an array, including an array of tables) or *Table.
package toml

import "fmt"

// Value is one TOML value: string, int64, float64, bool, []Value or *Table.
type Value = any

// Table is an ordered TOML table. The zero value is an empty table. A nil
// *Table reads as empty.
type Table struct {
	keys []string
	vals map[string]Value

	// Parse state, unused after Parse returns.
	explicit bool // declared by a [header] (or created by [[header]])
	inline   bool // an inline table: never extended by a header
}

// Keys returns the keys in the order they were first defined, as a new
// slice.
func (t *Table) Keys() []string {
	if t == nil {
		return nil
	}
	return append([]string(nil), t.keys...)
}

// Len is the number of keys.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.keys)
}

// Get returns the value under key.
func (t *Table) Get(key string) (Value, bool) {
	if t == nil {
		return nil, false
	}
	v, ok := t.vals[key]
	return v, ok
}

// Table returns the value under key when it is a table.
func (t *Table) Table(key string) (*Table, bool) {
	v, _ := t.Get(key)
	sub, ok := v.(*Table)
	return sub, ok
}

func (t *Table) set(key string, v Value) {
	if t.vals == nil {
		t.vals = map[string]Value{}
	}
	if _, ok := t.vals[key]; !ok {
		t.keys = append(t.keys, key)
	}
	t.vals[key] = v
}

// Error is a parse error at a 1-based line and column (in characters).
type Error struct {
	Line, Col int
	Msg       string
}

// Error reads like tomllib's: "<msg> (at line L, column C)".
func (e *Error) Error() string {
	return fmt.Sprintf("%s (at line %d, column %d)", e.Msg, e.Line, e.Col)
}
