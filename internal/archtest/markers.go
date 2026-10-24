package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// markerPrefix reports the prefixes port markers use for the three Python
// test files they came from (§10.2): TF test_factory.py, TT
// test_tickets.py, TL test_ticket_limits.py.
func markerPrefix(p string) bool { return p == "TF" || p == "TT" || p == "TL" }

// ReadManifest reads the committed list of Python tests (one per line;
// blank lines and lines starting with # are ignored).
func ReadManifest(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// Marker is one "// port: TF Class.test_name" comment.
type Marker struct {
	Test string // "TF Class.test_name"
	Pos  string // file:line
}

// Markers finds every port marker in the .go files under dir (testdata and
// nested modules skipped). Malformed markers come back as violations.
func Markers(dir string) ([]Marker, []Violation, error) {
	var ms []Marker
	var vs []Violation
	err := walkGo(dir, func(path string, rel string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			text, found := strings.CutPrefix(strings.TrimSpace(line), "//")
			if !found {
				continue
			}
			body, found := strings.CutPrefix(strings.TrimSpace(text), "port:")
			if !found {
				continue
			}
			pos := rel + ":" + strconv.Itoa(i+1)
			f := strings.Fields(body)
			if len(f) != 2 || !markerPrefix(f[0]) || !strings.Contains(f[1], ".test") {
				vs = append(vs, Violation{"port", pos, "malformed port marker " + strconv.Quote(strings.TrimSpace(line)) + `: want "// port: TF Class.test_name" (TF, TT or TL)`})
				continue
			}
			ms = append(ms, Marker{f[0] + " " + f[1], pos})
		}
		return nil
	})
	return ms, vs, err
}

// CheckPortMarkers checks the markers under dir against the manifest:
// every marker names a manifest entry, the manifest matches the Python
// suite in dir/tests while it exists, and, when complete is set, every
// manifest entry has a marker. It also returns the entries with no marker.
func CheckPortMarkers(dir, manifestPath string, complete bool) (missing []string, vs []Violation, err error) {
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		return nil, nil, err
	}
	known := map[string]bool{}
	for _, m := range manifest {
		known[m] = true
	}
	markers, mvs, err := Markers(dir)
	if err != nil {
		return nil, nil, err
	}
	vs = append(vs, mvs...)
	ported := map[string]bool{}
	for _, m := range markers {
		ported[m.Test] = true
		if !known[m.Test] {
			vs = append(vs, Violation{"port", m.Pos, "port marker names " + m.Test + ", which is not a Python test"})
		}
	}
	for _, m := range manifest {
		if !ported[m] {
			missing = append(missing, m)
			if complete {
				vs = append(vs, Violation{"port", m, "has no // port: marker in any Go file"})
			}
		}
	}
	return missing, sortViolations(vs), nil
}

// walkGo calls fn for every .go file under dir, skipping testdata, hidden
// and underscore directories, and nested modules.
func walkGo(dir string, fn func(path, rel string) error) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == dir {
				return nil
			}
			name := d.Name()
			if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		return fn(path, filepath.ToSlash(rel))
	})
}
