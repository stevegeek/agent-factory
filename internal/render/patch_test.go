package render

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The patch over Python's golden: the text after, and the outcome.
func TestPatchMatchesPython(t *testing.T) {
	var golden struct {
		Rows []struct {
			Name   string `json:"name"`
			Before string `json:"before"`
			After  string `json:"after"`
			Exit   int    `json:"exit"`
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		} `json:"rows"`
	}
	readJSON(t, filepath.Join("testdata", "py", "patch_scion.json"), &golden)
	if len(golden.Rows) != 4 {
		t.Fatalf("%d rows", len(golden.Rows))
	}
	for _, row := range golden.Rows {
		t.Run(row.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "provision.py")
			if err := os.WriteFile(path, []byte(row.Before), 0o640); err != nil {
				t.Fatal(err)
			}
			msg, err := PatchFile(path)
			if row.Exit != 0 {
				var p Problem
				if !errors.As(err, &p) || err.Error()+"\n" != row.Stderr {
					t.Fatalf("err = %v, want the problem %q", err, row.Stderr)
				}
			} else if err != nil || msg+"\n" != row.Stdout {
				t.Fatalf("PatchFile = %q, %v; want %q", msg, err, row.Stdout)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != row.After {
				t.Errorf("after:\n%q\nwant\n%q", after, row.After)
			}
			if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
				t.Errorf("mode %o, want 0640 kept", st.Mode().Perm())
			}
		})
	}
}

const anchorLine = "    new_projects: dict[str, Any] = {ctx.workspace: project_settings}\n"

// The anchor rules (new tests): the anchor must be the whole line, with its
// indentation, exactly once; the marker alone means already patched, even
// where the anchor has gone; patching twice changes nothing.
func TestPatchAnchor(t *testing.T) {
	cases := []struct {
		name, in string
		state    PatchState
		problem  bool
	}{
		{"anchor at the end", "def f(ctx):\n" + anchorLine, Patched, false},
		{"anchor without its newline", "def f(ctx):\n" + strings.TrimSuffix(anchorLine, "\n"), 0, true},
		{"anchor with eight spaces (a substring, as in Python)", "def f(ctx):\n        " + strings.TrimLeft(anchorLine, " "), Patched, false},
		{"anchor twice", anchorLine + anchorLine, 0, true},
		{"anchor with tabs", "\tnew_projects: dict[str, Any] = {ctx.workspace: project_settings}\n", 0, true},
		{"empty file", "", 0, true},
		{"marker without the anchor", "# agent-factory local patch: in local mode the manifest carries the HOST\n", AlreadyPatched, false},
		{"marker beside two anchors", anchorLine + anchorLine + "# local patch: in local mode the manifest\n", AlreadyPatched, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, state, err := Patch(c.in)
			if c.problem {
				var p Problem
				if !errors.As(err, &p) || p.Error() != "anchor not found; provision.py changed upstream, patch by hand" {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if state != c.state {
				t.Fatalf("state = %v, want %v", state, c.state)
			}
			if state == AlreadyPatched {
				if out != c.in {
					t.Error("already patched text changed")
				}
				return
			}
			i := strings.Index(out, anchorLine)
			if i < 0 || !strings.HasPrefix(out[i+len(anchorLine):], "\n    # agent-factory local patch:") {
				t.Errorf("addition not right after the anchor:\n%s", out)
			}
			again, state2, err := Patch(out)
			if err != nil || state2 != AlreadyPatched || again != out {
				t.Errorf("second patch: %v %v", state2, err)
			}
		})
	}
}

// The anchor's leading spaces are part of it: an anchor indented by two
// spaces only matches as a substring when four are there.
func TestPatchIndentIsPartOfTheAnchor(t *testing.T) {
	_, _, err := Patch("def f(ctx):\n  new_projects: dict[str, Any] = {ctx.workspace: project_settings}\n")
	if err == nil {
		t.Error("a two-space anchor was patched")
	}
}

// Python reads and writes the file in text mode: CRLF and CR become LF.
func TestPatchUniversalNewlines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provision.py")
	in := "import x\r\n" + strings.TrimSuffix(anchorLine, "\n") + "\r\n    return new_projects\r"
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg, err := PatchFile(path); err != nil || msg != "patched" {
		t.Fatalf("%q %v", msg, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "\r") || !strings.HasPrefix(string(b), "import x\n"+anchorLine+"\n") || !strings.HasSuffix(string(b), "    return new_projects\n") {
		t.Errorf("got %q", b)
	}
}

// A link to the harness file is followed: the file it names is patched and
// the link stays a link (Python's open(path, "w") writes through it).
func TestPatchFollowsALink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.py")
	if err := os.WriteFile(real, []byte(anchorLine), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "provision.py")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchFile(link); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link replaced: %v", err)
	}
	b, _ := os.ReadFile(real)
	if !strings.Contains(string(b), "local patch: in local mode the manifest") {
		t.Error("target not patched")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Errorf("left files behind: %v", ents)
	}
}

func TestPatchFileErrors(t *testing.T) {
	if _, err := PatchFile(filepath.Join(t.TempDir(), "missing.py")); err == nil {
		t.Error("missing file patched")
	}
	path := filepath.Join(t.TempDir(), "bin.py")
	if err := os.WriteFile(path, []byte("\xff"+anchorLine), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchFile(path); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("err = %v", err)
	}
}
