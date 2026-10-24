package ticket

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// fixture is one ticket file under testdata.
type fixture struct {
	name string
	data []byte
	// python: written by json.dump(indent=2) itself (not hand-written, not
	// with a trailing newline added by a test)
	python bool
}

func readFixtures(t *testing.T, pattern string, python bool) []fixture {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	var out []fixture
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fixture{name: p, data: b, python: python})
	}
	if len(out) == 0 {
		t.Fatalf("no fixtures match %s", pattern)
	}
	return out
}

// allFixtures: the legacy boards, the Python-written contract tickets, the
// Python history-order goldens, the nested-order fixture and new tickets.
func allFixtures(t *testing.T) []fixture {
	var out []fixture
	out = append(out, readFixtures(t, "testdata/legacy/board/T*.json", true)...)
	out = append(out, readFixtures(t, "testdata/legacy/contract/T*.json", false)...)
	out = append(out, readFixtures(t, "testdata/py/history-order/*/*/T*.json", true)...)
	out = append(out, readFixtures(t, "testdata/nested/T*.json", true)...)
	out = append(out, readFixtures(t, "testdata/new/T*.json", true)...)
	return out
}

// §3.1 property 1: decode then encode is byte-identical, and the files
// decode with no problems (the nested fixture has its one wrong-typed field).
func TestDecodeEncodeIsByteIdentical(t *testing.T) {
	for _, f := range allFixtures(t) {
		tk, problems, err := Decode(f.data)
		if err != nil {
			t.Errorf("%s: %v", f.name, err)
			continue
		}
		wantProblems := 0
		if filepath.Base(filepath.Dir(f.name)) == "nested" {
			wantProblems = 1
		}
		if len(problems) != wantProblems {
			t.Errorf("%s: problems %v", f.name, problems)
		}
		if got := Encode(tk); !bytes.Equal(got, f.data) {
			t.Errorf("%s: decode+encode changed the file:\n%s", f.name, testkit.Diff(string(f.data), string(got)))
		}
		// a typed view written back with no change is the same tree, so a
		// Python-written file re-encodes to its own bytes
		if f.python {
			if got := jsonx.EncodePython(tk.Tree(), 2); !bytes.Equal(got, f.data) {
				t.Errorf("%s: the merged tree encodes differently:\n%s", f.name, testkit.Diff(string(f.data), string(got)))
			}
		}
	}
}

// The live board, when a private snapshot is at hand (FACTORY_LIVE_SNAPSHOT):
// every file decodes with no problems and survives a typed save unchanged.
func TestLiveBoardRoundTrips(t *testing.T) {
	dir := testkit.LiveSnapshot(t)
	paths, _ := filepath.Glob(filepath.Join(dir, "board", "tickets", "T*.json"))
	if len(paths) == 0 {
		t.Skip("no tickets in the live snapshot")
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		tk, problems, err := Decode(b)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
			continue
		}
		if len(problems) > 0 {
			t.Errorf("%s: problems %v", filepath.Base(p), problems)
		}
		if got := jsonx.EncodePython(tk.Tree(), 2); !bytes.Equal(got, b) {
			t.Errorf("%s: a typed save would change it:\n%s", filepath.Base(p), testkit.Diff(string(b), string(got)))
		}
		for _, e := range tk.History {
			if _, legacy := Interpret(e); legacy && e.Type != "" {
				t.Errorf("%s: typed %q needed the legacy reader", filepath.Base(p), e.Type)
			}
		}
	}
}

func TestDecodeRefusesWhatIsNotATicket(t *testing.T) {
	for _, s := range []string{"", "[]", "null", `{"id": "T0001"`, "\"x\""} {
		if _, _, err := Decode([]byte(s)); err == nil {
			t.Errorf("Decode(%q) accepted it", s)
		}
	}
	tk, problems, err := Decode([]byte(`{"id": "T0001", "column": 4, "history": "no", "pr": {"number": "7", "url": "u"}, "notes": [{"at": 1, "text": "t"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range problems {
		got[p.String()] = true
	}
	for _, want := range []string{"column: want string, have number", "history: want array, have string",
		"pr.number: want int, have string", "notes[0].at: want string, have number"} {
		if !got[want] {
			t.Errorf("no problem %q in %v", want, problems)
		}
	}
	if pr, _ := tk.PR.Get(); tk.Column != "" || pr.Number != 0 || pr.URL != "u" || len(tk.Notes) != 1 || tk.Notes[0].Text != "t" {
		t.Errorf("lenient decode: %+v", tk)
	}
	// and a save keeps every wrong-typed value as it was
	tk.Title = "now typed"
	out := string(Encode(tk))
	for _, want := range []string{`"column": 4`, `"history": "no"`, `"number": "7"`, `"at": 1`, `"title": "now typed"`} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("save lost %s:\n%s", want, out)
		}
	}
}

// A gate saved before walls existed saves byte-identical when its view is
// set again; walls are written only when there are some, and the hygiene
// count is never stored.
func TestGateRoundTripOldTicket(t *testing.T) {
	data, err := os.ReadFile("testdata/legacy/board/T0001.json")
	if err != nil {
		t.Fatal(err)
	}
	tk, _, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := tk.Gate.Get()
	if !ok {
		t.Fatal("the fixture has no gate")
	}
	g.HygieneFindings = 2
	tk.Gate = Some(g)
	if got := Encode(tk); !bytes.Equal(got, data) {
		t.Errorf("re-set gate changed the ticket:\n%s", got)
	}
	g.Walls = []string{"internal/gate/gate.go"}
	tk.Gate = Some(g)
	if got := Encode(tk); !bytes.Contains(got, []byte(`"walls": [`)) || bytes.Contains(got, []byte("hygiene")) {
		t.Errorf("walls not written, or the count written:\n%s", got)
	}
}

// A PR saved before merge commits were recorded saves byte-identical when
// it is set again; the merge commit is written only once there is one.
func TestPRRoundTripOldTicket(t *testing.T) {
	data, err := os.ReadFile("testdata/legacy/board/T0001.json")
	if err != nil {
		t.Fatal(err)
	}
	tk, _, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	pr, ok := tk.PR.Get()
	if !ok {
		t.Fatal("the fixture has no pr")
	}
	tk.PR = Some(pr)
	if got := Encode(tk); !bytes.Equal(got, data) {
		t.Errorf("re-set pr changed the ticket:\n%s", got)
	}
	pr.MergeCommit = "abc123"
	tk.PR = Some(pr)
	if got := Encode(tk); !bytes.Contains(got, []byte(`"merge_commit": "abc123"`)) {
		t.Errorf("merge commit not written:\n%s", got)
	}
}
