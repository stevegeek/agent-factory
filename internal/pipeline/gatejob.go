package pipeline

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/hosts"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Gate jobs: Gate 1's checks (install, tests, lint on a fresh checkout of
// the gated commit) run on a pull host instead of here, so a heavy gate
// does not need this host to itself. Only the checks go: the heads are
// taken here when the job starts, and the changed files, the size, the
// decision, the move, the PR update and the audit are done here when it is
// collected, from those heads. A gate job is an agent record with stage
// "gate" (and the heads), so the agent pass polls, times out and collects
// it as it does a helper's job. A lost one is never a failed gate: the
// ticket stays in the gate column and is gated again, here once
// maxGateJobs of its jobs were lost.

// gateStage is a gate job's stage in its agent record and events: the
// gate column's name, which is no agent stage.
const gateStage = string(model.ColGate)

// maxGateJobs is how many gate jobs a ticket gets in the gate column
// before its gate runs here.
const maxGateJobs = 2

// gateHost is the pull host a gate goes to instead of this one, or false
// to run (or wait for) it here as before. A pull host with "gate" in
// prefer takes every gate; one with "gate" in stages takes a gate that
// would have to wait here: a heavy one (gate memory above [gate]
// heavy_memory), or one with every slot here taken. The host must not be
// away, paused or down, and must have a free slot. Pins are for agents and
// do not route gates.
func gateHost(cfg *config.Config, p config.Project, t *ticket.Ticket, busy map[string]int, down map[string]bool) (string, bool) {
	if gateJobs(t) >= maxGateJobs {
		return "", false
	}
	waitsHere := cfg.HeavyGate(p) || busy[hosts.Local] >= cfg.MaxAgents
	for _, list := range []func(h config.HostCfg) []string{
		func(h config.HostCfg) []string { return h.Prefer },
		func(h config.HostCfg) []string {
			if waitsHere {
				return h.Stages
			}
			return nil
		},
	} {
		for _, h := range cfg.Hosts {
			if h.Pull() && slices.Contains(list(h), gateStage) && !down[h.Name] && busy[h.Name] < h.Slots {
				return h.Name, true
			}
		}
	}
	return "", false
}

// gateJobs counts the gate jobs started since the ticket last came into the
// gate column.
func gateJobs(t *ticket.Ticket) int {
	n := 0
	for i := len(t.History) - 1; i >= 0; i-- {
		e := t.History[i]
		if m, ok := e.Data.(ticket.Moved); ok && m.To == string(model.ColGate) {
			break
		}
		if s, ok := e.Data.(ticket.Started); ok && s.Stage == gateStage {
			n++
		}
	}
	return n
}

// startGateJob starts t's gate as a job on host: the branch's and the
// base's heads are taken now (the decision is about them), the agent
// record is written ahead, and the job (gate.Script, a bundle of the base
// and the branch, the pinned files) is handed to the host.
func (d Deps) startGateJob(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket, host string) error {
	h, ok := d.Helpers.Helper(cfg, host)
	if !ok {
		return fmt.Errorf("unknown host %s", host)
	}
	branch := t.Branch.Or("")
	if branch == "" {
		return fmt.Errorf("%s has no branch to gate", t.ID)
	}
	repo := d.Git.Repo(*proj)
	head, err := repo.Head(ctx, branch)
	if err != nil {
		return fmt.Errorf("gate %s: %w", t.ID, err)
	}
	base, err := repo.Head(ctx, proj.Base)
	if err != nil {
		return fmt.Errorf("gate %s: %w", t.ID, err)
	}
	script, err := gate.Script(*proj)
	if err != nil {
		return err
	}
	name := strings.ToLower(string(t.ID)) + "-g" + strconv.Itoa(ticket.Starts(t, model.Stage(gateStage))+1)
	now := d.now()
	rec := ticket.Agent{Name: name, Stage: gateStage, Started: clock.Stamp(now), Host: host,
		Gate: ticket.Some(ticket.GateJob{Head: head, Base: base})}
	ahead := rec
	ahead.Starting = ticket.Some(true)
	t.Agent = ticket.Some(ahead)
	if err := d.save(ctx, t); err != nil {
		return err
	}
	job := hostrun.Job{Ticket: t, Stage: model.Stage(gateStage), Name: name, Project: proj, Repo: jobRepo{repo},
		Gate: &hostrun.GateRun{Head: head, Script: script}}
	if err := h.Start(ctx, job); err != nil {
		t.Agent = ticket.Nil[ticket.Agent]()
		return err
	}
	t.Agent = ticket.Some(rec)
	ticket.RecordStart(t, ticket.StartIn{Stage: model.Stage(gateStage), Agent: name, Host: host, Branch: branch}, now)
	return nil
}

// gateAgent is the agent pass for a gate job (pollRemote's shape): a done
// job is collected, removed and decided here; one that is gone, past its
// time, or that reports no checks run is lost (gateLost). A host that
// cannot be asked is an error, until the job is past its time.
func (d Deps) gateAgent(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket) error {
	a := t.Agent.V
	job, ok := a.Gate.Get()
	h, known := d.Helpers.Helper(cfg, a.Host)
	if !ok || !known || proj == nil || d.Gate == nil {
		if known {
			_ = h.Remove(ctx, a.Name) // best effort
		}
		d.gateLost(t, "it cannot be followed here")
		return nil
	}
	state, err := h.Poll(ctx, a.Name)
	if err != nil {
		if late, _ := d.pastTimeout(cfg, t); late && ctx.Err() == nil {
			_ = h.Remove(ctx, a.Name) // best effort
			d.gateLost(t, "its host did not answer: "+textx.Prefix(err.Error(), 150))
			return nil
		}
		return err
	}
	switch state {
	case jobproto.StateRunning, jobproto.StateQueued:
		late, err := d.pastTimeout(cfg, t)
		if err != nil || !late {
			return err
		}
		if err := h.Remove(ctx, a.Name); err != nil {
			return err
		}
		d.gateLost(t, "it ran out of time")
		return nil
	case jobproto.StateDone:
	default:
		if err := h.Remove(ctx, a.Name); err != nil {
			return err
		}
		d.gateLost(t, "it disappeared")
		return nil
	}
	res, err := h.Collect(ctx, t.ID, a.Name)
	if err != nil {
		return err
	}
	if err := h.Remove(ctx, a.Name); err != nil {
		return err
	}
	raw, _ := res.Report()
	exit, ok, why := gateExit(raw)
	if !ok {
		d.gateLost(t, "it ran no checks: "+why)
		return nil
	}
	output := podman.LastRunes(jobproto.DecodeText(res.Files[jobproto.MemberLog]), podman.GateTail)
	g, err := d.Gate.Decide(ctx, cfg, *proj, t, gate.Remote{Head: job.Head, Base: job.Base, ChecksPass: exit == 0, Output: output, Host: a.Host})
	if err != nil {
		return err // the job is gone: the next pass finds it lost and gates again
	}
	now := d.now()
	t.Agent = ticket.Nil[ticket.Agent]()
	ticket.RecordFinish(t, model.Stage(gateStage), a.Name, now)
	gate.Record(t, cfg, g, now)
	return d.afterGate(ctx, cfg, proj, t)
}

// gateExit reads a gate job's report.json: {"status": "done", "gate_exit":
// N} is the checks' exit; anything else is no checks run, with why (the
// report's summary, or what is wrong with it). The report comes from
// another machine: it is read, never followed.
func gateExit(raw []byte) (exit int, ok bool, why string) {
	if raw == nil {
		return 0, false, "no report"
	}
	v, err := jsonx.Parse(raw)
	if err != nil {
		return 0, false, "an unreadable report"
	}
	if status, _ := v.Get("status").Str(); status != "done" {
		summary, _ := v.Get("summary").Str()
		if summary == "" {
			summary = "status " + strconv.Quote(status)
		}
		return 0, false, textx.Prefix(summary, 150)
	}
	n := v.Get("gate_exit")
	lit, _ := n.Number()
	code, err := strconv.Atoi(lit)
	if !n.IsInt() || err != nil {
		return 0, false, "no exit in the report"
	}
	return code, true, ""
}

// gateLost ends a gate job that gave no result: the ticket stays in the
// gate column, to be gated again.
func (d Deps) gateLost(t *ticket.Ticket, why string) {
	a := t.Agent.V
	t.Agent = ticket.Nil[ticket.Agent]()
	text := "gate job " + a.Name + " on " + a.Host + ": " + why + "; the gate runs again"
	ticket.Event(t, "error", text, "", ticket.Errored{Text: text}, d.now())
}
