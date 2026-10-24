package pull

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `ssh = "hub-factory"
key = "~/.ssh/factory-pull"
host = "desktop"
instance = "~/lever/factory-worker"
workers = ["fw1", "fw2", "fw3"]
beat_seconds = 45
git_name = "Factory Bot"
`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if c.SSH != "hub-factory" || c.Key != "/home/u/.ssh/factory-pull" || c.Host != "desktop" ||
		c.Instance != "/home/u/lever/factory-worker" || c.Tree != "/home/u/lever/factory-worker/tree" ||
		c.State != "/home/u/.local/state/factory-pull" || !slices.Equal(c.Workers, []string{"fw1", "fw2", "fw3"}) ||
		c.Lever != "lever" || c.SSHBin != "ssh" || c.Poll != 30*time.Second || c.Beat != 45*time.Second || c.GitName != "Factory Bot" ||
		c.Vitals != time.Minute {
		t.Errorf("config %+v", c)
	}
	if c.WorkerDir("fw2") != "/home/u/lever/factory-worker/tree/workers/fw2" ||
		c.OperatorSock() != "/home/u/lever/factory-worker/.lever-state/operator.sock" {
		t.Errorf("paths %s %s", c.WorkerDir("fw2"), c.OperatorSock())
	}
}

// The example config reads.
func TestTheExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", "examples", "lever", "factory-pull.toml"), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "desktop" || len(c.Workers) != 3 || c.Tree != "/home/u/lever/factory-worker/tree" {
		t.Errorf("config %+v", c)
	}
}

// vitals_seconds = 0 turns the vitals posts off.
func TestVitalsSeconds(t *testing.T) {
	base := "ssh = \"z\"\nhost = \"desktop\"\ninstance = \"/i\"\nworkers = [\"fw1\"]\n"
	for text, want := range map[string]time.Duration{"": time.Minute, "vitals_seconds = 0\n": 0, "vitals_seconds = 300\n": 5 * time.Minute} {
		c, err := LoadConfig(writeConfig(t, base+text), "/h")
		if err != nil || c.Vitals != want {
			t.Errorf("%q: %v %v, want %v", text, c.Vitals, err, want)
		}
	}
}

func TestLoadConfigRefuses(t *testing.T) {
	base := "ssh = \"z\"\nhost = \"desktop\"\ninstance = \"/i\"\nworkers = [\"fw1\"]\n"
	for _, tc := range []struct{ text, want string }{
		{strings.Replace(base, `ssh = "z"`, `ssh = "-oProxyCommand=x"`, 1), "ssh:"},
		{strings.Replace(base, `host = "desktop"`, `host = "../x"`, 1), "host:"},
		{strings.Replace(base, `instance = "/i"`, `instance = "rel"`, 1), "instance:"},
		{strings.Replace(base, `["fw1"]`, `["fw 1"]`, 1), "workers:"},
		{base + "git_name = \"x'; rm -rf ~; '\"\n", "git_name:"},
		{base + "poll_seconds = 0\n", "poll_seconds:"},
		{base + "vitals_seconds = -1\n", "vitals_seconds:"},
		{base + "vitals_seconds = 7200\n", "vitals_seconds:"},
		{base + "vitals_seconds = \"60\"\n", "vitals_seconds:"},
		{base + "colour = \"blue\"\n", "colour: not a key"},
	} {
		if _, err := LoadConfig(writeConfig(t, tc.text), "/h"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.text, err, tc.want)
		}
	}
}
