package jsonx

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

// Every JSON file of a private snapshot of the live board (named by
// FACTORY_LIVE_SNAPSHOT; skipped when unset) reads and writes back byte for
// byte: tickets and state.json as json.dump(indent=2) wrote them, index.json
// as json.dump with no indent.
func TestLiveSnapshotRoundTripsByteForByte(t *testing.T) {
	root := testkit.LiveSnapshot(t)
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		v, err := Parse(b)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			return nil
		}
		indent := -1
		if bytes.Contains(b, []byte("\n")) {
			indent = 2
		}
		if got := EncodePython(v, indent); !bytes.Equal(got, b) {
			t.Errorf("%s does not round-trip:\n%s", rel, testkit.Diff(string(b), string(got)))
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no JSON files in the snapshot")
	}
	t.Logf("%d files round-trip", n)
}
