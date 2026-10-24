package helper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.toml"), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote.Image != "localhost/factory-pi:dev" || cfg.Remote.Memory != "800m" || cfg.Remote.MaxJobs != 4 ||
		cfg.Image != "" || len(cfg.Remote.Mounts) != 0 || len(cfg.JobEnv()) != 0 {
		t.Errorf("defaults: %+v", cfg)
	}
}

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte(`
image = "localhost/factory-agent:scion"
max_agents = 2
[images]
claude = "2.1.296"
pi = "1.2.0"
crystal = 1.14
[identity]
git_name = "Bot"
git_email = ""
[remote]
max_jobs = "2"
env = { A = "x", F = 1.5, L = ["a", 1], T = false }
[projects.gem]
path = "/nowhere"
`), 0o644)
	cfg, err := LoadConfig(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image != "localhost/factory-agent:scion" || cfg.Remote.MaxJobs != 2 {
		t.Errorf("config %+v", cfg)
	}
	if want := map[string]string{"claude": "2.1.296", "pi": "1.2.0", "crystal": "1.14"}; !reflect.DeepEqual(cfg.Images, want) {
		t.Errorf("images %v, want %v", cfg.Images, want)
	}
	// git_email is empty, so only the name is set; values are Python's str()
	want := []string{"GIT_AUTHOR_NAME=Bot", "GIT_COMMITTER_NAME=Bot", "A=x", "F=1.5", "L=['a', 1]", "T=False"}
	if got := cfg.JobEnv(); !reflect.DeepEqual(got, want) {
		t.Errorf("job env %q, want %q", got, want)
	}
}

func TestLoadConfigMaxJobsLikeInt(t *testing.T) {
	for in, want := range map[string]int{`3`: 3, `3.9`: 3, `" 5 "`: 5, `true`: 1, `-1`: -1} {
		p := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(p, []byte("[remote]\nmax_jobs = "+in+"\n"), 0o644)
		cfg, err := LoadConfig(p, "")
		if err != nil || cfg.Remote.MaxJobs != want {
			t.Errorf("max_jobs = %s: %d, %v; want %d", in, cfg.Remote.MaxJobs, err, want)
		}
	}
}

func TestLoadConfigRefusesWhatPythonCrashesOn(t *testing.T) {
	for _, body := range []string{
		"[remote\n",
		"[remote]\nmax_jobs = \"four\"\n",
		"[remote]\nmax_jobs = [4]\n",
		"[remote]\nmemory = 800\n",
		"[remote]\nimage = true\n",
		"[remote]\nmounts = \"/a:/b\"\n",
		"[remote]\nmounts = [1]\n",
		"[remote]\nenv = \"A=1\"\n",
		"remote = 1\n",
		"identity = []\n",
		"image = 3\n",
	} {
		p := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := LoadConfig(p, ""); err == nil {
			t.Errorf("%q: no error", body)
		} else if !strings.Contains(err.Error(), "config.toml") {
			t.Errorf("%q: error %q does not name the file", body, err)
		}
	}
}

// The SSH_ORIGINAL_COMMAND split is Python's shlex.split, from a golden
// written by testdata/gen_shlex.py.
func TestShlexMatchesPython(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "shlex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		In    string   `json:"in"`
		Out   []string `json:"out"`
		Error string   `json:"error"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got, err := shlexSplit(c.In)
		switch {
		case c.Error != "":
			if err == nil || err.Error() != c.Error {
				t.Errorf("%q: %q, %v; want error %q", c.In, got, err, c.Error)
			}
		case err != nil:
			t.Errorf("%q: %v", c.In, err)
		case !reflect.DeepEqual(got, c.Out) && !(len(got) == 0 && len(c.Out) == 0):
			t.Errorf("%q: %q, want %q", c.In, got, c.Out)
		}
	}
}

// [remote.<host>] goes over [remote] on the host of that name only: env
// adds to [remote] env, the other keys replace. Claude Code defaults to
// the subscription; Pi has only the API key.
func TestLoadConfigHostOverlay(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte(`
[remote]
image = "localhost/factory-pi:dev"
mounts = ["/a:/a"]
env = { A = "1", B = "2" }
[remote.helperb]
agent = "claude"
image = "localhost/factory-agent:scion"
memory = "3g"
cpus = 4
max_jobs = 2
mounts = ["/b:/b"]
env = { B = "3", C = "4" }
[remote.other]
agent = "codex"
`), 0o644)
	cfg, err := LoadConfig(p, "helperb")
	if err != nil {
		t.Fatal(err)
	}
	want := Remote{Image: "localhost/factory-agent:scion", Memory: "3g", Mounts: []string{"/b:/b"},
		Env: [][2]string{{"A", "1"}, {"B", "2"}, {"B", "3"}, {"C", "4"}}, MaxJobs: 2, CPUs: "4", Agent: "claude", Auth: "subscription", BuildMemory: "2g"}
	if !reflect.DeepEqual(cfg.Remote, want) {
		t.Errorf("helperb %+v\nwant %+v", cfg.Remote, want)
	}
	if got := cfg.JobEnv(); !reflect.DeepEqual(got, []string{"A=1", "B=3", "C=4"}) {
		t.Errorf("job env %q", got)
	}
	for _, host := range []string{"", "hostx", "env"} {
		cfg, err := LoadConfig(p, host)
		if err != nil {
			t.Fatal(err)
		}
		want := Remote{Image: "localhost/factory-pi:dev", Memory: "800m", Mounts: []string{"/a:/a"},
			Env: [][2]string{{"A", "1"}, {"B", "2"}}, MaxJobs: 4, CPUs: "2", Agent: "pi", Auth: "api-key", BuildMemory: "2g"}
		if !reflect.DeepEqual(cfg.Remote, want) {
			t.Errorf("%q: %+v", host, cfg.Remote)
		}
	}
	if _, err := LoadConfig(p, "other"); err == nil || !strings.Contains(err.Error(), `agent = 'codex'`) {
		t.Errorf("another host's bad agent: %v", err)
	}
}

func TestLoadConfigAuth(t *testing.T) {
	for body, want := range map[string]string{
		"[remote]\nagent = \"claude\"\n":                                 "subscription",
		"[remote]\nagent = \"claude\"\nauth = \"api-key\"\n":             "api-key",
		"[remote]\nauth = \"api-key\"\n":                                 "api-key",
		"[remote]\nauth = \"api-key\"\n[remote.h]\nagent = \"claude\"\n": "api-key",
		"[remote.h]\nagent = \"claude\"\nauth = \"subscription\"\n":      "subscription",
	} {
		p := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(p, []byte("[remote]\nimage = \"localhost/factory-agent:scion\"\n"+strings.Replace(body, "[remote]\n", "", 1)), 0o644)
		cfg, err := LoadConfig(p, "h")
		if err != nil {
			t.Errorf("%q: %v", body, err)
		} else if cfg.Remote.Auth != want {
			t.Errorf("%q: auth %q; want %q", body, cfg.Remote.Auth, want)
		}
	}
}

func TestLoadConfigRefusesBadRemoteKeys(t *testing.T) {
	for body, msg := range map[string]string{
		"[remote]\nagent = \"codex\"\n":                         `[remote] agent = 'codex'`,
		"[remote]\nagent = 1\n":                                 "[remote] agent must be a string",
		"[remote]\nauth = \"oauth\"\n":                          `[remote] auth = 'oauth'`,
		"[remote]\nauth = \"subscription\"\n":                   "Pi needs an API key",
		"[remote]\ncpus = 0\n":                                  "[remote] cpus must be a positive number",
		"[remote]\ncpus = \"two\"\n":                            "[remote] cpus must be a positive number",
		"[remote]\ncpus = true\n":                               "[remote] cpus must be a positive number",
		"[remote.h]\ndeps = \"yes\"\n":                          "[remote.h] deps must be true or false",
		"[remote.deps]\nagent = \"pi\"\n":                       "[remote.deps] is not a host's table",
		"[remote.h]\nmemory = 3\n":                              "[remote.h] memory must be a string",
		"[remote.h]\nmounts = \"/a:/a\"\n":                      "[remote.h] mounts must be a list of strings",
		"[remote]\nh = 1\n":                                     "[remote] h must be a table",
		"[remote.h]\nagent = \"pi\"\nauth = \"subscription\"\n": "Pi needs an API key",
		"[remote.h]\nagent = \"claude\"\n":                      "needs image = the Claude Code image",
		"[remote]\nimage = \"localhost/factory-pi:dev\"\n[remote.h]\nagent = \"claude\"\n": "needs image",
		"[remote.memory]\nagent = \"claude\"\n":                                            "[remote.memory] is not a host's table: memory is a [remote] key",
	} {
		p := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := LoadConfig(p, "h"); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: %v, want %q", body, err, msg)
		}
	}
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("[remote]\ndeps = true\n[remote.h]\ndeps = false\n"), 0o644)
	for host, want := range map[string]bool{"": true, "h": false} {
		if cfg, err := LoadConfig(p, host); err != nil || cfg.Remote.Deps != want {
			t.Errorf("deps on %q: %+v %v", host, cfg, err)
		}
	}
	os.WriteFile(p, []byte("[remote]\ncpus = 1.5\n"), 0o644)
	if cfg, err := LoadConfig(p, ""); err != nil || cfg.Remote.CPUs != "1.5" {
		t.Errorf("cpus 1.5: %+v %v", cfg, err)
	}
}
