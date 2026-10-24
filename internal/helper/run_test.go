package helper

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

func (r *rig) job(t *testing.T, job, meta string) string {
	t.Helper()
	d := r.jobDir(job)
	for _, sub := range []string{"work", "home"} {
		if err := os.MkdirAll(filepath.Join(d, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.write(t, "jobs/"+job+"/meta.json", meta)
	return d
}

func (r *rig) key(t *testing.T, key string) {
	t.Helper()
	p := filepath.Join(r.Home, ".config", "factory", "anthropic-api-key")
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The job's container: the caps, the user mapping, the helper's own image,
// memory, mounts and env; the caller picks only the branch, model and
// timeout; the key travels in podman's environment, never in argv.
func TestRunJobPodmanArgv(t *testing.T) {
	r := newRig(t)
	r.write(t, "config.toml", `
[identity]
git_name = "Factory Bot"
git_email = "bot@example.com"

[remote]
image = "localhost/factory-pi:test"
memory = "600m"
mounts = ["{root}/cache/bundle:/home/scion/.bundle-cache:ro"]
env = { BUNDLE_PATH = "/home/scion/.bundle-cache", GIT_AUTHOR_NAME = "Override", JOB_BRANCH = "ignored", N = 3, B = true }
`)
	d := r.job(t, "t0001-w1", goodMeta)
	r.key(t, "sk-test-key\n")
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Code: 7, Stdout: []byte("cloned\n"), Stderr: []byte("warning\n")}, nil
	}
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	calls := r.exec.calls
	if len(calls) != 1 {
		t.Fatalf("ran %v", r.exec.argvs())
	}
	want := []string{"podman", "run", "--rm", "--name", "factory-job-t0001-w1", "--timeout", "1800",
		"--memory", "600m", "--cpus", "2", "--pids-limit", "512", "--cap-drop=all",
		"--security-opt=no-new-privileges", "--userns=keep-id:uid=1000,gid=1000", "--user", "1000:1000",
		"-e", "ANTHROPIC_API_KEY",
		"-e", "GIT_AUTHOR_NAME=Override", "-e", "GIT_COMMITTER_NAME=Factory Bot",
		"-e", "GIT_AUTHOR_EMAIL=bot@example.com", "-e", "GIT_COMMITTER_EMAIL=bot@example.com",
		"-e", "BUNDLE_PATH=/home/scion/.bundle-cache", "-e", "JOB_BRANCH=bot/t0001", "-e", "N=3", "-e", "B=True",
		"-e", "JOB_START=main", "-e", "JOB_MODEL=claude-sonnet-5-5", "-e", "JOB_BUNDLE=1", "-e", "HOME=/home/scion",
		"-v", d + "/work:/job", "-v", d + "/home:/home/scion",
		"-v", r.Root + "/cache/bundle:/home/scion/.bundle-cache:ro",
		"-w", "/job", "--entrypoint", "/bin/sh", "localhost/factory-pi:test", "-c", jobScript,
	}
	c := calls[0]
	if !slices.Equal(c.Argv, want) {
		t.Errorf("podman argv\n got %q\nwant %q", c.Argv, want)
	}
	if c.Dir != "/" {
		t.Errorf("podman runs in %q, want /", c.Dir)
	}
	if !slices.Contains(c.Env, "ANTHROPIC_API_KEY=sk-test-key") {
		t.Errorf("the key is not in podman's environment: %v", c.Env)
	}
	if slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, "SECRET_TOKEN=") }) {
		t.Errorf("podman's environment is not the minimal one: %v", c.Env)
	}
	for _, a := range c.Argv {
		if strings.Contains(a, "sk-test-key") {
			t.Errorf("the key is in argv: %q", a)
		}
	}
	if c.Cleanup == nil {
		t.Error("no cleanup for a cancelled run")
	}
	if got := readFile(t, filepath.Join(d, "exit")); got != "7" {
		t.Errorf("exit = %q, want 7", got)
	}
	if got := readFile(t, filepath.Join(d, "run.log")); got != "cloned\nwarning\n" {
		t.Errorf("run.log = %q", got)
	}
}

func TestRunJobDefaults(t *testing.T) {
	r := newRig(t)
	d := r.job(t, "t0001-r1", `{"branch": "review", "start": "bot/t0001", "timeout_minutes": "7"}`)
	r.key(t, "k")
	if code := r.argv(t, "run", "t0001-r1"); code != 0 {
		t.Fatalf("run: %d %s", code, r.stderr)
	}
	a := r.exec.calls[0].Argv
	for _, pair := range [][2]string{{"--memory", "800m"}, {"--timeout", "420"}} {
		i := slices.Index(a, pair[0])
		if i < 0 || a[i+1] != pair[1] {
			t.Errorf("%s: want %s in %q", pair[0], pair[1], a)
		}
	}
	if a[len(a)-3] != "localhost/factory-pi:dev" {
		t.Errorf("image %q", a[len(a)-3])
	}
	if slices.ContainsFunc(a, func(s string) bool { return strings.HasPrefix(s, "GIT_AUTHOR_NAME=") }) || !slices.Contains(a, "JOB_BUNDLE=0") || !slices.Contains(a, "JOB_MODEL=claude-sonnet-5-5") {
		t.Errorf("env %q", a)
	}
	if got := readFile(t, filepath.Join(d, "exit")); got != "0" {
		t.Errorf("exit = %q", got)
	}
}

// The exit file is always written, outside the agent's dirs: 255 when the
// run failed before podman gave a status.
func TestRunJobAlwaysWritesExit(t *testing.T) {
	r := newRig(t)
	d := r.job(t, "t0001-w1", goodMeta)
	if code := r.argv(t, "run", "t0001-w1"); code == 0 {
		t.Error("a run with no key file succeeded")
	}
	if r.exec.ran("podman") {
		t.Error("podman ran without a key")
	}
	if got := readFile(t, filepath.Join(d, "exit")); got != "255" {
		t.Errorf("exit = %q, want 255", got)
	}

	r.reset()
	os.Remove(filepath.Join(d, "exit"))
	r.key(t, "k")
	r.exec.answer = func(execx.Cmd) (execx.Result, error) { return execx.Result{}, errors.New("podman: not found") }
	if code := r.argv(t, "run", "t0001-w1"); code == 0 {
		t.Error("a run whose podman could not start succeeded")
	}
	if got := readFile(t, filepath.Join(d, "exit")); got != "255" {
		t.Errorf("exit = %q, want 255", got)
	}

	r.reset()
	os.Remove(filepath.Join(d, "exit"))
	r.write(t, "jobs/t0001-w1/meta.json", `{"branch": "../x"}`)
	if code := r.argv(t, "run", "t0001-w1"); code == 0 || r.exec.ran("podman") {
		t.Errorf("a run with a bad meta: %d", code)
	}
	if got := readFile(t, filepath.Join(d, "exit")); got != "255" {
		t.Errorf("exit = %q, want 255", got)
	}
}

// A cancelled run (systemd stopping the unit) removes the container.
func TestRunJobCleanupRemovesTheContainer(t *testing.T) {
	r := newRig(t)
	r.job(t, "t0001-w1", goodMeta)
	r.key(t, "k")
	r.argv(t, "run", "t0001-w1")
	cleanup := r.exec.calls[0].Cleanup
	r.reset()
	cleanup()
	want := []string{"podman", "rm", "-f", "--time", "5", "factory-job-t0001-w1"}
	if got := r.exec.argvs(); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("cleanup ran %v, want %v", got, want)
	}
}

// The job script is Python's, byte for byte (the container contract), but
// for one deliberate change: Pi reads the prompt on stdin, not as its
// last argument. It is frozen in testdata/py/job-script.sh, copied from
// the Python factory-remote's JOB_SCRIPT before the cutover deleted it.
func TestJobScriptMatchesPython(t *testing.T) {
	frozen, err := os.ReadFile(filepath.Join("testdata", "py", "job-script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(frozen) != jobScript {
		t.Errorf("job script differs from Python's (testdata/py/job-script.sh):\n%s", jobScript)
	}
	if strings.Contains(jobScript, "$(cat /job/prompt.md)") || !strings.Contains(jobScript, "/job/role.md \\\n   < /job/prompt.md > /job/agent.log") {
		t.Error("pi does not read the prompt on stdin")
	}
	if h, ok := harnessFor("pi"); !ok || h.Script != jobScript {
		t.Error("the pi harness does not run the job script")
	}
	if _, ok := harnessFor("codex"); ok {
		t.Error("an unknown harness was found")
	}
}

func (r *rig) token(t *testing.T, token string) {
	t.Helper()
	p := filepath.Join(r.Home, ".config", "factory", "claude-oauth-token")
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
}

const claudeHostConfig = `
[remote]
image = "localhost/factory-pi:test"
env = { A = "1", ANTHROPIC_API_KEY = "from-env", CLAUDE_CODE_OAUTH_TOKEN = "from-env" }

[remote.helperb]
agent = "claude"
image = "localhost/factory-agent:scion"
memory = "3g"
cpus = 3
`

// A helper whose [remote.<name>] says agent = "claude" runs Claude Code
// in its image with the subscription token, the only credential: the API
// key stays out, and [remote] env cannot set either.
func TestRunClaudeJob(t *testing.T) {
	r := newRig(t)
	r.Host = "helperb"
	r.write(t, "config.toml", claudeHostConfig)
	d := r.job(t, "t0001-w1", goodMeta)
	r.key(t, "sk-api-key\n")
	r.token(t, "sk-ant-oat-token\n")
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	c := r.exec.calls[0]
	want := []string{"podman", "run", "--rm", "--name", "factory-job-t0001-w1", "--timeout", "1800",
		"--memory", "3g", "--cpus", "3", "--pids-limit", "512", "--cap-drop=all",
		"--security-opt=no-new-privileges", "--userns=keep-id:uid=1000,gid=1000", "--user", "1000:1000",
		"-e", "CLAUDE_CODE_OAUTH_TOKEN", "-e", "A=1",
		"-e", "JOB_BRANCH=bot/t0001", "-e", "JOB_START=main", "-e", "JOB_MODEL=claude-sonnet-5-5", "-e", "JOB_BUNDLE=1", "-e", "HOME=/home/scion",
		"-v", d + "/work:/job", "-v", d + "/home:/home/scion",
		"-w", "/job", "--entrypoint", "/bin/sh", "localhost/factory-agent:scion", "-c", claudeScript,
	}
	if !slices.Equal(c.Argv, want) {
		t.Errorf("podman argv\n got %q\nwant %q", c.Argv, want)
	}
	if !slices.Contains(c.Env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-token") {
		t.Errorf("the token is not in podman's environment: %v", c.Env)
	}
	for _, kv := range append(slices.Clone(c.Env), c.Argv...) {
		if strings.Contains(kv, "ANTHROPIC_API_KEY") || strings.Contains(kv, "sk-api-key") || strings.Contains(kv, "from-env") {
			t.Errorf("another credential reaches the job: %q", kv)
		}
	}
	for _, a := range c.Argv {
		if strings.Contains(a, "sk-ant-oat-token") {
			t.Errorf("the token is in argv: %q", a)
		}
	}
}

// auth = "api-key" gives Claude Code the API key instead, and only it.
func TestRunClaudeJobWithAPIKey(t *testing.T) {
	r := newRig(t)
	r.Host = "helperb"
	r.write(t, "config.toml", claudeHostConfig+"auth = \"api-key\"\n")
	r.job(t, "t0001-w1", goodMeta)
	r.key(t, "sk-api-key\n")
	r.token(t, "sk-ant-oat-token\n")
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: exit %d: %s", code, r.stderr)
	}
	c := r.exec.calls[0]
	if i := slices.Index(c.Argv, "--user"); c.Argv[i+3] != "ANTHROPIC_API_KEY" || c.Argv[len(c.Argv)-1] != claudeScript {
		t.Errorf("argv %q", c.Argv)
	}
	if !slices.Contains(c.Env, "ANTHROPIC_API_KEY=sk-api-key") {
		t.Errorf("the key is not in podman's environment: %v", c.Env)
	}
	for _, kv := range append(slices.Clone(c.Env), c.Argv...) {
		if strings.Contains(kv, "CLAUDE_CODE_OAUTH_TOKEN") || strings.Contains(kv, "sk-ant-oat-token") {
			t.Errorf("the token reaches an api-key job: %q", kv)
		}
	}
}

// The chosen credential's file missing or empty fails the run before
// podman, naming the file; the other credential is never a fallback.
func TestRunClaudeJobNeedsItsCredential(t *testing.T) {
	for _, tc := range []struct{ name, extra, token string }{
		{"no token", "", ""},
		{"empty token", "", " \n"},
		{"no key", "auth = \"api-key\"\n", "sk-ant-oat-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.Host = "helperb"
			r.write(t, "config.toml", claudeHostConfig+tc.extra)
			d := r.job(t, "t0001-w1", goodMeta)
			if tc.extra == "" {
				r.key(t, "sk-api-key")
			}
			if tc.token != "" {
				r.token(t, tc.token)
			}
			if code := r.argv(t, "run", "t0001-w1"); code == 0 || r.exec.ran("podman") {
				t.Errorf("ran without its credential: %d", code)
			}
			file := "claude-oauth-token"
			if tc.extra != "" {
				file = "anthropic-api-key"
			}
			if !strings.Contains(r.stderr.String(), file) {
				t.Errorf("stderr %q does not name %s", r.stderr, file)
			}
			if got := readFile(t, filepath.Join(d, "exit")); got != "255" {
				t.Errorf("exit = %q, want 255", got)
			}
		})
	}
}

// The claude script is the job script with Claude Code in Pi's place: the
// same checkout before and the same head, bundle and memory after.
func TestClaudeScript(t *testing.T) {
	if h, ok := harnessFor("claude"); !ok || h.Script != claudeScript {
		t.Fatal("no claude harness")
	}
	if !strings.HasPrefix(claudeScript, jobSetup) || !strings.HasSuffix(claudeScript, jobFinish) ||
		!strings.HasPrefix(jobScript, jobSetup) || !strings.HasSuffix(jobScript, jobFinish) {
		t.Error("the scripts do not share the checkout and the finish")
	}
	for _, s := range []string{`claude -p --model "$JOB_MODEL" --dangerously-skip-permissions`,
		`--append-system-prompt="$(cat /job/role.md)" < /job/prompt.md >`, `echo "claude exited $?" >> /job/agent.log`} {
		if !strings.Contains(claudeScript, s) {
			t.Errorf("claude script has no %q", s)
		}
	}
	if strings.Contains(claudeScript, "agent-rules") || strings.Contains(claudeScript, "pi ") || strings.Contains(claudeScript, "$(cat /job/prompt.md)") {
		t.Error("the claude script runs Pi's parts")
	}
}

// The host's agent picks the harness: meta.json may name only that one.
func TestHarnessIsTheHosts(t *testing.T) {
	r := newRig(t)
	r.Host = "helperb"
	r.write(t, "config.toml", claudeHostConfig)
	if code := r.start(t, "t0001-w1", jobInput(t, `{"branch": "x", "start": "main", "harness": "claude"}`)); code != 0 {
		t.Errorf("claude on a claude host: %d %s", code, r.stderr)
	}
	r.Host = "hostx"
	r.reset()
	if code := r.start(t, "t0001-w2", jobInput(t, `{"branch": "x", "start": "main", "harness": "claude"}`)); code != 2 ||
		r.stderr.String() != "no harness \"claude\" on this host\n" {
		t.Errorf("claude on a pi host: %d %q", code, r.stderr)
	}
	if onDisk(r.jobDir("t0001-w2")) || r.exec.ran("systemd-run") {
		t.Error("a refused harness left a job")
	}
}

// [remote] deps = true installs the checkout's dependencies before the
// agent, with the pull host's deps function; a failed install runs no
// agent and leaves a blocked report. Off, the script is unchanged.
func TestRunJobWithDeps(t *testing.T) {
	for _, agent := range []string{"pi", "claude"} {
		r := newRig(t)
		r.Host = "h"
		r.write(t, "config.toml", `
[remote]
mounts = ["{root}/cache/bundle:/home/scion/.bundle-cache:ro"]
[remote.h]
agent = "`+agent+`"
image = "localhost/factory-agent:scion"
mounts = ["{root}/cache/bundle:/home/scion/.bundle-cache"]
deps = true
auth = "api-key"
`)
		r.job(t, "t0001-w1", goodMeta)
		r.key(t, "k")
		if code := r.argv(t, "run", "t0001-w1"); code != 0 {
			t.Fatalf("%s: run: %d %s", agent, code, r.stderr)
		}
		a := r.exec.calls[0].Argv
		h, _ := harnessFor(agent)
		script := a[len(a)-1]
		if script != withDeps(h.Run) || script == h.Script {
			t.Errorf("%s: the script is not the deps one:\n%s", agent, script)
		}
		if !slices.Contains(a, r.Root+"/cache/bundle:/home/scion/.bundle-cache") {
			t.Errorf("%s: the cache is not mounted writable: %q", agent, a)
		}
		for _, want := range []string{"JOB=/job\n" + jobproto.DepsFunc + "\n" + depsStep, "if deps_step; then\n" + h.Run + "else\n",
			`'` + depsFailed + `' > /home/scion/report.json`} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: no %q in\n%s", agent, want, script)
			}
		}
		if !strings.HasPrefix(script, jobSetup) || !strings.HasSuffix(script, jobFinish) {
			t.Errorf("%s: the deps script does not keep the checkout and the finish", agent)
		}
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v %s", agent, err, out)
		}
	}
	var report map[string]string
	if err := json.Unmarshal([]byte(depsFailed), &report); err != nil || report["status"] != "blocked" {
		t.Errorf("depsFailed %v %v", report, err)
	}
}

// The deps script, run for real against a fake bundle that records its
// environment: an install that fails leaves the blocked report and the
// log's end, and no agent ran; the install never sees a credential, the
// agent still does; a lock another job holds past the wait is a failure.
func TestDepsScriptRuns(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	_, hasFlock := exec.LookPath("flock")
	for _, tc := range []struct {
		name         string
		ok, held     bool
		cache, agent bool // BUNDLE_PATH set; the agent ran
	}{
		{"install ok", true, false, true, true},
		{"install fails", false, false, true, false},
		{"no cache", true, false, false, true},
		{"lock held", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.held && hasFlock != nil {
				t.Skip("no flock")
			}
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			cache := filepath.Join(dir, "cache")
			for _, d := range []string{bin, cache} {
				os.MkdirAll(d, 0o755)
			}
			code := "1"
			if tc.ok {
				code = "0"
			}
			job := filepath.Join(dir, "job")
			os.WriteFile(filepath.Join(bin, "bundle"), []byte("#!/bin/sh\nenv > "+job+"/bundle.env\n[ \"$1\" = check ] && exit 1\necho installing\nexit "+code+"\n"), 0o755)
			os.WriteFile(filepath.Join(dir, "Gemfile"), nil, 0o644)
			home := filepath.Join(dir, "home")
			os.MkdirAll(job, 0o755)
			os.MkdirAll(home, 0o755)
			// the deps step alone, with /job and /home/scion pointed at the scratch dirs
			step := strings.NewReplacer("/job", job, "/home/scion", home, "-w 900", "-w 1").Replace(
				strings.TrimSuffix(strings.TrimPrefix(withDeps("env > /job/agent.env\necho agent ran > /job/agent.log\n"), jobSetup), jobFinish))
			cmd := exec.Command("sh", "-c", "set -u\n"+step)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "ANTHROPIC_API_KEY=sk-key", "CLAUDE_CODE_OAUTH_TOKEN=sk-token"}
			if tc.cache {
				cmd.Env = append(cmd.Env, "BUNDLE_PATH="+cache)
			}
			if tc.held {
				f, err := os.OpenFile(filepath.Join(cache, ".factory-deps.lock"), os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			log := readFile(t, filepath.Join(job, "agent.log"))
			_, reportErr := os.Stat(filepath.Join(home, "report.json"))
			if tc.agent && (log != "agent ran\n" || reportErr == nil) {
				t.Errorf("agent.log %q, report %v", log, reportErr)
			}
			if !tc.agent && (strings.Contains(log, "agent ran") || reportErr != nil) {
				t.Errorf("agent.log %q, report %v", log, reportErr)
			}
			if !tc.ok && !strings.Contains(log, "installing") {
				t.Errorf("agent.log has not the install's end: %q", log)
			}
			if tc.held && !strings.Contains(log, "waited 900 s for another job's install") {
				t.Errorf("agent.log does not say it waited: %q", log)
			}
			if !tc.held {
				if env := readFile(t, filepath.Join(job, "bundle.env")); strings.Contains(env, "sk-") {
					t.Errorf("the install saw a credential:\n%s", env)
				}
				if tc.cache && hasFlock == nil && !onDisk(filepath.Join(cache, ".factory-deps.lock")) {
					t.Error("the install took no lock")
				}
			}
			if tc.agent {
				env := readFile(t, filepath.Join(job, "agent.env"))
				if !strings.Contains(env, "ANTHROPIC_API_KEY=sk-key") || !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-token") {
					t.Errorf("the agent lost its credential:\n%s", env)
				}
			}
		})
	}
}

// After the agent exits, the job's home is read for the Claude Code
// transcript, and its usage is left in work/usage.json for collect; a
// usage.json the agent planted there is never what goes back.
func TestRunJobRecordsUsage(t *testing.T) {
	r := newRig(t)
	d := r.job(t, "t0001-w1", goodMeta)
	r.key(t, "k")
	r.write(t, "jobs/t0001-w1/home/.claude/projects/-job-repo/s.jsonl",
		`{"type":"assistant","message":{"id":"m1","model":"claude-opus","usage":{"input_tokens":3,"output_tokens":4,"cache_creation_input_tokens":5,"cache_read_input_tokens":6}}}`+"\n")
	r.write(t, "jobs/t0001-w1/work/usage.json", `{"models": {"forged": {"input": 1}}}`)
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: %d %s", code, r.stderr)
	}
	got := readFile(t, filepath.Join(d, "work", "usage.json"))
	var u struct {
		Models map[string]map[string]int64 `json:"models"`
	}
	if err := json.Unmarshal([]byte(got), &u); err != nil {
		t.Fatal(err)
	}
	if m := u.Models["claude-opus"]; len(u.Models) != 1 || m["input"] != 3 || m["output"] != 4 || m["cache_write"] != 5 || m["cache_read"] != 6 || m["messages"] != 1 {
		t.Errorf("usage.json %s", got)
	}

	// no transcript (a Pi job): no usage.json, the planted one removed
	r.reset()
	os.RemoveAll(filepath.Join(d, "home", ".claude"))
	os.Remove(filepath.Join(d, "exit"))
	r.write(t, "jobs/t0001-w1/work/usage.json", `{"models": {"forged": {"input": 1}}}`)
	if code := r.argv(t, "run", "t0001-w1"); code != 0 {
		t.Fatalf("run: %d %s", code, r.stderr)
	}
	if _, err := os.Lstat(filepath.Join(d, "work", "usage.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("usage.json after a run with no transcript: %v", err)
	}

	// a home the agent made unreadable: still no usage.json, the planted
	// one removed, and the run still ends with its exit file
	if os.Getuid() == 0 {
		return // root reads a mode-000 directory all the same
	}
	r.reset()
	os.Remove(filepath.Join(d, "exit"))
	r.write(t, "jobs/t0001-w1/work/usage.json", `{"models": {"forged": {"input": 1}}}`)
	if err := os.Chmod(filepath.Join(d, "home"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(d, "home"), 0o755) })
	r.argv(t, "run", "t0001-w1")
	if _, err := os.Lstat(filepath.Join(d, "work", "usage.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("usage.json after a run with an unreadable home: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(d, "exit")); err != nil {
		t.Errorf("no exit file: %v", err)
	}
}
