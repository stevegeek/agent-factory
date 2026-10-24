package cli

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/doors"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/memory"
	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/upkeep"
)

// The doors and upkeep group (§7.3, slice S16): `sweep` and `clock` (the
// weekly and daily timers), `rebuild` (the rebuild unit) and `versions`.
// None prints anything on stdout; what they did is in the log. The
// command table (commands.go) names their runners:
//
//	sweep: runSweep, clock: runClock, rebuild: runRebuild, versions: runVersions

// runSweep is `factory sweep`: tickets for the open GitHub issues.
func runSweep(ctx context.Context, e *Env, _ Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	return doorsService(e).Sweep(ctx, cfg)
}

// runClock is `factory clock`: the schedules that are due, the monthly
// memory report and the weekly version check.
func runClock(ctx context.Context, e *Env, _ Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	return doorsService(e).Clock(ctx, cfg)
}

// runVersions is `factory versions`: a notice for each pin a newer release
// has passed.
func runVersions(ctx context.Context, e *Env, _ Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	return doorsUpkeep(e).Versions(ctx, cfg)
}

// runRebuild is `factory rebuild`: the weekly image rebuild, under the
// tick lock; with --rollback KIND or --unhold KIND, the §15.1 controls;
// with --latest, the latest-toolchain trial alone, now.
func runRebuild(ctx context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	up := doorsUpkeep(e)
	given := 0
	for _, set := range []bool{a.Str("rollback") != "", a.Str("unhold") != "", a.Bool("latest")} {
		if set {
			given++
		}
	}
	switch {
	case given > 1:
		return Usage("--rollback, --unhold and --latest are one at a time")
	case a.Str("rollback") != "":
		ts, err := up.Rollback(ctx, cfg, a.Str("rollback"))
		if err != nil {
			return err
		}
		if ts.Held == "" {
			fmt.Fprintf(e.Stdout, "%s: back to the previous image, which has the same version (%s); nothing is held\n",
				a.Str("rollback"), orUnknown(ts.Version))
			return nil
		}
		fmt.Fprintf(e.Stdout, "%s: back to the previous image (%s); %s is held until a newer release is out\n",
			a.Str("rollback"), orUnknown(ts.Version), orUnknown(ts.Held))
		return nil
	case a.Str("unhold") != "":
		if _, err := up.Unhold(ctx, a.Str("unhold")); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "%s: the next rebuild may try the latest release again\n", a.Str("unhold"))
		return nil
	}
	if err := e.Git(ctx); err != nil {
		return err // the project checks clone with git (§4.8)
	}
	if a.Bool("latest") {
		return up.Latest(ctx, cfg)
	}
	return up.Rebuild(ctx, cfg)
}

func orUnknown(v string) string {
	if v == "" {
		return "version not recorded"
	}
	return v
}

// doorsService wires the doors for one invocation: the board, the
// unauthenticated GitHub client, the memory stores and the version check.
func doorsService(e *Env) doors.Service {
	p := e.Paths()
	return doors.Service{
		Board:  e.Board(),
		Exec:   e.Exec,
		Issues: doorsGitHub(e),
		Memory: &memory.Service{
			Store: &memstore.Store{Paths: p, Exec: e.Exec},
			Paths: p,
			Clock: e.Clock,
		},
		Versions: doorsUpkeep(e),
		Log:      e.Log(),
	}
}

// doorsUpkeep wires upkeep: notices through the doors, the releases
// from GitHub and npm, rebuilds through imagebuild (without its own lock:
// Rebuild takes the tick lock) and the projects' checks through the gate.
func doorsUpkeep(e *Env) upkeep.Service {
	p := e.Paths()
	log := e.Log()
	pm := podman.Client{Exec: e.Exec, Dir: p.Home, Env: podman.PassEnv(doorsEnviron(e.Environ))}
	return upkeep.Service{
		Board:    e.Board(),
		Releases: doorsGitHub(e),
		Notices:  doors.Service{Board: e.Board(), Log: log},
		Builder:  imagebuild.Runner{Podman: pm, Clock: e.Clock, Log: log, LogDir: p.Logs},
		Gate: doorsGate{gate.Runner{
			Paths:  p,
			Exec:   e.Exec,
			Guard:  gitx.NewGuarder(e.Exec, nil, log),
			Podman: pm,
			Log:    log,
			Clock:  e.Clock,
		}},
		Tagger: pm,
		Clock:  e.Clock,
		Log:    log,
	}
}

// doorsGitHub is the client for GitHub's and npm's public reads. The
// differential tests' seams point it elsewhere: FACTORY_GITHUB_API and
// FACTORY_NPM_REGISTRY (seams 1 and 2).
func doorsGitHub(e *Env) *github.Client {
	return &github.Client{
		BaseURL: e.Environ["FACTORY_GITHUB_API"],
		NPMURL:  e.Environ["FACTORY_NPM_REGISTRY"],
		HTTP:    &http.Client{},
	}
}

// doorsGate gives upkeep the gate's checks and a project's base head,
// read through the gate's own guarded git.
type doorsGate struct{ gate.Runner }

func (g doorsGate) BaseHead(ctx context.Context, p config.Project) (string, error) {
	return g.Repo(p).Head(ctx, p.Base)
}

// doorsEnviron turns the environment map back into KEY=value lines, sorted.
func doorsEnviron(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
