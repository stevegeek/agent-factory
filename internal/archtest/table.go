package archtest

import (
	"slices"
	"strings"
)

// A row of the allowed-imports table. Imports name other rows by their
// short name; "agentfactory" is the module root package.
type row struct {
	layer   int
	name    string
	imports []string
}

// rows is the import table. pipeline and cli are computed from
// the layers in Table.
func rows() []row {
	return []row{
		{0, "clock", nil},
		{0, "textx", nil},
		{0, "jsonx", nil},
		{0, "toml", nil},
		{0, "fsx", nil},
		{0, "execx", nil},
		{0, "paths", nil},
		{0, "buildinfo", nil},
		{0, "logx", []string{"clock", "fsx"}},
		{0, "vitals", []string{"buildinfo"}},

		{1, "model", nil},
		{1, "ticket", []string{"model", "clock", "jsonx", "textx"}},
		{1, "config", []string{"model", "toml", "textx", "fsx", "logx"}},

		{2, "boardread", []string{"paths", "ticket", "model", "jsonx", "fsx", "clock", "logx"}},
		{2, "store", []string{"boardread", "paths", "ticket", "model", "jsonx", "fsx", "clock", "logx"}},
		{2, "tokens", []string{"paths", "jsonx", "fsx", "clock"}},
		{2, "jobproto", []string{"jsonx", "textx"}},
		{2, "mdout", []string{"ticket", "model", "textx"}},
		{2, "mdhtml", []string{"textx"}},
		{2, "wake", []string{"paths", "execx", "fsx", "clock"}},
		{2, "gitread", []string{"execx", "paths", "textx"}},
		{2, "gitx", []string{"gitread", "execx", "fsx", "paths", "textx"}},
		{2, "scionread", []string{"execx", "paths", "jsonx"}},
		{2, "scion", []string{"scionread", "execx", "fsx", "paths", "jsonx", "textx"}},
		{2, "podman", []string{"execx", "textx"}},
		{2, "github", []string{"clock", "jsonx", "fsx", "textx"}},
		{2, "jobtail", []string{"jobproto", "execx", "fsx"}},
		{2, "mailbox", []string{"jobproto", "vitals", "fsx", "clock"}},
		{2, "sshjobs", []string{"jobproto", "execx"}},
		{2, "screens", []string{"fsx", "clock"}},
		{2, "deployrec", []string{"fsx"}},
		{2, "sysinfo", []string{"vitals", "config", "deployrec", "ticket", "model", "paths", "fsx", "clock"}},

		{3, "imagebuild", []string{"podman", "execx", "fsx", "logx", "clock", "textx"}},
		{3, "memstore", []string{"gitx", "gitread", "execx", "fsx", "paths", "textx"}},
		{3, "memory", []string{"memstore", "ticket", "config", "model", "paths", "jsonx", "textx", "clock"}},
		{3, "security", []string{"model", "config", "textx"}},
		{3, "panel", []string{"model", "ticket", "jsonx", "textx"}},
		{3, "hosts", []string{"model", "ticket", "config", "clock", "textx"}},
		{3, "gate", []string{"security", "gitx", "gitread", "podman", "config", "ticket", "model", "jsonx", "execx", "fsx", "paths", "clock", "textx"}},
		{3, "stage", []string{"model", "ticket", "config", "security", "panel", "memory", "mdout", "jsonx", "textx", "clock"}},
		{3, "hostrun", []string{"scion", "scionread", "sshjobs", "jobtail", "jobproto", "ticket", "config", "model", "execx", "fsx", "paths", "clock", "textx"}},
		{3, "ticketapi", []string{"store", "boardread", "tokens", "config", "ticket", "model", "mdout", "jsonx", "clock", "textx", "logx"}},
		{3, "present", []string{"model", "ticket", "textx", "clock"}},
		{3, "builds", []string{"jobproto", "fsx"}},
		{3, "tiers", []string{"model", "config", "toml", "textx"}},

		{4, "ops", []string{"store", "boardread", "deployrec", "config", "ticket", "model", "wake", "gitread", "scionread", "jobtail", "paths", "jsonx", "textx", "clock", "logx"}},
		{4, "prflow", []string{"github", "gitx", "gitread", "gate", "security", "store", "config", "ticket", "model", "mdout", "jsonx", "textx", "clock", "logx"}},
		{4, "doors", []string{"store", "boardread", "config", "ticket", "model", "github", "memory", "execx", "fsx", "paths", "jsonx", "textx", "clock", "logx"}},
		{4, "upkeep", []string{"imagebuild", "doors", "gate", "github", "store", "config", "ticket", "model", "execx", "paths", "clock", "logx", "textx"}},
		{4, "backup", []string{"store", "boardread", "doors", "config", "paths", "fsx", "jsonx", "clock", "logx", "textx"}},
		{4, "pipeline", nil}, // computed
		{4, "render", []string{"config", "model", "paths", "fsx", "textx", "agentfactory"}},
		{4, "helper", []string{"jobproto", "mailbox", "vitals", "imagebuild", "podman", "toml", "fsx", "execx", "logx", "textx", "clock", "buildinfo"}},
		{4, "pull", []string{"jobproto", "vitals", "toml", "fsx", "execx", "clock", "textx", "mailbox"}},
		{4, "onboard", []string{"config", "github", "paths", "jsonx", "textx"}},
		{4, "deploy", []string{"deployrec", "sshjobs", "hostrun", "jobproto", "gitx", "store", "boardread", "config", "ticket", "model", "execx", "fsx", "paths", "clock", "logx", "textx"}}, // boardread: its tests build a store.Board
		{4, "operator", []string{"tiers", "github", "gitx", "gitread", "store", "boardread", "config", "ticket", "model", "execx", "fsx", "paths", "clock", "logx", "textx"}},                // boardread: its tests build a store.Board
		{4, "fizzy", []string{"store", "boardread", "paths", "ticketapi", "config", "ticket", "model", "execx", "fsx", "jsonx", "clock", "textx"}},

		{5, "cli", nil}, // computed
		{5, "auth", []string{"paths", "fsx", "jsonx", "clock", "textx"}},
		{5, "guard", []string{"tokens", "jsonx", "clock"}},
		{5, "push", []string{"boardread", "present", "model", "ticket", "paths", "fsx", "jsonx", "clock"}},
		{5, "api", []string{"ticketapi", "guard", "model", "jsonx", "clock"}},
		{5, "mcp", []string{"ticketapi", "guard", "model", "jsonx", "clock", "buildinfo"}},
		{5, "web", []string{"present", "mdhtml", "boardread", "deployrec", "ops", "ticketapi", "auth", "push", "api", "mcp", "guard", "tokens", "config", "model", "ticket", "paths", "fsx", "jsonx", "clock", "logx", "textx", "buildinfo", "sysinfo", "vitals"}},
	}
}

// ForemanForbidden lists the packages that must never be in the dependency
// closure of cmd/foreman, plus the test-only packages.
func ForemanForbidden() []string {
	return []string{
		"github", "gitx", "scion", "sshjobs", "podman", "imagebuild", "memstore", "memory", "gate",
		"security", "stage", "panel", "hostrun", "hosts", "prflow", "doors", "upkeep", "backup", "pipeline",
		"render", "cli", "helper", "fizzy", "onboard", "deploy", "operator", "testkit", "archtest",
	}
}

// HelperAllowed is the whole permitted closure of cmd/factory-remote. It
// holds jsonx because the table's jobproto row imports it (pure layer-0
// encoding), mailbox for factory-remote's mailbox mode (the forced command
// of a pull host's key on the dispatcher), and vitals for the vitals verb.
func HelperAllowed() []string {
	return []string{"helper", "jobproto", "mailbox", "vitals", "imagebuild", "podman", "toml", "fsx", "execx", "logx", "textx", "clock", "buildinfo", "jsonx"}
}

// PullAllowed is the whole permitted closure of cmd/factory-pull, the
// desktop's job proxy (CheckPullClosure), vitals among it for what it
// posts. Its tests also reach mailbox, to play the dispatcher's forced
// command.
func PullAllowed() []string {
	return []string{"pull", "jobproto", "vitals", "toml", "fsx", "execx", "clock", "textx", "jsonx", "buildinfo"}
}

// Module-relative package paths of the special packages.
const (
	rootPkg     = "."
	testkitPkg  = "internal/testkit"
	foremanCmd  = "cmd/foreman"
	helperCmd   = "cmd/factory-remote"
	pullCmd     = "cmd/factory-pull"
	factoryCmd  = "cmd/factory"
	archtestPkg = "internal/archtest"
)

// relOf turns a short table name into a module-relative package path.
func relOf(name string) string {
	switch name {
	case "agentfactory":
		return rootPkg
	}
	return "internal/" + name
}

// Table is the allowed-imports table keyed by module-relative package path
// ("internal/store", "cmd/foreman", "."). Each value is the set of
// module-relative paths that package may import. A package that is not a
// key is unknown and fails the table check. The testkit family
// (internal/testkit and everything under it) is handled by allowedEdge.
func Table() map[string]map[string]bool {
	t := map[string]map[string]bool{}
	layer := map[string]int{}
	for _, r := range rows() {
		layer[r.name] = r.layer
		set := map[string]bool{}
		for _, imp := range r.imports {
			set[relOf(imp)] = true
		}
		t[relOf(r.name)] = set
	}
	// pipeline: every layer 0-3 package except ticketapi, present, mdhtml,
	// tokens; plus prflow, doors, upkeep.
	pipe := t[relOf("pipeline")]
	for name, l := range layer {
		if l <= 3 && !slices.Contains([]string{"ticketapi", "present", "mdhtml", "tokens"}, name) {
			pipe[relOf(name)] = true
		}
	}
	for _, name := range []string{"prflow", "doors", "upkeep"} {
		pipe[relOf(name)] = true
	}
	// cli: every package of layers 0-4 except helper and pull; plus buildinfo.
	cli := t[relOf("cli")]
	for name, l := range layer {
		if l <= 4 && name != "helper" && name != "pull" {
			cli[relOf(name)] = true
		}
	}
	cli[relOf("buildinfo")] = true

	t[rootPkg] = map[string]bool{}

	// The binaries. Their closures are checked separately (rules 3 and 5).
	t[factoryCmd] = set(relOf("cli"), relOf("buildinfo"), relOf("paths"), relOf("clock"), relOf("execx"), relOf("fsx"), relOf("logx"))
	fm := set(relOf("web"), relOf("ops"), relOf("store"), relOf("wake"), relOf("gitread"), relOf("scionread"), relOf("jobtail"), relOf("jobproto"), relOf("execx"), relOf("screens"))
	for imp := range t[relOf("web")] {
		fm[imp] = true
	}
	t[foremanCmd] = fm
	hc := set(relOf("helper"))
	for imp := range t[relOf("helper")] {
		hc[imp] = true
	}
	t[helperCmd] = hc
	t[pullCmd] = set(relOf("pull"), relOf("buildinfo"), relOf("clock"), relOf("execx"))

	// archtest parses source; it imports nothing internal.
	t[archtestPkg] = map[string]bool{}

	// testkit (test-only fakes and fixture builders) may build on layers 0-2.
	tk := map[string]bool{}
	for name, l := range layer {
		if l <= 2 {
			tk[relOf(name)] = true
		}
	}
	t[testkitPkg] = tk
	return t
}

func set(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, it := range items {
		m[it] = true
	}
	return m
}

// inTestkit reports whether rel is internal/testkit or below it.
func inTestkit(rel string) bool {
	return rel == testkitPkg || strings.HasPrefix(rel, testkitPkg+"/")
}

// tableKey maps a package to its table row: everything under
// internal/testkit shares testkit's row.
func tableKey(rel string) string {
	if inTestkit(rel) {
		return testkitPkg
	}
	return rel
}

// allowedEdge reports whether from may import to, for production code
// (test=false) or test code (test=true).
func allowedEdge(t map[string]map[string]bool, from, to string, test bool) bool {
	if from == to {
		return true // an external test package importing the package under test
	}
	if inTestkit(to) {
		// testkit is for tests and its own family.
		return test || inTestkit(from)
	}
	if to == archtestPkg {
		return false
	}
	if strings.HasPrefix(to, "cmd/") {
		return false
	}
	return t[tableKey(from)][to]
}
