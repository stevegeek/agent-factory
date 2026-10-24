package jsonx

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

// Tickets written by Python's real save_ticket (json.dump(indent=2)): the
// history-order goldens of the Python goldens tool (nested pr keys and reports
// in history-dependent order) and the copy of Foreman's contract fixtures
// in internal/ticket/testdata/legacy. Each reads and writes back byte for
// byte (at the jsonx level). The files belong to other packages; this
// test only reads them.
func TestPythonWrittenTicketsRoundTrip(t *testing.T) {
	var files []string
	for _, pat := range []string{
		"../ticket/testdata/py/history-order/*/*/T*.json",
		"../ticket/testdata/legacy/contract/T*.json",
	} {
		m, err := filepath.Glob(pat)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Skip("no Python-written ticket fixtures found")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := Parse(b)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		got := EncodePython(v, 2)
		if bytes.HasSuffix(b, []byte("}\n")) {
			got = append(got, '\n') // the contract fixtures are frozen with a final newline
		}
		if !bytes.Equal(got, b) {
			t.Errorf("%s does not round-trip:\n%s", f, testkit.Diff(string(b), string(got)))
		}
	}
	t.Logf("%d tickets round-trip", len(files))
}
