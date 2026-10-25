package pipeline

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/hosts"
	"github.com/stevegeek/agent-factory/internal/memory"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/prflow"
	"github.com/stevegeek/agent-factory/internal/stage"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// start is start_stage: what the stage records before it runs
// (a worker's branch, a reviewer's starting head, the security tier and
// questions), the prompt (for public review, with the texts the PR would
// carry and the private values), the run's memory, the write-ahead save, the
// launch here or on a helper, and the start's move and event.
//
// On a failed launch the agent is cleared and the launch's error returned
// (a helper's refusal is hostrun.ErrRemote, which lets the tick try another
// host). Everything recorded before the launch stays on the ticket, as in
// Python.
func (d Deps) start(ctx context.Context, cfg *config.Config, proj *config.Project, t *ticket.Ticket, s model.Stage, host string) error {
	info, ok := model.StageInfoOf(s)
	if !ok {
		return fmt.Errorf("no stage %q", s)
	}
	name := stage.AgentName(t, s)
	var repo *gitread.Repo
	if proj != nil {
		repo = d.Git.Repo(*proj)
	}
	needRepo := func() error {
		if repo == nil {
			return fmt.Errorf("a %s run needs the project's repository; %s has none here", s, t.Project)
		}
		return nil
	}
	var begin stage.BeginIn
	reviewMD := false
	var known []stage.KnownItem
	knownNote := ""
	var public *stage.PublicIn
	switch s {
	case model.StageVerify:
		var err error
		if known, knownNote, err = d.known(ctx, proj, t); err != nil {
			return err
		}
	case model.StageWork:
		if t.Branch.Or("") == "" {
			if err := needRepo(); err != nil {
				return err
			}
			b, err := prflow.PickBranch(ctx, prflow.Repos{Repo: d.Git.Repo}, *proj, t.Title, cfg.Hygiene.Deny)
			if err != nil {
				return err
			}
			begin.Branch = b
		}
	case model.StageReview, model.StageSecurity, model.StagePublic:
		if err := needRepo(); err != nil {
			return err
		}
		h, err := repo.Head(ctx, t.Branch.Or(""))
		if err != nil {
			return err
		}
		begin.Head = h
		if s == model.StagePublic {
			if public, err = publicIn(ctx, cfg, *proj, repo, t); err != nil {
				return err
			}
			begin.Texts = prflow.PublicDigest(prflow.PublicTexts(cfg, *proj, t))
		} else if s == model.StageSecurity {
			if begin.Tier, err = gate.ClassifyBranch(ctx, repo, t.Branch.Or(""), *proj); err != nil {
				return err
			}
		} else if reviewMD, err = repo.CatFileExists(ctx, proj.Base, "REVIEW.md"); err != nil {
			return err
		}
	}
	stage.Begin(t, s, begin)

	local := host == hosts.Local
	var pins []string
	if local && repo != nil {
		var err error
		if pins, err = pinned(ctx, repo, proj, t.Branch.Or("")); err != nil {
			return err
		}
	}
	ref, runConfig, err := d.prepareMemory(ctx, cfg, t, s, name, local)
	if err != nil {
		return err
	}
	toolchain := ""
	if proj != nil {
		toolchain = cfg.Toolchain(*proj)
	}
	prompt := stage.For(s).Prompt(stage.PromptIn{Ticket: t, Project: proj, ReviewMD: reviewMD, Memory: ref, Local: local,
		Known: known, KnownNote: knownNote, Public: public, Toolchain: toolchain})
	mdl := hosts.Model(cfg, t, string(s), host)

	// write-ahead: if this process dies between starting the agent and
	// saving the ticket, the next tick still knows the agent (and finds it
	// gone, or running) instead of an agent no ticket points at
	t.Agent = ticket.Some(ticket.Agent{Name: name, Stage: string(s), Started: clock.Stamp(d.now()), Host: host, Starting: ticket.Some(true)})
	if err := d.save(ctx, t); err != nil {
		return err
	}
	job := hostrun.Job{Ticket: t, Stage: s, Name: name, Prompt: prompt, Model: mdl, Template: cfg.Template(string(s)),
		Project: proj, RunConfig: runConfig, Pinned: pins, Toolchain: toolchain}
	if err := d.launch(ctx, cfg, host, job, repo, ref); err != nil {
		if ref != nil && ref.Dir != "" {
			_ = memory.Discard(ref)
		}
		t.Agent = ticket.Nil[ticket.Agent]()
		return err
	}
	a := ticket.Agent{Name: name, Stage: string(s), Started: clock.Stamp(d.now()), Host: host}
	if ref != nil {
		a.Memory = ticket.Some(*ref)
	}
	t.Agent = ticket.Some(a)
	now := d.now()
	if t.Column != string(info.RunningColumn) {
		ticket.Move(t, string(info.RunningColumn), model.ReasonStarted, "", "", "", now)
	}
	branch := ""
	if info.Branch {
		branch = t.Branch.Or("")
	}
	ticket.RecordStart(t, ticket.StartIn{Stage: s, Agent: name, Host: host, Model: mdl, Branch: branch}, now)
	return nil
}

// publicIn is what public review reads beside the commits: every free
// text the PR would carry, as OpenPR would send it (prflow.PublicTexts),
// and the private values: the names config holds that the project does not
// already publish at its base.
func publicIn(ctx context.Context, cfg *config.Config, proj config.Project, repo *gitread.Repo, t *ticket.Ticket) (*stage.PublicIn, error) {
	in := &stage.PublicIn{Texts: publicTexts(cfg, proj, t)}
	private, err := repo.Absent(ctx, proj.Base, cfg.PrivateNames(t.Project))
	if err != nil {
		return nil, err
	}
	in.Private = private
	return in, nil
}

// publicTexts are prflow.PublicTexts as the stage reads them.
func publicTexts(cfg *config.Config, proj config.Project, t *ticket.Ticket) []stage.PublicText {
	var out []stage.PublicText
	for _, x := range prflow.PublicTexts(cfg, proj, t) {
		out = append(out, stage.PublicText{Name: x.Name, Text: x.Text, Fixed: x.Fixed})
	}
	return out
}

// maxKnownTickets bounds the open tickets a verifier is shown.
const maxKnownTickets = 100

// known is what a verifier checks findings against for duplicates: the
// project's other open tickets, and its open GitHub issues and PRs read
// without credentials (the dispatcher's, not the agent's). A tracker that
// cannot be read is a note for the verifier, not a failed start.
func (d Deps) known(ctx context.Context, proj *config.Project, t *ticket.Ticket) ([]stage.KnownItem, string, error) {
	var out []stage.KnownItem
	ts, _ := d.Board.Reader.List()
	for _, x := range ts {
		if x.Project != t.Project || x.ID == t.ID || x.Board() || model.Closed(x.Column) || len(out) == maxKnownTickets {
			continue
		}
		out = append(out, stage.KnownItem{Ref: string(x.ID), Where: model.ColumnLabel(x.Column), Title: x.Title})
	}
	if proj == nil || proj.GitHub == "" {
		return out, "", nil
	}
	if d.Issues == nil {
		return out, "They are not read here: check the open tickets above, and say so in your summary.", nil
	}
	items, err := d.Issues.OpenItems(ctx, proj.GitHub)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, "", ctxErr
		}
		return out, "They could not be read (" + textx.Prefix(err.Error(), 150) + "): check the open tickets above, and say so in your summary.", nil
	}
	for _, it := range items {
		where := "issue"
		if it.PR {
			where = "PR"
		}
		out = append(out, stage.KnownItem{Ref: "#" + strconv.FormatInt(it.Number, 10), Where: where, Title: it.Title})
	}
	return out, "", nil
}

// prepareMemory is the memory half of start_stage: the store's latest
// commit for the stage's role and, for a run here, its copy and the inline
// config that mounts it. A failure means the run goes without memory,
// which the ticket says; only the context ending is an error.
func (d Deps) prepareMemory(ctx context.Context, cfg *config.Config, t *ticket.Ticket, s model.Stage, name string, local bool) (*ticket.MemoryRef, string, error) {
	if d.Memory == nil {
		return nil, "", nil
	}
	ref, err := d.Memory.Prepare(ctx, cfg, t, s, name, local)
	runConfig := ""
	if err == nil && ref != nil && ref.Dir != "" {
		if runConfig, err = d.Memory.Volume(name, ref.Dir); err != nil {
			_ = memory.Discard(ref)
		}
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, "", ctxErr
		}
		d.Memory.NoMemory(t, err)
		return nil, "", nil
	}
	return ref, runConfig, nil
}

// launch starts the job here or on the helper.
func (d Deps) launch(ctx context.Context, cfg *config.Config, host string, job hostrun.Job, repo *gitread.Repo, ref *ticket.MemoryRef) error {
	if host == hosts.Local {
		return d.Local.Start(ctx, job)
	}
	h, ok := d.Helpers.Helper(cfg, host)
	if !ok {
		return fmt.Errorf("unknown host %s", host)
	}
	if repo != nil {
		job.Repo = jobRepo{repo}
	}
	job.TimeoutMinutes = int(hosts.Timeout(cfg, job.Ticket) / time.Minute)
	if ref != nil && d.Memory != nil {
		files, err := d.Memory.JobFiles(ctx, job.Ticket, *ref)
		if err != nil {
			return err
		}
		job.Memory = files
	}
	return h.Start(ctx, job)
}

// jobRepo is what a helper job's start reads of the project's clone: the
// branches, and the bundle it ships.
type jobRepo struct{ r *gitread.Repo }

func (j jobRepo) BranchExists(ctx context.Context, branch string) (bool, error) {
	return j.r.BranchExists(ctx, branch)
}

func (j jobRepo) Head(ctx context.Context, branch string) (string, error) {
	return j.r.Head(ctx, branch)
}

func (j jobRepo) Bundle(ctx context.Context, refs []string) ([]byte, error) {
	return gitx.Writer{Repo: j.r}.Bundle(ctx, refs)
}
