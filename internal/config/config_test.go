package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

func mustParse(t *testing.T, text string) *Config {
	t.Helper()
	c, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, text)
	}
	return c
}

// The Python defaults (cfg.get(key, default) at every use) live here once.
func TestDefaults(t *testing.T) {
	c := mustParse(t, "")
	checks := []struct {
		name      string
		got, want any
	}{
		{"max_agents", c.MaxAgents, 2},
		{"agent_timeout_minutes", c.AgentTimeoutMinutes, 45},
		{"template_flavour", c.TemplateFlavour, "ruby-gem"},
		{"image", c.Image, ""},
		{"upkeep.rebuild_at", c.Upkeep.RebuildAt, "Sun *-*-* 02:30:00"},
		{"host.factory_user", c.Host.FactoryUser, "factory"},
		{"host.factory_uid", c.Host.FactoryUID, ""},
		{"host.factory_home", c.Host.FactoryHome, "/home/factory"},
		{"gate.max_changed_lines", c.Gate.MaxChangedLines, 10},
		{"MaxChangedLines()", c.MaxChangedLines(), 10},
		{"gate.memory", c.Gate.Memory, "900m"},
		{"panel.voters", c.Panel.Voters, 3},
		{"memory.enabled", c.Memory.Enabled, true},
		{"memory.max_kb", c.Memory.MaxKB, 512},
		{"memory.roles set", c.Memory.RolesSet, false},
		{"foreman.listen", c.Foreman.Listen, "127.0.0.1:8790"},
		{"remote.image", c.Remote.Image, "localhost/factory-pi:dev"},
		{"remote.memory", c.Remote.Memory, "800m"},
		{"remote.max_jobs", c.Remote.MaxJobs, 4},
		{"github", c.HasGitHub(), false},
		{"people", c.People(), []string{"maintainer"}},
		{"maintainer", c.Maintainer(), "maintainer"},
		{"auto_accept", c.AutoAccept("github"), false},
		{"template", c.Template("work"), "worker"},
		{"template vote", c.Template("vote"), "voter"},
		{"model", c.Model("work"), ""},
		{"projects", len(c.Projects), 0},
		{"recipes", len(c.Recipes()), 0},
		// new in the port
		{"images.auto_update", c.AutoUpdate, []string{"claude", "pi"}},
		{"backup", c.Backup, Backup{Region: "auto", IncludePushKey: true, Daily: 7, Weekly: 4, Monthly: 6}},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %#v, want %#v", ch.name, ch.got, ch.want)
		}
	}
	if len(c.Unknown) != 0 || len(c.Mistyped) != 0 {
		t.Errorf("an empty config has unknown %v / mistyped %v", c.Unknown, c.Mistyped)
	}
}

func TestExampleConfig(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "config", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	c := mustParse(t, string(b))
	if len(c.Unknown) != 0 || len(c.Mistyped) != 0 {
		t.Fatalf("the example config has unknown keys %v or mistyped values %v", c.Unknown, c.Mistyped)
	}
	if c.Image != "localhost/factory-agent:scion" || c.MaxAgents != 2 || c.AgentTimeoutMinutes != 45 {
		t.Errorf("top level: %q %d %d", c.Image, c.MaxAgents, c.AgentTimeoutMinutes)
	}
	if c.Images["claude"] != "2.1.296" || c.Images["pi"] != "1.4.0" {
		t.Errorf("images: %v", c.Images)
	}
	if c.Identity.GitName != "Your Name" || c.Identity.PRFooter == "" {
		t.Errorf("identity: %+v", c.Identity)
	}
	if c.Host.FactoryUID != "1001" {
		t.Errorf("factory_uid as render prints it: %q", c.Host.FactoryUID)
	}
	if c.Template("work") != "gem-worker" || c.Template("manage") != "manager" {
		t.Errorf("templates: %q %q", c.Template("work"), c.Template("manage"))
	}
	if !reflect.DeepEqual(c.Gate.Env, []EnvVar{{"BUNDLE_PATH", "/home/scion/.bundle-cache"}}) {
		t.Errorf("gate env: %#v", c.Gate.Env)
	}
	if len(c.Gate.AllowedPaths) != 7 || c.Gate.Mounts[0] != "{root}/cache/bundle:/home/scion/.bundle-cache:ro" {
		t.Errorf("gate: %+v", c.Gate)
	}
	if !c.AutoAccept("clock") || c.AutoAccept("github") || c.AutoAccept("agent") {
		t.Errorf("auto_accept: clock %v github %v agent %v", c.AutoAccept("clock"), c.AutoAccept("github"), c.AutoAccept("agent"))
	}
	if !reflect.DeepEqual(c.People(), []string{"alice"}) {
		t.Errorf("people: %v", c.People())
	}
	var keys []string
	for _, p := range c.Projects {
		keys = append(keys, p.Key)
	}
	if !reflect.DeepEqual(keys, []string{"my_gem", "my_shard"}) {
		t.Fatalf("projects in declaration order: %v", keys)
	}
	gem, shard := c.Projects[0], c.Projects[1]
	if gem.Mode != ModeFactory || !gem.Enabled || gem.Base != "main" || gem.Test != "bundle exec rake test" ||
		!reflect.DeepEqual(gem.PinnedUntracked, []string{"Gemfile.lock"}) || gem.LanguageLabel() != "Ruby gem" || gem.Language != "" {
		t.Errorf("my_gem: %+v", gem)
	}
	if shard.LanguageLabel() != "Crystal shard" || shard.Notes == "" || c.GateMemory(shard) != "900m" {
		t.Errorf("my_shard: %+v", shard)
	}
	if len(c.Schedules) != 3 || c.Schedules[0].Every != 30*24*time.Hour || c.Schedules[2].Kind != "prospect" || c.Schedules[0].Column != "triage" ||
		!c.Schedules[1].Findings || c.Findings.AutoFile != AutoFileNone {
		t.Errorf("schedules: %+v", c.Schedules)
	}
	if c.Schedules[0].ProjectsSet || c.Schedules[0].Projects != nil {
		t.Errorf("schedule projects: %+v", c.Schedules[0])
	}
	if rs := c.Recipes(); len(rs) != 3 || !rs[0].Scheduled || rs[2].Kind != "prospect" || !rs[1].Findings {
		t.Errorf("recipes: %+v", rs)
	}
	if c.Remote.Image != "localhost/factory-pi:dev" || c.Remote.MaxJobs != 4 || len(c.Remote.Env) != 1 {
		t.Errorf("remote: %+v", c.Remote)
	}
	if c.Raw == nil || c.Raw.Len() == 0 {
		t.Error("Raw is not kept")
	}
}

func TestTypedSections(t *testing.T) {
	c := mustParse(t, `
max_agents = 3
agent_timeout_minutes = 30.0

[github]
app_client_id = "Iv23x"
installation_id = 12345678
key = "/k.pem"
open_prs = "auto"
reviewers = ["Alice", "bob"]
app_slug = "a-bot"

[panel]
voters = 4
models = ["m1", "m2"]

[memory]
enabled = false
roles = ["worker"]
max_kb = 64

[foreman]
hosts = ["box.example"]
trust_tailscale = true
tailscale_users = ["you@example.com"]

[triage.auto_accept]
github = true
clock = 0
agent = "yes"

[models]
work = "claude-x"

[hosts.helper]
ssh = "factory@helper.example"
slots = 2
stages = ["triage"]
prefer = ["task"]
model = "m"
key = "~/.ssh/k"
notes = "n"

[hosts.second]
ssh = "f@second.example"

[gate]
env = { A = 1, B = "x", C = true, D = 1.5 }

[[watches]]
name = "enquiries"
project = "factory"
command = ["/bin/x", "--y"]
title = "t"
body = "{output}"
every_minutes = 0.5
timeout_seconds = 30
kind = "intent"
column = "triage"
file_first = 1
`)
	if len(c.Unknown) != 0 || len(c.Mistyped) != 0 {
		t.Fatalf("unknown %v mistyped %v", c.Unknown, c.Mistyped)
	}
	if c.MaxAgents != 3 || c.AgentTimeoutMinutes != 30 {
		t.Errorf("numbers: %d %d", c.MaxAgents, c.AgentTimeoutMinutes)
	}
	if !c.HasGitHub() || c.GitHub.InstallationID != "12345678" || c.GitHub.OpenPRs != "auto" || c.GitHub.AppSlug != "a-bot" ||
		!reflect.DeepEqual(c.GitHub.Reviewers, []string{"Alice", "bob"}) || c.GitHub.Key != "/k.pem" {
		t.Errorf("github: %+v", c.GitHub)
	}
	if c.Panel.Voters != 4 || len(c.Panel.Models) != 2 {
		t.Errorf("panel: %+v", c.Panel)
	}
	if c.Memory.Enabled || !c.Memory.RolesSet || c.Memory.MaxKB != 64 {
		t.Errorf("memory: %+v", c.Memory)
	}
	if !c.Foreman.TrustTailscale || c.Foreman.Hosts[0] != "box.example" {
		t.Errorf("foreman: %+v", c.Foreman)
	}
	// [triage.auto_accept] is read for its truth, as .get(door, False) is
	if !c.AutoAccept("github") || c.AutoAccept("clock") || !c.AutoAccept("agent") || c.AutoAccept("human") {
		t.Error("auto_accept truthiness")
	}
	if c.Model("work") != "claude-x" || c.Model("review") != "" {
		t.Error("models")
	}
	if len(c.Hosts) != 2 || c.Hosts[0].Name != "helper" || c.Hosts[0].Slots != 2 || c.Hosts[1].Slots != 1 || c.Hosts[1].Key != "" {
		t.Errorf("hosts: %+v", c.Hosts)
	}
	if h, ok := c.HostNamed("second"); !ok || h.SSH != "f@second.example" {
		t.Errorf("HostNamed: %+v %v", h, ok)
	}
	if _, ok := c.HostNamed("local"); ok {
		t.Error("HostNamed(local)")
	}
	// env values as f"{k}={v}" and str(v) print them
	want := []EnvVar{{"A", "1"}, {"B", "x"}, {"C", "True"}, {"D", "1.5"}}
	if !reflect.DeepEqual(c.Gate.Env, want) {
		t.Errorf("gate env: %#v", c.Gate.Env)
	}
	w := c.Watches[0]
	if w.Every != 30*time.Second || w.Timeout != 30*time.Second || w.Kind != "intent" || w.Column != "triage" || !w.FileFirst ||
		!reflect.DeepEqual(w.Command, []string{"/bin/x", "--y"}) {
		t.Errorf("watch: %+v", w)
	}
	d := mustParse(t, "[[watches]]\nname = \"w\"\nproject = \"p\"\ncommand = [\"c\"]\ntitle = \"t\"\nbody = \"b\"\n")
	if w := d.Watches[0]; w.Every != 10*time.Minute || w.Timeout != 120*time.Second || w.Kind != "task" || w.Column != "" || w.FileFirst {
		t.Errorf("watch defaults: %+v", w)
	}
}

// Installation id 0 or "" is no GitHub, as Python's truth test says.
// [images] auto_update is a list beside the pins; [backup] holds
// the R2 target.
func TestImagesAutoUpdateAndBackup(t *testing.T) {
	c := mustParse(t, `[images]
claude = "2.1.296"
auto_update = []
pi = 7

[backup]
endpoint = "https://acct.r2.cloudflarestorage.com"
bucket = "factory-backups"
prefix = "z/"
credentials = "/k/r2"
include_push_key = false
keep_daily = 3
`)
	if !reflect.DeepEqual(c.Images, map[string]string{"claude": "2.1.296"}) {
		t.Errorf("pins %v", c.Images)
	}
	if c.AutoUpdate == nil || len(c.AutoUpdate) != 0 {
		t.Errorf("auto_update = [] is off: %#v", c.AutoUpdate)
	}
	want := Backup{Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "factory-backups", Prefix: "z/",
		Region: "auto", Credentials: "/k/r2", Daily: 3, Weekly: 4, Monthly: 6}
	if c.Backup != want {
		t.Errorf("backup %+v", c.Backup)
	}
	if !reflect.DeepEqual(c.Mistyped, []string{"images.pi: 7 is not a string; it is ignored"}) || len(c.Unknown) != 0 {
		t.Errorf("mistyped %q unknown %q", c.Mistyped, c.Unknown)
	}
	c = mustParse(t, "[images]\nauto_update = \"claude\"\n")
	if !reflect.DeepEqual(c.AutoUpdate, []string{"claude", "pi"}) || len(c.Mistyped) != 1 {
		t.Errorf("a string auto_update keeps the default: %v %q", c.AutoUpdate, c.Mistyped)
	}
}

func TestHasGitHub(t *testing.T) {
	for text, want := range map[string]bool{
		"[github]\ninstallation_id = 0\n":     false,
		"[github]\ninstallation_id = \"\"\n":  false,
		"[github]\ninstallation_id = \"7\"\n": true,
		"[github]\napp_client_id = \"x\"\n":   false,
	} {
		if got := mustParse(t, text).HasGitHub(); got != want {
			t.Errorf("%q: HasGitHub = %v", text, got)
		}
	}
}

func TestProjects(t *testing.T) {
	c := mustParse(t, `
[projects.a]
path = "/a"
base = "main"
enabled = false
sweep = false
clock = false
tier3_paths = []
gate_memory = "1500m"
language = ""

[projects.b]
mode = "board"
enabled = "no"
sweep = 0

[projects.c]
mode = "kanban"
`)
	a, _ := c.Project("a")
	if a.Enabled || a.Sweep || a.Clock || !a.Tier3Set || a.Tier3Paths == nil || len(a.Tier3Paths) != 0 || a.TextSet {
		t.Errorf("a: %+v", a)
	}
	if c.GateMemory(*a) != "1500m" || a.LanguageLabel() != "" || !a.Has("language") || a.Has("lint") {
		t.Errorf("a: gate memory %q, language %q", c.GateMemory(*a), a.LanguageLabel())
	}
	// `enabled = "no"` is not False, so the project runs, as in Python
	if !c.Enabled("b") || c.Enabled("a") || !c.Enabled("nope") {
		t.Error("Enabled follows `is not False`")
	}
	b, _ := c.Project("b")
	if !b.Sweep {
		t.Error("sweep = 0 is not False either")
	}
	if mode, ok := c.ProjectMode("c"); !ok || mode != "kanban" {
		t.Errorf("an unknown mode is kept as written: %q %v", mode, ok)
	}
	if _, ok := c.Project("zz"); ok {
		t.Error("Project(zz)")
	}
}

func TestUnknownAndMistyped(t *testing.T) {
	c := mustParse(t, `
max_agent = 3
max_agents = "two"

[gate]
max_changed_lines = 10.5
allowed_paths = ["*.md", 3]
memmory = "1g"

[projects.gem]
path = "/g"
tset = "x"
pinned_untracked = "Gemfile.lock"

[hosts.h]
ssh = "s"
slot = 1

[[watches]]
name = "w"
project = "p"
command = "true"
title = "t"
body = "b"

[[schedules]]
name = "s"
title = "t"
body = "b"

[[recipes]]
title = "no name"
body = "b"

[nonsense]
x = 1
`)
	wantUnknown := []string{"max_agent", "gate.memmory", "projects.gem.tset", "hosts.h.slot", "nonsense"}
	if !reflect.DeepEqual(c.Unknown, wantUnknown) {
		t.Errorf("Unknown = %q, want %q", c.Unknown, wantUnknown)
	}
	if c.MaxAgents != 2 || c.Gate.MaxChangedLines != 10 || !reflect.DeepEqual(c.Gate.AllowedPaths, []string{"*.md"}) {
		t.Errorf("a mistyped value falls back to the default: %d %d %v", c.MaxAgents, c.Gate.MaxChangedLines, c.Gate.AllowedPaths)
	}
	joined := strings.Join(c.Mistyped, "\n")
	for _, want := range []string{
		"max_agents: 'two' is not a whole number; the default 2 applies",
		"[gate] max_changed_lines: 10.5 is not a whole number; the default 10 applies",
		"[gate] allowed_paths: 3 is not a string; it is left out",
		"[projects.gem] pinned_untracked: 'Gemfile.lock' is not a list; it is ignored",
		"[[watches]] w: command: 'true' is not a list; it is ignored",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Mistyped lacks %q:\n%s", want, joined)
		}
	}
	if len(c.Watches) != 0 {
		t.Errorf("a watch with no command is skipped: %+v", c.Watches)
	}
	if len(c.Schedules) != 0 {
		t.Errorf("a schedule with no every_days is skipped: %+v", c.Schedules)
	}
	if len(c.Recipes()) != 1 || c.Recipes()[0].Name != "s" {
		t.Errorf("a schedule with no every_days is still a recipe; a recipe with no name is not: %+v", c.Recipes())
	}
	_, warnings := c.Problems(nil)
	all := strings.Join(warnings, "\n")
	for _, want := range []string{
		"max_agent: not a key the factory knows",
		"[projects.gem] tset: not a key the factory knows",
		"[nonsense]: not a key the factory knows",
		"[[watches]] w: no command; the watch is skipped",
		"[[schedules]] s: no every_days; the schedule is skipped",
		"[[recipes]] #1: no name; the recipe is skipped",
		"max_agents: 'two' is not a whole number",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings lack %q:\n%s", want, all)
		}
	}
}

func TestParseRefusesBadTOML(t *testing.T) {
	_, err := Parse([]byte("[a]\n[a]\n"))
	if err == nil || !strings.Contains(err.Error(), "(at line 2") {
		t.Fatalf("Parse = %v", err)
	}
	_, err = Parse([]byte("when = 2026-01-01\n"))
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("a date: %v", err)
	}
}

func TestLoad(t *testing.T) {
	env := testkit.Root(t)
	path := env.WriteConfig(t, "max_agents = 5\n")
	c, err := Load(path)
	if err != nil || c.MaxAgents != 5 {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	if _, err := Load(filepath.Join(env.Root, "missing.toml")); err == nil {
		t.Fatal("Load of a missing file worked")
	}
}

// The live config, when a private snapshot is named: it parses, and every
// key and type in it is one the port knows (otherwise a key is silently
// unused).
func TestLiveConfig(t *testing.T) {
	dir := testkit.LiveSnapshot(t)
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Skipf("no config.toml: %v", err)
	}
	c := mustParse(t, string(b))
	if len(c.Unknown) != 0 || len(c.Mistyped) != 0 {
		t.Errorf("live config: unknown %q, mistyped %q", c.Unknown, c.Mistyped)
	}
	problems, warnings := c.Problems(nil)
	if len(problems) != 0 {
		t.Errorf("live config problems: %q", problems)
	}
	t.Logf("live config: %d projects, %d hosts, %d schedules, %d recipes, %d watches; %d warnings",
		len(c.Projects), len(c.Hosts), len(c.Schedules), len(c.Recipes()), len(c.Watches), len(warnings))
}

// A gate whose memory is above [gate] heavy_memory (1200m by default)
// needs this host to itself; a size that does not read is heavy, and
// factory check says so.
func TestHeavyGate(t *testing.T) {
	c := mustParse(t, "[projects.small]\npath = \"/x\"\n\n[projects.bigproj]\npath = \"/y\"\ngate_memory = \"1800m\"\n\n"+
		"[projects.edge]\npath = \"/z\"\ngate_memory = \"1.2G\"\n\n[projects.odd]\npath = \"/w\"\ngate_memory = \"lots\"\n")
	if c.Gate.HeavyMemory != "1200m" {
		t.Fatalf("heavy_memory default %q", c.Gate.HeavyMemory)
	}
	for key, want := range map[string]bool{"small": false, "bigproj": true, "edge": true, "odd": true} {
		p, _ := c.Project(key)
		if got := c.HeavyGate(*p); got != want {
			t.Errorf("%s: heavy %v, want %v", key, got, want)
		}
	}
	c = mustParse(t, "[gate]\nheavy_memory = \"2g\"\n\n[projects.bigproj]\npath = \"/y\"\ngate_memory = \"1800m\"\n")
	if p, _ := c.Project("bigproj"); c.HeavyGate(*p) {
		t.Error("1800m is not above 2g")
	}
	c = mustParse(t, "[gate]\nheavy_memory = \"big\"\n\n[projects.a]\npath = \"/y\"\ninstall = \"i\"\ntest = \"t\"\nlint = \"l\"\n")
	if p, _ := c.Project("a"); !c.HeavyGate(*p) {
		t.Error("an unreadable heavy_memory is not heavy")
	}
	if _, warnings := c.Problems(nil); !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "heavy_memory") }) {
		t.Errorf("warnings %q", warnings)
	}
}

// Only a pull host runs gate jobs: "gate" on an ssh host is a warning.
func TestGateOnAHelperIsAWarning(t *testing.T) {
	c := mustParse(t, "[hosts.pi]\nssh = \"f@pi\"\nstages = [\"gate\"]\n\n[hosts.desk]\ntransport = \"pull\"\nprefer = [\"gate\"]\n")
	_, warnings := c.Problems(nil)
	var gate []string
	for _, w := range warnings {
		if strings.Contains(w, "\"gate\"") {
			gate = append(gate, w)
		}
	}
	if len(gate) != 1 || !strings.HasPrefix(gate[0], "[hosts.pi] lists \"gate\"") {
		t.Errorf("warnings %q", warnings)
	}
}

func TestMemoryBytes(t *testing.T) {
	for in, want := range map[string]int64{"900m": 900 << 20, "1800M": 1800 << 20, "1g": 1 << 30, "1.5GiB": 3 << 29,
		"512k": 512 << 10, "2048": 2048, "2048b": 2048, "1 gb": 1 << 30} {
		if got, ok := MemoryBytes(in); !ok || got != want {
			t.Errorf("%q = %d %v, want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "m", "-1m", "1x", "1mm"} {
		if _, ok := MemoryBytes(in); ok {
			t.Errorf("%q reads as a size", in)
		}
	}
}

// [deploy]'s keys are known: no unknown-key warning.
func TestDeployKeysAreKnown(t *testing.T) {
	c := mustParse(t, "[deploy]\ncore = \"a/b\"\nconfig = \"a/c\"\nconfig_dir = \"config\"\n")
	if len(c.Unknown) > 0 || len(c.Mistyped) > 0 {
		t.Errorf("unknown %q mistyped %q", c.Unknown, c.Mistyped)
	}
}

// [backup] name names the archive line, so it survives a change of
// machine; "" is the machine's host name.
func TestBackupNameDecodes(t *testing.T) {
	if c := mustParse(t, "[backup]\nname = \"master\"\n"); c.Backup.Name != "master" || len(c.Unknown) != 0 {
		t.Errorf("name %q unknown %q", c.Backup.Name, c.Unknown)
	}
	if c := mustParse(t, ""); c.Backup.Name != "" {
		t.Errorf("absent name = %q", c.Backup.Name)
	}
}

// [upkeep] latest_every_days is the latest-toolchain trial's
// cadence, 30 by default and 0 for off; the key is known.
func TestUpkeepLatestEveryDays(t *testing.T) {
	if c := mustParse(t, ""); c.Upkeep.LatestEveryDays != 30 {
		t.Errorf("default %d", c.Upkeep.LatestEveryDays)
	}
	c := mustParse(t, "[upkeep]\nlatest_every_days = 0\n")
	if c.Upkeep.LatestEveryDays != 0 || len(c.Unknown) > 0 || len(c.Mistyped) > 0 {
		t.Errorf("days %d unknown %q mistyped %q", c.Upkeep.LatestEveryDays, c.Unknown, c.Mistyped)
	}
}
