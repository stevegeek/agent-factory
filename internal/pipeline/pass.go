package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/hosts"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/stage"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// workspaceReadme is the manager workspace's README (manager_project).
const workspaceReadme = "The factory manager's scratch workspace. Nothing here is used by anything else.\n"

// pass is tick_pass.
func (d Deps) pass(ctx context.Context, cfg *config.Config) error {
	running, runErr := d.Local.Running(ctx, cfg)
	if err := ctx.Err(); err != nil {
		return err
	}
	if runErr != nil {
		running = nil // a failed list is not an empty one
	}
	ts, problems := d.Board.Reader.List()
	if err := d.notice(ctx, problems); err != nil {
		return err
	}
	stepErr := d.beforeDrain(ctx, cfg, ts, running)
	if d.Deploys != nil && ctx.Err() == nil {
		// a deploy that cannot start is logged: the pass goes on. It
		// drains whatever the steps before it did, so a GitHub outage or a
		// bad ticket does not keep the maintainer's Deploy it waiting.
		if err := d.Deploys.Drain(ctx, cfg, ts); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			d.logf("deploys: %v", err)
		}
	}
	if d.Operator != nil && ctx.Err() == nil {
		// after the drain, so a deploy the maintainer asked for starts
		// before the operator offers another; an item's own failure is on
		// the item, and anything else is logged: the pass goes on
		if err := d.Operator.Settle(ctx, cfg, ts); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			d.logf("operator: %v", err)
		}
	}
	if stepErr != nil {
		return stepErr
	}

	nowRunning, err := d.Local.Running(ctx, cfg)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	busy := map[string]int{hosts.Local: len(nowRunning)}
	if err != nil {
		busy[hosts.Local] = cfg.MaxAgents // fails safe: nothing new starts here
	}
	for _, t := range ts {
		if a, ok := t.Agent.Get(); ok && hostOf(a) != hosts.Local {
			busy[a.Host]++
		}
	}
	down := map[string]bool{} // hosts paused after a usage limit, or pull hosts away
	for _, p := range d.Board.LoadPauses() {
		down[p.Host] = true
	}
	for _, h := range d.Helpers.Away(cfg) {
		down[h] = true
	}
	waiting := func(t *ticket.Ticket) bool { return isWaiting(cfg, t) }
	var queue []*ticket.Ticket
	for _, t := range ts {
		if waiting(t) {
			queue = append(queue, t)
		}
	}
	if err := d.refreshBases(ctx, cfg, queue); err != nil {
		return err
	}
	return d.Board.EachLocked(ctx, queue, waiting, func(t *ticket.Ticket) error {
		return d.startOne(ctx, cfg, t, busy, down)
	})
}

// isWaiting is the start loop's is_waiting: no agent, a gate or a stage to
// run, an enabled project, and a mode from config that is not board (a
// board project never gets agents, whatever a ticket file on it says).
func isWaiting(cfg *config.Config, t *ticket.Ticket) bool {
	if t.Agent.Present() {
		return false
	}
	if _, ok := stage.ForTicket(t); !ok && t.Column != string(model.ColGate) {
		return false
	}
	if !cfg.Enabled(t.Project) {
		return false
	}
	mode, known := cfg.ProjectMode(t.Project)
	return !known || mode != config.ModeBoard
}

// hostOf is the agent's host; an old record without one ran here.
func hostOf(a ticket.Agent) string {
	if a.Host == "" {
		return hosts.Local
	}
	return a.Host
}

// reconcile is reconcile(tickets): a ticket left in Working with
// no agent (a crash, a kill at the wrong moment) goes to the desk to be
// retried, instead of sitting where nothing will pick it up. Review,
// security, triage and voting are not among these: tickets wait there too.
func (d Deps) reconcile(ctx context.Context, ts []*ticket.Ticket) error {
	stranded := func(t *ticket.Ticket) bool {
		return t.Column == string(model.ColWorking) && !t.Agent.Present()
	}
	return d.Board.EachLocked(ctx, ts, stranded, func(t *ticket.Ticket) error {
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonAgentLost,
			"it was left in Working with no agent (the factory stopped mid-step); retry it", "", "", d.now())
		return d.save(ctx, t)
	})
}

// save is a commit point: nothing is saved once the context has ended.
func (d Deps) save(ctx context.Context, t *ticket.Ticket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.Board.Save(t)
}

// refreshBases brings each GitHub project's base up to origin once per
// pass, for the projects with waiting tickets, so new agents start from
// what has merged. A failure is logged and never stops the tick.
func (d Deps) refreshBases(ctx context.Context, cfg *config.Config, queue []*ticket.Ticket) error {
	seen := map[string]bool{}
	for _, t := range queue {
		if seen[t.Project] {
			continue
		}
		seen[t.Project] = true
		p := cfgOf(cfg, t.Project)
		if p == nil || p.GitHub == "" {
			continue
		}
		if err := (gitx.Writer{Repo: d.Git.Repo(*p)}).RefreshBase(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			d.logf("refresh %s: %s", t.Project, textx.Prefix(err.Error(), 300))
		}
	}
	return nil
}

// beforeDrain is the pass's steps up to the GitHub pass, each stopping
// the rest when it fails.
func (d Deps) beforeDrain(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket, running map[string]bool) error {
	if err := d.reconcile(ctx, ts); err != nil {
		return err
	}
	if err := d.agents(ctx, cfg, ts, running); err != nil {
		return err
	}
	if d.GitHub != nil {
		return d.GitHub.Pass(ctx, cfg, ts)
	}
	return nil
}

// projectFor is project_for: the checkout a ticket's agents run in.
// A manager's is its workspace here, made on first use; nil is a task with
// no repository (or a project config does not name).
func (d Deps) projectFor(ctx context.Context, cfg *config.Config, t *ticket.Ticket) (*config.Project, error) {
	if t.KindName() == "manage" {
		p := config.Project{Key: t.Project, Path: d.Paths.Manager, Base: "main"}
		if err := (gitx.Writer{Repo: d.Git.Repo(p)}).InitWorkspace(ctx, workspaceReadme); err != nil {
			return nil, fmt.Errorf("manager workspace: %w", err)
		}
		if err := d.managerMounts(); err != nil {
			return nil, fmt.Errorf("manager mounts: %w", err)
		}
		return &p, nil
	}
	return cfgOf(cfg, t.Project), nil
}

// startOne is one ticket of the start loop: its gate, or its
// stage's agent on the host routing picks. Any error that is not a helper
// refusing becomes a dispatcher error at the desk.
func (d Deps) startOne(ctx context.Context, cfg *config.Config, t *ticket.Ticket, busy map[string]int, down map[string]bool) error {
	skip, err := d.startStep(ctx, cfg, t, busy, down)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if skip {
		return nil
	}
	if err != nil {
		now := d.now()
		ticket.Event(t, "error", head300(err), "", ticket.Errored{Text: head300(err)}, now)
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonDispatcherError, "dispatcher error: "+textx.Prefix(err.Error(), 150), "", "", now)
	}
	return d.save(ctx, t)
}

// startStep does the step; skip leaves the ticket unsaved, as Python's
// continue does.
func (d Deps) startStep(ctx context.Context, cfg *config.Config, t *ticket.Ticket, busy map[string]int, down map[string]bool) (skip bool, err error) {
	proj, err := d.projectFor(ctx, cfg, t)
	if err != nil {
		return false, err
	}
	if proj == nil && t.KindName() != "task" {
		return false, fmt.Errorf("unknown project %s", t.Project)
	}
	if t.Column == string(model.ColGate) {
		return d.runGate(ctx, cfg, proj, t, busy, down)
	}
	s, _ := stage.ForTicket(t)
	localOK := proj != nil
	host, ok := hosts.Pick(cfg, string(s), busy, down, t.Host.Or(""), localOK)
	for ok {
		err := d.start(ctx, cfg, proj, t, s, host)
		if err == nil {
			busy[host]++
			break
		}
		if ctx.Err() != nil || !errors.Is(err, hostrun.ErrRemote) {
			return false, err
		}
		down[host] = true
		text := textx.Prefix(err.Error(), 250)
		ticket.Event(t, "error", host+": "+text, "", ticket.Errored{Text: text}, d.now())
		host, ok = hosts.Pick(cfg, string(s), busy, down, t.Host.Or(""), localOK)
	}
	return false, nil
}

// runGate is Gate 1 in the tick: as a gate job on a pull host
// when gateHost picks one (gatejob.go), else here (runGateHere). A host
// that refuses the job is down for this pass, and the gate runs here.
func (d Deps) runGate(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket, busy map[string]int, down map[string]bool) (bool, error) {
	if proj == nil || d.Gate == nil || d.Helpers == nil {
		return d.runGateHere(ctx, cfg, proj, t, busy)
	}
	host, ok := gateHost(cfg, *proj, t, busy, down)
	if !ok {
		return d.runGateHere(ctx, cfg, proj, t, busy)
	}
	err := d.startGateJob(ctx, cfg, proj, t, host)
	if err == nil {
		busy[host]++
		return false, nil
	}
	if ctx.Err() != nil || !errors.Is(err, hostrun.ErrRemote) {
		return false, err
	}
	down[host] = true
	text := textx.Prefix(err.Error(), 250)
	ticket.Event(t, "error", host+": "+text, "", ticket.Errored{Text: text}, d.now())
	_, err = d.runGateHere(ctx, cfg, proj, t, busy)
	return false, err // saved whatever happened here: it carries the host's error
}

// runGateHere is Gate 1 run in the tick. A gate runs on this host and
// takes one of its slots while it runs (gate.Fits): it
// waits while every slot is taken, and a heavy one (more memory than
// [gate] heavy_memory) waits until no agent runs here; meanwhile no new
// agent starts here, so the host drains for it. The tick runs a gate to
// its end before it starts anything else, so the slot is held for as
// long as the gate runs. A gate starts only with enough of the tick's
// time left to finish.
func (d Deps) runGateHere(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket, busy map[string]int) (bool, error) {
	if busy[hosts.Local] >= cfg.MaxAgents {
		return true, nil
	}
	if proj == nil {
		return false, fmt.Errorf("unknown project %s", t.Project)
	}
	if !gate.Fits(cfg, *proj, busy[hosts.Local]) {
		d.logf("%s: the gate waits for this host to be idle: its gate memory (%s) is above [gate] heavy_memory (%s) and %d agents run here",
			t.ID, cfg.GateMemory(*proj), cfg.Gate.HeavyMemory, busy[hosts.Local])
		busy[hosts.Local] = cfg.MaxAgents
		return true, nil
	}
	if d.Gate == nil {
		return true, nil
	}
	if !d.timeLeft(ctx) {
		d.logf("%s: the gate waits for the next tick: too little of this tick's time is left", t.ID)
		return true, nil
	}
	g, err := d.Gate.Run(ctx, cfg, *proj, t)
	if err != nil {
		return false, err
	}
	gate.Record(t, cfg, g, d.now())
	return false, d.afterGate(ctx, cfg, proj, t)
}

// afterGate follows Gate 1's recorded result, wherever its checks ran: a
// rework's open PR is updated, and the audit written.
func (d Deps) afterGate(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket) error {
	// a rework goes straight onto its PR: until then GitHub shows the old
	// commit, and an approval given there would be for code that is no
	// longer the change
	if t.PR.V.Number != 0 && proj.GitHub != "" && cfg.HasGitHub() && d.GitHub != nil {
		// a commit point first: the gate's outcome is saved, with an
		// open intent, before the push. The tick's deadline may end the
		// push; then the gate is not run again, and the next tick's
		// GitHub pass updates the PR from the intent.
		intent := !t.PR.V.OpenRequested.Set && (t.Column == string(model.ColReadyForPR) || t.Column == string(model.ColPROpen))
		if intent {
			t.PR.V.OpenRequested = ticket.Some(clock.Stamp(d.now()))
			if err := d.save(ctx, t); err != nil {
				return err
			}
		}
		_, err := d.GitHub.OpenPR(ctx, cfg, *proj, t)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if intent {
			t.PR.V.OpenRequested = ticket.Opt[string]{}
		}
		if err != nil {
			text := textx.Prefix(err.Error(), 250)
			ticket.Event(t, "error", "github: "+text, "", ticket.Errored{Text: text}, d.now())
		}
	}
	return gate.WriteAudit(d.Paths, t)
}

// managerMounts makes the sources of the manager's read-only mounts that
// a fresh factory may not have yet (templates/*/manager/scion-agent.yaml):
// the deploy records, the live context, and System status, which the
// tick writes at its end (an empty object until then).
func (d Deps) managerMounts() error {
	for _, dir := range []string{d.Paths.Deploys(), d.Paths.Context} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(sysinfo.File(d.Paths), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case errors.Is(err, fs.ErrExist):
		return nil
	case err != nil:
		return err
	}
	if _, err := f.WriteString("{}\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
