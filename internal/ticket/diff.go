package ticket

import (
	"strconv"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// Change is one difference between two tickets: the path of a value and its
// JSON before and after ("" where it is absent).
type Change struct {
	Path     string // "column", "pr.merge_requested", "history[12]"
	Old, New string
}

func (c Change) String() string {
	show := func(s string) string {
		if s == "" {
			return "(absent)"
		}
		return s
	}
	return c.Path + ": " + show(c.Old) + " -> " + show(c.New)
}

// Diff lists what changed from before to after, as each would be saved.
// Objects are compared key by key (after's order, then the keys after
// dropped), arrays element by element, and anything else by its encoding.
func Diff(before, after *Ticket) []Change {
	var out []Change
	diffValue("", before.Tree(), true, after.Tree(), true, &out)
	return out
}

func compact(v jsonx.Value) string {
	return string(jsonx.Format{Indent: -1}.Encode(v))
}

func diffValue(path string, a jsonx.Value, hasA bool, b jsonx.Value, hasB bool, out *[]Change) {
	switch {
	case !hasA && !hasB:
		return
	case !hasA:
		*out = append(*out, Change{Path: path, New: compact(b)})
		return
	case !hasB:
		*out = append(*out, Change{Path: path, Old: compact(a)})
		return
	}
	if ao, ok := a.Object(); ok {
		if bo, ok := b.Object(); ok {
			for _, m := range bo {
				av, has := ao.Lookup(m.Key)
				diffValue(join(path, m.Key), av, has, m.Value, true, out)
			}
			for _, m := range ao {
				if _, has := bo.Lookup(m.Key); !has {
					diffValue(join(path, m.Key), m.Value, true, jsonx.Null(), false, out)
				}
			}
			return
		}
	}
	if aa, ok := a.Array(); ok {
		if ba, ok := b.Array(); ok {
			for i := 0; i < len(aa) || i < len(ba); i++ {
				var av, bv jsonx.Value
				if i < len(aa) {
					av = aa[i]
				}
				if i < len(ba) {
					bv = ba[i]
				}
				diffValue(path+"["+strconv.Itoa(i)+"]", av, i < len(aa), bv, i < len(ba), out)
			}
			return
		}
	}
	if !jsonx.Equal(a, b) {
		*out = append(*out, Change{Path: path, Old: compact(a), New: compact(b)})
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
