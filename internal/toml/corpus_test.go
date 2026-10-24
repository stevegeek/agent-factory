package toml

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// corpusFile holds every corpus document with what Python's tomllib made of
// it, written from python3 tomllib before the cutover and static since,
// so the suite checks against tomllib without Python.
const corpusFile = "testdata/py/tomllib.json"

type corpusCase struct {
	Name string `json:"name"`
	TOML string `json:"toml,omitempty"`
	// TOMLBase64 holds a document that is not valid UTF-8 (JSON cannot).
	TOMLBase64 string `json:"toml_base64,omitempty"`
	// Repr is repr(tomllib.loads(TOML)), empty when tomllib refused it.
	Repr string `json:"repr,omitempty"`
	// Error is tomllib's message when it refused the document.
	Error string `json:"error,omitempty"`
	// Refused marks a document tomllib reads but the subset refuses on
	// purpose (dates, dotted keys, integers beyond 64 bits).
	Refused bool `json:"refused,omitempty"`
}

type corpus struct {
	About  string       `json:"about"`
	Python string       `json:"python"`
	Cases  []corpusCase `json:"cases"`
}

// corpusDocs is every document the oracle sees: the spec table, the
// example config and the generated documents.
func corpusDocs(t testing.TB) []corpusCase {
	var docs []corpusCase
	for _, c := range specInputs() {
		docs = append(docs, corpusCase{Name: "spec/" + c[0], TOML: c[1]})
	}
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "config", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	docs = append(docs, corpusCase{Name: "examples/config/config.toml", TOML: string(example)})
	r := rand.New(rand.NewPCG(83, 2026))
	for i := range 200 {
		g := &gen{r: r}
		doc := g.document()
		if i%5 == 4 {
			doc = g.mutate(doc)
		}
		docs = append(docs, corpusCase{Name: fmt.Sprintf("gen/%03d", i), TOML: doc})
	}
	return docs
}

// specInputs lists the spec table's documents (name, toml), so the oracle
// checks the table's expectations too.
func specInputs() [][2]string {
	var out [][2]string
	for _, c := range specCases() {
		out = append(out, [2]string{c.name, c.in})
	}
	return out
}

// stored gives the case as the golden keeps it.
func (c corpusCase) stored() corpusCase {
	if !utf8.ValidString(c.TOML) {
		c.TOMLBase64, c.TOML = base64.StdEncoding.EncodeToString([]byte(c.TOML)), ""
	}
	return c
}

// loaded turns a golden case back into its document.
func (c corpusCase) loaded() (corpusCase, error) {
	if c.TOMLBase64 != "" {
		b, err := base64.StdEncoding.DecodeString(c.TOMLBase64)
		if err != nil {
			return c, err
		}
		c.TOML, c.TOMLBase64 = string(b), ""
	}
	return c, nil
}

func TestCorpusMatchesTomllib(t *testing.T) {
	b, err := os.ReadFile(corpusFile)
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	docs := corpusDocs(t)
	if len(c.Cases) != len(docs) {
		t.Fatalf("%s has %d cases, the corpus %d: regenerate it", corpusFile, len(c.Cases), len(docs))
	}
	for i, want := range c.Cases {
		want, err := want.loaded()
		if err != nil {
			t.Fatal(err)
		}
		if docs[i].TOML != want.TOML || docs[i].Name != want.Name {
			t.Fatalf("case %d (%s) changed since %s was written: regenerate it", i, want.Name, corpusFile)
		}
		checkAgainstTomllib(t, want)
	}
}

func checkAgainstTomllib(t *testing.T, want corpusCase) {
	t.Helper()
	tab, err := Parse([]byte(want.TOML))
	switch {
	case want.Refused:
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: tomllib reads it and the subset refuses it, but Parse = %v, %v\n%s", want.Name, err, reprOrNil(tab), want.TOML)
		}
	case want.Error != "":
		if err == nil {
			t.Errorf("%s: tomllib refused it (%s) but Parse accepted %s\n%q", want.Name, want.Error, PyRepr(tab), want.TOML)
		}
	default:
		if err != nil {
			t.Errorf("%s: Parse: %v, tomllib read %s\n%q", want.Name, err, want.Repr, want.TOML)
		} else if got := PyRepr(tab); got != want.Repr {
			t.Errorf("%s:\n   go %s\npython %s\n%q", want.Name, got, want.Repr, want.TOML)
		}
	}
}

func reprOrNil(t *Table) string {
	if t == nil {
		return "nil"
	}
	return PyRepr(t)
}

// gen writes random documents in (mostly) the subset.
type gen struct {
	r     *rand.Rand
	names []string
}

func (g *gen) pick(xs ...string) string { return xs[g.r.IntN(len(xs))] }

func (g *gen) key() string {
	switch g.r.IntN(6) {
	case 0:
		return g.basic(g.text(4))
	case 1:
		return "'" + strings.NewReplacer("'", "", "\n", "", "\r", "").Replace(g.text(4)) + "'"
	default:
		const bare = "abcxyzAZ09_-"
		n := 1 + g.r.IntN(6)
		var b strings.Builder
		for range n {
			b.WriteByte(bare[g.r.IntN(len(bare))])
		}
		return b.String()
	}
}

// text is a short string of runes chosen to stress escaping and repr:
// quotes, backslashes, controls, non-ASCII letters and digits, NEL, LS, PS,
// zero-width and astral characters.
func (g *gen) text(max int) string {
	pool := []string{"a", "Z", "0", " ", "'", "\"", "\\", "\t", "é", "ß", "中", "٣", "１", "\u0301", "\u0085",
		"\u00a0", "\u2028", "\u2029", "\u200b", "\ufeff", "😀", "\U000E0001", "\x1c", "\x1f", "#", "=", "[", "]", "{", "}", ","}
	n := g.r.IntN(max + 1)
	var b strings.Builder
	for range n {
		b.WriteString(pool[g.r.IntN(len(pool))])
	}
	return b.String()
}

func (g *gen) basic(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\t' && g.r.IntN(2) == 0:
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		case r > 0x7f && g.r.IntN(4) == 0:
			if r > 0xffff {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (g *gen) value(depth int) string {
	n := 9
	if depth > 2 {
		n = 7
	}
	switch g.r.IntN(n) {
	case 0:
		return g.basic(g.text(8))
	case 1:
		s := strings.NewReplacer("'", "", "\n", "", "\r", "", "\x1c", "", "\x1f", "").Replace(g.text(8))
		return "'" + s + "'"
	case 2:
		return g.pick(`"""`+"\n"+`line one
line "two" \
   continued"""`, "'''\nraw \\n 'x'\n'''", `"""a""""`, `"""\u00e9\t"""`, "'''''x'''''")
	case 3:
		return g.pick("0", "1", "-17", "+42", "1_000_000", "0xff", "0o17", "0b101", "9223372036854775807", "-9223372036854775808")
	case 4:
		return g.pick("1.5", "-0.0", "3e2", "6.02e+23", "1e-7", "2.5E-3", "1_0.0_1", "inf", "-inf", "nan", "1e16", "1e15", "123.456")
	case 5:
		return g.pick("true", "false")
	case 6:
		return g.pick("[]", "[ ]", "[1, 2, 3]", "['a', \"b\"]", "[\n  1, # c\n  2,\n]")
	case 7:
		k := 1 + g.r.IntN(3)
		parts := make([]string, k)
		for i := range parts {
			parts[i] = g.value(depth + 1)
		}
		return "[" + strings.Join(parts, ", ") + g.pick("", ",") + "]"
	default:
		k := g.r.IntN(3)
		seen := map[string]bool{}
		var parts []string
		for range k {
			key := g.key()
			if seen[key] {
				continue
			}
			seen[key] = true
			parts = append(parts, key+" = "+g.value(depth+1))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
}

func (g *gen) document() string {
	var b strings.Builder
	nl := "\n"
	if g.r.IntN(8) == 0 {
		nl = "\r\n"
	}
	keyvals := func() {
		seen := map[string]bool{}
		for range g.r.IntN(4) {
			k := g.key()
			if seen[k] {
				continue
			}
			seen[k] = true
			b.WriteString(k + g.pick(" = ", "=", "\t=  ") + g.value(0))
			if g.r.IntN(4) == 0 {
				b.WriteString(" # " + strings.NewReplacer("\n", "", "\r", "", "\x1c", "", "\x1f", "").Replace(g.text(5)))
			}
			b.WriteString(nl)
		}
	}
	keyvals()
	headers := []string{"a", "b", "a.x", "c.\"q k\".d", "hosts.one", "projects.gem"}
	for range g.r.IntN(4) {
		h := headers[g.r.IntN(len(headers))]
		if g.r.IntN(3) == 0 {
			b.WriteString(nl + "[[" + h + "]]" + nl)
		} else {
			b.WriteString(nl + "[" + h + "]" + nl)
		}
		keyvals()
	}
	return b.String()
}

// mutate breaks a document a little, so the corpus has refusals too.
func (g *gen) mutate(doc string) string {
	if doc == "" {
		return "="
	}
	i := g.r.IntN(len(doc))
	switch g.r.IntN(3) {
	case 0:
		return doc[:i] + doc[i+1:]
	case 1:
		return doc[:i] + g.pick("\"", "'", "[", "]", "=", "\\", ",", "{", "\n", ".") + doc[i:]
	default:
		return doc[:i] + doc[i:] + g.pick("\n=1", "\n[", "\na = 1\na = 2", "\nx = \"\\q\"")
	}
}
