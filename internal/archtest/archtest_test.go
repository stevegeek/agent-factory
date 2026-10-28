package archtest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

const manifestFile = "testdata/python-tests.txt"

// moduleRoot is the core root: two levels above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", dir, err)
	}
	return dir
}

// TestRepository runs every rule against the real module.
func TestRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the whole module with go list -export")
	}
	root := moduleRoot(t)
	g, err := Load(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if g.Module != "github.com/stevegeek/agent-factory" {
		t.Fatalf("module path = %q", g.Module)
	}
	t.Logf("%d module packages, %d packages listed in all", len(g.Pkgs), len(g.All))
	for _, want := range []string{".", "cmd/factory", "cmd/foreman", "cmd/factory-remote", "cmd/factory-pull", "internal/archtest"} {
		if g.pkg(want) == nil {
			t.Errorf("package %s not loaded", want)
		}
	}
	check := func(name string, vs []Violation) {
		t.Run(name, func(t *testing.T) {
			for _, v := range vs {
				t.Error(v)
			}
		})
	}
	check("load", CheckLoad(g))
	check("table", CheckTable(g))
	check("foreman-closure", CheckForemanClosure(g))
	check("helper-closure", CheckHelperClosure(g))
	check("pull-closure", CheckPullClosure(g))
	check("stdlib", CheckStdlibOnly(g))
	check("size", CheckSize(g, MaxPackageLines))
	check("globals", CheckGlobals(g))
	check("purity", CheckDomainPurity(g))
	check("interfaces", CheckInterfaceSize(g))
	check("hermetic", CheckHermeticTests(g))

	t.Run("regex", func(t *testing.T) {
		files, err := RegexGoldenFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		golden, err := GoldenPatterns(files)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range CheckRegexGoldens(g, golden) {
			t.Error(v)
		}
	})

	t.Run("port", func(t *testing.T) {
		// every Python test has its Go port; FACTORY_PORT_COMPLETE=0
		// relaxes this while a slice is half done
		complete := os.Getenv("FACTORY_PORT_COMPLETE") != "0"
		missing, vs, err := CheckPortMarkers(root, manifestFile, complete)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vs {
			t.Error(v)
		}
		if len(missing) > 0 && !complete {
			t.Logf("%d Python tests have no // port: marker yet (FACTORY_PORT_COMPLETE=0 only)", len(missing))
		}
	})

	t.Run("personal", func(t *testing.T) {
		path := os.Getenv("FACTORY_PERSONAL_STRINGS")
		if path == "" {
			t.Skip("FACTORY_PERSONAL_STRINGS not set (the list lives in the private config repo)")
		}
		strs, err := ReadPersonalStrings(path)
		if err != nil {
			t.Fatal(err)
		}
		vs, err := CheckPersonalStrings(root, strs)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vs {
			t.Error(v)
		}
	})
}

// The committed manifest, frozen from tests/*.py at the cutover, has all
// 192 methods, each once.
func TestManifest(t *testing.T) {
	manifest, err := ReadManifest(manifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) < 192 {
		t.Errorf("manifest has %d tests, want 192", len(manifest))
	}
	seen := map[string]bool{}
	for _, m := range manifest {
		if seen[m] {
			t.Errorf("duplicate manifest entry %s", m)
		}
		seen[m] = true
	}
}

// The table has a row for every package and nothing else, and the computed
// rows hold what the rules say.
func TestTableMatchesThePlan(t *testing.T) {
	tab := Table()
	var names []string
	for k := range tab {
		names = append(names, k)
	}
	sort.Strings(names)
	want := []string{
		".", "cmd/factory", "cmd/factory-pull", "cmd/factory-remote", "cmd/foreman",
		"internal/api", "internal/archtest", "internal/auth", "internal/backup", "internal/boardread", "internal/buildinfo", "internal/builds", "internal/cli",
		"internal/clock", "internal/config", "internal/deploy", "internal/deployrec", "internal/desk", "internal/doors", "internal/execx", "internal/fizzy", "internal/fsx",
		"internal/gate", "internal/github", "internal/gitread", "internal/gitx", "internal/gocache", "internal/guard", "internal/helper",
		"internal/hostrun", "internal/hosts", "internal/imagebuild", "internal/jobproto", "internal/jobtail",
		"internal/jsonx", "internal/logx", "internal/mailbox", "internal/mcp", "internal/mdhtml", "internal/mdout", "internal/memory",
		"internal/memstore", "internal/model", "internal/onboard", "internal/operator", "internal/ops", "internal/panel", "internal/paths", "internal/pipeline",
		"internal/podman", "internal/present", "internal/prflow", "internal/pull", "internal/push", "internal/render",
		"internal/scion", "internal/scionread", "internal/screens", "internal/security", "internal/sshjobs", "internal/stage",
		"internal/store", "internal/sysinfo", "internal/testkit", "internal/textx", "internal/thread", "internal/ticket", "internal/ticketapi", "internal/tiers",
		"internal/tokens", "internal/toml", "internal/upkeep", "internal/usage", "internal/vitals", "internal/wake", "internal/web", "internal/webpush",
	}
	if !slices.Equal(names, want) {
		t.Errorf("table rows:\n got %v\nwant %v", names, want)
	}
	// Every import names a row.
	for from, imps := range tab {
		for imp := range imps {
			if _, ok := tab[imp]; !ok {
				t.Errorf("%s may import %s, which has no row", from, imp)
			}
		}
	}
	pipe := tab["internal/pipeline"]
	for _, no := range []string{"ticketapi", "present", "mdhtml", "tokens", "ops", "cli", "web", "render", "helper"} {
		if pipe["internal/"+no] {
			t.Errorf("pipeline may import %s", no)
		}
	}
	for _, yes := range []string{"prflow", "doors", "upkeep", "hostrun", "stage", "store", "clock"} {
		if !pipe["internal/"+yes] {
			t.Errorf("pipeline may not import %s", yes)
		}
	}
	cli := tab["internal/cli"]
	if cli["internal/helper"] || !cli["internal/pipeline"] || !cli["internal/render"] || !cli["internal/buildinfo"] || cli["internal/web"] {
		t.Errorf("cli row wrong: %v", cli)
	}
	// Decisions stay out of HTTP: no presentation or HTTP package imports store.
	for _, p := range []string{"web", "api", "mcp", "push", "present"} {
		if tab["internal/"+p]["internal/store"] {
			t.Errorf("%s may import store", p)
		}
	}
	// No allowed edge leads Foreman to a forbidden package: walk the table
	// from cmd/foreman.
	forbidden := map[string]bool{}
	for _, f := range ForemanForbidden() {
		forbidden[relOf(f)] = true
	}
	reach := map[string]bool{}
	var walk func(string)
	walk = func(p string) {
		if reach[p] {
			return
		}
		reach[p] = true
		for imp := range tab[p] {
			walk(imp)
		}
	}
	walk("cmd/foreman")
	for p := range reach {
		if forbidden[p] {
			t.Errorf("the table lets cmd/foreman reach %s", p)
		}
	}
	reach = map[string]bool{}
	walk("cmd/factory-remote")
	allowed := map[string]bool{}
	for _, a := range HelperAllowed() {
		allowed[relOf(a)] = true
	}
	for p := range reach {
		if p != "cmd/factory-remote" && !allowed[p] {
			t.Errorf("the table lets cmd/factory-remote reach %s", p)
		}
	}
	// The helper's watchdog pushes through webpush, and webpush leads to
	// no board: its whole closure is inside the helper's.
	if !reach["internal/webpush"] || !tab["internal/push"]["internal/webpush"] {
		t.Errorf("webpush is not in the helper's and push's reach")
	}
	reach = map[string]bool{}
	walk("internal/webpush")
	for p := range reach {
		if !allowed[p] {
			t.Errorf("webpush reaches %s, outside the helper's closure", p)
		}
	}
}

func TestViolationsSortAndPrint(t *testing.T) {
	vs := sortViolations([]Violation{{"b", "x", "2"}, {"a", "y", "1"}, {"a", "x", "1"}})
	var b bytes.Buffer
	for _, v := range vs {
		b.WriteString(v.String() + "\n")
	}
	if got := b.String(); got != "a: x: 1\na: y: 1\nb: x: 2\n" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(Violation{"r", "w", "d"}.String(), "r: w: d") {
		t.Fatal("String")
	}
}
