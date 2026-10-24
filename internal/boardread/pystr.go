package boardread

import (
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// index_entry reads a ticket as the dict json.load made of it: `x or y`
// takes y when x is falsy, and the byte counts call len(str(x)) on whatever
// is there. These are those readings. (Package ticket has its own unexported
// copy for the asks; they must agree, and both follow Python.)

// or is Python's `v or def`.
func or(v jsonx.Value, def jsonx.Value) jsonx.Value {
	if jsonx.Truthy(v) {
		return v
	}
	return def
}

// pyLen is len(s) of a Python str: code points, where a lone surrogate
// (kept by jsonx as its three WTF-8 bytes) is one.
func pyLen(s string) int {
	n := 0
	for i := 0; i < len(s); n++ {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 && isSurrogate(s[i:]) {
			size = 3
		}
		i += size
	}
	return n
}

// isSurrogate: s starts with the WTF-8 bytes of U+D800..U+DFFF.
func isSurrogate(s string) bool {
	return len(s) >= 3 && s[0] == 0xED && s[1] >= 0xA0 && s[1] <= 0xBF && s[2] >= 0x80 && s[2] <= 0xBF
}
