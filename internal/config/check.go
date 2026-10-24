package config

import (
	"cmp"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/toml"
)

// tokenName is TOKEN_NAME, used with fullmatch.
//
// py: ^[a-z][a-z0-9-]{0,31}$
var tokenName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}\z`)

// reservedNames is RESERVED_NAMES: no person or token may be called these
// (except "maintainer", the default person). The one list is in model.
func reservedNames() []string { return model.ReservedNames() }

// TokenRecord is what the check needs of one API token: its name (as str()
// prints it) and the projects it names.
type TokenRecord struct {
	Name     string
	Projects []string
}

// TokenRecords is the token store as the check reads it. The error's text
// is reported as a problem verbatim, as config_problems does with
// load_tokens' RuntimeError or ValueError.
type TokenRecords interface {
	Records() ([]TokenRecord, error)
}

// Problems is config_problems: what `factory check` reports. Problems exit
// 1; warnings print "warning: …" and exit 0. The problems and the first
// warnings are Python's, word for word; the port then adds warnings for
// unknown keys and wrong types, entries it skips, a factory project missing
// a gate command and GitHub with no app_slug. A nil tokens
// skips the token checks.
func (c *Config) Problems(tokens TokenRecords) (problems, warnings []string) {
	projects, _ := c.Raw.Table("projects")
	board, known := map[string]bool{}, map[string]bool{}
	for _, key := range projects.Keys() {
		known[key] = true
		p, ok := projects.Table(key)
		if !ok {
			continue
		}
		mode, ok := p.Get("mode")
		if !ok {
			mode = ModeFactory
		}
		switch mode {
		case ModeFactory:
		case ModeBoard:
			board[key] = true
			var unused []string
			for _, k := range []string{"path", "github", "base", "install", "test", "lint", "language", "kind"} {
				if _, ok := p.Get(k); ok {
					unused = append(unused, k)
				}
			}
			if len(unused) > 0 {
				warnings = append(warnings, fmt.Sprintf("[projects.%s] is a board project; %s are not used", key, strings.Join(unused, ", ")))
			}
		default:
			problems = append(problems, fmt.Sprintf("[projects.%s] mode = %s: it is one of %s, %s", key, toml.PyRepr(mode), ModeFactory, ModeBoard))
		}
	}
	for _, what := range []string{"watches", "schedules", "recipes"} {
		jobs, _ := c.Raw.Get(what)
		arr, _ := jobs.([]toml.Value)
		for _, j := range arr {
			job, ok := j.(*toml.Table)
			if !ok {
				continue
			}
			var named []string
			if what == "watches" {
				if v, ok := job.Get("project"); ok {
					if s, ok := v.(string); ok {
						named = []string{s}
					}
				}
			} else if v, _ := job.Get("projects"); toml.Truthy(v) {
				named = stringNames(v)
			}
			name, _ := job.Get("name")
			if v, _ := job.Get("findings"); what != "watches" && toml.Truthy(v) {
				if k, _ := job.Get("kind"); toml.PyStr(k) != "task" {
					warnings = append(warnings, fmt.Sprintf("[[%s]] %s: findings = true is for a task (kind = \"task\"); it is ignored here", what, toml.PyStr(name)))
				}
			}
			for _, key := range named {
				if board[key] {
					warnings = append(warnings, fmt.Sprintf("[[%s]] %s names %s, a board project: the pipeline never works on one", what, toml.PyStr(name), key))
				} else if !known[key] && key != NoticeProject {
					// kept as Python runs it, but said
					warnings = append(warnings, fmt.Sprintf("[[%s]] %s names %s, which is not a project", what, toml.PyStr(name), key))
				}
			}
		}
	}

	ppl, ok := c.board.Get("people")
	if !ok {
		ppl = []toml.Value{DefaultPerson}
	}
	list, isList := ppl.([]toml.Value)
	if !isList || len(list) == 0 {
		problems = append(problems, "[board] people: a list of names, the maintainer first")
	} else {
		seen := map[string]bool{}
		for _, x := range list {
			s, isStr := x.(string)
			if !isStr || !tokenName.MatchString(s) || (oneOf(s, reservedNames()) && s != DefaultPerson) {
				problems = append(problems, fmt.Sprintf("[board] people: %s is not a name a person can have here", toml.PyRepr(x)))
			}
			seen[toml.PyStr(x)] = true
		}
		if len(seen) != len(list) {
			problems = append(problems, "[board] people: a name is there twice")
		}
	}

	if tokens != nil {
		records, err := tokens.Records()
		if err != nil {
			problems = append(problems, err.Error())
			records = nil
		}
		for _, r := range records {
			var gone []string
			for _, x := range r.Projects {
				if _, ok := projects.Get(x); !ok {
					gone = append(gone, x)
				}
			}
			if len(gone) > 0 {
				problems = append(problems, fmt.Sprintf("token %s names %s, not in config", r.Name, strings.Join(gone, ", ")))
			}
			for _, x := range list {
				if s, ok := x.(string); ok && s == r.Name {
					problems = append(problems, fmt.Sprintf("token %s has a person's name", r.Name))
					break
				}
			}
		}
	}

	if a := c.Findings.AutoFile; a != AutoFileNone && a != AutoFileConfirmedSafe {
		problems = append(problems, fmt.Sprintf("[findings] auto_file = %q: it is %q or %q", a, AutoFileNone, AutoFileConfirmedSafe))
	}
	remotes := append([]RemoteHost{{Agent: c.Remote.Agent, Auth: c.Remote.Auth, Image: c.Remote.Image}}, c.Remote.Hosts...)
	for i, h := range remotes {
		label, agent, auth, image := "[remote]", h.Agent, h.Auth, h.Image
		if i > 0 {
			label = "[remote." + h.Name + "]"
			agent, auth, image = cmp.Or(agent, c.Remote.Agent), cmp.Or(auth, c.Remote.Auth), cmp.Or(image, c.Remote.Image)
			if slices.Contains(RemoteKeys(), h.Name) {
				problems = append(problems, fmt.Sprintf("%s is not a helper's table: %s is a [remote] key", label, h.Name))
				continue
			}
			if h.Name != strings.ToLower(h.Name) {
				problems = append(problems, fmt.Sprintf("%s never applies: a helper reads the table named by its lowercased short hostname", label))
			}
		}
		if agent == "claude" && image == "localhost/factory-pi:dev" {
			problems = append(problems, fmt.Sprintf("%s agent = \"claude\" needs image = the Claude Code image, not the Pi image %s", label, image))
		}
		agent = cmp.Or(agent, "pi")
		if agent == "claude" {
			auth = cmp.Or(auth, "subscription")
		}
		switch {
		case agent != "pi" && agent != "claude":
			problems = append(problems, fmt.Sprintf("%s agent = %q: it is \"pi\" or \"claude\"", label, agent))
		case auth != "" && auth != "api-key" && auth != "subscription":
			problems = append(problems, fmt.Sprintf("%s auth = %q: it is \"subscription\" or \"api-key\"", label, auth))
		case auth == "subscription" && agent == "pi":
			problems = append(problems, fmt.Sprintf("%s auth = \"subscription\" is for Claude Code only: Pi needs an API key", label))
		}
	}
	for _, h := range c.Hosts {
		if h.Kind != HostJobs {
			problems = append(problems, fmt.Sprintf("[hosts.%s] kind = %q: the only host kind is %q", h.Name, h.Kind, HostJobs))
		}
		switch h.Transport {
		case TransportSSH:
			if h.SSH == "" {
				problems = append(problems, fmt.Sprintf("[hosts.%s] has no ssh destination (or transport = %q)", h.Name, TransportPull))
			}
		case TransportPull:
		default:
			problems = append(problems, fmt.Sprintf("[hosts.%s] transport = %q: a host is reached by %q or %q", h.Name, h.Transport, TransportSSH, TransportPull))
		}
		for _, m := range []struct {
			key string
			n   int
		}{{"claim_minutes", h.ClaimMinutes}, {"beat_minutes", h.BeatMinutes}} {
			if m.n < 1 {
				problems = append(problems, fmt.Sprintf("[hosts.%s] %s must be at least 1", h.Name, m.key))
			}
		}
	}

	if n := c.Backup.Name; n != "" && (textx.Slugify(n) != n || n == "host" || n == "change") {
		problems = append(problems, fmt.Sprintf("[backup] name = %q must be a slug (lowercase letters, digits and dashes) other than \"host\" or \"change\"", n))
	}

	if _, ok := c.HostNamed(BuildSelf); ok {
		problems = append(problems, fmt.Sprintf("[hosts.%s]: %q is [build] host's name for this host", BuildSelf, BuildSelf))
	}
	if c.Build.Host != "" && c.Build.Host != BuildSelf {
		switch h, ok := c.HostNamed(c.Build.Host); {
		case !ok:
			problems = append(problems, fmt.Sprintf("[build] host = %q is not in [hosts]", c.Build.Host))
		case h.Pull():
			problems = append(problems, fmt.Sprintf("[build] host = %q is a pull host: only an ssh helper builds", c.Build.Host))
		}
	}

	problems = append(problems, c.kindProblems()...)
	selfProblems, selfWarnings := c.selfHostProblems()
	problems = append(problems, selfProblems...)
	if _, ok := c.Raw.Table("deploy"); ok {
		problems = append(problems, c.deployProblems()...)
	}

	for _, h := range c.Hosts {
		if !h.Pull() && (slices.Contains(h.Prefer, "gate") || slices.Contains(h.Stages, "gate")) {
			warnings = append(warnings, fmt.Sprintf("[hosts.%s] lists \"gate\": only a pull host runs gate jobs, so its gates run here", h.Name))
		}
	}
	warnings = append(warnings, c.unknownNotes...)
	warnings = append(warnings, c.Mistyped...)
	warnings = append(warnings, c.skipped...)
	for _, p := range c.FactoryProjects() {
		var missing []string
		for _, k := range []string{"install", "test", "lint"} {
			if !p.Has(k) {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			warnings = append(warnings, fmt.Sprintf("[projects.%s] has no %s: the gate refuses to run its checks without them", p.Key, strings.Join(missing, ", ")))
		}
	}
	if _, ok := MemoryBytes(c.Gate.HeavyMemory); !ok {
		warnings = append(warnings, fmt.Sprintf("[gate] heavy_memory = %q is not a memory size: every gate waits for this host to be idle", c.Gate.HeavyMemory))
	}
	warnings = append(warnings, selfWarnings...)
	if c.HasGitHub() && c.GitHub.AppSlug == "" {
		warnings = append(warnings, "[github] app_slug is not set: gate-workflow needs the App's slug")
	}
	return problems, warnings
}

// HostProblems are the checks that depend on the machine the config is
// on, host (ThisHost): `factory check` runs them after Problems, while a
// restored archive's check (backup verify, adopt) does not, since a
// master's archive is checked on other machines. [build] host = "self"
// needs build = true for host's own factory-remote.
func (c *Config) HostProblems(host string) []string {
	if c.Build.Host == BuildSelf && !c.remoteBuilds(host) {
		return []string{fmt.Sprintf("[build] host = %q needs build = true in [remote.%s] (this host's factory-remote builds)", BuildSelf, host)}
	}
	return nil
}

// remoteBuilds is factory-remote's build switch on host: [remote.<host>]
// build, else [remote] build (helper.LoadConfig's override).
func (c *Config) remoteBuilds(host string) bool {
	r, ok := c.Raw.Table("remote")
	if !ok {
		return false
	}
	v, _ := r.Get("build")
	if t, ok := r.Table(host); ok && host != "" {
		if hv, ok := t.Get("build"); ok {
			v = hv
		}
	}
	return v == true
}

// ownerRepo is a GitHub owner/repo as gitx.Mirror takes one (its
// mirrorRepo): no leading dot or dash in either part, so it can be neither
// a path nor an option.
var ownerRepo = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*\z`) // re: go

// deployProblems checks a [deploy] that is present: core and config are
// owner/repo, config_dir stays inside the config repo, and the App (for
// config's token) and a builder are set.
func (c *Config) deployProblems() []string {
	var out []string
	for _, r := range []struct{ key, v string }{{"core", c.Deploy.Core}, {"config", c.Deploy.Config}} {
		if !ownerRepo.MatchString(r.v) {
			out = append(out, fmt.Sprintf("[deploy] %s = %q is not owner/repo", r.key, r.v))
		}
	}
	if d := c.Deploy.ConfigDir; d == "" || path.IsAbs(d) || slices.Contains(strings.Split(d, "/"), "..") {
		out = append(out, fmt.Sprintf("[deploy] config_dir = %q must be a relative path inside the config repo", d))
	}
	if !c.HasGitHub() {
		out = append(out, "[deploy] needs [github] installation_id: the config repo is fetched with an App token")
	}
	if c.Build.Host == "" {
		out = append(out, "[deploy] needs [build] host: a self-deploy builds on a helper")
	}
	return out
}

// minDenyRunes is the shortest [hygiene] deny term: a shorter one would
// match ordinary text everywhere.
const minDenyRunes = 3

// selfHostProblems checks the keys of the factory's own projects: each
// self_deploy is core or config, at most once each, on a factory project
// with github; walls and deny are lists of strings; no wall is empty; every
// deny term is long enough and has no surrounding whitespace. A hygiene
// project with nothing to deny is only a warning. Only a term too short to
// be a secret is ever quoted.
func (c *Config) selfHostProblems() (problems, warnings []string) {
	if c.Hygiene.denyBad {
		problems = append(problems, "[hygiene] deny is not a list of strings: the scan cannot run until it is")
	}
	padded := false
	for _, term := range c.Hygiene.Deny {
		trimmed := strings.TrimSpace(term)
		switch {
		case len([]rune(trimmed)) < minDenyRunes:
			problems = append(problems, fmt.Sprintf("[hygiene] deny: %q is shorter than %d characters", term, minDenyRunes))
		case trimmed != term:
			padded = true
		}
	}
	if padded { // a long term may be real, so it is not quoted
		problems = append(problems, "[hygiene] deny: a term starts or ends with whitespace, so it misses a hit at a line's start or end")
	}
	deploys := map[string]string{} // self_deploy value -> the project that has it
	for _, p := range c.Projects {
		label := "[projects." + p.Key + "]"
		if p.wallsBad {
			problems = append(problems, label+" walls is not a list of strings: no wall applies until it is")
		}
		if p.selfDeployBad {
			problems = append(problems, fmt.Sprintf("%s self_deploy is not a string: it is %q or %q", label, SelfDeployCore, SelfDeployConfig))
		}
		if slices.Contains(p.Walls, "") {
			problems = append(problems, label+" walls: an entry is empty")
		}
		if p.Hygiene && len(c.Hygiene.Deny) == 0 {
			warnings = append(warnings, label+" hygiene = true with an empty [hygiene] deny: the scan finds nothing")
		}
		v := p.SelfDeploy
		switch {
		case v == "":
		case v != SelfDeployCore && v != SelfDeployConfig:
			problems = append(problems, fmt.Sprintf("%s self_deploy = %q: it is %q or %q", label, v, SelfDeployCore, SelfDeployConfig))
		case p.Mode != ModeFactory:
			problems = append(problems, fmt.Sprintf("%s self_deploy = %q is on a board project: the pipeline never merges one", label, v))
		case p.GitHub == "":
			problems = append(problems, fmt.Sprintf("%s self_deploy = %q needs github: a deploy follows a merged pull request", label, v))
		case deploys[v] != "":
			problems = append(problems, fmt.Sprintf("%s self_deploy = %q: [projects.%s] already deploys %s", label, v, deploys[v], v))
		default:
			deploys[v] = p.Key
		}
	}
	return problems, warnings
}

// stringNames is list(v) keeping only the string elements (only a string can
// name a project).
func stringNames(v toml.Value) []string {
	var out []string
	switch x := v.(type) {
	case []toml.Value:
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	default:
		out = pyList(v)
	}
	return out
}
