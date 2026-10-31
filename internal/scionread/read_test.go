package scionread

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
)

const bin = "/home/f/.local/bin/scion"

// fake records each command and answers with res.
type fake struct {
	calls []execx.Cmd
	res   execx.Result
	err   error
}

func (f *fake) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	f.calls = append(f.calls, c)
	r := f.res
	r.Argv = c.Argv
	return r, f.err
}

func reader(f *fake, env ...string) Reader { return Reader{Bin: bin, Exec: f, Env: env} }

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestListArgvAndNames(t *testing.T) {
	f := &fake{res: execx.Result{Stdout: []byte(`[{"name":"t0001-w1","phase":"running"},{"name":"t0002-r1"},{"name":"t0001-w1"}]`)}}
	names, err := reader(f).List(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"t0001-w1", "t0002-r1"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %q, want %q", names, want)
	}
	c := f.calls[0]
	if want := []string{bin, "list", "--format", "json"}; !reflect.DeepEqual(c.Argv, want) {
		t.Fatalf("argv %q", c.Argv)
	}
	if c.Dir != "/repo" || c.Timeout != Timeout {
		t.Fatalf("dir %q timeout %v", c.Dir, c.Timeout)
	}
}

func TestListLeavesOutAgentsWhoseContainerEnded(t *testing.T) {
	out := `[{"name":"a1","phase":"running","activity":"working"},{"name":"a2","phase":"stopped"},` +
		`{"name":"a3","phase":"running-container-down","activity":"offline"},{"name":"a4"},{"name":"a5","phase":"error"},` +
		`{"name":"a6","phase":7},{"name":"a7","phase":"running","activity":"completed"},{"name":"a8","phase":"suspended"}]`
	names, err := reader(&fake{res: execx.Result{Stdout: []byte(out)}}).List(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a1", "a4", "a6", "a7", "a8"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %q, want %q", names, want)
	}
}

// running_agents: a failed list is not an empty one.
func TestListFailureIsUnknownNotEmpty(t *testing.T) {
	for name, f := range map[string]*fake{
		"non-zero exit":       {res: execx.Result{Code: 1, Stderr: []byte("no daemon")}},
		"not JSON":            {res: execx.Result{Stdout: []byte("oops")}},
		"blank but not empty": {res: execx.Result{Stdout: []byte("\n")}},
		"null":                {res: execx.Result{Stdout: []byte("null")}},
		"object":              {res: execx.Result{Stdout: []byte(`{"name":"x"}`)}},
		"entry no name":       {res: execx.Result{Stdout: []byte(`[{"id":"x"}]`)}},
		"entry not object":    {res: execx.Result{Stdout: []byte(`["x"]`)}},
		"exec error":          {err: errors.New("no such file")},
	} {
		t.Run(name, func(t *testing.T) {
			names, err := reader(f).List(context.Background(), "/repo")
			if err == nil {
				t.Fatalf("got %q and no error", names)
			}
		})
	}
	names, err := reader(&fake{}).List(context.Background(), "/repo")
	if err != nil || len(names) != 0 {
		t.Fatalf("empty stdout means no agents (json.loads(\"\" or \"[]\")): %q %v", names, err)
	}
}

func TestLookArgv(t *testing.T) {
	f := &fake{res: execx.Result{Stdout: []byte("screen\n")}}
	out, err := reader(f).Look(context.Background(), "/repo", "t0001-w1", 150)
	if err != nil || out != "screen\n" {
		t.Fatalf("%q %v", out, err)
	}
	if want := []string{bin, "look", "t0001-w1", "--plain", "-n", "150"}; !reflect.DeepEqual(f.calls[0].Argv, want) {
		t.Fatalf("argv %q", f.calls[0].Argv)
	}
	f = &fake{res: execx.Result{Code: 1, Stderr: []byte("no agent")}}
	if _, err := reader(f).Look(context.Background(), "/repo", "t0001-w1", 10); err == nil {
		t.Fatal("a failed look is an error")
	}
}

func TestOptionLikeOrPathNamesAreRefused(t *testing.T) {
	for _, name := range []string{"", "-rf", "--plain", "a/b", "..", "a\nb", "a b", "*"} {
		f := &fake{}
		if _, err := reader(f).Look(context.Background(), "/repo", name, 10); err == nil {
			t.Errorf("look %q ran", name)
		}
		if len(f.calls) != 0 {
			t.Errorf("look %q execed", name)
		}
	}
}

// Security: read verbs run without the Claude token or
// an API key in their environment, whatever the caller's base holds; PATH
// gets scion's directory first, as scion_env() does.
func TestReadVerbsCarryNoSecrets(t *testing.T) {
	f := &fake{res: execx.Result{Stdout: []byte("[]")}}
	r := reader(f, "PATH=/usr/bin", "CLAUDE_CODE_OAUTH_TOKEN=secret", "ANTHROPIC_API_KEY=key", "GIT_CONFIG_NOSYSTEM=1")
	if _, err := r.List(context.Background(), "/repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Look(context.Background(), "/repo", "a", 10); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		env := envMap(c.Env)
		if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
			t.Errorf("%q has the token", c.Argv)
		}
		if _, ok := env["ANTHROPIC_API_KEY"]; ok {
			t.Errorf("%q has the API key", c.Argv)
		}
		if env["PATH"] != "/home/f/.local/bin:/usr/bin" || env["GIT_CONFIG_NOSYSTEM"] != "1" {
			t.Errorf("env %q", c.Env)
		}
	}
}

func TestEnvWithoutPathIsLikePython(t *testing.T) {
	// os.path.dirname(SCION) + ":" + env.get("PATH", "")
	if got := envMap(ReadEnv(nil, bin))["PATH"]; got != "/home/f/.local/bin:" {
		t.Fatalf("PATH %q", got)
	}
}

// The real OS execer and a fake scion binary that fails when it sees the
// token: Look and List work through execx.OS without it.
func TestLookWithoutTheTokenThroughARealProcess(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "scion")
	body := "#!/bin/sh\n" +
		"[ -n \"$CLAUDE_CODE_OAUTH_TOKEN\" ] && { echo token seen >&2; exit 9; }\n" +
		"case \"$1\" in list) echo '[{\"name\":\"t0001-w1\"}]';; look) echo \"screen of $2\";; esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	r := Reader{Bin: script, Exec: execx.OS{}, Env: []string{"PATH=/usr/bin:/bin", "CLAUDE_CODE_OAUTH_TOKEN=secret"}}
	out, err := r.Look(context.Background(), dir, "t0001-w1", 40)
	if err != nil || out != "screen of t0001-w1\n" {
		t.Fatalf("%q %v", out, err)
	}
	names, err := r.List(context.Background(), dir)
	if err != nil || !reflect.DeepEqual(names, []string{"t0001-w1"}) {
		t.Fatalf("%q %v", names, err)
	}
}
