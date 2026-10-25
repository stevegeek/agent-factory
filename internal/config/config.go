// Package config is the typed config.toml.
//
// Parse decodes a document read by the strict toml parser into Config by
// hand-written field mapping, applies the Python defaults (each default
// lives here once), keeps declaration order for projects, hosts and the
// arrays of tables, and records keys it does not know and values of the
// wrong type, which Problems reports as warnings. Source re-reads the file
// when it changes, for the resident Foreman.
//
// A *Config is shared between goroutines once parsed: treat it and its
// slices and maps as read-only.
package config

import (
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/toml"
)

// Project modes and the factory's own project, as plain strings: they are
// model.Mode and model.NoticeProject, which hold the contract.
const (
	ModeFactory   = string(model.ModeFactory)
	ModeBoard     = string(model.ModeBoard)
	NoticeProject = model.NoticeProject
	// DefaultPerson is DEFAULT_PEOPLE[0]: the maintainer when [board]
	// people names no one.
	DefaultPerson = "maintainer"
)

// maxConfigBytes caps what Load and Source read (the live config is ~20 KB).
const maxConfigBytes = 4 << 20

// Config is config.toml, typed. Fields hold the value in the file or the
// Python default when the key is absent.
type Config struct {
	Image               string // gate and agent image tag; "" when unset
	MaxAgents           int    // 2
	AgentTimeoutMinutes int    // 45
	TemplateFlavour     string // "ruby-gem"

	Images map[string]string // [images]: pins by name (claude, pi, crystal, ameba); a floor for AutoUpdate's
	// AutoUpdate is [images] auto_update: the tools the weekly rebuild
	// updates by itself; ["claude", "pi"] when absent, [] is off.
	AutoUpdate []string
	Backup     Backup
	Build      Build
	Deploy     Deploy
	Upkeep     Upkeep
	GitHub     GitHub
	Identity   Identity
	Host       HostUser
	Gate       Gate
	Panel      Panel
	Memory     Memory
	Foreman    Foreman
	Remote     Remote
	Findings   Findings
	Hygiene    HygieneCfg

	Hosts     []HostCfg  // [hosts.*] in declaration order
	Projects  []Project  // [projects.*] in declaration order: clock and sweep file in this order
	Schedules []Schedule // [[schedules]] that can run (see Problems for the ones skipped)
	Watches   []Watch    // [[watches]] that can run

	// Unknown lists the dotted keys the factory does not know.
	Unknown []string
	// Mistyped describes values of the wrong type; each was replaced by its
	// default or left out.
	Mistyped []string
	// Raw is the whole document, for the few readers that need Python's
	// exact view of a value (render's str() of factory_uid, for example).
	Raw *toml.Table

	templates    map[string]string
	models       map[string]string
	autoAccept   map[string]toml.Value
	board        *toml.Table // [board], for people and its check
	recipes      []Recipe
	kinds        []ProjectKind // [kinds.*] in file order, each over its built-in
	skipped      []string      // array-of-tables entries left out, and why
	unknownNotes []string      // the Unknown keys as warnings
	unknown      []unknown
}

// Build is [build]: the helper `factory build` builds core commits on. It
// must be an ssh helper whose own [remote.<hostname>] says build = true,
// or BuildSelf: this host builds through its own factory-remote, by its
// own [remote.<hostname>].
type Build struct {
	Host string // a [hosts] name, or BuildSelf; "" for none
}

// BuildSelf is [build] host's name for this host: the master runs
// `factory-remote build` itself instead of over ssh.
const BuildSelf = "self"

// ThisHost is this machine's name as factory-remote reads its
// [remote.<name>] (helper.HostName): the short host name, lowercased.
func ThisHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

// Deploy is [deploy]: the repos a self-deploy takes core main and config
// main from (docs/plans/2026-10-24-p2-self-deploy.md), each owner/repo,
// and the config repo's directory that becomes ~/factory's config
// (ConfigDir, "config"). Core is fetched with no credential (it is public),
// config with an App token for that one repo.
type Deploy struct {
	Core, Config string // owner/repo; "" for none
	ConfigDir    string // "config"
}

// On reports whether the master deploys itself.
func (d Deploy) On() bool { return d.Core != "" }

// Backup is [backup]: the R2 bucket the daily backup goes to.
// Credentials and KeyFile are paths; "" means the defaults under
// ~/.config/factory (r2-credentials, backup-key). Name is the archive
// line's name (factory-<name>-<stamp>); "" is the machine's host name, so a
// line named in config survives a move of the master to another machine.
type Backup struct {
	Name           string // a slug; "" for the host name
	Endpoint       string // https://<account>.r2.cloudflarestorage.com
	Bucket         string
	Prefix         string // "" or ending in "/"
	Region         string // "auto"
	Credentials    string
	KeyFile        string
	IncludePushKey bool // true: Foreman's push key goes in (every device is bound to it)
	Daily          int  // 7
	Weekly         int  // 4
	Monthly        int  // 6
}

// DefaultAutoUpdate is [images] auto_update when absent.
func DefaultAutoUpdate() []string { return []string{"claude", "pi"} }

// Upkeep is [upkeep].
type Upkeep struct {
	RebuildAt string // "Sun *-*-* 02:30:00"
	// LatestEveryDays is how often the weekly rebuild also runs the
	// latest-toolchain trial (default 30); 0 or less is off.
	LatestEveryDays int
}

// GitHub is [github]. InstallationID is the id as Python's f-string prints
// it, or "" when the value is false (absent, 0 or ""): GitHub out is off.
type GitHub struct {
	AppClientID    string
	InstallationID string
	Key            string // the App key's path; "" means the default under ~/.config/factory
	OpenPRs        string // "manual" or "auto"; "" when unset
	Reviewers      []string
	AppSlug        string // required by gate-workflow; no default
}

// Identity is [identity].
type Identity struct {
	GitName, GitEmail, PRFooter string
}

// HostUser is [host] (render only). FactoryUID is str() of the value, ""
// when unset.
type HostUser struct {
	FactoryUser string // "factory"
	FactoryUID  string
	FactoryHome string // "/home/factory"
}

// Gate is [gate].
type Gate struct {
	MaxChangedLines int // 10
	AllowedPaths    []string
	Memory          string // "900m"
	// HeavyMemory is the gate memory above which a gate needs this host
	// to itself, with no agent beside it ("1200m").
	HeavyMemory string
	Mounts      []string
	Env         []EnvVar
}

// EnvVar is one entry of an env table, the value as str() prints it.
type EnvVar struct {
	Name, Value string
}

// Panel is [panel].
type Panel struct {
	Voters int // 3
	Models []string
}

// Memory is [memory]. Roles is used only when RolesSet; the default role
// list belongs to the memory package.
type Memory struct {
	Enabled  bool // false only when `enabled = false`
	Roles    []string
	RolesSet bool
	MaxKB    int // 512
}

// Findings is [findings]: what becomes of the findings a verifier
// confirms (AutoFile: AutoFileNone or AutoFileConfirmedSafe).
type Findings struct {
	AutoFile string // AutoFileNone
}

// The [findings] auto_file modes: every finding waits for the maintainer,
// or the confirmed ones that change no behaviour are filed as changes at
// once (the rest still wait on the desk).
const (
	AutoFileNone          = "none"
	AutoFileConfirmedSafe = "confirmed-safe"
)

// HygieneCfg is [hygiene]: the terms a public repo's changes must never
// carry (host names, addresses, account ids), checked on every project
// with `hygiene = true`. The terms are private: only config holds them.
// Allow are names public review may see published: a project, host or
// kind name that is a plain word, or a term a project may name.
type HygieneCfg struct {
	Deny  []string
	Allow []string

	denyBad bool // deny is there but not a list of strings
}

// The values of a project's self_deploy: which half of the factory a merge
// on that project deploys.
const (
	SelfDeployCore   = "core"
	SelfDeployConfig = "config"
)

// Foreman is [foreman] (render only).
type Foreman struct {
	Listen         string // "127.0.0.1:8790"
	Hosts          []string
	TrustTailscale bool
	TailscaleUsers []string
}

// Remote is [remote]: the helper's own settings, and on the dispatcher the
// pi image tag.
type Remote struct {
	Image   string // "localhost/factory-pi:dev"
	Memory  string // "800m"
	Mounts  []string
	Env     []EnvVar
	MaxJobs int // 4
	// Agent and Auth are what [remote] sets ("" for the helper's
	// defaults: pi, and the API key; claude defaults to the subscription).
	Agent, Auth string
	// Watchdog and WatchdogIP are what [remote] sets of a helper's outside
	// check of the master: its /healthz URL, and the address dialled for
	// it ("" for none, and for the URL's host).
	Watchdog, WatchdogIP string
	// Hosts are the [remote.<name>] tables, each read only on the helper
	// whose short hostname is name, over [remote].
	Hosts []RemoteHost
}

// RemoteHost is what one [remote.<name>] sets of the agent, its
// credential, its image and its watchdog ("" when it leaves them to
// [remote]).
type RemoteHost struct {
	Name, Agent, Auth, Image string
	Watchdog, WatchdogIP     string
	Mounts                   []string
}

// RemoteKeys are the keys of a [remote] table; no helper is named after
// one.
func RemoteKeys() []string {
	return []string{"image", "memory", "mounts", "env", "max_jobs", "cpus", "agent", "auth", "deps", "build", "build_memory", "watchdog", "watchdog_ip"}
}

// HostJobs is the host kind of a [hosts.<name>] that names none: a helper
// reached over ssh with the factory-remote job protocol.
const HostJobs = "jobs"

// HostCfg is one [hosts.<name>].
type HostCfg struct {
	Name string
	// Kind is the host type: "jobs", the six-verb factory-remote
	// protocol, when absent. It is the one type there is; Problems refuses
	// any other until a second host type exists.
	Kind   string
	SSH    string
	Slots  int // 1
	Stages []string
	Prefer []string
	Model  string
	Key    string // "" means the default ~/.ssh/factory-remote
	Notes  string
	// Transport is how jobs reach the host: TransportSSH (factory-remote
	// over ssh, the default) or TransportPull (a mailbox here that the host
	// claims its jobs from, for a host the dispatcher cannot reach).
	Transport string
	// ClaimMinutes is how long a pull host's queued job waits to be
	// claimed before it counts as lost (30).
	ClaimMinutes int
	// BeatMinutes is how long a pull host may go without calling in: past
	// it the host is away (no new jobs go there) and a claimed job with no
	// beat is lost (10).
	BeatMinutes int
}

// The transports of a [hosts.<name>].
const (
	TransportSSH  = "ssh"
	TransportPull = "pull"
)

// Pull reports a host that claims its jobs from a mailbox here.
func (h HostCfg) Pull() bool { return h.Transport == TransportPull }

// Project is one [projects.<key>].
type Project struct {
	Key string
	// Mode is the mode as written ("factory" when absent; str() of a
	// non-string value). Only "factory" and "board" mean anything.
	Mode string
	// Enabled is whether the factory works on the project: config.toml's
	// `enabled` (false only when `enabled = false`), unless the maintainer
	// switched it from Foreman (Switch; see WithSwitches).
	Enabled bool
	// ConfigEnabled is config.toml's own `enabled`, whatever a switch says.
	ConfigEnabled bool
	// Switch is the maintainer's switch that overrides `enabled`; nil when
	// the project follows config.toml.
	Switch *Switch
	// Kind is the project's `kind` as written; "" when absent (KindOf
	// then infers one from the language, which fills no keys).
	Kind string

	Path, GitHub, Base          string
	Install, Test, Lint         string
	Language, Notes, GateMemory string
	PinnedUntracked             []string
	Tier3Paths, TextPaths       []string
	Tier3Set, TextSet           bool // the project overrides the tier patterns
	Sweep                       bool // false only when `sweep = false`
	Clock                       bool // false only when `clock = false`
	// Walls are fnmatch patterns of the files that hold the factory's own
	// safety (the gate, the rules, the deploy): a change to one never
	// merges without the maintainer.
	Walls []string
	// Hygiene: the project's repo is public, so what a PR publishes is
	// scanned against [hygiene] deny.
	Hygiene bool
	// SelfDeploy is SelfDeployCore or SelfDeployConfig when a merge here
	// deploys the factory; "" for an ordinary project.
	SelfDeploy string

	wallsBad, selfDeployBad bool // walls / self_deploy there but mistyped

	raw      *toml.Table
	fromKind []string // the keys the project's kind filled (resolveKinds)
}

// Has reports whether the project's table sets key, or its kind filled
// it.
func (p Project) Has(key string) bool {
	_, ok := p.raw.Get(key)
	return ok || slices.Contains(p.fromKind, key)
}

// LanguageLabel is what agents are told the project is:
// project.get("language", "Ruby gem").
func (p Project) LanguageLabel() string {
	if !p.Has("language") {
		return "Ruby gem"
	}
	return p.Language
}

// Schedule is one [[schedules]] entry: the clock door.
type Schedule struct {
	Name        string
	Every       time.Duration // every_days
	Title, Body string
	Kind        string // as written; "" when unset
	Intent      bool
	Projects    []string
	ProjectsSet bool   // absent means every factory project
	Column      string // "triage"
	About       string
	// Findings: a task whose agent reports structured findings, which a
	// verifier checks before they come to the desk (findings = true; only
	// for kind = "task").
	Findings bool
}

// Watch is one [[watches]] entry.
type Watch struct {
	Name, Project string
	Every         time.Duration // every_minutes, 10
	Command       []string
	Title, Body   string
	Kind          string        // "task"
	Column        string        // "" when unset: ready, or triage for a change
	Timeout       time.Duration // timeout_seconds, 120
	FileFirst     bool
}

// Recipe is one entry of recipes(cfg): every schedule then every recipe,
// keyed by name.
type Recipe struct {
	Name, Title, Body string
	Kind              string // change, task, intent or prospect
	Projects          []string
	About             string
	Scheduled         bool
	Findings          bool // findings = true on a task: see Schedule.Findings
}

// Parse decodes a config document. It fails only when the TOML does not
// parse; everything else becomes a default and a warning.
func Parse(data []byte) (*Config, error) {
	tab, err := toml.Parse(data)
	if err != nil {
		return nil, err
	}
	return decode(tab), nil
}

// Load reads and parses the config file at path, as load_config does.
func Load(path string) (*Config, error) {
	b, err := readCapped(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	return readAll(f, path)
}

func readAll(f *os.File, path string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if len(b) > maxConfigBytes {
		return nil, fmt.Errorf("config %s: larger than %d bytes", path, maxConfigBytes)
	}
	return b, nil
}

// Project returns the project with key.
func (c *Config) Project(key string) (*Project, bool) {
	for i := range c.Projects {
		if c.Projects[i].Key == key {
			return &c.Projects[i], true
		}
	}
	return nil, false
}

// SelfDeployProject is the project whose merges deploy the factory's
// which (SelfDeployCore or SelfDeployConfig): a factory project on GitHub
// with that self_deploy. False when there is none, or when two claim it
// (Problems refuses that; no caller guesses between them).
func (c *Config) SelfDeployProject(which string) (*Project, bool) {
	var found *Project
	for i := range c.Projects {
		p := &c.Projects[i]
		if which == "" || p.SelfDeploy != which || p.Mode != ModeFactory || p.GitHub == "" {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = p
	}
	return found, found != nil
}

// ProjectMode is project_mode(cfg, key): the project's mode, "factory" for
// the notice project when config does not name it, and false for a project
// config does not name.
func (c *Config) ProjectMode(key string) (string, bool) {
	if p, ok := c.Project(key); ok {
		return p.Mode, true
	}
	if key == NoticeProject {
		return ModeFactory, true
	}
	return "", false
}

// Enabled is project_enabled: false only for a configured project with
// `enabled = false`, or switched off (WithSwitches).
func (c *Config) Enabled(key string) bool {
	if p, ok := c.Project(key); ok {
		return p.Enabled
	}
	return true
}

// FactoryProjects are the projects the pipeline works on, in declaration
// order. The slice is new; its elements are the config's.
func (c *Config) FactoryProjects() []Project { return c.projectsIn(ModeFactory) }

// BoardProjects are the board-mode projects, in declaration order.
func (c *Config) BoardProjects() []Project { return c.projectsIn(ModeBoard) }

func (c *Config) projectsIn(mode string) []Project {
	var out []Project
	for _, p := range c.Projects {
		if p.Mode == mode {
			out = append(out, p)
		}
	}
	return out
}

// HostNamed returns the [hosts.<name>] entry.
func (c *Config) HostNamed(name string) (*HostCfg, bool) {
	for i := range c.Hosts {
		if c.Hosts[i].Name == name {
			return &c.Hosts[i], true
		}
	}
	return nil, false
}

// People is people(cfg): list([board] people or DEFAULT_PEOPLE), each name
// as str() prints it. The first is the maintainer.
func (c *Config) People() []string {
	v, _ := c.board.Get("people")
	if !toml.Truthy(v) {
		return []string{DefaultPerson}
	}
	return pyList(v)
}

// Maintainer is the first person: whom the CLI acts as.
func (c *Config) Maintainer() string { return c.People()[0] }

// pyList is list(v) as str()s: an array's elements, a string's characters,
// a table's keys.
func pyList(v toml.Value) []string {
	var out []string
	switch x := v.(type) {
	case []toml.Value:
		for _, e := range x {
			out = append(out, toml.PyStr(e))
		}
	case string:
		for _, r := range x {
			out = append(out, string(r))
		}
	case *toml.Table:
		out = x.Keys()
	}
	return out
}

// Template is the Scion template for a stage: [templates] overrides the
// built-in name.
func (c *Config) Template(stage string) string {
	if t, ok := c.templates[stage]; ok {
		return t
	}
	return defaultTemplates()[stage]
}

// defaultTemplates is TEMPLATES (bin/factory): stage to template name,
// from model.Stages()' Template column.
func defaultTemplates() map[string]string {
	stages := model.Stages()
	out := make(map[string]string, len(stages))
	for _, s := range stages {
		out[string(s.Name)] = s.Template
	}
	return out
}

// Templates returns the [templates] overrides (a new map).
func (c *Config) Templates() map[string]string {
	out := make(map[string]string, len(c.templates))
	for k, v := range c.templates {
		out[k] = v
	}
	return out
}

// Model is [models] for a stage, "" when unset (the harness default, or
// the helper's own model).
func (c *Config) Model(stage string) string { return c.models[stage] }

// AutoAccept is [triage.auto_accept].get(door, False), read for its truth.
func (c *Config) AutoAccept(door string) bool {
	return toml.Truthy(c.autoAccept[door])
}

// HasGitHub reports whether GitHub out is on: [github] installation_id is
// true.
func (c *Config) HasGitHub() bool { return c.GitHub.InstallationID != "" }

// MaxChangedLines is the gate's size limit.
func (c *Config) MaxChangedLines() int { return c.Gate.MaxChangedLines }

// GateMemory is the gate container's memory for a project:
// project.get("gate_memory", [gate] memory).
func (c *Config) GateMemory(p Project) string {
	if p.Has("gate_memory") {
		return p.GateMemory
	}
	return c.Gate.Memory
}

// HeavyGate reports whether a project's gate needs this host to itself:
// its memory (GateMemory) is above [gate] heavy_memory. A size that does
// not read as one is taken as heavy, which only makes the gate wait for
// an idle host; factory check warns about an unreadable heavy_memory.
func (c *Config) HeavyGate(p Project) bool {
	limit, ok1 := MemoryBytes(c.Gate.HeavyMemory)
	mem, ok2 := MemoryBytes(c.GateMemory(p))
	return !ok1 || !ok2 || mem > limit
}

// memorySize is a container memory size as podman reads one: a number,
// then an optional binary unit k, m, g, t or p in any case, with "b" or
// "ib" after it allowed.
var memorySize = regexp.MustCompile(`^(\d+(?:\.\d+)?) ?([kKmMgGtTpP])?[iI]?[bB]?$`) // re: go

// MemoryBytes is a memory size ("900m", "1.5g", "2048") in bytes.
func MemoryBytes(s string) (int64, bool) {
	m := memorySize.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	if m[2] != "" {
		n *= math.Pow(1024, float64(strings.Index("kmgtp", strings.ToLower(m[2]))+1))
	}
	return int64(n), true
}

// Recipes is recipes(cfg), as a new slice.
func (c *Config) Recipes() []Recipe {
	out := make([]Recipe, len(c.recipes))
	for i, r := range c.recipes {
		r.Projects = append([]string{}, r.Projects...)
		out[i] = r
	}
	return out
}

// Recipe returns the recipe named name.
func (c *Config) Recipe(name string) (Recipe, bool) {
	for _, r := range c.Recipes() {
		if r.Name == name {
			return r, true
		}
	}
	return Recipe{}, false
}

// PrivateNames are the names public review tells a project's agent never
// to publish: the [hygiene] deny terms; the names of the other
// [projects.*] and their GitHub repositories (owner/repo, and each half);
// the [hosts.*] and the hosts their ssh reaches; the [remote.<name>]
// tables; the kinds only config has; and the people config names
// ([board] people when set, [github] reviewers, [foreman] tailscale_users).
// Each is listed once (without regard to case), less [hygiene] allow, the
// project's own name and its own repository's owner and name. The
// pipeline drops the ones the project already names at its base.
func (c *Config) PrivateNames(project string) []string {
	skip := map[string]bool{textx.Lower(project): true}
	if own, ok := c.Project(project); ok && own.GitHub != "" {
		owner, repo, _ := strings.Cut(own.GitHub, "/")
		for _, n := range []string{own.GitHub, owner, repo} {
			skip[textx.Lower(n)] = true
		}
	}
	for _, a := range c.Hygiene.Allow {
		skip[textx.Lower(strings.TrimSpace(a))] = true
	}
	var out []string
	add := func(names ...string) {
		for _, name := range names {
			name = strings.TrimSpace(name)
			if k := textx.Lower(name); name != "" && !skip[k] {
				skip[k] = true
				out = append(out, name)
			}
		}
	}
	add(c.Hygiene.Deny...)
	for _, p := range c.Projects {
		add(p.Key)
		if p.GitHub != "" {
			owner, repo, _ := strings.Cut(p.GitHub, "/")
			add(p.GitHub, owner, repo)
		}
	}
	for _, h := range c.Hosts {
		add(h.Name, sshHost(h.SSH))
	}
	for _, r := range c.Remote.Hosts {
		add(r.Name)
	}
	for _, k := range c.Kinds() {
		if _, builtin := builtinKind(k.Name); !builtin {
			add(k.Name)
		}
	}
	if v, _ := c.board.Get("people"); toml.Truthy(v) {
		add(c.People()...)
	}
	add(c.GitHub.Reviewers...)
	add(c.Foreman.TailscaleUsers...)
	return out
}

// sshHost is the host an ssh destination reaches: "user@host:port" and
// "ssh://user@host:port" give host.
func sshHost(dest string) string {
	dest = strings.TrimPrefix(dest, "ssh://")
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		dest = dest[i+1:]
	}
	host, _, _ := strings.Cut(dest, ":")
	host, _, _ = strings.Cut(host, "/")
	return host
}
