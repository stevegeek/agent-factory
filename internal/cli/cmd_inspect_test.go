package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"os/exec"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// audit, gate-workflow, look, memory and github.

// The inspect commands on the board Python's goldens ran on.
func TestInspectCommandsMatchPython(t *testing.T) {
	f := goldenRoot(t)
	n := 0
	for _, c := range readGolden(t, "commands.json").Commands {
		if c.Argv[0] != "audit" && c.Argv[0] != "gate-workflow" {
			continue
		}
		n++
		code, out, errs := f.run("", c.Argv...)
		if code != c.Exit || out != c.Stdout || strings.TrimSpace(errs) != strings.TrimSpace(c.Stderr) {
			t.Errorf("%q: got %d, want %d\n%s\nstderr %q, want %q", c.Argv, code, c.Exit, testkit.Diff(c.Stdout, out), errs, c.Stderr)
		}
	}
	if n == 0 {
		t.Fatal("no audit or gate-workflow goldens")
	}
}

// Every audit golden of package gate, printed by `factory audit`.
// port: TF TransitionTest.test_gate_blocks_auto_merge_without_stated_reasoning_and_audit_says_why
func TestAuditPrintsTheAuditMarkdown(t *testing.T) {
	dir := filepath.Join("..", "gate", "testdata", "py", "audit")
	cases, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(cases) == 0 {
		t.Skip("no audit goldens")
	}
	for _, c := range cases {
		want, err := os.ReadFile(strings.TrimSuffix(c, ".json") + ".md")
		if err != nil {
			continue // a case Python refused
		}
		raw, err := os.ReadFile(c)
		if err != nil {
			t.Fatal(err)
		}
		var head struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &head) != nil || head.ID == "" {
			continue
		}
		t.Run(filepath.Base(c), func(t *testing.T) {
			f := newFixture(t)
			f.env.WriteConfig(t, "[projects.gem]\npath = \"/nonexistent/gem\"\n")
			f.env.WriteFile(t, filepath.Join("board", "tickets", head.ID+".json"), string(raw))
			code, out, errs := f.run("", "audit", head.ID)
			if code != 0 || out != string(want) {
				t.Errorf("exit %d, stderr %q\n%s", code, errs, testkit.Diff(string(want), out))
			}
		})
	}
}

// Every workflow golden of package gate, printed by `factory gate-workflow`.
// port: TF GateWorkflowTest.test_the_workflow_mirrors_the_gate_and_only_approves_the_factorys_own_eligible_prs
func TestGateWorkflowPrintsTheYAML(t *testing.T) {
	dir := filepath.Join("..", "gate", "testdata", "py", "workflow")
	cases, _ := filepath.Glob(filepath.Join(dir, "*.toml"))
	if len(cases) == 0 {
		t.Fatal("no workflow goldens")
	}
	for _, c := range cases {
		base := strings.TrimSuffix(c, ".toml")
		want, err := os.ReadFile(base + ".yml")
		if err != nil {
			continue
		}
		var meta struct {
			Project string `json:"project"`
		}
		raw, _ := os.ReadFile(base + ".json")
		if json.Unmarshal(raw, &meta) != nil || meta.Project == "" {
			t.Fatalf("%s: no project", base)
		}
		cfg, _ := os.ReadFile(c)
		t.Run(filepath.Base(base), func(t *testing.T) {
			f := newFixture(t)
			f.env.WriteConfig(t, string(cfg))
			code, out, errs := f.run("", "gate-workflow", meta.Project)
			if code != 0 || out != string(want) {
				t.Errorf("exit %d, stderr %q\n%s", code, errs, testkit.Diff(string(want), out))
			}
		})
	}
	// no App slug: refused, nothing printed
	f := newFixture(t)
	f.env.WriteConfig(t, "[projects.gem]\npath = \"/srv/gem\"\n")
	code, out, errs := f.run("", "gate-workflow", "gem")
	if code != 1 || out != "" || !strings.Contains(errs, "app_slug") || strings.HasPrefix(errs, "factory:") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// port: TF LookTest.test_look_reads_the_local_screen_or_the_helper_hosts_tail
func TestLookShowsTheHeaderAndTheScreen(t *testing.T) {
	f := goldenRoot(t)
	ex := &fakeExec{answer: func(argv []string) (int, string) {
		switch {
		case len(argv) > 1 && strings.HasSuffix(argv[0], "scion") && argv[1] == "look":
			return 0, "2026/10/18 10:00:00 WARN noise\n$ rake test   \nall green\n\n\n"
		case argv[0] == "ssh" && slices.Contains(argv, "tail"):
			return 0, "line one\nline two\n"
		}
		return 1, ""
	}}
	tk := load(t, f, "T0008")
	code, out, errs := opsRun(t, f, opsCLI{}, ex, "look", "T0008", "--lines", "150")
	want := "t0008-w1 · work · started " + tk.Agent.V.Started + "\n$ rake test\nall green\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, stderr %q\n%s", code, errs, testkit.Diff(want, out))
	}
	var lines string
	for _, a := range ex.argv {
		if strings.HasSuffix(a[0], "scion") {
			lines = a[len(a)-1]
		}
	}
	if lines != "150" {
		t.Fatalf("scion asked for %q lines: %q", lines, ex.argv)
	}
	tk = load(t, f, "T0009")
	code, out, errs = opsRun(t, f, opsCLI{}, ex, "look", "T0009")
	want = "t0009-k1 · task @helper · started " + tk.Agent.V.Started + "\nline one\nline two\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, stderr %q\n%s", code, errs, testkit.Diff(want, out))
	}
	// a look clamps what it asks for to 10..400
	ex.argv = nil
	if code, _, errs := opsRun(t, f, opsCLI{}, ex, "look", "T0008", "--lines", "5000"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if a := ex.argv[len(ex.argv)-1]; a[len(a)-1] != "400" {
		t.Fatalf("argv %q", a)
	}
}

// memory against real bare git stores.
// port: TF MemoryTest.test_undo_a_memory_change
func TestMemoryListsAndUndoes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	f := goldenRoot(t)
	e := &Env{Environ: f.env.Vars}
	e.init()
	ctx := context.Background()
	st := &memstore.Store{Paths: e.Paths(), Exec: execx.OS{}}
	repo, err := st.Ensure(ctx, "gem", "worker")
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files, err := repo.Snapshot(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	files["knowledge/bad.md"] = []byte("wrong\n")
	tree, err := repo.WriteTree(ctx, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := repo.CommitTree(ctx, tree, []string{head}, "T0001 work: knowledge/bad.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateRef(ctx, bad, head); err != nil {
		t.Fatal(err)
	}
	code, out, errs := f.run("", "memory")
	if code != 0 || !strings.HasPrefix(out, "== gem/worker\n") || !strings.Contains(out, "T0001 work: knowledge/bad.md") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if code, out, _ := f.run("", "memory", "other"); code != 0 || out != "" {
		t.Fatalf("other project: %d %q", code, out)
	}
	code, out, errs = f.run("", "memory", "gem", "worker", "--revert", bad)
	if code != 0 || out != "undid "+bad+" in gem/worker\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	now, _ := repo.Head(ctx)
	after, err := repo.Snapshot(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after["knowledge/bad.md"]; ok {
		t.Fatal("the change is still there")
	}
	// the refusals: the message, exit 1
	for argv, want := range map[string]string{
		"memory gem --revert " + bad:          "--revert needs the project and the role",
		"memory gem worker --revert " + head:  "that is the store's first commit; there is nothing before it to go back to",
		"memory gem reviewer --revert " + bad: "no memory store for gem/reviewer",
	} {
		code, out, errs := f.run("", strings.Fields(argv)...)
		if code != 1 || out != "" || !strings.Contains(errs, want) || strings.HasPrefix(errs, "factory:") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", argv, code, out, errs)
		}
	}
}

func TestGitHubListsTheReposAndThePermissions(t *testing.T) {
	f := githubRoot(t)
	gh := &fakeGitHub{answer: func(method, path, body string) (int, string) {
		if method == "GET" && path == "/installation/repositories?per_page=100&page=1" {
			return 200, `{"total_count":2,"repositories":[{"full_name":"example/gem","private":true},{"full_name":"example/pub","private":false}]}`
		}
		return 404, `{"message":"Not Found"}`
	}}
	code, out, errs := opsRun(t, f, opsCLI{HTTP: gh}, nil, "github")
	want := "example/gem (private)\nexample/pub \npermissions: contents=write, metadata=read, pull_requests=write\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, stderr %q\n%s", code, errs, testkit.Diff(want, out))
	}
	calls := gh.seen()
	wantCalls := []string{"POST /app/installations/1/access_tokens", "GET /installation/repositories?per_page=100&page=1", "POST /app/installations/1/access_tokens"}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("calls %q, want %q", calls, wantCalls)
	}
	// GitHub refusing is an error: exit 1, nothing on stdout
	gh = &fakeGitHub{answer: func(string, string, string) (int, string) { return 401, `{"message":"Bad credentials"}` }}
	if code, out, _ := opsRun(t, f, opsCLI{HTTP: gh}, nil, "github"); code != 1 || out != "" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
}
