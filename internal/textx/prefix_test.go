package textx

import "testing"

// Prefix is s[:n] in code points: the one copy every package uses.
func TestPrefix(t *testing.T) {
	for _, c := range []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 3, "hel"}, {"hello", 0, ""}, {"hello", 9, "hello"}, {"", 2, ""},
		{"héllo wörld", 7, "héllo w"}, {"日本語テキスト", 3, "日本語"}, {"a\xffb", 2, "a\xff"}, {"abc", -1, ""},
	} {
		if got := Prefix(c.s, c.n); got != c.want {
			t.Errorf("Prefix(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}
