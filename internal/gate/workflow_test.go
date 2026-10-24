package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

func mustConfig(t *testing.T, text string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The workflows tools/goldens.py had gate_workflow write for each config.
func TestWorkflowGoldens(t *testing.T) {
	dir := filepath.Join("testdata", "py", "workflow")
	tomls, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil || len(tomls) == 0 {
		t.Fatalf("no workflow goldens: %v", err)
	}
	for _, path := range tomls {
		name := strings.TrimSuffix(filepath.Base(path), ".toml")
		t.Run(name, func(t *testing.T) {
			text, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var in struct {
				Project string `json:"project"`
			}
			b, err := os.ReadFile(filepath.Join(dir, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &in); err != nil {
				t.Fatal(err)
			}
			got, err := Workflow(mustConfig(t, string(text)), in.Project)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(dir, name+".yml"))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("workflow differs from Python's (-want +got):\n%s", testkit.Diff(string(want), got))
			}
		})
	}
}

// port: TF GateWorkflowTest.test_the_workflow_mirrors_the_gate_and_only_approves_the_factorys_own_eligible_prs
func TestTheWorkflowMirrorsTheGateAndOnlyApprovesTheFactorysOwnEligiblePRs(t *testing.T) {
	cfg := mustConfig(t, `
[gate]
max_changed_lines = 7
allowed_paths = ["*.md", "test/*"]
[github]
app_slug = "acme-factory"
[projects.gem]
base = "main"
test = "bundle exec rake test"
lint = "bundle exec standardrb"
[projects.shard]
language = "Crystal shard"
test = "crystal spec"
lint = ""
`)
	wf, err := Workflow(cfg, "gem")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`run: "bundle exec rake test"`, "lines <= 7", `["*.md", "test/*"]`, "'acme-factory[bot]'",
		"startsWith(github.head_ref, 'bot/')", "eligible for automatic approval.", "name: factory gate"} {
		if !strings.Contains(wf, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
	// the job that runs the PR's code holds no approving token; the one that approves runs none of it
	testJob, approveJob, ok := strings.Cut(wf, "\n  approve:\n")
	if !ok || strings.Contains(approveJob, "\n  approve:\n") {
		t.Fatal("the workflow has no single approve job")
	}
	if strings.Contains(testJob, "pull-requests: write") || !strings.Contains(testJob, "persist-credentials: false") {
		t.Error("the gate job can approve, or keeps its credentials")
	}
	if !strings.Contains(approveJob, "pull-requests: write") || !strings.Contains(approveJob, "needs: gate") || strings.Contains(approveJob, "checkout") {
		t.Error("the approve job does not need the gate, or checks out the PR's code")
	}
	cr, err := Workflow(cfg, "shard")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cr, "crystal-lang/install-crystal") || !strings.Contains(cr, `run: "true"`) {
		t.Error("the Crystal workflow lacks its setup or the empty lint")
	}
	if !strings.Contains(cr, "--branch v1.7.0 ") {
		t.Error("the default ameba is not 1.7.0")
	}
	for _, text := range []string{wf, cr} {
		checkYAMLShape(t, text)
	}
}

// checkYAMLShape is the structural check that stands in for Python's
// optional `ruby -ryaml`: no tabs, indentation in steps of two, every
// mapping line a key, and quoted scalars that close.
func checkYAMLShape(t *testing.T, text string) {
	t.Helper()
	block := -1 // the indentation of a literal block (run: |, if: >-) being read
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if trimmed == "" {
			continue
		}
		if block >= 0 {
			if indent > block {
				continue
			}
			block = -1
		}
		if strings.Contains(line, "\t") {
			t.Errorf("line %d has a tab: %q", i+1, line)
		}
		if indent%2 != 0 {
			t.Errorf("line %d is indented by %d: %q", i+1, indent, line)
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		item := strings.TrimPrefix(trimmed, "- ")
		key, value, ok := strings.Cut(item, ":")
		if !ok || key == "" || strings.ContainsAny(key, `"' `) && !strings.HasPrefix(trimmed, "- ") {
			t.Errorf("line %d is not a mapping entry: %q", i+1, line)
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case value == "|" || value == ">-":
			block = indent
		case strings.HasPrefix(value, `"`):
			var s string
			if err := json.Unmarshal([]byte(value), &s); err != nil {
				t.Errorf("line %d has a bad double-quoted scalar %s: %v", i+1, value, err)
			}
		}
	}
}

func TestWorkflowRefusals(t *testing.T) {
	cfg := mustConfig(t, `
[github]
app_slug = "acme-factory"
[projects.gem]
path = "/srv/gem"
[projects.notes]
mode = "board"
`)
	for _, c := range []struct{ key, want string }{
		{"nope", "unknown project nope"},
		{"notes", "unknown project notes (a board project has no gate)"},
	} {
		if _, err := Workflow(cfg, c.key); err == nil || err.Error() != c.want {
			t.Errorf("Workflow(%q) = %v, want %q", c.key, err, c.want)
		}
	}
	// D15: no default App slug in the code.
	noSlug := mustConfig(t, "[projects.gem]\npath = \"/srv/gem\"\n")
	_, err := Workflow(noSlug, "gem")
	if err == nil || !strings.Contains(err.Error(), "app_slug") {
		t.Errorf("a config with no app_slug: %v", err)
	}
}

func TestPyJSON(t *testing.T) {
	for in, want := range map[string]string{
		"plain":            `"plain"`,
		`a"b\c`:            `"a\"b\\c"`,
		"tab\tnl\ncr\r":    `"tab\tnl\ncr\r"`,
		"\b\f\x01\x7f":     `"\b\f\u0001\u007f"`,
		"caf\u00e9 \u2713": `"caf\u00e9 \u2713"`,
		"\U0001F600":       `"\ud83d\ude00"`,
		"</script>&'":      `"</script>&'"`,
		"":                 `""`,
		"\u2028\u00a0\x1f": `"\u2028\u00a0\u001f"`,
	} {
		if got := pyJSONString(in); got != want {
			t.Errorf("pyJSONString(%q) = %s, want %s", in, got, want)
		}
	}
	if got := pyJSONList(nil); got != "[]" {
		t.Errorf("empty list: %s", got)
	}
	if got := pyJSONList([]string{"a", "b"}); got != `["a", "b"]` {
		t.Errorf("list: %s", got)
	}
}

// The Crystal setup follows the project's kind, whatever its language
// says; the kind's commands are the gate's.
func TestWorkflowCrystalByKind(t *testing.T) {
	cfg := mustConfig(t, "[github]\napp_slug = \"acme-factory\"\n[projects.ext]\nkind = \"crystal-shard\"\nlanguage = \"Framework extension\"\n")
	wf, err := Workflow(cfg, "ext")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`run: "shards install"`, "crystal-lang/install-crystal", `run: "crystal spec"`} {
		if !strings.Contains(wf, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
	if strings.Contains(wf, "setup-ruby") {
		t.Error("a Crystal kind got the Ruby setup")
	}
	p, _ := cfg.Project("ext")
	if script, err := Script(*p); err != nil || !strings.Contains(script, "shards check || shards install") {
		t.Errorf("Script = %q, %v", script, err)
	}
}
