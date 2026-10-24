package archtest

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture tests build a small module in a temp dir that obeys every
// rule, then break one rule at a time and check that exactly the matching
// check reports it.

const fixMod = "example.com/fix"

// marker builds a port marker line without writing one into this file,
// so the repository's own marker scan does not see fixture markers.
func marker(s string) string { return "//" + " port: " + s }

func goodModule() map[string]string {
	return map[string]string{
		"go.mod":  "module " + fixMod + "\n\ngo 1.25\n",
		"root.go": "package fix\n\n// Name is a constant, not state.\nconst Name = \"fix\"\n",

		"internal/clock/clock.go": "package clock\n\nimport \"time\"\n\n" +
			"type Clock interface{ Now() time.Time }\n",
		"internal/logx/logx.go": "package logx\n\nimport \"" + fixMod + "/internal/clock\"\n\n" +
			"type Log struct{ C clock.Clock }\n",
		"internal/logx/logx_test.go": "package logx\n\nimport (\n\t\"testing\"\n\n\t\"" + fixMod + "/internal/testkit\"\n)\n\n" +
			marker("TF A.test_one") + "\nfunc TestOne(t *testing.T) { _ = testkit.Name }\n",

		"internal/textx/textx.go": "package textx\n\nimport (\n\t\"errors\"\n\t\"regexp\"\n)\n\n" +
			"// py: ^a+$\nvar word = regexp.MustCompile(`^a+$`)\n\n" +
			"var tail = regexp.MustCompile(`b+`) // re: go\n\n" +
			"// ErrEmpty is a sentinel.\nvar ErrEmpty = errors.New(\"empty\")\n\n" +
			"func table() map[string]*regexp.Regexp {\n\treturn map[string]*regexp.Regexp{\n\t\t// py: ^c$\n\t\t\"c\": regexp.MustCompile(`^c$`),\n\t}\n}\n\n" +
			"func Word(s string) bool { return word.MatchString(s) || tail.MatchString(s) || table()[\"c\"].MatchString(s) }\n",
		// The shape the Python-era generator wrote.
		"internal/textx/testdata/py/regex.json": `{"about": "x", "patterns": [` +
			`{"name": "a", "pattern": "^a+$", "py": "^a+$", "flags": [], "op": "search", "cases": [{"in": "a", "out": ["a"]}]},` +
			`{"name": "c", "pattern": "^c\n$", "py": "^c$", "flags": [], "op": "match", "cases": []}]}`,

		"internal/web/web.go": "package web\n\nimport (\n\t\"embed\"\n\t\"io/fs\"\n\n\t\"" + fixMod + "/internal/clock\"\n)\n\n" +
			"//go:embed static\nvar static embed.FS\n\n// Static is the embedded tree.\nvar Static fs.FS = static\n\n" +
			"type Server struct{ C clock.Clock }\n",
		"internal/web/static/app.css": "body{}\n",

		"internal/helper/helper.go": "package helper\n\nimport \"" + fixMod + "/internal/clock\"\n\n" +
			"type Server struct{ C clock.Clock }\n",

		"internal/testkit/testkit.go": "package testkit\n\nimport _ \"" + fixMod + "/internal/clock\"\n\n// Name names the kit.\nconst Name = \"kit\"\n",

		"cmd/foreman/main.go":        "package main\n\nimport \"" + fixMod + "/internal/web\"\n\nfunc main() { _ = web.Server{} }\n",
		"cmd/factory-remote/main.go": "package main\n\nimport \"" + fixMod + "/internal/helper\"\n\nfunc main() { _ = helper.Server{} }\n",

		"manifest.txt": "# fixture\nTF A.test_one\nTF A.test_two\nTF B.test_three\n",
	}
}

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if body == "" {
			continue // deleted by an overlay
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadFixture(t *testing.T, overlay map[string]string) (*Graph, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go list")
	}
	files := goodModule()
	maps.Copy(files, overlay)
	dir := writeModule(t, files)
	g, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if vs := CheckLoad(g); len(vs) > 0 {
		t.Fatalf("fixture does not load: %v", vs)
	}
	return g, dir
}

// allChecks runs every check with the fixture's settings.
func allChecks(t *testing.T, g *Graph, dir string, sizeLimit int, complete bool) map[string][]Violation {
	t.Helper()
	out := map[string][]Violation{}
	add := func(vs []Violation) {
		for _, v := range vs {
			out[v.Rule] = append(out[v.Rule], v)
		}
	}
	add(CheckTable(g))
	add(CheckForemanClosure(g))
	add(CheckHelperClosure(g))
	add(CheckStdlibOnly(g))
	add(CheckSize(g, sizeLimit))
	add(CheckGlobals(g))
	add(CheckDomainPurity(g))
	add(CheckInterfaceSize(g))
	_, rvs := CheckReasons(g)
	add(rvs)
	_, vs, err := CheckPortMarkers(dir, filepath.Join(dir, "manifest.txt"), complete)
	if err != nil {
		t.Fatal(err)
	}
	add(vs)
	files, err := RegexGoldenFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := GoldenPatterns(files)
	if err != nil {
		t.Fatal(err)
	}
	add(CheckRegexGoldens(g, golden))
	add(CheckHermeticTests(g))
	pvs, err := CheckPersonalStrings(dir, []string{"Secret-Name"})
	if err != nil {
		t.Fatal(err)
	}
	add(pvs)
	return out
}

func TestGoodFixturePassesEveryCheck(t *testing.T) {
	g, dir := loadFixture(t, nil)
	got := allChecks(t, g, dir, MaxPackageLines, false)
	for rule, vs := range got {
		t.Errorf("rule %s: unexpected violations:\n%s", rule, join(vs))
	}
	missing, _, err := CheckPortMarkers(dir, filepath.Join(dir, "manifest.txt"), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(missing, ",") != "TF A.test_two,TF B.test_three" {
		t.Errorf("missing = %v", missing)
	}
}

func join(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString("  " + v.String() + "\n")
	}
	return b.String()
}

// Each case breaks one rule; want lists every rule that must fire, and each
// fired violation must contain the given text.
func TestFixtureBreakingEachRule(t *testing.T) {
	cases := []struct {
		name      string
		overlay   map[string]string
		sizeLimit int
		complete  bool
		want      map[string]string // rule -> text one of its violations contains
	}{
		{
			name: "edge not in the table (web imports store)",
			overlay: map[string]string{
				"internal/store/store.go": "package store\n\nconst X = 1\n",
				"internal/web/edge.go":    "package web\n\nimport \"" + fixMod + "/internal/store\"\n\nconst Y = store.X\n",
			},
			want: map[string]string{"table": "internal/web: imports internal/store"},
		},
		{
			name: "test import not in the table",
			overlay: map[string]string{
				"internal/store/store.go":     "package store\n\nconst X = 1\n",
				"internal/clock/edge_test.go": "package clock\n\nimport \"" + fixMod + "/internal/store\"\n\nconst y = store.X\n",
			},
			want: map[string]string{"table": "tests import internal/store"},
		},
		{
			name:    "package not in the table",
			overlay: map[string]string{"internal/bogus/bogus.go": "package bogus\n"},
			want:    map[string]string{"table": "internal/bogus: package is not in the allowed-imports table"},
		},
		{
			name:    "a package outside internal and cmd",
			overlay: map[string]string{"tools/tool.go": "package tools\n"},
			want:    map[string]string{"table": "tools: package is not in the allowed-imports table"},
		},
		{
			name: "testkit in production code",
			overlay: map[string]string{
				"internal/logx/kit.go": "package logx\n\nimport \"" + fixMod + "/internal/testkit\"\n\nconst k = testkit.Name\n",
			},
			want: map[string]string{"table": "production code imports internal/testkit"},
		},
		{
			name: "foreman links github",
			overlay: map[string]string{
				"internal/github/github.go": "package github\n\nconst API = \"x\"\n",
				"internal/web/gh.go":        "package web\n\nimport \"" + fixMod + "/internal/github\"\n\nconst api = github.API\n",
			},
			want: map[string]string{
				"table":           "internal/web: imports internal/github",
				"foreman-closure": "links internal/github",
			},
		},
		{
			name: "helper links config",
			overlay: map[string]string{
				"internal/config/config.go": "package config\n\nconst P = 1\n",
				"internal/helper/cfg.go":    "package helper\n\nimport \"" + fixMod + "/internal/config\"\n\nconst p = config.P\n",
			},
			want: map[string]string{
				"table":          "internal/helper: imports internal/config",
				"helper-closure": "links internal/config",
			},
		},
		{
			name: "a dependency outside the standard library",
			overlay: map[string]string{
				"go.mod":                "module " + fixMod + "\n\ngo 1.25\n\nrequire example.com/other v0.0.0\n\nreplace example.com/other => ./other\n",
				"other/go.mod":          "module example.com/other\n\ngo 1.25\n",
				"other/other.go":        "package other\n\nconst V = 1\n",
				"internal/clock/dep.go": "package clock\n\nimport \"example.com/other\"\n\nconst v = other.V\n",
			},
			want: map[string]string{"stdlib": "example.com/other"},
		},
		{
			name: "a package over the size limit",
			overlay: map[string]string{
				"internal/clock/big.go": "package clock\n" + strings.Repeat("\n", 40),
			},
			sizeLimit: 30,
			want:      map[string]string{"size": "internal/clock: 4"},
		},
		{
			name: "package-level state",
			overlay: map[string]string{
				"internal/clock/state.go": "package clock\n\nimport (\n\t\"fmt\"\n\t\"io/fs\"\n\t\"os\"\n)\n\n" +
					"var cache = map[string]int{}\n\nvar errX = fmt.Errorf(\"x\")\n\nvar dir fs.FS = os.DirFS(\".\")\n\n" +
					"func use() (int, error, fs.FS) { return cache[\"a\"], errX, dir }\n",
			},
			want: map[string]string{"globals": "var cache has type map[string]int"},
		},
		{
			name: "error not from errors.New",
			overlay: map[string]string{
				"internal/clock/state.go": "package clock\n\nimport \"fmt\"\n\nvar errX = fmt.Errorf(\"x\")\n\nfunc use() error { return errX }\n",
			},
			want: map[string]string{"globals": "is not created by errors.New"},
		},
		{
			name: "fs.FS not from embed",
			overlay: map[string]string{
				"internal/clock/state.go": "package clock\n\nimport (\n\t\"io/fs\"\n\t\"os\"\n)\n\nvar dir fs.FS = os.DirFS(\".\")\n\nfunc use() fs.FS { return dir }\n",
			},
			want: map[string]string{"globals": "is not initialised from an embed.FS"},
		},
		{
			name: "an allowed global assigned in a function",
			overlay: map[string]string{
				"internal/textx/set.go": "package textx\n\nfunc reset() { tail = nil }\n",
			},
			want: map[string]string{"globals": "assigns to package-level var tail"},
		},
		{
			name: "an allowed global assigned in a test",
			overlay: map[string]string{
				"internal/textx/set_test.go": "package textx\n\nimport \"testing\"\n\nfunc TestSet(t *testing.T) { word = nil }\n",
			},
			want: map[string]string{"globals": "assigns to package-level var word"},
		},
		{
			name: "an allowed global assigned from an external test",
			overlay: map[string]string{
				"internal/web/set_test.go": "package web_test\n\nimport (\n\t\"testing\"\n\n\t\"" + fixMod + "/internal/web\"\n)\n\nfunc TestSet(t *testing.T) { web.Static = nil }\n",
			},
			want: map[string]string{"globals": "assigns to package-level var Static"},
		},
		{
			name: "a port marker naming no Python test",
			overlay: map[string]string{
				"internal/clock/m_test.go": "package clock\n\n" + marker("TF A.test_nope") + "\n",
			},
			want: map[string]string{"port": "names TF A.test_nope, which is not a Python test"},
		},
		{
			name: "a malformed port marker",
			overlay: map[string]string{
				"internal/clock/m_test.go": "package clock\n\n" + marker("XX A.test_one") + "\n",
			},
			want: map[string]string{"port": "malformed port marker"},
		},
		{
			name:     "an unported Python test when completeness is required",
			complete: true,
			want:     map[string]string{"port": "TF A.test_two: has no // port: marker"},
		},
		{
			name: "a regex without a py comment",
			overlay: map[string]string{
				"internal/textx/bare.go": "package textx\n\nimport \"regexp\"\n\nvar bare = regexp.MustCompile(`x`)\n\nfunc Bare() bool { return bare.MatchString(\"x\") }\n",
			},
			want: map[string]string{"regex": `needs "// py: <python pattern>"`},
		},
		{
			name: "a regex whose pattern has no golden",
			overlay: map[string]string{
				"internal/textx/bare.go": "package textx\n\nimport \"regexp\"\n\n// py: ^x$\nvar bare = regexp.MustCompile(`^x$`)\n\nfunc Bare() bool { return bare.MatchString(\"x\") }\n",
			},
			want: map[string]string{"regex": `pattern "^x$" has no entry in a regex golden`},
		},
		{
			name: "a test reading HOME",
			overlay: map[string]string{
				"internal/clock/home_test.go": "package clock\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestHome(t *testing.T) { _ = os.Getenv(\"HOME\") }\n",
			},
			want: map[string]string{"hermetic": "reads HOME"},
		},
		{
			name: "a personal string",
			overlay: map[string]string{
				"internal/clock/notes.txt": "deployed by secret-name\n",
			},
			want: map[string]string{"personal": "internal/clock/notes.txt:1"},
		},
		{
			name:    "the domain does file I/O",
			overlay: map[string]string{"internal/model/io.go": "package model\n\nimport \"os\"\n\nfunc Read() ([]byte, error) { return os.ReadFile(\"x\") }\n"},
			want:    map[string]string{"purity": "imports os"},
		},
		{
			name:    "the domain asks for the time",
			overlay: map[string]string{"internal/model/now.go": "package model\n\nimport \"time\"\n\nfunc Stamp() time.Time { return time.Now() }\n"},
			want:    map[string]string{"purity": "calls time.Now"},
		},
		{
			name: "an exported interface of five methods, counting an embedded one",
			overlay: map[string]string{
				"internal/clock/big.go": "package clock\n\ntype small interface{ A(); B() }\n\n" +
					"// Big is too big.\ntype Big interface {\n\tsmall\n\tC()\n\tD()\n\tE()\n}\n\n" +
					"type unexported interface{ A(); B(); C(); D(); E() }\n\n// Four is fine.\ntype Four interface{ A(); B(); C(); D() }\n",
			},
			want: map[string]string{"interfaces": "exported interface Big has 5 methods"},
		},
		{
			name: "a reason never written, and a reason spelled as a literal",
			overlay: map[string]string{
				"internal/model/model.go": "package model\n\n// Reason is why.\ntype Reason string\n\n" +
					"const (\n\tReasonA Reason = \"a\"\n\tReasonB Reason = \"b\"\n\tReasonC Reason = \"c\"\n)\n",
				"internal/ticket/ticket.go": "package ticket\n\nimport \"" + fixMod + "/internal/model\"\n\n" +
					"// Move moves.\nfunc Move(r model.Reason) string { return string(r) }\n\n" +
					"// Moves uses A, compares with B (a read) and spells c as a literal.\n" +
					"func Moves(r model.Reason) string {\n\tif r == model.ReasonB {\n\t\treturn \"\"\n\t}\n\tswitch r {\n\tcase model.ReasonB:\n\t}\n" +
					"\treturn Move(model.ReasonA) + Move(\"c\")\n}\n",
			},
			want: map[string]string{"reasons": "model.Reason b is never written"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, dir := loadFixture(t, tc.overlay)
			limit := tc.sizeLimit
			if limit == 0 {
				limit = MaxPackageLines
			}
			got := allChecks(t, g, dir, limit, tc.complete)
			for rule, text := range tc.want {
				found := false
				for _, v := range got[rule] {
					if strings.Contains(v.String(), text) {
						found = true
					}
				}
				if !found {
					t.Errorf("rule %s: no violation containing %q; got:\n%s", rule, text, join(got[rule]))
				}
			}
			for rule, vs := range got {
				if _, ok := tc.want[rule]; !ok {
					t.Errorf("rule %s fired unexpectedly:\n%s", rule, join(vs))
				}
			}
		})
	}
}

func TestGoldenPatternsShapes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := write("a.json", `{"about": "x", "patterns": [{"pattern": "^T\\d{4,6}$", "py": "^T\\d{4,6}$"}, {"pattern": "a\nb", "py": "a\\nb"}]}`)
	b := write("b.json", `{"^x$": {"cases": []}, "^y$": []}`)
	got, err := GoldenPatterns([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`^T\d{4,6}$`, "a\nb", `a\nb`, "^x$", "^y$"} {
		if !got[want] {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	if got["patterns"] || got["about"] {
		t.Errorf("structural keys taken as patterns: %v", got)
	}
}
