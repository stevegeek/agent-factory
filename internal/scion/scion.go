// Package scion is Scion that changes state: init, start, stop and delete,
// the project bootstrap, the agent's home and the environment that carries
// the Claude token. It keeps calling the external scion binary with
// Python's argv.
//
// Every call guards the project's repository first (Python scion() runs
// guard_repo), through the Guard the caller passes: this package may not
// import gitx. The token is read from its file for the verbs that start or
// change an agent only; list and look run through scionread without it.
package scion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/scionread"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Client runs scion's write verbs. The zero Guard and Logf do nothing.
type Client struct {
	// Reader holds the binary, the execer and the base environment (the
	// safe git environment, no secrets); its List and Look are the read
	// verbs.
	scionread.Reader
	// TokenFile is paths.OAuthToken (~/.config/factory/claude-oauth-token).
	TokenFile string
	// RunDir is paths.Run: a failed start removes its --config file when
	// the file lies directly in it (fixed at the port).
	RunDir string
	// Guard checks a project's repository before scion or git runs in it
	// (gitx.Guard: scion runs git on the host too). An error stops the call.
	Guard func(ctx context.Context, dir string) error
	// Logf receives the one log line EnsureProject writes.
	Logf func(format string, args ...any)
}

// Env is scion_env() over the base: the token from TokenFile (stripped) as
// CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY removed (agents bill the
// subscription, not an API key), and scion's directory first on PATH.
func (c Client) Env() ([]string, error) {
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("scion env: %w", err)
	}
	env := scionread.ReadEnv(c.Reader.Env, c.Bin)
	return append(env, scionread.TokenVar+"="+textx.Strip(string(b))), nil
}

// ErrRefused is the repository guard refusing a project (a .git that is
// not a plain directory): scion is not run there at all.
var ErrRefused = errors.New("repository refused")

func (c Client) guard(ctx context.Context, dir string) error {
	if c.Guard == nil {
		return nil
	}
	if err := c.Guard(ctx, dir); err != nil {
		return fmt.Errorf("scion in %s: %w: %w", dir, ErrRefused, err)
	}
	return nil
}

// run is Python scion(project, *args): guard, then scion with the token
// environment in the project, 600 s.
func (c Client) run(ctx context.Context, dir string, args ...string) (execx.Result, error) {
	if err := c.guard(ctx, dir); err != nil {
		return execx.Result{}, err
	}
	env, err := c.Env()
	if err != nil {
		return execx.Result{}, err
	}
	argv := append([]string{c.Bin}, args...)
	res, err := c.Exec.Run(ctx, execx.Cmd{Argv: argv, Dir: dir, Env: env, Timeout: scionread.Timeout})
	if err != nil {
		return res, fmt.Errorf("scion %s: %w", args[0], err)
	}
	return res, nil
}

// checked is run with check=True.
func (c Client) checked(ctx context.Context, dir string, args ...string) error {
	res, err := c.run(ctx, dir, args...)
	if err != nil {
		return err
	}
	return execx.CheckErr(res)
}

// List is scionread's List after the repository guard.
func (c Client) List(ctx context.Context, dir string) ([]string, error) {
	if err := c.guard(ctx, dir); err != nil {
		return nil, err
	}
	return c.Reader.List(ctx, dir)
}

// Look is scionread's Look after the repository guard.
func (c Client) Look(ctx context.Context, dir, name string, lines int) (string, error) {
	if err := c.guard(ctx, dir); err != nil {
		return "", err
	}
	return c.Reader.Look(ctx, dir, name, lines)
}

// Init is `scion init` in dir, checked.
func (c Client) Init(ctx context.Context, dir string) error {
	return c.checked(ctx, dir, "init")
}

// StartArgs is one `scion start`.
type StartArgs struct {
	Name     string // the agent: t0042-w1
	Template string // [templates] name or model.Stages() default
	Config   string // an inline Scion config (run/<name>.yaml, the memory mount); "" for none
	Branch   string // -b, for branch stages only; ""
	Model    string // --model; "" leaves the template's
	Prompt   string // the last positional argument
}

// Argv is the argv after the scion binary:
// start <name> -t <template> --message-mode none [--config C] [-b B] [--model M] <prompt>.
// A prompt that starts with "-" goes after "--", so it is never read as an
// option; every other prompt gives Python's argv exactly.
func (a StartArgs) Argv() ([]string, error) {
	if err := scionread.CheckName(a.Name); err != nil {
		return nil, err
	}
	for _, v := range []struct{ what, val string }{{"template", a.Template}} {
		if v.val == "" || strings.HasPrefix(v.val, "-") {
			return nil, fmt.Errorf("scion start: bad %s %q", v.what, v.val)
		}
	}
	for _, v := range []struct{ what, val string }{{"config", a.Config}, {"branch", a.Branch}, {"model", a.Model}} {
		if strings.HasPrefix(v.val, "-") {
			return nil, fmt.Errorf("scion start: bad %s %q", v.what, v.val)
		}
	}
	args := []string{"start", a.Name, "-t", a.Template, "--message-mode", "none"}
	if a.Config != "" {
		args = append(args, "--config", a.Config)
	}
	if a.Branch != "" {
		args = append(args, "-b", a.Branch)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if strings.HasPrefix(a.Prompt, "-") {
		args = append(args, "--")
	}
	return append(args, a.Prompt), nil
}

// Start starts an agent in the project at dir, checked. When it fails, its
// --config file is removed if it lies in RunDir.
func (c Client) Start(ctx context.Context, dir string, a StartArgs) error {
	args, err := a.Argv()
	if err != nil {
		return err
	}
	if err := c.checked(ctx, dir, args...); err != nil {
		c.dropRunConfig(a.Config)
		return err
	}
	return nil
}

func (c Client) dropRunConfig(conf string) {
	if conf == "" || c.RunDir == "" || filepath.Dir(filepath.Clean(conf)) != filepath.Clean(c.RunDir) {
		return
	}
	os.Remove(conf) // best effort, as remove_agent's own removal
}

// Stop is `scion stop <name>`, unchecked (remove_agent): only a command
// that did not run is an error.
func (c Client) Stop(ctx context.Context, dir, name string) error {
	if err := scionread.CheckName(name); err != nil {
		return err
	}
	_, err := c.run(ctx, dir, "stop", name)
	return err
}

// Delete is `scion delete <name> [-b] -y`, unchecked. keepBranch adds -b,
// exactly as remove_agent(keep_branch=True) does: branch stages pass true,
// no-branch stages false.
func (c Client) Delete(ctx context.Context, dir, name string, keepBranch bool) error {
	if err := scionread.CheckName(name); err != nil {
		return err
	}
	args := []string{"delete", name}
	if keepBranch {
		args = append(args, "-b")
	}
	_, err := c.run(ctx, dir, append(args, "-y")...)
	return err
}

// AgentHome is the agent's home directory under ~/.scion/project-configs
// (agent_home): pattern is paths.AgentHomes, whose {name} stands for
// the agent. "" when there is none, or the name is not an agent name.
func AgentHome(pattern, name string) string {
	if scionread.CheckName(name) != nil {
		return ""
	}
	homes, err := filepath.Glob(strings.ReplaceAll(pattern, "{name}", name))
	if err != nil || len(homes) == 0 {
		return ""
	}
	return homes[0]
}

// ErrNoProject is a scion init that left no .scion/project-id.
var ErrNoProject = errors.New("scion init made no project")
