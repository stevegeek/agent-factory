package memstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The memory_key cases of the regex corpus (written from Python's
// MEMORY_KEY by tools/goldens.py into textx's testdata).
func TestKeyOKMatchesTheRegexGolden(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "textx", "testdata", "py", "regex.json"))
	if err != nil {
		t.Skipf("regex golden: %v", err)
	}
	var g struct {
		Patterns []struct {
			Name  string `json:"name"`
			Cases []struct {
				In  string          `json:"in"`
				Out json.RawMessage `json:"out"`
			} `json:"cases"`
		} `json:"patterns"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range g.Patterns {
		if p.Name != "memory_key" {
			continue
		}
		for _, c := range p.Cases {
			n++
			want := string(c.Out) != "null"
			if want && strings.HasSuffix(c.In, "\n") {
				// Python's `$` lets one trailing newline through; a store
				// name with a newline in it is refused here (a deliberate
				// tightening: no config can name one).
				want = false
			}
			if got := KeyOK(c.In); got != want {
				t.Errorf("KeyOK(%q) = %v, want %v", c.In, got, want)
			}
		}
	}
	if n == 0 {
		t.Fatal("no memory_key cases in the regex golden")
	}
}

// port: TF MemoryTest.test_a_store_stays_small_and_plain
func TestAStoreStaysSmallAndPlain(t *testing.T) {
	big := []byte(strings.Repeat("x", MaxFile-1))
	files := Files{}
	for i := range 20 {
		files["knowledge/"+strconv.Itoa(i)+".md"] = big
	}
	files["a"] = []byte("file")
	files["a/b"] = []byte("under a file")
	kept, skipped := Capped(files, nil)
	total := 0
	for _, d := range kept {
		total += len(d)
	}
	if total > MaxBytes {
		t.Errorf("kept %d bytes, over %d", total, MaxBytes)
	}
	if !contains(skipped, "a") {
		t.Errorf("a file that is also a directory was kept: skipped %v", skipped)
	}
	if _, ok := kept["a/b"]; !ok {
		t.Errorf("a/b was dropped: %v", skipped)
	}
	if PathOK(`"..`) {
		t.Error(`PathOK("\"..") = true`)
	}
	// sorted order, and a later small file still fits after a big one is left out
	kept, skipped = Capped(Files{"b": make([]byte, MaxBytes), "a": []byte("1"), "c": []byte("2")}, []string{"walk"})
	if len(kept) != 2 || kept["a"] == nil || kept["c"] == nil {
		t.Errorf("kept %v", keys(kept))
	}
	if strings.Join(skipped, ",") != "walk,b" {
		t.Errorf("skipped %v, want [walk b]", skipped)
	}
}

func TestPathOKMatchesTheGolden(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "memory", "testdata", "py", "path_ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Rows []struct {
			In        string `json:"in"`
			OK        bool   `json:"ok"`
			InContext bool   `json:"in_context"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Rows) == 0 {
		t.Fatal("empty golden")
	}
	for _, r := range g.Rows {
		if got := PathOK(r.In); got != r.OK {
			t.Errorf("PathOK(%q) = %v, want %v", r.In, got, r.OK)
		}
		if got := InContext(r.In); got != r.InContext {
			t.Errorf("InContext(%q) = %v, want %v", r.In, got, r.InContext)
		}
	}
	// not valid UTF-8 (Python: a surrogate-escaped name that cannot be encoded)
	if PathOK("a\xffb") {
		t.Error("PathOK accepted invalid UTF-8")
	}
	for _, p := range []string{"context", "context/a", "contextual", "a/context"} {
		want := p == "context" || p == "context/a"
		if InContext(p) != want {
			t.Errorf("InContext(%q) != %v", p, want)
		}
	}
}

func TestBlobSHAIsGitsObjectName(t *testing.T) {
	// git hash-object of "" and of "hello\n"
	for data, want := range map[string]string{
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
	} {
		if got := BlobSHA([]byte(data)); got != want {
			t.Errorf("BlobSHA(%q) = %s, want %s", data, got, want)
		}
	}
}

func TestPyReprIsPythonsRepr(t *testing.T) {
	for in, want := range map[string]string{
		"a":          `'a'`,
		"it's":       `"it's"`,
		`a'b"c`:      `'a\'b"c'`,
		"a\nb\tc":    `'a\nb\tc'`,
		"x\x1fy":     `'x\x1fy'`,
		"\x7f":       `'\x7f'`,
		`back\`:      `'back\\'`,
		"é😀":         `'é😀'`,
		"a b":        `'a\xa0b'`,
		"a\u2028b":   "'a\\u2028b'",
		"\xff/x":     `'\udcff/x'`,
		"\U000e0001": `'\U000e0001'`,
	} {
		if got := PyRepr(in); got != want {
			t.Errorf("PyRepr(%q) = %s, want %s", in, got, want)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func keys(f Files) []string {
	var out []string
	for k := range f {
		out = append(out, k)
	}
	return out
}
