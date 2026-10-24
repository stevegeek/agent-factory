package config

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/toml"
)

// decode maps the document onto Config. Each getter marks its key as known;
// a section's done() records the keys nobody asked for.
func decode(raw *toml.Table) *Config {
	c := &Config{Raw: raw}
	top := &section{c: c, t: raw, used: map[string]bool{}}

	c.Image = top.str("image", "")
	c.MaxAgents = top.integer("max_agents", 2)
	c.AgentTimeoutMinutes = top.integer("agent_timeout_minutes", 45)
	c.TemplateFlavour = top.str("template_flavour", "ruby-gem")
	c.Images, c.AutoUpdate = top.images()
	c.templates = top.strMap("templates")
	c.models = top.strMap("models")

	s := top.sub("backup")
	c.Backup = Backup{
		Name:           s.str("name", ""),
		Endpoint:       s.str("endpoint", ""),
		Bucket:         s.str("bucket", ""),
		Prefix:         s.str("prefix", ""),
		Region:         s.str("region", "auto"),
		Credentials:    s.str("credentials", ""),
		KeyFile:        s.str("key_file", ""),
		IncludePushKey: s.notFalse("include_push_key"),
		Daily:          s.integer("keep_daily", 7),
		Weekly:         s.integer("keep_weekly", 4),
		Monthly:        s.integer("keep_monthly", 6),
	}
	s.done()

	s = top.sub("build")
	c.Build.Host = s.str("host", "")
	s.done()

	s = top.sub("deploy")
	c.Deploy = Deploy{Core: s.str("core", ""), Config: s.str("config", ""), ConfigDir: s.str("config_dir", "config")}
	s.done()

	s = top.sub("upkeep")
	c.Upkeep.RebuildAt = s.str("rebuild_at", "Sun *-*-* 02:30:00")
	c.Upkeep.LatestEveryDays = s.integer("latest_every_days", 30)
	s.done()

	s = top.sub("github")
	c.GitHub = GitHub{
		AppClientID: s.str("app_client_id", ""),
		Key:         s.str("key", ""),
		OpenPRs:     s.str("open_prs", ""),
		Reviewers:   s.strList("reviewers"),
		AppSlug:     s.str("app_slug", ""),
	}
	if v, ok := s.raw("installation_id"); ok && toml.Truthy(v) {
		c.GitHub.InstallationID = toml.PyStr(v)
	}
	s.done()

	s = top.sub("identity")
	c.Identity = Identity{GitName: s.str("git_name", ""), GitEmail: s.str("git_email", ""), PRFooter: s.str("pr_footer", "")}
	s.done()

	s = top.sub("host")
	c.Host = HostUser{FactoryUser: s.str("factory_user", "factory"), FactoryHome: s.str("factory_home", "/home/factory")}
	if v, ok := s.raw("factory_uid"); ok {
		c.Host.FactoryUID = toml.PyStr(v)
	}
	s.done()

	s = top.sub("gate")
	c.Gate = Gate{
		MaxChangedLines: s.integer("max_changed_lines", 10),
		AllowedPaths:    s.strList("allowed_paths"),
		Memory:          s.str("memory", "900m"),
		HeavyMemory:     s.str("heavy_memory", "1200m"),
		Mounts:          s.strList("mounts"),
		Env:             s.env("env"),
	}
	s.done()

	s = top.sub("panel")
	c.Panel = Panel{Voters: s.integer("voters", 3), Models: s.strList("models")}
	s.done()

	s = top.sub("memory")
	c.Memory = Memory{Enabled: s.notFalse("enabled"), MaxKB: s.integer("max_kb", 512)}
	c.Memory.Roles, c.Memory.RolesSet = s.strListSet("roles")
	s.done()

	s = top.sub("findings")
	c.Findings = Findings{AutoFile: s.str("auto_file", AutoFileNone)}
	s.done()

	s = top.sub("hygiene")
	c.Hygiene.Deny, c.Hygiene.denyBad = s.strictStrList("deny")
	c.Hygiene.Allow = s.strList("allow")
	s.done()

	s = top.sub("foreman")
	c.Foreman = Foreman{
		Listen:         s.str("listen", "127.0.0.1:8790"),
		Hosts:          s.strList("hosts"),
		TrustTailscale: s.truthy("trust_tailscale"),
		TailscaleUsers: s.strList("tailscale_users"),
	}
	s.done()

	s = top.sub("triage")
	if aa := s.table("auto_accept"); aa != nil {
		c.autoAccept = map[string]toml.Value{}
		for _, k := range aa.Keys() {
			c.autoAccept[k], _ = aa.Get(k)
		}
	}
	s.done()

	s = top.sub("board")
	c.board = s.t
	s.raw("people") // its shape is the check's business
	s.done()

	s = top.sub("remote")
	c.Remote = Remote{
		Image:   s.str("image", "localhost/factory-pi:dev"),
		Memory:  s.str("memory", "800m"),
		Mounts:  s.strList("mounts"),
		Env:     s.env("env"),
		MaxJobs: s.integer("max_jobs", 4),
		Agent:   s.str("agent", ""),
		Auth:    s.str("auth", ""),
	}
	s.number("cpus", 2)
	s.truthy("deps")
	s.truthy("build")
	s.str("build_memory", "")
	// [remote.<name>]: a helper's own settings, read on that helper
	for _, name := range s.t.Keys() {
		v, _ := s.t.Get(name)
		t, ok := v.(*toml.Table)
		if !ok || name == "env" {
			continue
		}
		s.used[name] = true
		h := s.child(name, -1, "[remote."+name+"]", "remote."+name+".", t)
		h.str("memory", "")
		h.strList("mounts")
		h.env("env")
		h.integer("max_jobs", 4)
		h.number("cpus", 2)
		h.truthy("deps")
		h.truthy("build")
		h.str("build_memory", "")
		c.Remote.Hosts = append(c.Remote.Hosts, RemoteHost{Name: name, Agent: h.str("agent", ""), Auth: h.str("auth", ""), Image: h.str("image", "")})
		h.done()
	}
	s.done()

	top.subTables("hosts", func(name string, s *section) {
		c.Hosts = append(c.Hosts, HostCfg{
			Name: name, Kind: s.str("kind", HostJobs), SSH: s.str("ssh", ""), Slots: s.integer("slots", 1),
			Stages: s.strList("stages"), Prefer: s.strList("prefer"),
			Model: s.str("model", ""), Key: s.str("key", ""), Notes: s.str("notes", ""),
			Transport: s.str("transport", TransportSSH), ClaimMinutes: s.integer("claim_minutes", 30),
			BeatMinutes: s.integer("beat_minutes", 10),
		})
	})
	top.subTables("kinds", func(name string, s *section) {
		c.kinds = append(c.kinds, decodeKind(name, s))
	})
	top.subTables("projects", func(key string, s *section) {
		c.Projects = append(c.Projects, decodeProject(key, s))
	})
	c.resolveKinds()

	schedules := top.entries("schedules")
	for _, e := range schedules {
		if sc, ok := decodeSchedule(e); ok {
			c.Schedules = append(c.Schedules, sc)
		}
	}
	recipes := top.entries("recipes")
	for _, e := range recipes {
		decodeRecipeDef(e)
	}
	c.recipes = buildRecipes(c, schedules, recipes)
	for _, e := range top.entries("watches") {
		if w, ok := decodeWatch(e); ok {
			c.Watches = append(c.Watches, w)
		}
	}
	top.done()
	slices.SortStableFunc(c.unknown, func(a, b unknown) int { return slices.Compare(a.rank, b.rank) })
	for _, u := range c.unknown {
		c.Unknown = append(c.Unknown, u.dotted)
		c.unknownNotes = append(c.unknownNotes, u.note)
	}
	c.unknown = nil
	return c
}

// unknown is one key nobody read, with its place in the document.
type unknown struct {
	rank         []int
	dotted, note string
}

func decodeProject(key string, s *section) Project {
	p := Project{Key: key, raw: s.t, Mode: ModeFactory}
	if v, ok := s.raw("mode"); ok {
		p.Mode = toml.PyStr(v)
	}
	p.Enabled = s.notFalse("enabled")
	p.ConfigEnabled = p.Enabled
	p.Kind = s.str("kind", "")
	p.Path, p.GitHub, p.Base = s.str("path", ""), s.str("github", ""), s.str("base", "")
	p.Install, p.Test, p.Lint = s.str("install", ""), s.str("test", ""), s.str("lint", "")
	p.Language, p.Notes, p.GateMemory = s.str("language", ""), s.str("notes", ""), s.str("gate_memory", "")
	p.PinnedUntracked = s.strList("pinned_untracked")
	p.Tier3Paths, p.Tier3Set = s.strListSet("tier3_paths")
	p.TextPaths, p.TextSet = s.strListSet("text_paths")
	p.Sweep, p.Clock = s.notFalse("sweep"), s.notFalse("clock")
	p.Walls, p.wallsBad = s.strictStrList("walls")
	p.Hygiene = s.truthy("hygiene")
	if v, ok := s.raw("self_deploy"); ok {
		p.SelfDeploy, ok = v.(string)
		p.selfDeployBad = !ok
	}
	return p
}

func decodeSchedule(s *section) (Schedule, bool) {
	sc := Schedule{Column: s.str("column", "triage"), Kind: s.str("kind", ""), About: s.str("about", "")}
	sc.Intent = s.truthy("intent")
	sc.Findings = s.truthy("findings") && sc.Kind == "task"
	sc.Projects, sc.ProjectsSet = s.strListSet("projects")
	name, ok1 := s.need("name")
	title, ok2 := s.need("title")
	body, ok3 := s.need("body")
	days, ok4 := s.needNumber("every_days")
	defer s.done()
	for _, m := range []struct {
		ok  bool
		key string
	}{{ok1, "name"}, {ok4, "every_days"}, {ok2, "title"}, {ok3, "body"}} {
		if !m.ok {
			s.c.skipped = append(s.c.skipped, fmt.Sprintf("%s: no %s; the schedule is skipped", s.label, m.key))
			return Schedule{}, false
		}
	}
	sc.Name, sc.Title, sc.Body, sc.Every = name, title, body, timedelta(days, 24*time.Hour)
	return sc, true
}

// decodeRecipeDef checks a [[recipes]] entry's keys; buildRecipes reads it.
func decodeRecipeDef(s *section) {
	for _, k := range []string{"name", "title", "body", "kind", "about"} {
		s.str(k, "")
	}
	s.truthy("intent")
	s.truthy("findings")
	s.strList("projects")
	s.done()
}

func decodeWatch(s *section) (Watch, bool) {
	w := Watch{
		Kind: s.str("kind", "task"), Column: s.str("column", ""),
		Every:     timedelta(s.number("every_minutes", 10), time.Minute),
		Timeout:   timedelta(s.number("timeout_seconds", 120), time.Second),
		FileFirst: s.truthy("file_first"),
	}
	name, ok1 := s.need("name")
	project, ok2 := s.need("project")
	cmd, ok3 := s.strListSet("command")
	title, ok4 := s.need("title")
	body, ok5 := s.need("body")
	defer s.done()
	for _, m := range []struct {
		ok  bool
		key string
	}{{ok1, "name"}, {ok2, "project"}, {ok3, "command"}, {ok4, "title"}, {ok5, "body"}} {
		if !m.ok {
			s.c.skipped = append(s.c.skipped, fmt.Sprintf("%s: no %s; the watch is skipped", s.label, m.key))
			return Watch{}, false
		}
	}
	w.Name, w.Project, w.Command, w.Title, w.Body = name, project, cmd, title, body
	return w, true
}

// timedelta is dt.timedelta(<unit>=x): rounded to a microsecond.
func timedelta(x float64, unit time.Duration) time.Duration {
	return time.Duration(math.RoundToEven(x*float64(unit)/float64(time.Microsecond))) * time.Microsecond
}

// section is one table being decoded.
type section struct {
	c     *Config
	label string // how messages name it: "", "[gate]", "[projects.gem]", "[[watches]] w"
	path  string // the dotted prefix for Unknown: "", "gate.", "watches[0]."
	t     *toml.Table
	used  map[string]bool
	rank  []int // where it sits in the document, for ordering Unknown
}

// child is the section for the table under key (at index i of an array
// when i >= 0).
func (s *section) child(key string, i int, label, path string, t *toml.Table) *section {
	rank := append(append([]int{}, s.rank...), indexOf(s.t.Keys(), key))
	if i >= 0 {
		rank = append(rank, i)
	}
	return &section{c: s.c, label: label, path: path, t: t, used: map[string]bool{}, rank: rank}
}

func indexOf(keys []string, key string) int {
	for i, k := range keys {
		if k == key {
			return i
		}
	}
	return -1
}

func (s *section) name(key string) string {
	switch {
	case s.label == "":
		return key
	case strings.HasPrefix(s.label, "[["):
		return s.label + ": " + key
	}
	return s.label + " " + key
}

func (s *section) mistyped(key string, v toml.Value, what, then string) {
	s.c.Mistyped = append(s.c.Mistyped, fmt.Sprintf("%s: %s is not %s; %s", s.name(key), toml.PyRepr(v), what, then))
}

func (s *section) raw(key string) (toml.Value, bool) {
	s.used[key] = true
	return s.t.Get(key)
}

func defaultNote(def string) string {
	if def == "" {
		return "it is ignored"
	}
	return "the default " + toml.PyRepr(def) + " applies"
}

func (s *section) str(key, def string) string {
	v, ok := s.raw(key)
	if !ok {
		return def
	}
	if x, ok := v.(string); ok {
		return x
	}
	s.mistyped(key, v, "a string", defaultNote(def))
	return def
}

// need is a required string: false when absent or not a string.
func (s *section) need(key string) (string, bool) {
	if _, ok := s.t.Get(key); !ok {
		s.used[key] = true
		return "", false
	}
	before := len(s.c.Mistyped)
	x := s.str(key, "")
	return x, len(s.c.Mistyped) == before
}

func (s *section) integer(key string, def int) int {
	v, ok := s.raw(key)
	if !ok {
		return def
	}
	switch x := v.(type) {
	case int64:
		if x >= math.MinInt && x <= math.MaxInt {
			return int(x)
		}
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int(x)
		}
	}
	s.mistyped(key, v, "a whole number", "the default "+strconv.Itoa(def)+" applies")
	return def
}

func (s *section) number(key string, def float64) float64 {
	v, ok := s.raw(key)
	if !ok {
		return def
	}
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return x
		}
	}
	s.mistyped(key, v, "a number", "the default "+toml.PyRepr(def)+" applies")
	return def
}

func (s *section) needNumber(key string) (float64, bool) {
	if _, ok := s.t.Get(key); !ok {
		s.used[key] = true
		return 0, false
	}
	before := len(s.c.Mistyped)
	x := s.number(key, 0)
	return x, len(s.c.Mistyped) == before
}

// truthy is Python's truth of the value, false when absent.
func (s *section) truthy(key string) bool {
	v, _ := s.raw(key)
	return toml.Truthy(v)
}

// notFalse is `.get(key, True) is not False`.
func (s *section) notFalse(key string) bool {
	v, ok := s.raw(key)
	return !ok || v != false
}

func (s *section) strList(key string) []string {
	l, _ := s.strListSet(key)
	return l
}

// strListSet is a list of strings, and whether the key was there as a list.
// An empty list is a non-nil empty slice.
func (s *section) strListSet(key string) ([]string, bool) {
	v, ok := s.raw(key)
	if !ok {
		return nil, false
	}
	arr, ok := v.([]toml.Value)
	if !ok {
		s.mistyped(key, v, "a list", "it is ignored")
		return nil, false
	}
	out := []string{}
	for _, e := range arr {
		if x, ok := e.(string); ok {
			out = append(out, x)
		} else {
			s.mistyped(key, e, "a string", "it is left out")
		}
	}
	return out, true
}

// strictStrList is a safety key's list of strings, and whether the key is
// there but is not one (not a list, or an element not a string). It notes
// nothing in Mistyped: such a key would turn its protection off, so the
// check refuses it instead of warning.
func (s *section) strictStrList(key string) (out []string, bad bool) {
	v, ok := s.raw(key)
	if !ok {
		return nil, false
	}
	arr, ok := v.([]toml.Value)
	if !ok {
		return nil, true
	}
	out = []string{}
	for _, e := range arr {
		if x, ok := e.(string); ok {
			out = append(out, x)
		} else {
			bad = true
		}
	}
	return out, bad
}

func (s *section) table(key string) *toml.Table {
	v, ok := s.raw(key)
	if !ok {
		return nil
	}
	t, ok := v.(*toml.Table)
	if !ok {
		s.mistyped(key, v, "a table", "it is ignored")
		return nil
	}
	return t
}

// strMap is a free-form table of strings ([images], [templates], [models]).
func (s *section) strMap(key string) map[string]string {
	t := s.table(key)
	out := map[string]string{}
	for _, k := range t.Keys() {
		v, _ := t.Get(k)
		if x, ok := v.(string); ok {
			out[k] = x
		} else {
			s.c.Mistyped = append(s.c.Mistyped, fmt.Sprintf("%s: %s is not a string; it is ignored", s.name(key+"."+k), toml.PyRepr(v)))
		}
	}
	return out
}

// images is [images]: the pins (strings, as strMap reads them) and
// auto_update, a list of tool names.
func (s *section) images() (map[string]string, []string) {
	t := s.table("images")
	pins, auto := map[string]string{}, DefaultAutoUpdate()
	if t == nil {
		return pins, auto
	}
	img := s.child("images", -1, "[images]", "images.", t)
	if l, ok := img.strListSet("auto_update"); ok {
		auto = l
	}
	for _, k := range t.Keys() {
		if k == "auto_update" {
			continue
		}
		v, _ := t.Get(k)
		if x, ok := v.(string); ok {
			pins[k] = x
		} else {
			s.c.Mistyped = append(s.c.Mistyped, fmt.Sprintf("%s: %s is not a string; it is ignored", s.name("images."+k), toml.PyRepr(v)))
		}
	}
	return pins, auto
}

// env is an env table, each value as str() prints it.
func (s *section) env(key string) []EnvVar {
	t := s.table(key)
	var out []EnvVar
	for _, k := range t.Keys() {
		v, _ := t.Get(k)
		out = append(out, EnvVar{Name: k, Value: toml.PyStr(v)})
	}
	return out
}

// sub is the section for a [key] table (empty when absent).
func (s *section) sub(key string) *section {
	return s.child(key, -1, "["+s.path+key+"]", s.path+key+".", s.table(key))
}

// subTables decodes each [key.<name>] table in order.
func (s *section) subTables(key string, fn func(name string, s *section)) {
	t := s.table(key)
	for _, name := range t.Keys() {
		v, _ := t.Get(name)
		sub, ok := v.(*toml.Table)
		if !ok {
			s.c.Mistyped = append(s.c.Mistyped, fmt.Sprintf("[%s] %s: %s is not a table; it is ignored", key, name, toml.PyRepr(v)))
			continue
		}
		sec := s.child(key, indexOf(t.Keys(), name), "["+key+"."+name+"]", key+"."+name+".", sub)
		fn(name, sec)
		sec.done()
	}
}

// entries are the tables of a [[key]] array, as sections named by their
// name (or their position).
func (s *section) entries(key string) []*section {
	v, ok := s.raw(key)
	if !ok {
		return nil
	}
	arr, ok := v.([]toml.Value)
	if !ok {
		s.mistyped(key, v, "a list of tables", "it is ignored")
		return nil
	}
	var out []*section
	for i, e := range arr {
		t, ok := e.(*toml.Table)
		if !ok {
			s.mistyped(key, e, "a table", "it is left out")
			continue
		}
		label := fmt.Sprintf("[[%s]] #%d", key, i+1)
		if n, ok := t.Get("name"); ok {
			if x, ok := n.(string); ok {
				label = "[[" + key + "]] " + x
			}
		}
		out = append(out, s.child(key, i, label, fmt.Sprintf("%s[%d].", key, i), t))
	}
	return out
}

// done records the keys of the section nobody read.
func (s *section) done() {
	for _, k := range s.t.Keys() {
		if s.used[k] {
			continue
		}
		v, _ := s.t.Get(k)
		u := unknown{rank: append(append([]int{}, s.rank...), indexOf(s.t.Keys(), k)), dotted: s.path + k,
			note: s.name(k) + ": not a key the factory knows"}
		if _, isTable := v.(*toml.Table); isTable && s.label == "" {
			u.note = "[" + k + "]: not a key the factory knows"
		}
		s.c.unknown = append(s.c.unknown, u)
	}
}
