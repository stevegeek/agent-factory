package helper

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/toml"
)

// Config is what the helper reads of its host's config.toml: [remote]
// (what each job gets), [identity] (the jobs' git author) and, for the
// rebuild, [images] and the top-level image. The rest of the file is the
// dispatcher's and is never looked at.
type Config struct {
	Image  string            // the top-level image: the claude image's tag, "" when absent
	Images map[string]string // [images] pins, str() of each truthy value: a floor
	// AutoUpdate is [images] auto_update: the tools the rebuild updates by
	// itself; ["claude", "pi"] when absent.
	AutoUpdate []string
	Remote     Remote
	Identity   Identity
	// Host is the name whose [remote.<host>] applies (HostName): "" for
	// none.
	Host string
}

// Remote is [remote], with Python's defaults, and over it this host's own
// [remote.<host>].
type Remote struct {
	Image   string   // "localhost/factory-pi:dev"
	Memory  string   // "800m"
	Mounts  []string // podman -v values; {root} is replaced when a job runs
	Env     [][2]string
	MaxJobs int    // 4
	CPUs    string // podman --cpus, "2"
	// Agent is what a job runs: AgentPi (the default) or AgentClaude, in
	// Image either way.
	Agent string
	// Auth is the credential a job gets, and the only one: AuthAPIKey
	// (Pi's only kind) or, the default for Claude Code, AuthSubscription.
	Auth string
	// Deps installs the checkout's dependencies before the agent runs
	// (withDeps), into the caches the host mounts writable.
	Deps bool
	// Build lets this host build core commits (`build`): the master's
	// source of a merged commit, built in a container (build.go).
	Build bool
	// BuildMemory is a build container's --memory ("2g").
	BuildMemory string
	// Watchdog is the master's /healthz URL that `factory-remote watchdog`
	// checks ("" for none), and WatchdogIP the address it dials for it
	// ("" for the URL's host); TLS still checks the URL's host.
	Watchdog, WatchdogIP string
}

// The agents a helper runs, and the credentials it gives them.
const (
	AgentPi          = "pi"
	AgentClaude      = "claude"
	AuthAPIKey       = "api-key"      // ANTHROPIC_API_KEY from ~/.config/factory/anthropic-api-key
	AuthSubscription = "subscription" // CLAUDE_CODE_OAUTH_TOKEN from ~/.config/factory/claude-oauth-token
)

// Identity is [identity]: the jobs' git author, when set.
type Identity struct {
	GitName, GitEmail string
}

// LoadConfig reads the config at path for the helper named host, whose
// [remote.<host>] goes over [remote]. A missing file is the defaults
// (Python's FileNotFoundError → {}). Python reads the values lazily and
// crashes on a wrong type where it uses one; here every value the helper
// uses is checked at load, and the error names the file and the key.
func LoadConfig(path, host string) (*Config, error) {
	cfg := &Config{Remote: Remote{Image: imagebuild.DefaultPiImage, Memory: "800m", MaxJobs: 4, CPUs: "2", Agent: AgentPi, BuildMemory: "2g"},
		Images: map[string]string{}, AutoUpdate: []string{"claude", "pi"}, Host: host}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		cfg.Remote.Auth = AuthAPIKey
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config.toml: %w", err)
	}
	doc, err := toml.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.decode(doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) decode(doc *toml.Table) error {
	if v, ok := doc.Get("image"); ok {
		s, ok := v.(string)
		if !ok {
			return errors.New("image must be a string")
		}
		c.Image = s
	}
	images, err := table(doc, "images")
	if err != nil {
		return err
	}
	for _, k := range images.Keys() {
		v, _ := images.Get(k)
		if k == "auto_update" {
			list, ok := v.([]toml.Value)
			if !ok {
				return errors.New("[images] auto_update must be a list of names")
			}
			c.AutoUpdate = []string{}
			for _, e := range list {
				name, ok := e.(string)
				if !ok {
					return errors.New("[images] auto_update must be a list of names")
				}
				c.AutoUpdate = append(c.AutoUpdate, name)
			}
			continue
		}
		if toml.Truthy(v) {
			c.Images[k] = toml.PyStr(v)
		}
	}
	ident, err := table(doc, "identity")
	if err != nil {
		return err
	}
	if v, _ := ident.Get("git_name"); toml.Truthy(v) {
		c.Identity.GitName = toml.PyStr(v)
	}
	if v, _ := ident.Get("git_email"); toml.Truthy(v) {
		c.Identity.GitEmail = toml.PyStr(v)
	}
	remote, err := table(doc, "remote")
	if err != nil {
		return err
	}
	if err := c.Remote.decode(remote, "[remote]"); err != nil {
		return err
	}
	// [remote.<host>] is this host's own: env adds to [remote] env, the
	// other keys replace. "env" is [remote]'s env table, never a host.
	if c.Host != "" && c.Host != "env" {
		own, err := table(remote, c.Host)
		if err != nil {
			return fmt.Errorf("[remote] %w", err)
		}
		if own != nil {
			if err := c.Remote.decode(own, "[remote."+c.Host+"]"); err != nil {
				return err
			}
		}
	}
	switch c.Remote.Agent {
	case AgentPi, AgentClaude:
	default:
		return fmt.Errorf("[remote] agent = %s: it is %q or %q", toml.PyRepr(c.Remote.Agent), AgentPi, AgentClaude)
	}
	if c.Remote.Agent == AgentClaude && c.Remote.Image == imagebuild.DefaultPiImage {
		return fmt.Errorf("[remote] agent = %q needs image = the Claude Code image, not the Pi image %s", AgentClaude, imagebuild.DefaultPiImage)
	}
	switch {
	case c.Remote.Auth == "" && c.Remote.Agent == AgentClaude:
		c.Remote.Auth = AuthSubscription
	case c.Remote.Auth == "":
		c.Remote.Auth = AuthAPIKey
	case c.Remote.Auth != AuthAPIKey && c.Remote.Auth != AuthSubscription:
		return fmt.Errorf("[remote] auth = %s: it is %q or %q", toml.PyRepr(c.Remote.Auth), AuthSubscription, AuthAPIKey)
	case c.Remote.Auth == AuthSubscription && c.Remote.Agent != AgentClaude:
		return fmt.Errorf("[remote] auth = %q is for Claude Code only: Pi needs an API key", AuthSubscription)
	}
	return nil
}

// decode reads one [remote] table, [remote] itself or a host's, over r.
func (r *Remote) decode(t *toml.Table, label string) error {
	for _, k := range remoteKeys() {
		if v, ok := t.Get(k); ok && k != "env" {
			if _, isTable := v.(*toml.Table); isTable && label == "[remote]" {
				return fmt.Errorf("[remote.%s] is not a host's table: %s is a [remote] key", k, k)
			}
		}
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"image", &r.Image}, {"memory", &r.Memory}, {"agent", &r.Agent}, {"auth", &r.Auth}, {"build_memory", &r.BuildMemory},
		{"watchdog", &r.Watchdog}, {"watchdog_ip", &r.WatchdogIP}} {
		if v, ok := t.Get(f.key); ok {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s %s must be a string", label, f.key)
			}
			*f.dst = s
		}
	}
	for _, f := range []struct {
		key string
		dst *bool
	}{{"deps", &r.Deps}, {"build", &r.Build}} {
		if v, ok := t.Get(f.key); ok {
			b, ok := v.(bool)
			if !ok {
				return fmt.Errorf("%s %s must be true or false", label, f.key)
			}
			*f.dst = b
		}
	}
	if v, ok := t.Get("build_memory"); ok && textx.Strip(toml.PyStr(v)) == "" {
		return fmt.Errorf("%s build_memory must not be empty", label)
	}
	if v, ok := t.Get("cpus"); ok {
		var s string
		switch v.(type) {
		case string, int64, float64:
			s = textx.Strip(toml.PyStr(v))
		}
		if n, err := strconv.ParseFloat(s, 64); err != nil || !(n > 0) || math.IsInf(n, 0) {
			return fmt.Errorf("%s cpus must be a positive number", label)
		}
		r.CPUs = s
	}
	if v, ok := t.Get("mounts"); ok {
		list, ok := v.([]toml.Value)
		if !ok {
			return fmt.Errorf("%s mounts must be a list of strings", label)
		}
		r.Mounts = nil
		for _, m := range list {
			s, ok := m.(string)
			if !ok {
				return fmt.Errorf("%s mounts must be a list of strings", label)
			}
			r.Mounts = append(r.Mounts, s)
		}
	}
	env, err := table(t, "env")
	if err != nil {
		return fmt.Errorf("%s %w", label, err)
	}
	for _, k := range env.Keys() {
		v, _ := env.Get(k)
		r.Env = append(r.Env, [2]string{k, toml.PyStr(v)})
	}
	if v, ok := t.Get("max_jobs"); ok {
		n, err := pyInt(v)
		if err != nil {
			return fmt.Errorf("%s max_jobs: %w", label, err)
		}
		r.MaxJobs = n
	}
	return nil
}

// remoteKeys are the keys of a [remote] table; no host is named after one.
func remoteKeys() []string {
	return []string{"image", "memory", "mounts", "env", "max_jobs", "cpus", "agent", "auth", "deps", "build", "build_memory", "watchdog", "watchdog_ip"}
}

// table is cfg.get(key, {}) where the value must be a table.
func table(t *toml.Table, key string) (*toml.Table, error) {
	v, ok := t.Get(key)
	if !ok {
		return nil, nil // a nil *Table reads as empty
	}
	sub, ok := v.(*toml.Table)
	if !ok {
		return nil, fmt.Errorf("%s must be a table", key)
	}
	return sub, nil
}

// pyInt is Python int() of a TOML scalar: ints as they are, floats
// truncated, bools 0 and 1, strings of digits with surrounding whitespace.
func pyInt(v toml.Value) (int, error) {
	switch x := v.(type) {
	case int64:
		if x > math.MaxInt32 || x < math.MinInt32 {
			return 0, fmt.Errorf("%d is out of range", x)
		}
		return int(x), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) > math.MaxInt32 {
			return 0, fmt.Errorf("%v is not a whole number", x)
		}
		return int(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(textx.Strip(x), "_", ""))
		if err != nil || strings.HasPrefix(textx.Strip(x), "_") || strings.Contains(x, "__") {
			return 0, fmt.Errorf("invalid literal for int(): %s", toml.PyRepr(x))
		}
		return n, nil
	}
	return 0, fmt.Errorf("%s is not a number", toml.PyRepr(v))
}

// JobEnv is the job's environment from config, as "K=V" in Python's dict
// order: the git author and committer from [identity], then [remote] env,
// a later key replacing an earlier one in its place.
func (c *Config) JobEnv() []string {
	var env orderedEnv
	if n := c.Identity.GitName; n != "" {
		env.set("GIT_AUTHOR_NAME", n)
		env.set("GIT_COMMITTER_NAME", n)
	}
	if e := c.Identity.GitEmail; e != "" {
		env.set("GIT_AUTHOR_EMAIL", e)
		env.set("GIT_COMMITTER_EMAIL", e)
	}
	for _, kv := range c.Remote.Env {
		env.set(kv[0], kv[1])
	}
	return env.list()
}

// orderedEnv is a Python dict of str: an update keeps a key's first place.
type orderedEnv struct {
	keys []string
	vals map[string]string
}

func (e *orderedEnv) set(k, v string) {
	if e.vals == nil {
		e.vals = map[string]string{}
	}
	if _, ok := e.vals[k]; !ok {
		e.keys = append(e.keys, k)
	}
	e.vals[k] = v
}

func (e *orderedEnv) list() []string {
	out := make([]string, 0, len(e.keys))
	for _, k := range e.keys {
		out = append(out, k+"="+e.vals[k])
	}
	return out
}
