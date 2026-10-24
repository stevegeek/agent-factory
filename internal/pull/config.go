package pull

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/toml"
)

// Config is factory-pull's config file (TOML), on the desktop. A leading ~
// in a path is the user's home.
//
//	ssh = "hub-factory"            # the dispatcher: an ssh destination (an alias in ~/.ssh/config)
//	key = "~/.ssh/factory-pull"    # optional: the key forced to `factory-remote mailbox <host>`
//	host = "desktop"               # this host's [hosts.<name>] on the dispatcher
//	instance = "~/lever/factory-worker"   # the Lever instance: lever.yaml, .lever-state/
//	tree = "tree"                  # its tree (lever.yaml's tree:), relative to instance
//	workers = ["fw1", "fw2", "fw3"]        # declared workers, each with dir workers/<name>
//	state = "~/.local/state/factory-pull" # the jobs in hand, kept across restarts
//	lever = "lever"                # the lever binary
//	ssh_bin = "ssh"
//	poll_seconds = 30              # how often to ask for a job while a worker is free
//	beat_seconds = 60              # how often to beat while a job runs
//	vitals_seconds = 60            # how often to post this host's vitals (0: never)
//	git_name = "Factory Bot"       # optional: the commit identity set in each job's checkout
//	git_email = "bot@example.com"
type Config struct {
	SSH, Key, Host        string
	Instance, Tree, State string
	Workers               []string
	Lever, SSHBin         string
	Poll, Beat            time.Duration
	Vitals                time.Duration // 0: no vitals posts
	GitName, GitEmail     string
}

// identity is what git_name and git_email may hold: they are written into
// each job's script in single quotes.
//
// re: go
var identity = regexp.MustCompile(`^[A-Za-z0-9 ._@+-]{1,100}\z`)

// LoadConfig reads and checks the config at path; home expands ~.
func LoadConfig(path, home string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	t, err := toml.Parse(b)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return parseConfig(t, home)
}

func parseConfig(t *toml.Table, home string) (Config, error) {
	var errs []error
	str := func(key, def string) string {
		v, ok := t.Get(key)
		if !ok {
			return def
		}
		s, ok := v.(string)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: not a string", key))
		}
		return s
	}
	secs := func(key string, def int64) time.Duration {
		v, ok := t.Get(key)
		if !ok {
			return time.Duration(def) * time.Second
		}
		n, ok := v.(int64)
		if !ok || n < 1 || n > 3600 {
			errs = append(errs, fmt.Errorf("%s: not a number of seconds from 1 to 3600", key))
		}
		return time.Duration(n) * time.Second
	}
	// vitals_seconds may also be 0, for none
	vitalsSecs := func() time.Duration {
		v, ok := t.Get("vitals_seconds")
		if !ok {
			return 60 * time.Second
		}
		if n, ok := v.(int64); ok && n == 0 {
			return 0
		}
		return secs("vitals_seconds", 60)
	}
	expand := func(p string) string {
		if p == "~" {
			return home
		}
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			return filepath.Join(home, rest)
		}
		return p
	}
	c := Config{
		SSH: str("ssh", ""), Key: expand(str("key", "")), Host: str("host", ""),
		Instance: expand(str("instance", "")), Tree: str("tree", "tree"),
		State: expand(str("state", filepath.Join(home, ".local", "state", "factory-pull"))),
		Lever: expand(str("lever", "lever")), SSHBin: expand(str("ssh_bin", "ssh")),
		Poll: secs("poll_seconds", 30), Beat: secs("beat_seconds", 60), Vitals: vitalsSecs(),
		GitName: str("git_name", ""), GitEmail: str("git_email", ""),
	}
	if v, ok := t.Get("workers"); ok {
		list, _ := v.([]toml.Value)
		for _, w := range list {
			s, ok := w.(string)
			if !ok || !jobproto.JobNameOK(s) {
				errs = append(errs, fmt.Errorf("workers: %v is not a worker name", w))
				continue
			}
			c.Workers = append(c.Workers, s)
		}
	}
	for _, k := range t.Keys() {
		switch k {
		case "ssh", "key", "host", "instance", "tree", "state", "workers", "lever", "ssh_bin",
			"poll_seconds", "beat_seconds", "vitals_seconds", "git_name", "git_email":
		default:
			errs = append(errs, fmt.Errorf("%s: not a key factory-pull knows", k))
		}
	}
	if !filepath.IsAbs(c.Tree) {
		c.Tree = filepath.Join(c.Instance, c.Tree)
	}
	switch {
	case c.SSH == "" || strings.HasPrefix(c.SSH, "-"):
		errs = append(errs, errors.New("ssh: the dispatcher's ssh destination is required"))
	case !jobproto.JobNameOK(c.Host):
		errs = append(errs, errors.New("host: this host's [hosts.<name>] on the dispatcher is required"))
	case !filepath.IsAbs(c.Instance):
		errs = append(errs, errors.New("instance: the Lever instance directory is required, as an absolute path"))
	case len(c.Workers) == 0:
		errs = append(errs, errors.New("workers: name at least one worker"))
	case !filepath.IsAbs(c.State):
		errs = append(errs, errors.New("state: must be an absolute path"))
	}
	for _, id := range []struct{ key, v string }{{"git_name", c.GitName}, {"git_email", c.GitEmail}} {
		if id.v != "" && !identity.MatchString(id.v) {
			errs = append(errs, fmt.Errorf("%s: only letters, digits, spaces and ._@+- are allowed", id.key))
		}
	}
	return c, errors.Join(errs...)
}

// WorkerDir is a worker's directory in the tree (lever.yaml's dir:
// workers/<name>).
func (c Config) WorkerDir(worker string) string {
	return filepath.Join(c.Tree, "workers", worker)
}

// OperatorSock is the Lever broker's operator socket (0600, the host
// user's own channel).
func (c Config) OperatorSock() string {
	return filepath.Join(c.Instance, ".lever-state", "operator.sock")
}
