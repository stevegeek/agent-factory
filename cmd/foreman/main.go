// Command foreman is the web desk, the ticket API and MCP server, and push
// (ARCHITECTURE.md §6). web.Main parses the flags and serves; this file
// builds the services it calls, in-process (§6.1):
//
//   - the maintainer's answers, the ＋ page and the Live screens go to
//     package ops, whose PR actions are ops.Intents: an answer that needs
//     GitHub records an intent on the ticket and wakes the tick, which does
//     the work with the credentials Foreman never holds (§6.3);
//   - /api/v1 and /mcp go to ticketapi through one shared guard;
//   - every service reads config.toml through one config.Source, so a change
//     to the file is seen by the next request without a restart (§3.4).
//
// Foreman's dependency closure holds no GitHub, git-write, Scion-write,
// gate or pipeline code (archtest); the subprocesses it may start are
// ssh … tail and the wake (§6.5). A local agent's screen is a file the
// screen relay writes (package screens): Foreman runs no scion.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/api"
	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/guard"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/jobtail"
	"github.com/stevegeek/agent-factory/internal/logx"
	"github.com/stevegeek/agent-factory/internal/mcp"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/screens"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
	"github.com/stevegeek/agent-factory/internal/tokens"
	"github.com/stevegeek/agent-factory/internal/wake"
	"github.com/stevegeek/agent-factory/internal/web"
)

func main() {
	os.Exit(run(os.Args[1:], environ(os.Environ()), os.Stdout, os.Stderr))
}

// run is the binary: `foreman version`, else web.Main with the services
// wired over the real subprocesses.
func run(args []string, env map[string]string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	}
	d := deps{env: env, exec: execx.OS{}, spawn: execx.OS{}}
	return web.Main(args, env, stdout, stderr, d.wire)
}

// environ is the process environment as a map (the last of a repeated
// name wins, as getenv's does).
func environ(kv []string) map[string]string {
	env := make(map[string]string, len(kv))
	for _, e := range kv {
		if k, v, ok := strings.Cut(e, "="); ok {
			env[k] = v
		}
	}
	return env
}

// deps are what the wiring takes from outside: the environment, and how
// subprocesses run (tests pass fakes).
type deps struct {
	env   map[string]string
	exec  execx.Execer
	spawn wake.Spawner
}

// wire is web.Wire: the services web calls, built on w's paths and clock.
// It never fails on the config: a missing or broken config.toml leaves
// answers refusing (and logged) until it parses, as a fresh CLI would.
func (d deps) wire(_ context.Context, w web.Wiring) (web.Services, error) {
	if w.Log == nil {
		w.Log = log.New(io.Discard, "", 0)
	}
	if w.Clock == nil {
		w.Clock = clock.System{}
	}
	// to the journal and foreman-state/foreman.log: logs/ is read-only to Foreman, so nothing it
	// does can plant a file there for the tick to follow (host/foreman-system.sh); the Live view
	// shows both logs, and the backup keeps foreman-state
	flog := &logx.Log{Dir: w.Paths.ForemanState(), Name: filepath.Base(w.Paths.ForemanLog()), Clock: w.Clock,
		Stderr: journalWriter{w.Log}}
	cfg := &config.Source{Path: w.Paths.Config, Switches: w.Paths.ProjectSwitches(), Log: w.Log}
	if _, err := cfg.Current(); err != nil {
		w.Log.Printf("config: %v; answers and new tickets are refused until it reads", err)
	}
	board := &store.Board{Reader: &boardread.Reader{Paths: w.Paths, Log: flog}, Paths: w.Paths, Clock: w.Clock, Log: flog}
	// the token store is the one --api-tokens names, for the verbs as for
	// the guard (the CLI it replaces got FACTORY_API_TOKENS set to it)
	tok := &tokens.Store{Path: w.Tokens, Clock: w.Clock, Log: w.Log}
	tickets := &ticketapi.Service{Board: board, Tokens: tok, Config: cfg, Clock: w.Clock}

	envList := environList(d.env)
	verbs := &ops.Service{
		Board:  board,
		Config: cfg,
		Clock:  w.Clock,
		// the trigger file, never the user manager's bus, which the
		// sandbox hides (§6.2): factory-wake.path starts the tick
		Wake: wake.Waker{Paths: w.Paths, Exec: d.exec, Spawn: d.spawn, Env: d.env, Clock: w.Clock,
			Trigger: w.Paths.WakeTrigger()},
		PR: ops.Intents{Clock: w.Clock},
		// a nil Guard is gitread.Verifier: a tampered .git/config is
		// refused, never rewritten, from here
		Git: ops.GitBranches{Repo: gitread.Repo{Exec: d.exec, Env: envList}},
		// the sandbox hides podman, so a local agent's screen is the one
		// the relay (factory-screens.path, `factory screens`) writes while
		// Live is open; Foreman never runs scion
		Relay: screens.Reader{Dir: w.Paths.Screens()},
		Jobs:  jobTails{tail: jobtail.Reader{Exec: d.exec, Env: envList}.Tail, paths: w.Paths},
		Log:   flog,
		AskOf: present.AskFor,
		// Wait zero is the CLI's 60 s, which answers wait for a ticket the
		// tick holds (§5.6); web's own timeout outlasts it
	}
	svc := web.OpsServices(verbs, cfg)
	// the maintainer's project switch on Boards: foreman-state/projects.json,
	// which every reader of the config applies, then a wake
	svc.Switch = verbs
	// the maintainer's deploy hold on System status:
	// foreman-state/deploy-hold.json, then a wake
	svc.Deploys = verbs
	// a person's answer to a board agent's question goes through the
	// ticket API's own answer verb, with or without the token APIs
	svc.Tickets = tickets
	if !w.API {
		return svc, nil
	}
	if err := tok.CheckPerms(); err != nil {
		w.Log.Printf("api: %v", err)
	}
	g := guard.New(guard.Options{Tokens: tok, Hosts: w.Hosts, Open: w.Open, Concurrency: w.APIConcurrency, Clock: w.Clock, Log: w.Log})
	svc.API = api.New(api.Options{Guard: g, Service: tickets, Log: w.Log})
	svc.MCP = mcp.New(mcp.Options{Guard: g, Service: tickets, Log: w.Log})
	return svc, nil
}

// environList is the environment map as KEY=value lines, sorted: the base
// every subprocess's own environment is built from (§5.3).
func environList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// journalWriter sends what the factory log echoes (and its own write
// failures) to Foreman's journal, one line at a time.
type journalWriter struct{ log web.Logger }

func (j journalWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		j.log.Printf("%s", line)
	}
	return len(p), nil
}

// jobTailTimeout is remote(h, "tail", …, timeout=20).
const jobTailTimeout = 20 * time.Second

// jobTails is ops.JobTails over jobtail, read-only: the tail of a helper
// job's output, for the Live tab.
type jobTails struct {
	tail  func(context.Context, jobproto.Host, string) (string, error)
	paths paths.Paths
}

func (j jobTails) Tail(ctx context.Context, h config.HostCfg, job string) (string, error) {
	if h.Pull() {
		// a pull host's job: the log tail its last beat left in the mailbox
		return jobtail.MailTail(j.paths.Mailbox(), h.Name, job)
	}
	ctx, cancel := context.WithTimeout(ctx, jobTailTimeout)
	defer cancel()
	return j.tail(ctx, hostOf(j.paths, h), job)
}

// hostOf is the helper as the dispatcher reaches it (hostrun.HostOf): its
// key from config, by default ~/.ssh/factory-remote, with ~ expanded.
func hostOf(p paths.Paths, h config.HostCfg) jobproto.Host {
	key := h.Key
	if key == "" {
		key = p.RemoteKey
	}
	if key == "~" {
		key = p.Home
	} else if rest, ok := strings.CutPrefix(key, "~/"); ok {
		key = filepath.Join(p.Home, rest)
	}
	return jobproto.Host{Name: h.Name, SSH: h.SSH, Key: key}
}
