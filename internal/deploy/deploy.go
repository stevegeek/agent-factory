// Package deploy is the master deploying itself
// (docs/plans/2026-10-24-p2-self-deploy.md): core main with config main,
// built on the builder helper, installed by the commit's own deploy.sh,
// checked through Foreman's /healthz and bin/factory version, and put
// back from the runner's own snapshot when the check fails. A good deploy
// then sends each opted-in ssh helper the new factory-remote (the install
// verb).
//
// The tick never deploys: deploy.sh install waits for the tick lock.
// Start writes deploys/request.json and starts the transient user unit
// factory-deploy, which runs `factory deploy run` (Run) with the old
// binary already in memory, so a bad build or a bad deploy.sh cannot stop
// the rollback. Drain is the tick's part: it starts the deploys the
// maintainer asked for on a Deploy? ticket and notices a deploy a reboot
// cut off.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Unit is the transient user unit a deploy runs in. Its name is fixed, so
// a second start while one runs does nothing (the running one takes the
// newer request when it is done with its own, or as it lets the lock go).
const Unit = "factory-deploy"

// The bounds of a deploy (Global Constraints). The unit's limit covers a
// fetch, deploy.sh, the check, a restore's wait for the tick lock and a
// second check, so systemd never kills a restore halfway.
const (
	UnitMaxRuntime    = "3h"
	SystemdRunTimeout = 30 * time.Second
	DeployShTimeout   = 100 * time.Minute
	HealthWait        = 90 * time.Second
	HealthEvery       = 2 * time.Second
	// RunLockWait is how long a runner waits for the deploy lock: the
	// tick holds it a moment when it looks for an interrupted deploy.
	RunLockWait = 5 * time.Second
	// VersionTimeout is bin/factory version's own time, after /healthz.
	VersionTimeout = 30 * time.Second
	TickLockWait   = 10 * time.Minute
	// DesktopDeployAge is how young a ~/factory/.deploy must be to stop a
	// self-deploy: the desktop's deploy.sh may be running.
	DesktopDeployAge = 2 * time.Hour
	// KeepLogs is how many deploy.sh logs deploys/logs keeps.
	KeepLogs = 10
)

// Logger is the factory log (*logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Noticer files a notice: a needs_human ticket about the factory itself
// (doors.Service.Notice, wrapped by the cli).
type Noticer interface {
	Notice(ctx context.Context, title, body string) error
}

// Asker files or updates the one Deploy? ticket of a held deploy
// (doors.Service.DeployAsk, wrapped by the cli).
type Asker interface {
	DeployAsk(ctx context.Context, rev, why string) error
}

// SSHDoer runs a factory-remote verb on a helper (sshjobs.Client).
type SSHDoer interface {
	Do(ctx context.Context, h jobproto.Host, verb, job string, stdin []byte, timeout time.Duration) ([]byte, error)
}

// Service deploys the factory on this host, the master.
type Service struct {
	Paths   paths.Paths
	Exec    execx.Execer // git, bash, systemd-run, systemctl, bin/factory
	Clock   clock.Clock
	Log     Logger  // nil logs nothing
	Notices Noticer // nil files nothing
	Board   *store.Board
	SSH     SSHDoer
	// Token mints the read token for the config repo; nil fetches config
	// with no credential.
	Token func(context.Context) (string, error)
	HTTP  *http.Client // nil: a client with a 3 s timeout
	// Env is this process's environment: deploy.sh runs with it, and the
	// unit gets its FACTORY_* settings.
	Env map[string]string

	// The test seams: where the mirrors fetch from ("" is GitHub), the
	// health check's, the restore's and the runner's lock waits (zero:
	// the constants above), and the file that says Foreman is a system
	// unit.
	MirrorBase   string
	HealthWait   time.Duration
	HealthEvery  time.Duration
	TickLockWait time.Duration
	RunLockWait  time.Duration
	SystemUnit   string // "" is /etc/systemd/system/foreman.service
	// afterLock runs when Run has let the lock go, before it looks for a
	// request once more.
	afterLock func()
}

func (s Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func (s Service) now() time.Time {
	if s.Clock == nil {
		return clock.System{}.Now()
	}
	return s.Clock.Now()
}

func (s Service) stamp() string { return clock.Stamp(s.now()) }

func (s Service) exec() execx.Execer {
	if s.Exec == nil {
		return execx.OS{}
	}
	return s.Exec
}

func (s Service) notice(ctx context.Context, title, body string) {
	if s.Notices == nil {
		return
	}
	if err := s.Notices.Notice(ctx, title, body); err != nil {
		s.logf("deploy: notice %q: %v", title, err)
	}
}

// Offer is how a merge asks for a deploy (P3): while deploys are held
// (hold, or the maintainer's hold file) it files or updates the Deploy?
// ask; otherwise it starts a deploy of main. rev and why are for the ask's
// display: a deploy always takes main's head when it runs.
func (s Service) Offer(ctx context.Context, cfg *config.Config, a Asker, rev, why, by string, hold bool) error {
	if !hold {
		_, held, err := deployrec.ReadHold(s.Paths.DeployHold())
		if err != nil {
			s.logf("deploy: the hold does not read (%v): holding", err)
		}
		hold = held
	}
	if hold {
		return a.DeployAsk(ctx, rev, why)
	}
	return s.Start(ctx, cfg, deployrec.Request{Rev: "main", By: by, Why: why})
}

// Start writes the request and starts the factory-deploy unit, which runs
// it. A unit already there counts as started: it takes the request when
// it is done with its own. With no user bus the request waits for
// `factory deploy run`. The hold is not looked at: a start is a deploy by
// hand.
func (s Service) Start(ctx context.Context, cfg *config.Config, r deployrec.Request) error {
	if !cfg.Deploy.On() {
		return errors.New("no [deploy] in config: this host does not deploy itself")
	}
	if r.Rev == "" {
		r.Rev = "main"
	}
	if r.At == "" {
		r.At = s.stamp()
	}
	if err := deployrec.WriteRequest(s.Paths.DeployRequest(), r); err != nil {
		return err
	}
	if !s.userBus() {
		return errors.New("no user bus: run factory deploy run")
	}
	argv := []string{"systemd-run", "--user", "--no-block", "--collect", "--unit=" + Unit,
		"--property=RuntimeMaxSec=" + UnitMaxRuntime}
	for _, kv := range s.factoryEnv() {
		argv = append(argv, "--setenv="+kv)
	}
	argv = append(argv, s.Paths.FactoryBin(), "deploy", "run")
	var env []string
	for _, k := range []string{"PATH", "HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := s.Env[k]; ok {
			env = append(env, k+"="+v)
		}
	}
	res, err := s.exec().Run(ctx, execx.Cmd{Argv: argv, Env: env, Timeout: SystemdRunTimeout})
	if err != nil {
		return fmt.Errorf("deploy start: %w", err)
	}
	if res.Code != 0 && !alreadyThere(res.Stderr) {
		return fmt.Errorf("deploy start: %w", execx.CheckErr(res))
	}
	s.logf("deploy %s requested by %s", r.Rev, orFactory(r.By))
	return nil
}

// alreadyThere recognises systemd refusing the unit name: a deploy runs.
func alreadyThere(stderr []byte) bool {
	e := strings.ToLower(string(stderr))
	return strings.Contains(e, "already exists") || strings.Contains(e, "already loaded")
}

// userBus reports whether XDG_RUNTIME_DIR holds the user manager's bus.
func (s Service) userBus() bool {
	dir := s.Env["XDG_RUNTIME_DIR"]
	if dir == "" {
		return false
	}
	st, err := os.Lstat(filepath.Join(dir, "bus"))
	return err == nil && st.Mode()&fs.ModeSocket != 0
}

// factoryEnv is FACTORY_ROOT and FACTORY_API_TOKENS from Paths, plus
// every other FACTORY_* setting of this process, sorted: the user
// manager's environment is not ours.
func (s Service) factoryEnv() []string {
	vars := map[string]string{}
	for k, v := range s.Env {
		if strings.HasPrefix(k, "FACTORY_") && v != "" && k != "FACTORY_GH_TOKEN" {
			vars[k] = v
		}
	}
	vars["FACTORY_ROOT"] = s.Paths.Root
	if s.Paths.Tokens != "" {
		vars["FACTORY_API_TOKENS"] = s.Paths.Tokens
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Drain is the tick's part. It starts a deploy of main for each Deploy?
// ticket the maintainer answered Deploy it (on the notice project, from
// the clock door, Requested and not Started) and marks it Started, so it
// starts once; the ticket's Rev is for display only. And when no runner
// holds the deploy lock, a deploy still marked running was cut off (a
// reboot): it becomes interrupted, with a notice. Nothing is restored by
// itself: the install may not have started.
func (s Service) Drain(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket) error {
	var errs []error
	if err := s.markInterrupted(ctx); err != nil {
		errs = append(errs, err)
	}
	for _, t := range ts {
		if !requested(t) {
			continue
		}
		err := s.Board.Changing(ctx, string(t.ID), store.NoWait, func(t *ticket.Ticket) error {
			if !requested(t) {
				return nil
			}
			if err := s.Start(ctx, cfg, deployrec.Request{Rev: "main", By: cfg.Maintainer(), Why: "Deploy it on " + string(t.ID), Ticket: string(t.ID)}); err != nil {
				return err
			}
			d := t.Deploy.V
			d.Started = ticket.Some(s.stamp())
			t.Deploy = ticket.Some(d)
			ticket.Move(t, t.Column, model.ReasonDeployStarted, "deploying main", "", "", s.now())
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("deploy %s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// requested is a Deploy? ticket whose Deploy it the tick has yet to start.
func requested(t *ticket.Ticket) bool {
	if t.Project != model.NoticeProject || t.Door != string(model.DoorClock) {
		return false
	}
	d, ok := t.Deploy.Get()
	return ok && d.Requested.Present() && !d.Started.Present()
}

// markInterrupted turns a running state with no runner into interrupted,
// with its notice. A held lock is a runner at work.
func (s Service) markInterrupted(ctx context.Context) error {
	st, err := deployrec.ReadState(s.Paths.DeployState())
	if err != nil || st.Phase != deployrec.PhaseRunning {
		return err
	}
	lock, err := fsx.TryLock(s.Paths.DeployLock())
	if errors.Is(err, fsx.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return s.interrupted(ctx)
}

// interrupted marks the state read afresh, under the deploy lock.
func (s Service) interrupted(ctx context.Context) error {
	st, err := deployrec.ReadState(s.Paths.DeployState())
	if err != nil || st.Phase != deployrec.PhaseRunning {
		return err
	}
	st.Phase, st.Result, st.Finished = deployrec.PhaseDone, deployrec.ResultInterrupted, s.stamp()
	if err := deployrec.WriteState(s.Paths.DeployState(), st); err != nil {
		return err
	}
	what := orString(st.Commit, st.Rev)
	s.logf("deploy %s: interrupted (at %s)", what, orString(st.Step, "the start"))
	if !st.Snapshotted {
		// deploys/before is still an older deploy's snapshot: no restore
		s.notice(ctx, "Deploy of "+what+" was interrupted before it installed anything",
			"The deploy was cut off at "+orString(st.Step, "its start")+" (a reboot, or the unit was stopped), "+
				"before its snapshot: deploy.sh never ran, so nothing was installed. `factory deploy start` tries again.")
		return nil
	}
	s.notice(ctx, "Deploy of "+what+" was interrupted: check System status; factory deploy restore puts the snapshot back",
		"The deploy was cut off at "+orString(st.Step, "its start")+" (a reboot, or the unit was stopped). "+
			"It may not have installed anything. If the factory runs badly, `factory deploy restore` puts back the files "+
			"the snapshot holds; `factory deploy start` tries again.")
	return nil
}

// desktopDeploy refuses while a ~/factory/.deploy younger than
// DesktopDeployAge is there: the desktop's deploy.sh may be running.
func (s Service) desktopDeploy() error {
	if st, err := os.Stat(filepath.Join(s.Paths.Root, ".deploy")); err == nil && s.now().Sub(st.ModTime()) < DesktopDeployAge {
		return errors.New("a desktop deploy is running, or one died: remove ~/factory/.deploy")
	}
	return nil
}

func orString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func orFactory(by string) string { return orString(by, "the factory") }
