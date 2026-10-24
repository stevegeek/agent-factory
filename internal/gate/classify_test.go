package gate

import (
	"context"
	"testing"
)

// The ClassifyTest and ConfigTest tier cases end to end, on a real branch
// (security/classify_test.go has them on what git says).
func TestClassifyBranch(t *testing.T) {
	cases := []struct {
		port    string
		files   map[string]string
		project string
		want    int
	}{
		// port: TF ClassifyTest.test_docs_only_is_tier_0
		{"ClassifyTest.test_docs_only_is_tier_0", map[string]string{"README.md": "hello world\n"}, "", 0},
		// port: TF ClassifyTest.test_plain_lib_code_is_tier_1
		{"ClassifyTest.test_plain_lib_code_is_tier_1", map[string]string{"lib/thing.rb": "def thing; 2; end\n"}, "", 1},
		// port: TF ClassifyTest.test_project_can_override_tier_paths
		{"ClassifyTest.test_project_can_override_tier_paths", map[string]string{"pyproject.toml": "[project]\n", "notes.adoc": "x\n"}, "", 1},
		{"ClassifyTest.test_project_can_override_tier_paths", map[string]string{"pyproject.toml": "[project]\n", "notes.adoc": "x\n"},
			"tier3_paths = [\"pyproject.toml\"]\ntext_paths = [\"*.adoc\"]", 3},
		// port: TF ConfigTest.test_dangerous_call_is_tier_2
		{"ConfigTest.test_dangerous_call_is_tier_2", map[string]string{"lib/thing.rb": "def thing(x); eval(x); end\n"}, "", 2},
		// port: TF ConfigTest.test_boundary_path_is_tier_2
		{"ConfigTest.test_boundary_path_is_tier_2", map[string]string{"lib/parser.rb": "X = 1\n"}, "", 2},
		// port: TF ConfigTest.test_gemspec_is_tier_3
		{"ConfigTest.test_gemspec_is_tier_3", map[string]string{"x.gemspec": "spec\n"}, "", 3},
		// port: TF ConfigTest.test_boundary_and_build_is_tier_4
		{"ConfigTest.test_boundary_and_build_is_tier_4", map[string]string{"x.gemspec": "spec\n", "lib/parser.rb": "X = 1\n"}, "", 4},
		// A removed sink is not an added one.
		{"removed lines", map[string]string{"lib/thing.rb": "\n"}, "", 1},
	}
	for _, c := range cases {
		t.Run(c.port, func(t *testing.T) {
			r := newRig(t)
			r.repo.Write("lib/thing.rb", "def thing(x); eval(x); end\n")
			r.repo.Commit("a sink on main")
			if c.port == "removed lines" {
				r.repo.BranchWith("b", c.files)
			} else {
				r.repo.Write("lib/thing.rb", "def thing; 1; end\n")
				r.repo.Commit("no sink on main")
				r.repo.BranchWith("b", c.files)
			}
			_, p := r.config(c.project, "")
			got, err := ClassifyBranch(context.Background(), r.runner.Repo(p), "b", p)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("tier %d, want %d", got, c.want)
			}
		})
	}
}
