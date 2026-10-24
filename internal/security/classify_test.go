package security

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// The Python ClassifyTest and ConfigTest tier tests ran classify() on a real
// branch. Classify takes what classify() read from git (the changed files
// and the added lines); gate/classify_test.go runs the same cases end to end
// on real git.

func TestClassify(t *testing.T) {
	sixteen := make([]string, 16)
	for i := range sixteen {
		sixteen[i] = fmt.Sprintf("docs/page%d.md", i)
	}
	cases := []struct {
		port  string
		name  string
		files []string
		added []string
		pats  Patterns
		want  int
	}{
		// port: TF ClassifyTest.test_docs_only_is_tier_0
		{port: "ClassifyTest.test_docs_only_is_tier_0", name: "docs only",
			files: []string{"README.md"}, added: []string{"hello world"}, want: 0},
		// port: TF ClassifyTest.test_plain_lib_code_is_tier_1
		{port: "ClassifyTest.test_plain_lib_code_is_tier_1", name: "plain lib code",
			files: []string{"lib/thing.rb"}, added: []string{"def thing; 2; end"}, want: 1},
		// port: TF ClassifyTest.test_project_can_override_tier_paths
		{port: "ClassifyTest.test_project_can_override_tier_paths", name: "unknown files are plain code",
			files: []string{"notes.adoc", "pyproject.toml"}, added: []string{"x", "[project]"}, want: 1},
		{port: "ClassifyTest.test_project_can_override_tier_paths", name: "the project's own tier paths",
			files: []string{"notes.adoc", "pyproject.toml"}, added: []string{"x", "[project]"},
			pats: Patterns{Tier3: []string{"pyproject.toml"}, Text: []string{"*.adoc"}}, want: 3},
		// port: TF ConfigTest.test_dangerous_call_is_tier_2
		{port: "ConfigTest.test_dangerous_call_is_tier_2", name: "eval added",
			files: []string{"lib/thing.rb"}, added: []string{"def thing(x); eval(x); end"}, want: 2},
		// port: TF ConfigTest.test_boundary_path_is_tier_2
		{port: "ConfigTest.test_boundary_path_is_tier_2", name: "a parser",
			files: []string{"lib/parser.rb"}, added: []string{"X = 1"}, want: 2},
		// port: TF ConfigTest.test_gemspec_is_tier_3
		{port: "ConfigTest.test_gemspec_is_tier_3", name: "a gemspec",
			files: []string{"x.gemspec"}, added: []string{"spec"}, want: 3},
		// port: TF ConfigTest.test_boundary_and_build_is_tier_4
		{port: "ConfigTest.test_boundary_and_build_is_tier_4", name: "a parser and a gemspec",
			files: []string{"lib/parser.rb", "x.gemspec"}, added: []string{"X = 1", "spec"}, want: 4},
		// §10.7 S10: more than 15 files is tier 4, whatever they are.
		{name: "sixteen doc files", files: sixteen, want: 4},
		{name: "fifteen doc files", files: sixteen[:15], want: 0},
		{name: "no files", want: 0},
		{name: "a text file with a sink in it", files: []string{"README.md"}, added: []string{"run `system(cmd)`"}, want: 2},
		{name: "a text file with a sink, and a build file", files: []string{"README.md", "Gemfile"},
			added: []string{"system(x)"}, want: 4},
		{name: "a build file wins over a text pattern", files: []string{"bin/README.md"}, want: 3},
		{name: "a text pattern wins over a path word", files: []string{"docs/parser.md"}, want: 0},
		{name: "* crosses /", files: []string{"lib/a/b.md"}, want: 0},
		{name: "fnmatch is case-sensitive", files: []string{"README.MD"}, want: 1},
		{name: "a path word in any case", files: []string{"lib/JSONish.rb"}, want: 2},
		{name: "a dotless i still folds to i (Python re.I)", files: []string{"lib/ınput.rb"}, want: 2},
		{name: "a project with no text paths", files: []string{"README.md"},
			pats: Patterns{Tier3: DefaultTier3Paths(), Text: []string{}}, want: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pats := c.pats
			if pats.Tier3 == nil && pats.Text == nil {
				pats = DefaultPatterns()
			}
			if got := Classify(c.files, c.added, pats); got != c.want {
				t.Errorf("Classify(%q, %q) = %d, want %d (%s)", c.files, c.added, got, c.want, c.port)
			}
		})
	}
}

// port: TF ProjectFactsTest.test_crystal_sinks_and_files_raise_the_security_tier
func TestCrystalSinksAndFilesRaiseTheTier(t *testing.T) {
	for _, line := range []string{`Process.run("sh", ["-c", input])`, "Config.from_json(body)", "ptr = pointerof(x)", "x.unsafe_as(Int32)"} {
		if !Tier2Content(line) {
			t.Errorf("Tier2Content(%q) = false", line)
		}
	}
	if !slices.ContainsFunc(DefaultTier3Paths(), func(p string) bool { return textx.Fnmatch("shard.yml", p) }) {
		t.Error("shard.yml is not a tier 3 path")
	}
	if got := Classify([]string{"shard.yml"}, nil, DefaultPatterns()); got != 3 {
		t.Errorf("shard.yml is tier %d", got)
	}
}

func TestProjectPatterns(t *testing.T) {
	cfg, err := config.Parse([]byte(`
[projects.plain]
path = "/srv/plain"
[projects.own]
path = "/srv/own"
tier3_paths = ["pyproject.toml"]
text_paths = []
`))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := cfg.Project("plain")
	if got := ProjectPatterns(*plain); !slices.Equal(got.Tier3, DefaultTier3Paths()) || !slices.Equal(got.Text, DefaultTextPaths()) {
		t.Errorf("defaults: %+v", got)
	}
	own, _ := cfg.Project("own")
	got := ProjectPatterns(*own)
	if !slices.Equal(got.Tier3, []string{"pyproject.toml"}) || len(got.Text) != 0 {
		t.Errorf("overrides: %+v", got)
	}
	// An empty override is still an override: README.md is plain code there.
	if tier := Classify([]string{"README.md"}, nil, got); tier != 1 {
		t.Errorf("README.md with text_paths = [] is tier %d", tier)
	}
}

func TestDefaultsAreFreshCopies(t *testing.T) {
	a := DefaultTier3Paths()
	a[0] = "changed"
	if DefaultTier3Paths()[0] == "changed" {
		t.Error("DefaultTier3Paths shares its slice")
	}
	b := AngleGroups()
	b[0][0] = "changed"
	if AngleGroups()[0][0] == "changed" {
		t.Error("AngleGroups shares its slices")
	}
}

// The tier patterns as tools/goldens.py wrote them: every default pattern
// has a row in textx's fnmatch golden, and the Python lists are exactly ours.
func TestPatternsMatchThePythonLists(t *testing.T) {
	var golden struct {
		Rows []struct {
			Pattern string `json:"pattern"`
		} `json:"rows"`
	}
	readGolden(t, filepath.Join("..", "textx", "testdata", "py", "fnmatch.json"), &golden)
	have := map[string]bool{}
	for _, r := range golden.Rows {
		have[r.Pattern] = true
	}
	for _, p := range append(DefaultTier3Paths(), DefaultTextPaths()...) {
		if !have[p] {
			t.Errorf("pattern %q has no fnmatch golden", p)
		}
	}
	wantTier3 := []string{"*.gemspec", "Gemfile", "Gemfile.lock", "gemfiles/*", "Rakefile", ".github/*", "bin/*",
		"*extconf.rb", "Appraisals", "package.json", "*.lock", "shard.yml", "Makefile", "script/*"}
	wantText := []string{"*.md", "*.txt", "*.rdoc", "docs/*", "LICENSE*", "CHANGELOG*", ".standard.yml", ".rubocop.yml",
		"*.rbs", "sig/*", ".ameba.yml", ".editorconfig"}
	if !slices.Equal(DefaultTier3Paths(), wantTier3) || !slices.Equal(DefaultTextPaths(), wantText) {
		t.Errorf("tier paths differ from TIER3_PATHS / TEXT_PATHS")
	}
}

// Every case tools/goldens.py ran TIER2_CONTENT (re.search) and TIER2_PATH
// (re.search, re.I) over: the same match, and the same text matched.
func TestTierRegexesMatchPython(t *testing.T) {
	var golden struct {
		Patterns []struct {
			Name  string `json:"name"`
			Cases []struct {
				In  string          `json:"in"`
				Out json.RawMessage `json:"out"`
			} `json:"cases"`
		} `json:"patterns"`
	}
	readGolden(t, filepath.Join("..", "textx", "testdata", "py", "regex.json"), &golden)
	funcs := map[string]func(string) (string, bool){"tier2_content": tier2ContentMatch, "tier2_path": tier2PathMatch}
	seen := 0
	for _, p := range golden.Patterns {
		f, ok := funcs[p.Name]
		if !ok {
			continue
		}
		seen++
		for _, c := range p.Cases {
			var out []*string
			if err := json.Unmarshal(c.Out, &out); err != nil {
				t.Fatalf("%s %q: %v", p.Name, c.In, err)
			}
			got, matched := f(c.In)
			if matched != (out != nil) || matched && got != *out[0] {
				t.Errorf("%s(%q) = %q, %v; Python: %s", p.Name, c.In, got, matched, c.Out)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("found %d of the 2 tier patterns in the regex golden", seen)
	}
}

func readGolden(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
