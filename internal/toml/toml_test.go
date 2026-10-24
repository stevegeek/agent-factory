package toml

import (
	"errors"
	"strings"
	"testing"
)

// The spec table: one row per construct of the subset (§8.3). want is the
// Python repr of what tomllib returns (dict order is insertion order), so a
// row reads like the tomllib oracle; err is a substring of the error.
type specCase struct{ name, in, want, err string }

func specCases() []specCase {
	return []specCase{
		// documents, whitespace and comments
		{"empty", "", "{}", ""},
		{"only comments", "# a\n  # b\n\n", "{}", ""},
		{"crlf", "a = 1\r\nb = 2\r\n", "{'a': 1, 'b': 2}", ""},
		{"no final newline", "a = 1", "{'a': 1}", ""},
		{"trailing comment", "a = 1 # one\n", "{'a': 1}", ""},
		{"tab whitespace", "\ta\t=\t1\t\n", "{'a': 1}", ""},
		{"lone cr", "a = 1\rb = 2\n", "", "Expected newline"},
		{"control in comment", "# a\x01b\n", "", "comment"},
		{"del in comment", "# a\x7f\n", "", "comment"},
		{"tab in comment", "# a\tb\n", "{}", ""},
		{"bad utf8", "a = \"\xff\"\n", "", "UTF-8"},
		{"bom", "\ufeffa = 1\n", "", "Invalid statement"},

		// keys
		{"bare keys", "a-b_C9 = 1\n1234 = 2\n-- = 3\n", "{'a-b_C9': 1, '1234': 2, '--': 3}", ""},
		{"quoted keys", "\"a b\" = 1\n'c.d' = 2\n\"\" = 3\n\"\\u00e9\" = 4\n", "{'a b': 1, 'c.d': 2, '': 3, 'é': 4}", ""},
		{"duplicate key", "a = 1\na = 2\n", "", "overwrite"},
		{"duplicate quoted key", "a = 1\n\"a\" = 2\n", "", "overwrite"},
		{"dotted key refused", "a.b = 1\n", "", "dotted keys are not supported"},
		{"dotted key with spaces refused", "a . b = 1\n", "", "dotted keys are not supported"},
		{"no value", "a =\n", "", "Invalid value"},
		{"no equals", "a 1\n", "", "Expected '='"},
		{"two on a line", "a = 1 b = 2\n", "", "Expected newline"},
		{"bare key bad char", "a$ = 1\n", "", "Expected '='"},
		{"multiline key refused", "\"\"\"a\"\"\" = 1\n", "", "key"},

		// tables
		{"table", "[a]\nb = 1\n", "{'a': {'b': 1}}", ""},
		{"empty table", "[a]\n", "{'a': {}}", ""},
		{"dotted header", "[a.b]\nc = 1\n", "{'a': {'b': {'c': 1}}}", ""},
		{"header spaces", "[ a . \"b c\" . 'd' ]\nx = 1\n", "{'a': {'b c': {'d': {'x': 1}}}}", ""},
		{"implicit then explicit", "[a.b]\nc = 1\n[a]\nd = 2\n", "{'a': {'b': {'c': 1}, 'd': 2}}", ""},
		{"explicit then sub", "[a]\nd = 2\n[a.b]\nc = 1\n", "{'a': {'d': 2, 'b': {'c': 1}}}", ""},
		{"table twice", "[a]\n[a]\n", "", "twice"},
		{"sub table twice", "[a.b]\n[a.b]\n", "", "twice"},
		{"table over value", "a = 1\n[a]\n", "", ""},
		{"sub table over value", "[a]\nb = 1\n[a.b]\n", "", "overwrite"},
		{"table into inline", "a = {b = 1}\n[a.c]\n", "", ""},
		{"table over inline", "a = {}\n[a]\n", "", ""},
		{"table into array", "a = [1]\n[a.b]\n", "", ""},
		{"unclosed header", "[a\n", "", "]"},
		{"empty header", "[]\n", "", "key"},
		{"header then junk", "[a] x\n", "", "Expected newline"},
		{"header comment", "[a] # c\nb = 1\n", "{'a': {'b': 1}}", ""},
		{"order kept", "z = 1\n[y]\n[x]\n[w.v]\n", "{'z': 1, 'y': {}, 'x': {}, 'w': {'v': {}}}", ""},
		{"root keys after table belong to it", "[a]\nb = 1\nc = 2\n", "{'a': {'b': 1, 'c': 2}}", ""},

		// arrays of tables
		{"aot", "[[a]]\nb = 1\n[[a]]\nb = 2\n", "{'a': [{'b': 1}, {'b': 2}]}", ""},
		{"aot empty element", "[[a]]\n[[a]]\nb = 1\n", "{'a': [{}, {'b': 1}]}", ""},
		{"aot sub table", "[[a]]\n[a.b]\nc = 1\n[[a]]\n[a.b]\nc = 2\n", "{'a': [{'b': {'c': 1}}, {'b': {'c': 2}}]}", ""},
		{"aot nested", "[[a]]\nx = 1\n[[a.b]]\ny = 1\n[[a.b]]\ny = 2\n[[a]]\nx = 2\n",
			"{'a': [{'x': 1, 'b': [{'y': 1}, {'y': 2}]}, {'x': 2}]}", ""},
		{"aot under table", "[p]\n[[p.q]]\nr = 1\n", "{'p': {'q': [{'r': 1}]}}", ""},
		{"aot implicit parent then explicit", "[[a.b]]\n[a]\nc = 1\n", "{'a': {'b': [{}], 'c': 1}}", ""},
		{"aot over table", "[a]\n[[a]]\n", "", ""},
		{"table over aot", "[[a]]\n[a]\n", "", "twice"},
		{"aot over static array", "a = []\n[[a]]\n", "", ""},
		{"aot over value", "a = 1\n[[a]]\n", "", ""},
		{"aot unclosed", "[[a]\n", "", "]]"},
		{"aot spaced brackets", "[ [a]]\n", "", ""},
		{"aot spaces inside", "[[ a ]]\n", "{'a': [{}]}", ""},

		// strings
		{"basic", `a = "x y"` + "\n", "{'a': 'x y'}", ""},
		{"basic escapes", `a = "\b\t\n\f\r\"\\\u00e9\U0001F600"` + "\n", `{'a': '\x08\t\n\x0c\r"\\é😀'}`, ""},
		{"basic tab", "a = \"x\ty\"\n", "{'a': 'x\\ty'}", ""},
		{"basic bad escape", `a = "\e"` + "\n", "", "Unescaped"},
		{"basic x escape", `a = "\x41"` + "\n", "", "Unescaped"},
		{"basic surrogate", `a = "\ud800"` + "\n", "", "scalar"},
		{"basic too big", `a = "\U00110000"` + "\n", "", "scalar"},
		{"basic short u", `a = "\u12"` + "\n", "", ""},
		{"basic control", "a = \"x\x01\"\n", "", "Illegal character"},
		{"basic newline", "a = \"x\ny\"\n", "", ""},
		{"basic unclosed", "a = \"x\n", "", ""},
		{"basic quote repr", `a = "it's"` + "\n", `{'a': "it's"}`, ""},
		{"basic both quotes repr", `a = "it's \"q\""` + "\n", `{'a': 'it\'s "q"'}`, ""},
		{"literal", "a = 'C:\\x\\y'\n", `{'a': 'C:\\x\\y'}`, ""},
		{"literal control", "a = 'x\x01'\n", "", "Illegal character"},
		{"literal unclosed", "a = 'x\n", "", ""},
		{"ml basic", "a = \"\"\"\nx\ny\"\"\"\n", "{'a': 'x\\ny'}", ""},
		{"ml basic first newline crlf", "a = \"\"\"\r\nx\"\"\"\n", "{'a': 'x'}", ""},
		{"ml basic crlf inside", "a = \"\"\"x\r\ny\"\"\"\n", "{'a': 'x\\ny'}", ""},
		{"ml basic line ending backslash", "a = \"\"\"x \\\n   \n  y\"\"\"\n", "{'a': 'x y'}", ""},
		{"ml basic backslash spaces", "a = \"\"\"x\\  \n  y\"\"\"\n", "{'a': 'xy'}", ""},
		{"ml basic backslash then text", "a = \"\"\"x\\  y\"\"\"\n", "", "Unescaped"},
		{"ml basic quotes inside", "a = \"\"\"a \"\" b\"\"\"\n", `{'a': 'a "" b'}`, ""},
		{"ml basic one closing quote", "a = \"\"\"a\"\"\"\"\n", `{'a': 'a"'}`, ""},
		{"ml basic two closing quotes", "a = \"\"\"a\"\"\"\"\"\n", `{'a': 'a""'}`, ""},
		{"ml basic three closing quotes", "a = \"\"\"a\"\"\"\"\"\"\n", "", ""},
		{"ml basic empty", "a = \"\"\"\"\"\"\n", "{'a': ''}", ""},
		{"ml basic escapes", "a = \"\"\"\\t\\u0041\"\"\"\n", "{'a': '\\tA'}", ""},
		{"ml basic control", "a = \"\"\"\x01\"\"\"\n", "", "Illegal character"},
		{"ml basic unclosed", "a = \"\"\"x\n", "", ""},
		{"ml literal", "a = '''\nx\\n\n'y'\n'''\n", `{'a': "x\\n\n'y'\n"}`, ""},
		{"ml literal closing quotes", "a = '''x'''''\n", `{'a': "x''"}`, ""},
		{"ml literal control", "a = '''\x01'''\n", "", "Illegal character"},
		{"ml literal tab", "a = '''\t'''\n", "{'a': '\\t'}", ""},
		{"non ascii repr", "a = \"é\u00a0\u2028\x7f\"\n", "", "Illegal character"},
		{"non printable repr", "a = \"é\u00a0\u2028\u200b\"\n", `{'a': 'é\xa0\u2028\u200b'}`, ""},
		{"astral non printable", "a = \"\U000E0001\"\n", `{'a': '\U000e0001'}`, ""},

		// integers
		{"ints", "a = 1\nb = +2\nc = -3\nd = 0\ne = 1_000\nf = -0\ng = +0\n", "{'a': 1, 'b': 2, 'c': -3, 'd': 0, 'e': 1000, 'f': 0, 'g': 0}", ""},
		{"prefixed ints", "a = 0xDEAD_beef\nb = 0o755\nc = 0b1101\n", "{'a': 3735928559, 'b': 493, 'c': 13}", ""},
		{"int64 bounds", "a = 9223372036854775807\nb = -9223372036854775808\n", "{'a': 9223372036854775807, 'b': -9223372036854775808}", ""},
		{"int too big refused", "a = 9223372036854775808\n", "", "integers beyond 64 bits are not supported"},
		{"leading zero", "a = 01\n", "", "Expected newline"},
		{"double underscore", "a = 1__0\n", "", "Expected newline"},
		{"trailing underscore", "a = 1_\n", "", "Expected newline"},
		{"leading underscore", "a = _1\n", "", "Invalid value"},
		{"signed hex", "a = +0x1\n", "", "Expected newline"},
		{"uppercase prefix", "a = 0X1\n", "", "Expected newline"},
		{"hex no digits", "a = 0x\n", "", "Expected newline"},

		// floats
		{"floats", "a = 1.5\nb = -0.0\nc = 1e3\nd = 6.26e-34\ne = 1_0.0_1\nf = 5E+22\ng = 1.0\n",
			"{'a': 1.5, 'b': -0.0, 'c': 1000.0, 'd': 6.26e-34, 'e': 10.01, 'f': 5e+22, 'g': 1.0}", ""},
		{"float repr range", "a = 1e16\nb = 1e15\nc = 0.0001\nd = 0.00001\ne = 123456789.123\n",
			"{'a': 1e+16, 'b': 1000000000000000.0, 'c': 0.0001, 'd': 1e-05, 'e': 123456789.123}", ""},
		{"special floats", "a = inf\nb = +inf\nc = -inf\nd = nan\ne = +nan\nf = -nan\n",
			"{'a': inf, 'b': inf, 'c': -inf, 'd': nan, 'e': nan, 'f': nan}", ""},
		{"float overflow", "a = 1e400\n", "{'a': inf}", ""},
		{"float no int part", "a = .5\n", "", "Invalid value"},
		{"float no frac", "a = 1.\n", "", "Expected newline"},
		{"float exp no digits", "a = 1e\n", "", "Expected newline"},
		{"float leading zero", "a = 01.5\n", "", "Expected newline"},
		{"float zero exp", "a = 0e0\nb = 0.0\n", "{'a': 0.0, 'b': 0.0}", ""},

		// booleans
		{"bools", "a = true\nb = false\n", "{'a': True, 'b': False}", ""},
		{"bool case", "a = True\n", "", "Invalid value"},
		{"bool suffix", "a = truex\n", "", "Expected newline"},

		// dates refused
		{"date refused", "a = 1979-05-27\n", "", "dates and times are not supported"},
		{"datetime refused", "a = 1979-05-27T07:32:00Z\n", "", "dates and times are not supported"},
		{"time refused", "a = 07:32:00\n", "", "dates and times are not supported"},
		{"date in array refused", "a = [1979-05-27]\n", "", "dates and times are not supported"},

		// arrays
		{"arrays", "a = [1, 2]\nb = []\nc = [ ]\nd = [\"x\", 'y']\n", "{'a': [1, 2], 'b': [], 'c': [], 'd': ['x', 'y']}", ""},
		{"array multiline", "a = [\n  1, # one\n  2,\n  # two\n]\n", "{'a': [1, 2]}", ""},
		{"array mixed", "a = [1, 'x', 1.5, [true], {b = 1}]\n", "{'a': [1, 'x', 1.5, [True], {'b': 1}]}", ""},
		{"array nested", "a = [[1, 2], [[]]]\n", "{'a': [[1, 2], [[]]]}", ""},
		{"array trailing comma", "a = [1,]\n", "{'a': [1]}", ""},
		{"array double comma", "a = [1,,2]\n", "", "Invalid value"},
		{"array leading comma", "a = [,1]\n", "", "Invalid value"},
		{"array unclosed", "a = [1\n", "", "Unclosed array"},
		{"array no comma", "a = [1 2]\n", "", "Unclosed array"},
		{"array crlf", "a = [\r\n1,\r\n2\r\n]\r\n", "{'a': [1, 2]}", ""},

		// inline tables
		{"inline", "a = {b = 1, c = 'x'}\n", "{'a': {'b': 1, 'c': 'x'}}", ""},
		{"inline empty", "a = {}\nb = { }\n", "{'a': {}, 'b': {}}", ""},
		{"inline nested", "a = {b = {c = [1]}}\n", "{'a': {'b': {'c': [1]}}}", ""},
		{"inline trailing comma", "a = {b = 1,}\n", "", "key"},
		{"inline newline", "a = {\nb = 1}\n", "", "key"},
		{"inline duplicate", "a = {b = 1, b = 2}\n", "", "Duplicate"},
		{"inline dotted refused", "a = {b.c = 1}\n", "", "dotted keys are not supported"},
		{"inline unclosed", "a = {b = 1\n", "", "Unclosed inline table"},
		{"inline in array", "a = [{b = 1}, {b = 2}]\n", "{'a': [{'b': 1}, {'b': 2}]}", ""},
		{"inline env", "env = { BUNDLE_PATH = \"/x\" }\n", "{'env': {'BUNDLE_PATH': '/x'}}", ""},
	}
}

func TestParseSpecTable(t *testing.T) {
	for _, c := range specCases() {
		t.Run(c.name, func(t *testing.T) {
			tab, err := Parse([]byte(c.in))
			if c.want == "" {
				if err == nil {
					t.Fatalf("Parse(%q) = %s, want an error", c.in, PyRepr(tab))
				}
				if !strings.Contains(err.Error(), c.err) {
					t.Fatalf("Parse(%q) error %q, want it to contain %q", c.in, err, c.err)
				}
				var pe *Error
				if !errors.As(err, &pe) || pe.Line < 1 || pe.Col < 1 {
					t.Fatalf("Parse(%q) error %#v carries no line and column", c.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.in, err)
			}
			if got := PyRepr(tab); got != c.want {
				t.Fatalf("Parse(%q)\n got %s\nwant %s", c.in, got, c.want)
			}
		})
	}
}

func TestErrorPosition(t *testing.T) {
	cases := []struct {
		in        string
		line, col int
	}{
		{"a = 1\nb = \n", 2, 5},
		{"a = 1\n[a]\n", 2, 3},
		{"é = 1\n", 1, 1},
		{"a = 'é' x\n", 1, 9},
		{"[a]\nb = 1\n[a.b]\n", 3, 5},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.in))
		var pe *Error
		if !errors.As(err, &pe) {
			t.Fatalf("Parse(%q) = %v, want *Error", c.in, err)
		}
		if pe.Line != c.line || pe.Col != c.col {
			t.Errorf("Parse(%q) at line %d column %d, want %d, %d (%v)", c.in, pe.Line, pe.Col, c.line, c.col, err)
		}
		if !strings.Contains(err.Error(), "(at line ") {
			t.Errorf("error %q does not say where, as tomllib does", err)
		}
	}
}

func TestTableAccess(t *testing.T) {
	tab, err := Parse([]byte("b = 1\na = 'x'\n[t]\nz = true\n[[l]]\nn = 1\n[[l]]\nn = 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tab.Keys(), ","); got != "b,a,t,l" {
		t.Fatalf("Keys = %s", got)
	}
	if tab.Len() != 4 {
		t.Fatalf("Len = %d", tab.Len())
	}
	keys := tab.Keys()
	keys[0] = "mutated"
	if tab.Keys()[0] != "b" {
		t.Fatal("Keys shares its slice with the table")
	}
	if v, ok := tab.Get("b"); !ok || v != int64(1) {
		t.Fatalf("Get(b) = %v, %v", v, ok)
	}
	if _, ok := tab.Get("nope"); ok {
		t.Fatal("Get(nope) found something")
	}
	sub, ok := tab.Table("t")
	if !ok || sub.Len() != 1 {
		t.Fatalf("Table(t) = %v, %v", sub, ok)
	}
	if _, ok := tab.Table("b"); ok {
		t.Fatal("Table(b) is not a table")
	}
	l, ok := tab.Get("l")
	arr, isArr := l.([]Value)
	if !ok || !isArr || len(arr) != 2 {
		t.Fatalf("Get(l) = %#v", l)
	}
	if el, ok := arr[1].(*Table); !ok || PyRepr(el) != "{'n': 2}" {
		t.Fatalf("l[1] = %#v", arr[1])
	}
	var nilTab *Table
	if nilTab.Len() != 0 || len(nilTab.Keys()) != 0 {
		t.Fatal("a nil table is not empty")
	}
	if _, ok := nilTab.Get("x"); ok {
		t.Fatal("a nil table has a key")
	}
}
