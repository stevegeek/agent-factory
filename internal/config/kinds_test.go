package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func kindProject(t *testing.T, c *Config, key string) Project {
	t.Helper()
	p, ok := c.Project(key)
	if !ok {
		t.Fatalf("no project %s", key)
	}
	return *p
}

func TestBuiltinKinds(t *testing.T) {
	ks := BuiltinKinds()
	var names, tcs []string
	for _, k := range ks {
		names = append(names, k.Name)
		tcs = append(tcs, k.Toolchain)
		if k.Source != "core" || k.From != "" || k.GateMemory != "" || !k.PinnedSet {
			t.Errorf("%s: %+v", k.Name, k)
		}
	}
	if !reflect.DeepEqual(names, []string{"ruby-gem", "rails-gem", "crystal-shard", "go-module", "docs"}) ||
		!reflect.DeepEqual(tcs, []string{"ruby", "ruby", "crystal", "go", "none"}) {
		t.Errorf("names %q toolchains %q", names, tcs)
	}
	ks[0].Name, ks[2].PinnedUntracked[0] = "changed", "changed"
	again := BuiltinKinds()
	if again[0].Name != "ruby-gem" || again[2].PinnedUntracked[0] != "lib" {
		t.Errorf("a caller's change reached the next call: %+v", again)
	}
	if !reflect.DeepEqual(ToolchainNames(), []string{"ruby", "crystal", "go", "none"}) {
		t.Errorf("ToolchainNames = %q", ToolchainNames())
	}
}

func TestProjectKindFillsMissingKeys(t *testing.T) {
	c := mustParse(t, "[projects.s]\nkind = \"crystal-shard\"\npath = \"/p\"\ngithub = \"o/s\"\nbase = \"main\"\n")
	p := kindProject(t, c, "s")
	if p.Kind != "crystal-shard" || p.Install != "shards check || shards install" || p.Test != "crystal spec" ||
		p.Lint != "crystal tool format --check src spec && ameba src spec" {
		t.Errorf("project %+v", p)
	}
	if !reflect.DeepEqual(p.PinnedUntracked, []string{"lib", "shard.lock"}) {
		t.Errorf("pinned_untracked %q", p.PinnedUntracked)
	}
	if p.Language != "Crystal shard" || p.LanguageLabel() != "Crystal shard" {
		t.Errorf("language %q, label %q", p.Language, p.LanguageLabel())
	}
	// gate.Script asks Has of each command
	for _, k := range []string{"install", "test", "lint", "language", "pinned_untracked"} {
		if !p.Has(k) {
			t.Errorf("Has(%q) false", k)
		}
	}
	if p.Has("gate_memory") || c.GateMemory(p) != "900m" {
		t.Errorf("gate_memory: Has %v, GateMemory %q", p.Has("gate_memory"), c.GateMemory(p))
	}
	if k, inferred := c.KindOf(p); k.Name != "crystal-shard" || inferred || c.Toolchain(p) != "crystal" {
		t.Errorf("KindOf %+v %v, toolchain %q", k, inferred, c.Toolchain(p))
	}
	if _, warnings := c.Problems(nil); slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "has no") }) {
		t.Errorf("warnings %q", warnings)
	}
}

func TestProjectKeysOverKind(t *testing.T) {
	c := mustParse(t, "[projects.s]\nkind = \"crystal-shard\"\ntest = \"make spec\"\npinned_untracked = []\nlanguage = \"Crystal shard: x\"\n")
	p := kindProject(t, c, "s")
	if p.Test != "make spec" || p.Install != "shards check || shards install" || p.Language != "Crystal shard: x" {
		t.Errorf("project %+v", p)
	}
	if p.PinnedUntracked == nil || len(p.PinnedUntracked) != 0 || !p.Has("pinned_untracked") {
		t.Errorf("pinned_untracked %#v", p.PinnedUntracked)
	}
}

func TestNoKindLeavesProjectAsToday(t *testing.T) {
	c := mustParse(t, "[projects.s]\nlanguage = \"Crystal shard: x\"\n\n[projects.g]\npath = \"/g\"\n")
	s := kindProject(t, c, "s")
	if k, inferred := c.KindOf(s); k.Name != "crystal-shard" || !inferred || c.Toolchain(s) != "crystal" {
		t.Errorf("KindOf %+v %v", k, inferred)
	}
	if s.Kind != "" || s.Has("pinned_untracked") || s.Install != "" || s.PinnedUntracked != nil || s.Has("install") {
		t.Errorf("shard %+v", s)
	}
	g := kindProject(t, c, "g")
	if k, inferred := c.KindOf(g); k.Name != "ruby-gem" || !inferred || c.Toolchain(g) != "ruby" {
		t.Errorf("KindOf %+v %v", k, inferred)
	}
	if g.Language != "" || g.LanguageLabel() != "Ruby gem" || g.Has("language") || g.Test != "" {
		t.Errorf("gem %+v", g)
	}
}

func TestConfigKindOverridesBuiltin(t *testing.T) {
	c := mustParse(t, "[kinds.ruby-gem]\nlint = \"true\"\n\n[projects.g]\nkind = \"ruby-gem\"\n")
	k, ok := c.KindNamed("ruby-gem")
	if !ok || k.Lint != "true" || k.Test != "bundle exec rake test" || k.Source != "config" || k.Toolchain != "ruby" ||
		!reflect.DeepEqual(k.PinnedUntracked, []string{"Gemfile.lock"}) {
		t.Errorf("kind %+v %v", k, ok)
	}
	ks := c.Kinds()
	if len(ks) != 5 || ks[0].Name != "ruby-gem" || ks[0].Source != "config" || ks[1].Source != "core" {
		t.Errorf("Kinds %+v", ks)
	}
	if p := kindProject(t, c, "g"); p.Lint != "true" || p.Test != "bundle exec rake test" {
		t.Errorf("project %+v", p)
	}
	if len(c.Unknown) != 0 {
		t.Errorf("unknown %q", c.Unknown)
	}
}

func TestConfigKindFrom(t *testing.T) {
	c := mustParse(t, "[gate]\nheavy_memory = \"1200m\"\n\n[kinds.ext-shard]\nfrom = \"crystal-shard\"\ngate_memory = \"1800m\"\n\n"+
		"[projects.s]\nkind = \"ext-shard\"\n")
	k, ok := c.KindNamed("ext-shard")
	if !ok || k.Toolchain != "crystal" || k.From != "crystal-shard" || k.Source != "config" || k.Test != "crystal spec" {
		t.Errorf("kind %+v %v", k, ok)
	}
	if ks := c.Kinds(); len(ks) != 6 || ks[5].Name != "ext-shard" {
		t.Errorf("Kinds %+v", ks)
	}
	p := kindProject(t, c, "s")
	if c.GateMemory(p) != "1800m" || !c.HeavyGate(p) || c.Toolchain(p) != "crystal" {
		t.Errorf("GateMemory %q, heavy %v, toolchain %q", c.GateMemory(p), c.HeavyGate(p), c.Toolchain(p))
	}
	if problems, _ := c.Problems(nil); len(problems) != 0 {
		t.Errorf("problems %q", problems)
	}
}

func TestKindProblems(t *testing.T) {
	c := mustParse(t, "[kinds.a]\ntoolchain = \"cobol\"\n\n[kinds.b]\nfrom = \"bigproj\"\n\n[kinds.c]\nlint = \"true\"\n\n"+
		"[projects.p]\nkind = \"x\"\ninstall = \"i\"\ntest = \"t\"\nlint = \"l\"\n\n[projects.b]\nmode = \"board\"\nkind = \"ruby-gem\"\n")
	problems, warnings := c.Problems(nil)
	for _, want := range []string{
		`[projects.p] kind = "x": no such kind (factory kinds lists them)`,
		`[kinds.a] toolchain = "cobol": one of ruby, crystal, go, none`,
		`[kinds.b] from = "bigproj": not a built-in kind`,
		`[kinds.c]: no toolchain (set toolchain or from)`,
	} {
		if n := slices.Index(problems, want); n < 0 || slices.Index(problems[n+1:], want) >= 0 {
			t.Errorf("want %q once in %q", want, problems)
		}
	}
	if len(problems) != 4 {
		t.Errorf("problems %q", problems)
	}
	if !slices.Contains(warnings, "[projects.b] is a board project; kind are not used") {
		t.Errorf("warnings %q", warnings)
	}
	if len(c.Unknown) != 0 {
		t.Errorf("unknown %q", c.Unknown)
	}
	if p := kindProject(t, c, "p"); c.Toolchain(p) != "" {
		t.Errorf("unknown kind's toolchain %q", c.Toolchain(p))
	}
	// a key a kind does not have is reported as for any table
	c = mustParse(t, "[kinds.d]\nfrom = \"ruby-gem\"\ncolour = \"red\"\n")
	if !reflect.DeepEqual(c.Unknown, []string{"kinds.d.colour"}) {
		t.Errorf("unknown %q", c.Unknown)
	}
}

func TestKindsKeepFixtureProjects(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "kinds", "projects.toml"))
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		kind, install, test, lint, mem, label, language string
		pinned                                          []string
	}
	shardLint := "crystal tool format --check src spec && ameba src spec"
	wants := map[string]want{
		"plain_gem":         {"ruby-gem", "bundle check", "bundle exec rake test", "bundle exec standardrb", "900m", "Ruby gem", "", []string{"Gemfile.lock"}},
		"own_install_shard": {"crystal-shard", "shards install --frozen", "crystal spec", shardLint, "900m", "Crystal shard", "Crystal shard", []string{"lib", "shard.lock"}},
		"heavy_shard": {"crystal-shard", "shards check || shards install", "crystal spec", shardLint, "1800m",
			"Crystal shard: an extension for a web framework", "Crystal shard: an extension for a web framework", []string{"lib", "shard.lock"}},
	}
	text := string(b)
	withKinds := text
	for key, w := range wants {
		withKinds = strings.Replace(withKinds, "[projects."+key+"]\n", "[projects."+key+"]\nkind = \""+w.kind+"\"\n", 1)
	}
	for name, src := range map[string]string{"no kind": text, "kind set": withKinds} {
		c := mustParse(t, src)
		if problems, warnings := c.Problems(nil); len(problems)+len(warnings) != 0 {
			t.Errorf("%s: %q %q", name, problems, warnings)
		}
		for key, w := range wants {
			// a kind fills a missing language, which LanguageLabel already said
			if key == "plain_gem" && name == "kind set" {
				w.language = "Ruby gem"
			}
			p := kindProject(t, c, key)
			k, inferred := c.KindOf(p)
			got := want{k.Name, p.Install, p.Test, p.Lint, c.GateMemory(p), p.LanguageLabel(), p.Language, p.PinnedUntracked}
			if !reflect.DeepEqual(got, w) || inferred != (name == "no kind") {
				t.Errorf("%s %s: got %+v (inferred %v)\nwant %+v", name, key, got, inferred, w)
			}
		}
	}
}

func TestKindOfInferredIgnoresConfigKinds(t *testing.T) {
	c := mustParse(t, `[kinds.ruby-gem]
toolchain = "cobol"

[kinds.crystal-shard]
toolchain = "cobol"

[projects.g]
repo = "example/g"
language = "Ruby gem"

[projects.s]
repo = "example/s"
language = "Crystal shard"

[projects.e]
repo = "example/e"
kind = "ruby-gem"
`)
	for key, want := range map[string]string{"g": "ruby", "s": "crystal", "e": "cobol"} {
		p := kindProject(t, c, key)
		if got := c.Toolchain(p); got != want {
			t.Errorf("%s: toolchain %q, want %q", key, got, want)
		}
	}
	if k, inferred := c.KindOf(kindProject(t, c, "g")); !inferred || k.Source != KindCore {
		t.Errorf("inferred kind %+v (inferred %v)", k, inferred)
	}
}

func TestGoModuleAndDocsKinds(t *testing.T) {
	c := mustParse(t, "[projects.f]\nlanguage = \"Go module, standard library only\"\npath = \"/f\"\n\n"+
		"[projects.n]\nkind = \"docs\"\npath = \"/n\"\n\n[projects.m]\nkind = \"go-module\"\npath = \"/m\"\n")
	if k, inferred := c.KindOf(kindProject(t, c, "f")); k.Name != "go-module" || !inferred || k.Toolchain != "go" {
		t.Errorf("a Go module's language infers %+v %v", k, inferred)
	}
	if p := kindProject(t, c, "m"); p.Install != "go version" || p.Test != "go vet ./... && go test -count=1 ./..." ||
		!strings.Contains(p.Lint, "gofmt -l .") || len(p.PinnedUntracked) != 0 || c.Toolchain(p) != "go" {
		t.Errorf("go-module project %+v", p)
	}
	if p := kindProject(t, c, "n"); p.Install != "true" || p.Test != "true" || p.Lint != "true" || c.Toolchain(p) != "none" {
		t.Errorf("docs project %+v", p)
	}
	if problems, _ := c.Problems(nil); slices.ContainsFunc(problems, func(s string) bool { return strings.Contains(s, "kind") }) {
		t.Errorf("problems %q", problems)
	}
}

// [hosts.<name>] kinds and not_kinds match the project's kind, as written
// or inferred; `factory check` names the rule and warns of a kind that
// does not exist.
func TestHostKinds(t *testing.T) {
	c, err := Parse([]byte(`[projects.g]
path = "/srv/g"
language = "Go module"

[projects.r]
path = "/srv/r"
kind = "ruby-gem"

[hosts.hostx]
transport = "pull"
not_kinds = ["go-module"]

[hosts.hosty]
transport = "pull"
kinds = ["ruby-gem", "nosuch"]

[hosts.hostz]
transport = "pull"
`))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := c.Project("g")
	r, _ := c.Project("r")
	if why, ok := c.HostRefuses("hostx", *g); !ok || why != "hostx does not take go-module projects ([hosts.hostx] never go-module)" {
		t.Errorf("hostx, g: %q %v", why, ok)
	}
	for _, x := range []struct {
		host string
		p    *Project
	}{{"hostx", r}, {"hosty", r}, {"hostz", g}, {"local", g}} {
		if why, ok := c.HostRefuses(x.host, *x.p); ok {
			t.Errorf("%s, %s: %q", x.host, x.p.Key, why)
		}
	}
	if _, ok := c.HostRefuses("hosty", *g); !ok {
		t.Error("hosty takes g")
	}
	want := []string{"[hosts.hostx] never go-module: never runs g", "[hosts.hosty] takes only ruby-gem, nosuch: never runs g"}
	if got := c.KindNotes(); !slices.Equal(got, want) {
		t.Errorf("notes %q", got)
	}
	_, warnings := c.Problems(nil)
	if !slices.Contains(warnings, `[hosts.hosty] kinds names "nosuch", which is no project kind (`+"`factory kinds`)") {
		t.Errorf("warnings %q", warnings)
	}
}
