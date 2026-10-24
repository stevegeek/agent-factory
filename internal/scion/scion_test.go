package scion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/scionread"
)

const bin = "/home/f/.local/bin/scion"

type recorder struct {
	calls  []execx.Cmd
	code   int
	err    error
	guards []string
}

func (r *recorder) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	r.calls = append(r.calls, c)
	return execx.Result{Argv: c.Argv, Code: r.code, Stderr: []byte("scion said no")}, r.err
}

func client(t *testing.T, r *recorder, base ...string) Client {
	t.Helper()
	token := filepath.Join(t.TempDir(), "claude-oauth-token")
	if err := os.WriteFile(token, []byte("  the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Client{
		Reader:    scionread.Reader{Bin: bin, Exec: r, Env: base},
		TokenFile: token,
		Guard: func(_ context.Context, dir string) error {
			r.guards = append(r.guards, dir)
			return nil
		},
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := m[k]; dup {
			m[k+"#dup"] = v
		}
		m[k] = v
	}
	return m
}

// §10.7 S7b (security): scion_env drops ANTHROPIC_API_KEY and reads the token
// file (stripped) into CLAUDE_CODE_OAUTH_TOKEN; PATH gets scion's directory.
func TestEnvDropsTheAPIKeyAndReadsTheToken(t *testing.T) {
	c := client(t, &recorder{}, "PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-x", "CLAUDE_CODE_OAUTH_TOKEN=stale", "GIT_CONFIG_COUNT=3")
	env, err := c.Env()
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(env)
	if _, ok := m["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY kept")
	}
	if m["CLAUDE_CODE_OAUTH_TOKEN"] != "the-token" {
		t.Errorf("token %q", m["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if m["PATH"] != "/home/f/.local/bin:/usr/bin" || m["GIT_CONFIG_COUNT"] != "3" {
		t.Errorf("env %q", env)
	}
	for k := range m {
		if strings.HasSuffix(k, "#dup") {
			t.Errorf("%s set twice", k)
		}
	}
}

func TestMissingTokenFileStopsAStart(t *testing.T) {
	r := &recorder{}
	c := client(t, r)
	c.TokenFile = filepath.Join(t.TempDir(), "absent")
	if err := c.Start(context.Background(), "/repo", StartArgs{Name: "t0001-w1", Template: "worker", Prompt: "p"}); err == nil {
		t.Fatal("started without a token")
	}
	if len(r.calls) != 0 {
		t.Fatalf("ran %q", r.calls[0].Argv)
	}
}

// §10.7 S7b: the full `scion start` argv (F:1792-1813), the prompt last, the
// token in the environment and never in argv.
func TestStartArgv(t *testing.T) {
	cases := []struct {
		name string
		in   StartArgs
		want []string
	}{
		{"triage: no branch, no model", StartArgs{Name: "t0001-t1", Template: "triage", Prompt: "Triage this."},
			[]string{"start", "t0001-t1", "-t", "triage", "--message-mode", "none", "Triage this."}},
		{"work: memory config, branch, model",
			StartArgs{Name: "t0001-w1", Template: "worker", Config: "/f/run/t0001-w1.yaml", Branch: "bot/fix-it", Model: "claude-opus-4", Prompt: "Task: fix it"},
			[]string{"start", "t0001-w1", "-t", "worker", "--message-mode", "none", "--config", "/f/run/t0001-w1.yaml", "-b", "bot/fix-it", "--model", "claude-opus-4", "Task: fix it"}},
		{"review: branch only", StartArgs{Name: "t0001-r1", Template: "reviewer", Branch: "bot/x", Prompt: "Review."},
			[]string{"start", "t0001-r1", "-t", "reviewer", "--message-mode", "none", "-b", "bot/x", "Review."}},
		{"a prompt that looks like an option is kept a positional", StartArgs{Name: "t0001-k1", Template: "task", Prompt: "--help me"},
			[]string{"start", "t0001-k1", "-t", "task", "--message-mode", "none", "--", "--help me"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{}
			c := client(t, r, "PATH=/usr/bin")
			if err := c.Start(context.Background(), "/repo", tc.in); err != nil {
				t.Fatal(err)
			}
			got := r.calls[0]
			if !reflect.DeepEqual(got.Argv, append([]string{bin}, tc.want...)) {
				t.Fatalf("argv\n got %q\nwant %q", got.Argv[1:], tc.want)
			}
			if got.Dir != "/repo" || got.Timeout != scionread.Timeout {
				t.Errorf("dir %q timeout %v", got.Dir, got.Timeout)
			}
			if envMap(got.Env)["CLAUDE_CODE_OAUTH_TOKEN"] != "the-token" {
				t.Error("start has no token")
			}
			if slices.ContainsFunc(got.Argv, func(a string) bool { return strings.Contains(a, "the-token") }) {
				t.Error("token in argv")
			}
			if !reflect.DeepEqual(r.guards, []string{"/repo"}) {
				t.Errorf("guards %q", r.guards)
			}
		})
	}
}

func TestStartRefusesOptionLikeValues(t *testing.T) {
	for _, in := range []StartArgs{
		{Name: "-x", Template: "worker", Prompt: "p"},
		{Name: "t1", Template: "-worker", Prompt: "p"},
		{Name: "t1", Template: "", Prompt: "p"},
		{Name: "t1", Template: "worker", Branch: "-b", Prompt: "p"},
		{Name: "t1", Template: "worker", Model: "--x", Prompt: "p"},
		{Name: "t1", Template: "worker", Config: "-c", Prompt: "p"},
	} {
		r := &recorder{}
		if err := client(t, r).Start(context.Background(), "/repo", in); err == nil || len(r.calls) != 0 {
			t.Errorf("%+v: err %v, calls %d", in, err, len(r.calls))
		}
	}
}

// Quirk 5, fixed at the port: a failed start removes its run/<name>.yaml.
func TestFailedStartRemovesItsRunConfig(t *testing.T) {
	runDir := t.TempDir()
	conf := filepath.Join(runDir, "t0001-w1.yaml")
	if err := os.WriteFile(conf, []byte("volumes: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &recorder{code: 1}
	c := client(t, r)
	c.RunDir = runDir
	err := c.Start(context.Background(), "/repo", StartArgs{Name: "t0001-w1", Template: "worker", Config: conf, Prompt: "p"})
	var ee *execx.ExitError
	if !errors.As(err, &ee) || !strings.Contains(err.Error(), "scion said no") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Fatalf("run config kept after a failed start: %v", err)
	}
	// a config outside the run directory is never removed
	other := filepath.Join(t.TempDir(), "x.yaml")
	os.WriteFile(other, nil, 0o644)
	c.Start(context.Background(), "/repo", StartArgs{Name: "t0001-w1", Template: "worker", Config: other, Prompt: "p"})
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("removed a config outside run/: %v", err)
	}
}

func TestSuccessfulStartKeepsItsRunConfig(t *testing.T) {
	runDir := t.TempDir()
	conf := filepath.Join(runDir, "t0001-w1.yaml")
	os.WriteFile(conf, nil, 0o644)
	c := client(t, &recorder{})
	c.RunDir = runDir
	if err := c.Start(context.Background(), "/repo", StartArgs{Name: "t0001-w1", Template: "worker", Config: conf, Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(conf); err != nil {
		t.Fatal(err)
	}
}

// remove_agent's tail (F:737): stop, then delete with -b when the branch is
// kept; both unchecked.
func TestStopAndDeleteArgv(t *testing.T) {
	r := &recorder{code: 1}
	c := client(t, r)
	ctx := context.Background()
	if err := c.Stop(ctx, "/repo", "t0001-w1"); err != nil {
		t.Fatalf("a failing stop is not an error (check=False): %v", err)
	}
	if err := c.Delete(ctx, "/repo", "t0001-w1", true); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "/repo", "t0001-t1", false); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{bin, "stop", "t0001-w1"},
		{bin, "delete", "t0001-w1", "-b", "-y"},
		{bin, "delete", "t0001-t1", "-y"},
	}
	for i, w := range want {
		if !reflect.DeepEqual(r.calls[i].Argv, w) {
			t.Errorf("call %d: %q, want %q", i, r.calls[i].Argv, w)
		}
	}
	if err := c.Stop(ctx, "/repo", "-x"); err == nil {
		t.Error("stop of an option-like name ran")
	}
}

func TestExecErrorsArePassedOn(t *testing.T) {
	r := &recorder{err: execx.ErrTimeout}
	c := client(t, r)
	if err := c.Stop(context.Background(), "/repo", "a"); !errors.Is(err, execx.ErrTimeout) {
		t.Fatalf("stop: %v", err)
	}
}

func TestInitArgv(t *testing.T) {
	r := &recorder{}
	if err := client(t, r).Init(context.Background(), "/repo"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.calls[0].Argv, []string{bin, "init"}) || r.calls[0].Dir != "/repo" {
		t.Fatalf("%q in %q", r.calls[0].Argv, r.calls[0].Dir)
	}
}

// List and Look through the Client guard the repository first and, like
// scionread, carry no token.
func TestClientListAndLookGuardAndCarryNoToken(t *testing.T) {
	r := &recorder{}
	c := client(t, r, "PATH=/usr/bin", "CLAUDE_CODE_OAUTH_TOKEN=x")
	ctx := context.Background()
	if _, err := c.List(ctx, "/repo"); err != nil {
		t.Fatal(err)
	}
	c.Look(ctx, "/repo", "t0001-w1", 40)
	if !reflect.DeepEqual(r.guards, []string{"/repo", "/repo"}) {
		t.Fatalf("guards %q", r.guards)
	}
	for _, call := range r.calls {
		if _, ok := envMap(call.Env)["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
			t.Errorf("%q carries the token", call.Argv)
		}
	}
}

func TestAgentHome(t *testing.T) {
	p := paths.FromEnv(map[string]string{"HOME": t.TempDir(), "FACTORY_ROOT": t.TempDir()})
	home := strings.ReplaceAll(p.AgentHomes, "{name}", "t0001-w1")
	home = strings.Replace(home, "*", "abc123", 1)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := AgentHome(p.AgentHomes, "t0001-w1"); got != home {
		t.Fatalf("got %q, want %q", got, home)
	}
	if got := AgentHome(p.AgentHomes, "t0002-w1"); got != "" {
		t.Fatalf("missing agent: %q", got)
	}
	for _, bad := range []string{"*", "t0001-w?", "../x", ""} {
		if got := AgentHome(p.AgentHomes, bad); got != "" {
			t.Errorf("%q matched %q", bad, got)
		}
	}
}
