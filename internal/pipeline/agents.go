package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/hosts"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/memory"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/stage"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// unknownHead is the head a run's facts carry when they could not be read:
// it matches no head a reviewer started from, so the check fails closed.
const unknownHead = "unknown"

// agents is the agent pass of tick_pass: every ticket with an
// agent, under its lock. A helper's agent is polled there; one here is
// finished when it left a report, requeued when it went quiet at a usage
// limit, lost when Scion no longer runs it, and stopped past its timeout.
// An error is recorded on the ticket, which is saved whatever happened.
// running is nil when Scion could not say what runs here.
func (d Deps) agents(ctx context.Context, cfg *config.Config, ts []*ticket.Ticket, running map[string]bool) error {
	hasAgent := func(t *ticket.Ticket) bool { return t.Agent.Present() }
	return d.Board.EachLocked(ctx, ts, hasAgent, func(t *ticket.Ticket) error {
		work := t.Agent.V.Stage == string(model.StageWork)
		if err := d.agent(ctx, cfg, t, running); err != nil {
			return err
		}
		if work && !t.Agent.Present() && t.Column == string(model.ColReview) {
			d.startEarly(ctx, cfg, t)
		}
		return nil
	})
}

// agent is one ticket of the agent pass, saved whatever happened.
func (d Deps) agent(ctx context.Context, cfg *config.Config, t *ticket.Ticket, running map[string]bool) error {
	proj, err := d.projectFor(ctx, cfg, t)
	if err == nil {
		switch {
		case t.Agent.V.Stage == gateStage:
			err = d.gateAgent(ctx, cfg, proj, t)
		case hostOf(t.Agent.V) != hosts.Local:
			d.remoteAgent(ctx, cfg, proj, t)
			return d.save(ctx, t)
		default:
			err = d.localAgent(ctx, cfg, proj, t, running)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		ticket.Event(t, "error", head300(err), "", ticket.Errored{Text: head300(err)}, d.now())
	}
	return d.save(ctx, t)
}

// pastTimeout reports an agent that has run longer than the ticket allows.
// A start time that does not read is not past it.
func (d Deps) pastTimeout(cfg *config.Config, t *ticket.Ticket) (bool, error) {
	started, err := clock.Parse(t.Agent.V.Started)
	if err != nil {
		return false, fmt.Errorf("agent %s: started %q: %w", t.Agent.V.Name, t.Agent.V.Started, err)
	}
	return d.now().Sub(started) > hosts.Timeout(cfg, t), nil
}

// localAgent is the local half of the agent pass.
func (d Deps) localAgent(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket, running map[string]bool) error {
	a := t.Agent.V
	dir := ""
	if proj != nil {
		dir = proj.Path
	}
	checkout := func() error {
		if dir == "" {
			return fmt.Errorf("agent %s: no checkout of project %s here", a.Name, t.Project)
		}
		return nil
	}
	raw, has, err := d.Local.Report(a.Name)
	if err != nil {
		return err
	}
	limit := ""
	if !has && running != nil && running[a.Name] && d.Screen.Quiet(a.Name, hosts.QuietFor) {
		// an agent that has gone quiet may be waiting on a usage limit:
		// no point waiting out its timeout
		if err := checkout(); err != nil {
			return err
		}
		screen, err := d.Screen.Look(ctx, dir, a.Name, hosts.LookLimitLines)
		if err != nil {
			return err
		}
		limit, _ = hosts.HitLimit(screen, hosts.DefaultLimitLines)
	}
	switch {
	case limit != "":
		if err := checkout(); err != nil {
			return err
		}
		if err := d.Local.Remove(ctx, dir, t.ID, a.Name, stage.KeepBranch(model.Stage(a.Stage))); err != nil {
			return err
		}
		d.recordUsage(t, a)
		return d.requeue(ctx, cfg, t, hosts.Local, limit)
	case has:
		return d.finish(ctx, cfg, proj, t, raw, "")
	case running != nil && !running[a.Name]:
		d.settle(ctx, cfg, t, nil)
		if err := d.Screen.DropRunConfig(a.Name); err != nil {
			return err
		}
		t.Agent = ticket.Nil[ticket.Agent]()
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonAgentLost, "agent "+a.Name+" disappeared without a report", "", "", d.now())
	default:
		late, err := d.pastTimeout(cfg, t)
		if err != nil || !late {
			return err
		}
		if err := checkout(); err != nil {
			return err
		}
		// the branch is kept even for a stage without one, as in Python
		if err := d.Local.Remove(ctx, dir, t.ID, a.Name, true); err != nil {
			return err
		}
		d.recordUsage(t, a)
		d.settle(ctx, cfg, t, nil)
		t.Agent = ticket.Nil[ticket.Agent]()
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonTimedOut, "agent "+a.Name+" timed out", "", "", d.now())
	}
	return nil
}

// remoteAgent is the helper half of the agent pass: poll_remote,
// and when it fails, an error event; past the agent's timeout the job is
// removed (what it spent recorded, when the helper read it on the way) and
// the agent counted lost.
func (d Deps) remoteAgent(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket) {
	a := t.Agent.V
	err := d.pollRemote(ctx, cfg, proj, t)
	if err == nil || ctx.Err() != nil {
		return
	}
	ticket.Event(t, "error", head300(err), "", ticket.Errored{Text: head300(err)}, d.now())
	if !t.Agent.Present() {
		return
	}
	if late, _ := d.pastTimeout(cfg, t); !late {
		return
	}
	if h, ok := d.Helpers.Helper(cfg, a.Host); ok {
		if h.Remove(ctx, t.ID, a.Name) == nil { // best effort, as Python
			d.recordUsage(t, a)
		}
	}
	if ctx.Err() != nil {
		return
	}
	d.lost(t, a.Host)
	ticket.Move(t, string(model.ColNeedsHuman), model.ReasonAgentLost,
		"agent "+a.Name+" on "+a.Host+": "+textx.Prefix(err.Error(), 150), "", "", d.now())
	t.Agent = ticket.Nil[ticket.Agent]()
}

// pollRemote is poll_remote: a running job is timed out once past
// its time, a job that is gone is lost; a done one is collected (its files
// archived, its memory settled, a worker's branch fetched from its bundle),
// removed from the helper, and finished, requeued at a usage limit, or sent
// to the desk without a report. A job removed before it was done records
// the usage its removal brought back.
func (d Deps) pollRemote(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket) error {
	a := t.Agent.V
	h, ok := d.Helpers.Helper(cfg, a.Host)
	if !ok {
		return fmt.Errorf("unknown host %s", a.Host)
	}
	state, err := h.Poll(ctx, a.Name)
	if err != nil {
		return err
	}
	// a pull host's job waiting for its claim is timed as a running one
	if state == jobproto.StateRunning || state == jobproto.StateQueued {
		late, err := d.pastTimeout(cfg, t)
		if err != nil || !late {
			return err
		}
		if err := h.Remove(ctx, t.ID, a.Name); err != nil {
			return err
		}
		d.recordUsage(t, a)
		d.lost(t, a.Host)
		t.Agent = ticket.Nil[ticket.Agent]()
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonTimedOut, "agent "+a.Name+" on "+a.Host+" timed out", "", "", d.now())
		return nil
	}
	if state != jobproto.StateDone {
		if err := h.Remove(ctx, t.ID, a.Name); err != nil {
			return err
		}
		d.recordUsage(t, a)
		d.lost(t, a.Host)
		t.Agent = ticket.Nil[ticket.Agent]()
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonAgentLost,
			"agent "+a.Name+" on "+a.Host+" disappeared without a report", "", "", d.now())
		return nil
	}
	res, err := h.Collect(ctx, t.ID, a.Name)
	if err != nil {
		return err
	}
	d.recordUsage(t, a)
	if a.Memory.Present() && a.Memory.V.Role != "" {
		var got *memory.Copy
		if tar, ok := res.MemoryTar(); ok {
			if got, err = memory.ReadTar(tar); err != nil {
				got = nil
				text := textx.Prefix(err.Error(), 200)
				ticket.Event(t, "error", "memory from "+a.Host+": "+text, "", ticket.Errored{Text: text}, d.now())
			}
		}
		if got == nil {
			d.lost(t, a.Host)
		} else {
			d.settle(ctx, cfg, t, got)
		}
	}
	if bundle, ok := res.OutBundle(); ok && model.Stage(a.Stage) == model.StageWork {
		if proj == nil {
			return fmt.Errorf("agent %s: no checkout of project %s to fetch its branch into", a.Name, t.Project)
		}
		if err := (gitx.Writer{Repo: d.Git.Repo(*proj)}).FetchBundle(ctx, t.Branch.Or(""), bundle); err != nil {
			return err
		}
	}
	if err := h.Remove(ctx, t.ID, a.Name); err != nil {
		return err
	}
	report, has := res.Report()
	if !has {
		if limit, hit := hosts.HitLimit(res.LimitText(hosts.RemoteLimitChars), hosts.RemoteLimitLines); hit {
			return d.requeue(ctx, cfg, t, a.Host, limit)
		}
		t.Agent = ticket.Nil[ticket.Agent]()
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonNoReport,
			"agent "+a.Name+" on "+a.Host+" finished without a report", "", "", d.now())
		return nil
	}
	return d.finish(ctx, cfg, proj, t, report, res.Head())
}

// requeue is requeue_after_limit: a run a usage limit stopped is not
// the ticket's fault. It goes back to where its stage starts, its memory
// settled, and the host is paused; no person is asked.
func (d Deps) requeue(ctx context.Context, cfg *config.Config, t *ticket.Ticket, host, why string) error {
	s := t.Agent.V.Stage
	d.settle(ctx, cfg, t, nil)
	t.Agent = ticket.Nil[ticket.Agent]()
	until, err := d.Board.PauseHost(host, why, hosts.PauseFor)
	if err != nil {
		return err
	}
	ticket.Move(t, hosts.RetryColumn(s), model.ReasonUsageLimit,
		host+" hit a usage limit ("+why+"); retried after "+until, "", "", d.now())
	return nil
}

func (d Deps) settle(ctx context.Context, cfg *config.Config, t *ticket.Ticket, remote *memory.Copy) {
	if d.Memory != nil {
		d.Memory.Settle(ctx, cfg, t, remote)
	}
}

func (d Deps) lost(t *ticket.Ticket, host string) {
	if d.Memory != nil {
		d.Memory.Lost(t, host)
	}
}

// finish is finish_stage: a local agent is removed (its branch kept
// for the stages that have one), the run's memory settled, the facts of the
// branch read, and the stage's finisher decides. The tickets it files are
// saved first, an Ask's answer is delivered to the ticket it was asked on
// under that ticket's lock, and t is left as the finisher changed it.
// head is where a helper job left the branch ("" for a local run, which is
// read from the branch).
func (d Deps) finish(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket, raw []byte, head string) error {
	a := t.Agent.V
	s := model.Stage(a.Stage)
	report := stage.ParseReport(raw)
	if hostOf(a) == hosts.Local {
		if proj == nil {
			return fmt.Errorf("agent %s: no checkout of project %s here", a.Name, t.Project)
		}
		if err := d.Local.Remove(ctx, proj.Path, t.ID, a.Name, stage.KeepBranch(s)); err != nil {
			return err
		}
		d.recordUsage(t, a)
	}
	d.settle(ctx, cfg, t, nil)
	facts, err := d.facts(ctx, proj, t, s, report, head)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// the branch could not be read: the finish fails closed (no
		// commits, an unknown head), and the error is on the ticket
		ticket.Event(t, "error", head300(err), "", ticket.Errored{Text: head300(err)}, d.now())
	}
	var parent *ticket.Ticket
	if s == model.StageTask && t.Parent.Or("") != "" {
		if parent, err = d.loadParent(t.Parent.V); err != nil {
			return err
		}
	}
	in := stage.FinishIn{Ticket: ticket.Clone(t), Report: report, Facts: facts, Config: cfg, Parent: parent, Now: d.now()}
	if s == model.StagePublic && proj != nil {
		// what the review was shown: nothing it reads changes while it runs
		in.Public = &stage.PublicIn{Texts: publicTexts(cfg, *proj, t)}
	}
	out, err := d.finishWith(ctx, in)
	if err != nil {
		return err
	}
	if out.AnswerParent != nil {
		delivered, err := d.deliver(ctx, *out.AnswerParent)
		if err != nil {
			return err
		}
		if !delivered { // the ticket it was asked on closed meanwhile
			in.Ticket, in.Parent = ticket.Clone(t), nil
			if out, err = d.finishWith(ctx, in); err != nil {
				return err
			}
		}
	}
	*t = *out.Ticket
	return nil
}

// startEarly starts Gate 1's checks of the head a worker just left for
// review (Deps.Early), once that finish is saved, so they run beside
// review, security and public review. Not for a paused or board project,
// whose tickets are not gated, nor for a heavy gate, which waits for an
// idle host, nor when a pull host prefers gates (they would run here, and
// the gate there). They take one of this host's slots, as a gate does, so
// they start only when one is free, and the tick counts them while they
// run. Nothing here fails the pass: a check that does not start is a line
// in the log, and the gate runs its own.
func (d Deps) startEarly(ctx context.Context, cfg *config.Config, t *ticket.Ticket) {
	if d.Early == nil || ctx.Err() != nil || !cfg.Enabled(t.Project) {
		return
	}
	if mode, _ := cfg.ProjectMode(t.Project); mode == config.ModeBoard || gatesPreferPull(cfg) {
		return
	}
	proj, err := d.projectFor(ctx, cfg, t)
	if err != nil || proj == nil || cfg.HeavyGate(*proj) {
		return
	}
	running, err := d.Local.Running(ctx, cfg)
	if err != nil || len(running) >= cfg.MaxAgents {
		d.logf("%s: not checked early: no free slot here (%d of %d agents run)", t.ID, len(running), cfg.MaxAgents)
		return
	}
	head, err := d.Git.Repo(*proj).Head(ctx, t.Branch.Or(""))
	if err == nil {
		err = d.Early.Start(ctx, t.ID, head)
	}
	if err != nil {
		d.logf("%s: early checks: %v", t.ID, err)
	}
}

func head300(err error) string { return textx.Prefix(err.Error(), 300) }

// finishWith runs the stage's finisher with the ids its outcome files
// tickets under, and saves those tickets.
func (d Deps) finishWith(ctx context.Context, in stage.FinishIn) (stage.Outcome, error) {
	n := stage.Needs(in)
	in.IDs = nil
	for i := 0; i < n; i++ {
		id, err := d.Board.NextID(ctx)
		if err != nil {
			return stage.Outcome{}, err
		}
		in.IDs = append(in.IDs, id)
	}
	out, err := stage.Finish(in)
	if err != nil {
		return stage.Outcome{}, err
	}
	for _, f := range out.File {
		if err := d.save(ctx, f); err != nil {
			return stage.Outcome{}, err
		}
	}
	return out, nil
}

// facts reads what the finisher needs of the branch after the run: for a
// worker that says it is done, whether the branch exists and its commits
// ahead of the base; for review, security and public review, where the
// branch is now.
func (d Deps) facts(ctx context.Context, proj *config.Project, t *ticket.Ticket, s model.Stage, report jsonx.Value, head string) (stage.Facts, error) {
	branch := t.Branch.Or("")
	switch s {
	case model.StageWork:
		if !stage.ReadReport(report).Is("status", "done") {
			return stage.Facts{}, nil
		}
		if proj == nil {
			return stage.Facts{}, fmt.Errorf("no checkout of project %s here", t.Project)
		}
		repo := d.Git.Repo(*proj)
		ok, err := repo.BranchExists(ctx, branch)
		if err != nil || !ok {
			return stage.Facts{}, err
		}
		n, err := repo.CommitsAhead(ctx, branch)
		if err != nil {
			return stage.Facts{}, err
		}
		return stage.Facts{BranchExists: true, CommitsAhead: n}, nil
	case model.StageReview, model.StageSecurity, model.StagePublic:
		if head != "" {
			return stage.Facts{Head: head}, nil
		}
		if proj == nil {
			return stage.Facts{Head: unknownHead}, fmt.Errorf("no checkout of project %s here", t.Project)
		}
		h, err := d.Git.Repo(*proj).Head(ctx, branch)
		if err != nil {
			return stage.Facts{Head: unknownHead}, err
		}
		return stage.Facts{Head: h}, nil
	}
	return stage.Facts{}, nil
}

// loadParent is the ticket a task's parent names, nil when there is none.
func (d Deps) loadParent(id string) (*ticket.Ticket, error) {
	pid, ok := ticket.ParseID(strings.ToUpper(id))
	if !ok {
		return nil, nil
	}
	p, err := d.Board.Reader.Load(pid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, boardread.ErrBadID) {
		return nil, nil
	}
	return p, err
}

// deliver is deliver_answer's write: under the lock of the ticket
// the Ask was asked on, the answer is recorded there and saved, unless that
// ticket closed meanwhile.
func (d Deps) deliver(ctx context.Context, a stage.ParentAnswer) (bool, error) {
	lock, err := d.Board.Lock(ctx, a.Parent, store.Forever)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Unlock() }()
	p, err := d.Board.Reader.Load(a.Parent)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !stage.Deliver(p, a, d.now()) {
		return false, nil
	}
	return true, d.save(ctx, p)
}
