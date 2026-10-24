package cli

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

// pyRun is one command of a golden file Python's main() wrote.
type pyRun struct {
	Argv   []string `json:"argv"`
	Exit   int      `json:"exit"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
}

type pyGolden struct {
	Config   string  `json:"config"`
	Commands []pyRun `json:"commands"`
}

func readGolden(t *testing.T, name string) pyGolden {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "py", name))
	if err != nil {
		t.Fatal(err)
	}
	var g pyGolden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// copyTree copies the directory src into dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fixture is a hermetic factory root for the CLI: testkit's root, with
// the token store where Python's scratch root kept it.
type fixture struct {
	t   *testing.T
	env *testkit.Env
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := testkit.Root(t)
	env.Vars["FACTORY_API_TOKENS"] = env.Path("api-tokens.json")
	env.Vars["FACTORY_TEST_NOW"] = "2026-10-18T10:00:00+00:00"
	return &fixture{t: t, env: env}
}

// goldenRoot is a copy of testdata/py/root: the board Python's goldens ran on.
func goldenRoot(t *testing.T) *fixture {
	f := newFixture(t)
	copyTree(t, filepath.Join("testdata", "py", "root"), f.env.Root)
	return f
}

// run is factory's Main with argv, stdin and the fixture's environment.
func (f *fixture) run(stdin string, argv ...string) (int, string, string) {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := Main(append([]string{"factory"}, argv...), Env{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Environ: f.env.Vars,
	})
	return code, out.String(), errb.String()
}

// Every read-only command on the board Python's goldens ran on.
func TestReadOnlyCommandsMatchPython(t *testing.T) {
	// audit and gate-workflow are S15b's commands.
	notMine := map[string]bool{"audit": true, "gate-workflow": true}
	f := goldenRoot(t)
	for _, c := range readGolden(t, "commands.json").Commands {
		if notMine[c.Argv[0]] {
			continue
		}
		code, out, errs := f.run("", c.Argv...)
		if c.Argv[0] == "check" {
			out = dropAddedWarnings(t, out)
		}
		if code != c.Exit || out != c.Stdout || strings.TrimSpace(errs) != strings.TrimSpace(c.Stderr) {
			t.Errorf("%q: got %d, want %d\n%s\nstderr %q, want %q", c.Argv, code, c.Exit, testkit.Diff(c.Stdout, out), errs, c.Stderr)
		}
	}
}

func TestReadOnlyCommandsOnAnEmptyBoard(t *testing.T) {
	g := readGolden(t, "empty.json")
	f := newFixture(t)
	f.env.WriteConfig(t, g.Config)
	for _, c := range g.Commands {
		code, out, errs := f.run("", c.Argv...)
		if code != c.Exit || out != c.Stdout || errs != c.Stderr {
			t.Errorf("%q: got %d %q %q, want %d %q %q", c.Argv, code, out, errs, c.Exit, c.Stdout, c.Stderr)
		}
	}
}

// addedWarning is a warning the Go config adds on purpose (quirk 6: a
// factory project with no gate commands); Python's check had none.
var addedWarning = regexp.MustCompile(`^warning: \[projects\.[a-z0-9_-]+\] has no [a-z, ]+: the gate refuses to run its checks without them\n$`)

// dropAddedWarnings takes the added warnings out of check's output.
func dropAddedWarnings(t *testing.T, out string) string {
	t.Helper()
	var keep []string
	for _, line := range strings.SplitAfter(out, "\n") {
		if addedWarning.MatchString(line) {
			t.Logf("added warning (allowed): %s", strings.TrimSpace(line))
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "")
}

// port: TF ModelTest.test_foremans_copy_of_the_model_is_current
// Ported here: `factory model` prints the old Foreman's model.json (frozen in
// internal/model/testdata at the cutover), with no config.
func TestModelNeedsNoConfig(t *testing.T) {
	f := newFixture(t) // no config.toml
	code, out, errs := f.run("", "model")
	want, err := os.ReadFile(filepath.Join("..", "model", "testdata", "model.json"))
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || out != string(want) || errs != "" {
		t.Errorf("model: %d %q\n%s", code, errs, testkit.Diff(string(want), out))
	}
}

func TestCommandsThatReadConfigFailWithoutIt(t *testing.T) {
	f := newFixture(t)
	for _, argv := range [][]string{{"ls"}, {"show", "T0001"}, {"check"}, {"recipes"}, {"tokens", "list"}, {"ticket", "list"}} {
		code, out, errs := f.run("", argv...)
		if code != 1 || out != "" || !strings.HasPrefix(errs, "factory: ") || !strings.Contains(errs, "config.toml") {
			t.Errorf("%q: %d %q %q", argv, code, out, errs)
		}
	}
}

func TestVersion(t *testing.T) {
	f := newFixture(t)
	code, out, _ := f.run("", "version")
	if code != 0 || out != "dev dev\n" {
		t.Errorf("version: %d %q", code, out)
	}
}

func TestShowAndResultRefuseWhatIsNotATicket(t *testing.T) {
	f := goldenRoot(t)
	for _, argv := range [][]string{{"show", "T0099"}, {"result", "T0099"}, {"show", "../config"}, {"result", "x"}} {
		code, out, errs := f.run("", argv...)
		if code != 1 || out != "" || errs == "" {
			t.Errorf("%q: %d %q %q", argv, code, out, errs)
		}
	}
	// An id is read upper-cased, as load_ticket does.
	code, out, _ := f.run("", "show", "t0001")
	if code != 0 || !strings.Contains(out, `"id": "T0001"`) {
		t.Errorf("show t0001: %d %q", code, out)
	}
}

func TestLsSkipsAFileThatWillNotRead(t *testing.T) {
	f := goldenRoot(t)
	f.env.WriteFile(t, "board/tickets/T0020.json", "{not json")
	code, out, errs := f.run("", "ls")
	if code != 0 || !strings.HasPrefix(out, "T0001  ready") || strings.Contains(out, "T0020") {
		t.Errorf("ls with a broken file: %d %q", code, out)
	}
	if !strings.Contains(errs, "T0020") {
		t.Errorf("the broken file should be logged: %q", errs)
	}
}

func TestProposalsKeepTiesInIDOrder(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, "[projects.gem]\npath = \"/x\"\n")
	for _, p := range []struct{ id, pri, title string }{{"T0001", "2", "Low"}, {"T0002", "3.5", "First of a tie"},
		{"T0003", "3.5", "Second of a tie"}, {"T0004", "4", "Top"}} {
		f.env.WriteFile(t, "board/tickets/"+p.id+".json", `{"id": "`+p.id+`", "project": "gem", "title": "`+p.title+
			`", "column": "needs_human", "kind": "proposal", "panel": {"scores": [4, 3], "mean": 3.5, "priority": `+p.pri+`}}`)
	}
	f.env.WriteFile(t, "board/tickets/T0005.json", `{"id": "T0005", "project": "gem", "title": "Decided", "column": "rejected", "kind": "proposal", "panel": {"scores": [], "mean": 0, "priority": 9}}`)
	code, out, _ := f.run("", "proposals")
	want := "T0004  priority 4    mean 3.5  scores [4, 3]  Top\n" +
		"T0002  priority 3.5  mean 3.5  scores [4, 3]  First of a tie\n" +
		"T0003  priority 3.5  mean 3.5  scores [4, 3]  Second of a tie\n" +
		"T0001  priority 2    mean 3.5  scores [4, 3]  Low\n"
	if code != 0 || out != want {
		t.Errorf("proposals: %d\n%s", code, testkit.Diff(want, out))
	}
}
