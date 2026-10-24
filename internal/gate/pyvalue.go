package gate

import (
	"fmt"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The audit and the gate read a ticket as Python's dict accesses do: report
// values are whatever an agent wrote, and the audit prints them with
// f-strings. A node is the ticket's encoded tree (jsonx keeps key order and
// number literals as Python's json.loads sees them) with the dict and list
// operations the audit uses, each failing as Python would; str() and
// repr() are jsonx's.
type node struct{ v jsonx.Value }

func stringNode(s string) node { return node{jsonx.String(s)} }
func objectNode() node         { return node{jsonx.ObjectOf()} }
func listNode() node           { return node{jsonx.ArrayOf()} }

// pyTicket is the ticket as it would be saved now, as a tree.
func pyTicket(t *ticket.Ticket) (node, error) {
	v, err := jsonx.Parse(ticket.Encode(ticket.Clone(t)))
	if err != nil {
		return node{}, fmt.Errorf("gate: read ticket %s: %w", t.ID, err)
	}
	return node{v}, nil
}

func (n node) typeName() string {
	return map[jsonx.Kind]string{jsonx.KindNull: "NoneType", jsonx.KindBool: "bool", jsonx.KindNumber: "int",
		jsonx.KindString: "str", jsonx.KindArray: "list", jsonx.KindObject: "dict"}[n.v.Kind()]
}

// isStr reports a string equal to s.
func (n node) isStr(s string) bool {
	x, ok := n.v.Str()
	return ok && x == s
}

// text is a string's text, and whether it is a string.
func (n node) text() (string, bool) { return n.v.Str() }

func (n node) isList() bool { return n.v.Kind() == jsonx.KindArray }
func (n node) isDict() bool { return n.v.Kind() == jsonx.KindObject }

// items are a list's items (none for anything else).
func (n node) items() []node {
	a, _ := n.v.Array()
	out := make([]node, len(a))
	for i, e := range a {
		out[i] = node{e}
	}
	return out
}

// get is the value of key in an object (the last one, as json.loads keeps).
func (n node) get(key string) (node, bool) {
	o, ok := n.v.Object()
	if !ok {
		return node{}, false
	}
	for i := len(o) - 1; i >= 0; i-- {
		if o[i].Key == key {
			return node{o[i].Value}, true
		}
	}
	return node{}, false
}

// index is n[key] on a dict: a KeyError when it is missing.
func (n node) index(key string) (node, error) {
	if !n.isDict() {
		return node{}, fmt.Errorf("%q object is not subscriptable by %q", n.typeName(), key)
	}
	v, ok := n.get(key)
	if !ok {
		return node{}, fmt.Errorf("KeyError: %s", jsonx.PyReprString(key))
	}
	return v, nil
}

// dictGet is n.get(key, def): an AttributeError when n is not a dict.
func (n node) dictGet(key string, def node) (node, error) {
	if !n.isDict() {
		return node{}, fmt.Errorf("AttributeError: %s object has no attribute 'get' (reading %s)",
			jsonx.PyReprString(n.typeName()), jsonx.PyReprString(key))
	}
	if v, ok := n.get(key); ok {
		return v, nil
	}
	return def, nil
}

// iter is what a for loop over n visits: a list's items, a dict's keys
// (each once), a string's characters; anything else is a TypeError.
func (n node) iter() ([]node, error) {
	switch n.v.Kind() {
	case jsonx.KindArray:
		return n.items(), nil
	case jsonx.KindObject:
		o, _ := n.v.Object()
		var out []node
		seen := map[string]bool{}
		for _, m := range o {
			if !seen[m.Key] {
				seen[m.Key] = true
				out = append(out, stringNode(m.Key))
			}
		}
		return out, nil
	case jsonx.KindString:
		s, _ := n.v.Str()
		var out []node
		for _, r := range s {
			out = append(out, stringNode(string(r)))
		}
		return out, nil
	}
	return nil, fmt.Errorf("TypeError: %s object is not iterable", jsonx.PyReprString(n.typeName()))
}

// length is len(n).
func (n node) length() (int, error) {
	switch n.v.Kind() {
	case jsonx.KindArray, jsonx.KindObject:
		keys, _ := n.iter()
		if n.isList() {
			return len(n.items()), nil
		}
		return len(keys), nil
	case jsonx.KindString:
		s, _ := n.v.Str()
		return utf8.RuneCountInString(s), nil
	}
	return 0, fmt.Errorf("TypeError: object of type %s has no len()", jsonx.PyReprString(n.typeName()))
}

// truthy is bool(n).
func (n node) truthy() bool { return jsonx.Truthy(n.v) }

// str is Python's str(n) (and f"{n}").
func (n node) str() string { return jsonx.PyStr(n.v) }

// repr is Python's repr(n) of what json.loads made.
func (n node) repr() string { return jsonx.PyRepr(n.v) }
