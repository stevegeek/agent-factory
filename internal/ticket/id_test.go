package ticket

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// ParseID is changing()'s check, against Python's own answers for
// re.fullmatch(r"T\d{4,6}", ...) (internal/textx's regex golden), after
// upper().
func TestParseIDMatchesPython(t *testing.T) {
	b, err := os.ReadFile("../textx/testdata/py/regex.json")
	if err != nil {
		t.Skipf("regex golden: %v", err)
	}
	var g struct {
		Patterns []struct {
			Name  string
			Cases []struct {
				In  string
				Out json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range g.Patterns {
		if p.Name != "ticket_id_fullmatch" {
			continue
		}
		for _, c := range p.Cases {
			n++
			_, ok := ParseID(c.In)
			if want := string(c.Out) != "null"; ok != want && (len(c.In) == 0 || c.In[0] != 't') {
				t.Errorf("ParseID(%q) = %v, Python %v", c.In, ok, want)
			}
		}
	}
	if n == 0 {
		t.Error("no ticket_id_fullmatch cases")
	}
	for in, want := range map[string]ID{"t0001": "T0001", "T123456": "T123456"} {
		if got, ok := ParseID(in); !ok || got != want {
			t.Errorf("ParseID(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"../T0001", "T0001/..", "T001", "T1234567", "", "T"} {
		if _, ok := ParseID(in); ok {
			t.Errorf("ParseID(%q) accepted it", in)
		}
	}
}

func TestIDNumbersAndOrder(t *testing.T) {
	if FormatID(7) != "T0007" || FormatID(12345) != "T12345" {
		t.Error("FormatID")
	}
	for id, n := range map[ID]int{"T0042": 42, "T٠٠٠١": 1, "T𝟏𝟐𝟑𝟒": 1234, "T": -1} {
		if got := id.Num(); got != n {
			t.Errorf("%q.Num() = %d, want %d", id, got, n)
		}
	}
	ids := []ID{"T10000", "T0002", "T9999", "T0010"}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Less(ids[j]) })
	if ids[0] != "T0002" || ids[1] != "T0010" || ids[2] != "T9999" || ids[3] != "T10000" {
		t.Errorf("order %v", ids)
	}
}
