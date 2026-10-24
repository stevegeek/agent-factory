package cli

import (
	"context"
	"path/filepath"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/doors"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobtail"
	"github.com/stevegeek/agent-factory/internal/memory"
	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/operator"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/pipeline"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/prflow"
	"github.com/stevegeek/agent-factory/internal/scion"
	"github.com/stevegeek/agent-factory/internal/scionread"
	"github.com/stevegeek/agent-factory/internal/sshjobs"
)

// The tick: `factory tick`, run by factory-tick.timer
// and by wake. It prints nothing on stdout; what it did is in the log.
// The command table (commands.go) names its runner:
//
//	tick: runTick

// runTick is `factory tick`.
func runTick(ctx context.Context, e *Env, _ Args) error {
	cfg, err := e.Config() // main() loads it first, as Python's did
	if err != nil {
		return err
	}
	if err := e.Git(ctx); err != nil {
		return err // an old git is refused before the tick runs anything
	}
	err = pipeline.Tick(ctx, tickDeps(e, cfg))
	recordSysinfo(ctx, e, cfg) // for Foreman's Settings page; never fails the tick (cli/sysinfo.go)
	return err
}

// tickDeps wires the tick for one invocation. One guard serves every git
// and scion call of the process (its stat cache spares an unchanged config
// a second listing); git and scion run with the safe git environment over
// the process's, scion's write verbs with the Claude token as well.
func tickDeps(e *Env, cfg *config.Config) pipeline.Deps {
	p := e.Paths()
	log := e.Log()
	env := doorsEnviron(e.Environ)
	src := e.ConfigSource()
	guard := gitx.NewGuarder(e.Exec, env, log)
	pm := podman.Client{Exec: e.Exec, Dir: p.Home, Env: podman.PassEnv(env)}
	sc := scion.Client{
		Reader:    scionread.Reader{Bin: p.Scion, Exec: e.Exec, Env: gitread.SafeEnv(env)},
		TokenFile: p.OAuthToken,
		RunDir:    p.Run,
		Guard: func(ctx context.Context, dir string) error {
			return guard.Check(ctx, dir, tickGitHubOf(src, dir))
		},
		Logf: log.Printf,
	}
	local := &hostrun.Local{Paths: p, Scion: sc, Clock: e.Clock, Logf: log.Printf}
	runner := gate.Runner{Paths: p, Exec: e.Exec, Env: env, Guard: guard, Podman: pm, Log: log, Clock: e.Clock,
		Agents: func(ctx context.Context, cfg *config.Config) (int, error) {
			running, err := local.Running(ctx, cfg)
			return len(running), err
		}}
	mem := &memory.Service{Store: &memstore.Store{Paths: p, Exec: e.Exec}, Paths: p, Clock: e.Clock}
	board := e.Board()
	deploys := deployService(e, cfg)
	gh := opsCLI{}.githubClient(e, cfg)
	return pipeline.Deps{
		Board:  board,
		Config: src,
		Paths:  p,
		Clock:  e.Clock,
		Log:    log,
		Local:  local,
		Screen: local,
		Helpers: pipeline.JobHosts{Paths: p, SSH: sshjobs.Client{Exec: e.Exec}, Read: jobtail.Reader{Exec: e.Exec},
			Clock: e.Clock},
		Git:    runner,
		Memory: mem,
		Gate:   runner,
		GitHub: &prflow.Flow{
			GitHub:  gh,
			Git:     prflow.Repos{Repo: runner.Repo},
			Checks:  runner,
			Board:   board,
			LogsDir: p.Logs,
			Clock:   e.Clock,
			Log:     log,
			Deploys: selfDeploys{Config: cfg, Deploy: deploys, Asks: deployAsks{e.doors()}, Notices: deployNotices{e.doors()}},
		},
		Doors:   doors.Service{Board: board, Exec: e.Exec, Log: log},
		Issues:  doorsGitHub(e),
		Deploys: deploys,
		// the operator: the tick alone touches git, GitHub, systemd and the
		// pause file for the manager's items; it pushes only bot/* (Writer.Push)
		Operator: operator.Service{
			Paths:    p,
			Board:    board,
			GitHub:   gh,
			Token:    gh.Auth.Token,
			Repos:    runner.Repo,
			Exec:     e.Exec,
			Deploys:  operatorDeploys{Deploy: deploys, Asks: deployAsks{e.doors()}},
			Switches: &ops.Service{Board: board, Config: loadedConfig{cfg}, Clock: e.Clock, Log: log},
			Clock:    e.Clock,
			Log:      log,
			Env:      e.Environ,
		},
	}
}

// tickGitHubOf is the GitHub repository of the project checked out at dir,
// so the guard Scion's calls go through keeps origin only when it points
// there, as Python's scion(project) guarded the project itself.
func tickGitHubOf(src *config.Source, dir string) string {
	cfg, err := src.Current()
	if err != nil {
		return ""
	}
	for _, p := range cfg.Projects {
		if p.Path != "" && filepath.Clean(p.Path) == filepath.Clean(dir) {
			return p.GitHub
		}
	}
	return ""
}
