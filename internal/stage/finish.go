package stage

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/panel"
	"github.com/stevegeek/agent-factory/internal/security"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The caps on what a report can put on the board.
const (
	MaxPreExisting     = 3      // bugs a reviewer files per review
	MaxProposedTickets = 6      // tickets a design proposes
	MaxProposals       = 5      // proposals a prospector makes
	MaxDrafts          = 5      // config drafts a manager writes
	MaxPatch           = 50_000 // characters of one draft's patch
	MaxActions         = 5      // typed actions a manager proposes per run
	MaxKeptItems       = 15     // drafts and actions a manager ticket keeps
	MaxQuestion        = 2000   // characters of an agent's question
)

// Errors a finish can return; each means the pipeline called it wrongly.
var (
	ErrNoAgent    = errors.New("stage: the ticket has no agent to finish")
	ErrWrongStage = errors.New("stage: the ticket's agent runs another stage")
	ErrTooFewIDs  = errors.New("stage: too few ticket ids for what the report files")
)

// Facts are what the pipeline read of the branch after the run.
type Facts struct {
	// Head is where the branch is now: the head a helper job reported, or
	// the local branch's. Review and security compare it with the head they
	// started from; review records it as reviewed.
	Head string
	// BranchExists and CommitsAhead: work, the branch and its commits ahead
	// of the base.
	BranchExists bool
	CommitsAhead int
}

// FinishIn is everything a finish decides from.
type FinishIn struct {
	// Ticket is a clone the finish changes and returns. Its agent is the run
	// that finished; the agent has been removed and its memory settled.
	Ticket *ticket.Ticket
	// Report is the run's report.json, from ParseReport.
	Report jsonx.Value
	Facts  Facts
	Config *config.Config
	// Parent is, for a task, the ticket its parent names (nil when it has
	// none, or the id names no ticket): an Ask's answer goes back to it.
	Parent *ticket.Ticket
	// IDs are the ids for the tickets the outcome files: Needs(in) of them,
	// allocated by the caller.
	IDs []ticket.ID
	Now time.Time
}

// ParentAnswer is an Ask's answer to bring back to the ticket it was asked
// on: a typed answered event "<task>: <text>" on Parent.
type ParentAnswer struct {
	Parent ticket.ID
	Task   ticket.ID
	Text   string
}

// Outcome is what a finish decided, as data. The pipeline saves the filed
// tickets, delivers AnswerParent under the parent's lock, then saves Ticket.
type Outcome struct {
	// Ticket is the input's ticket, changed: the report recorded, the agent
	// cleared, the move made through ticket.Move.
	Ticket *ticket.Ticket
	// File are new tickets to save, before Ticket: pre-existing bugs from a
	// review, a panel's proposals.
	File []*ticket.Ticket
	// AnswerParent is set when a task answered the Ask it was filed for;
	// Ticket is then in done. If Deliver finds the parent closed meanwhile,
	// the pipeline finishes again on a fresh clone with Parent nil.
	AnswerParent *ParentAnswer
	// DeleteBranch: the stage works without a branch, so removing its agent
	// deletes Scion's (keep_branch = false).
	DeleteBranch bool
}

// Finish is finish_stage for the stage of the ticket's agent.
func Finish(in FinishIn) (Outcome, error) {
	a, ok := in.Ticket.Agent.Get()
	if !ok {
		return Outcome{}, ErrNoAgent
	}
	return finishAs(model.Stage(a.Stage), in)
}

// Needs is how many ticket ids Finish files tickets with for in: the
// pipeline allocates that many before it finishes. It runs the finish on a
// copy, so it agrees with Finish by construction.
func Needs(in FinishIn) int {
	if in.Ticket == nil || !in.Ticket.Agent.Present() {
		return 0
	}
	in.Ticket = ticket.Clone(in.Ticket)
	in.IDs = nil
	f := finisher{FinishIn: in, counting: true}
	a, _ := in.Ticket.Agent.Get()
	if _, err := f.run(model.Stage(a.Stage)); err != nil {
		return 0
	}
	return f.used
}

func finishAs(s model.Stage, in FinishIn) (Outcome, error) {
	a, ok := in.Ticket.Agent.Get()
	if !ok {
		return Outcome{}, ErrNoAgent
	}
	if model.Stage(a.Stage) != s {
		return Outcome{}, fmt.Errorf("%w: %s, not %s", ErrWrongStage, a.Stage, s)
	}
	f := finisher{FinishIn: in}
	return f.run(s)
}

// finisher is one finish in progress.
type finisher struct {
	FinishIn
	counting bool // Needs: count the ids instead of using them
	used     int
	out      Outcome
}

// id is the next id to file a ticket with.
func (f *finisher) id() (ticket.ID, error) {
	f.used++
	if f.counting {
		return ticket.FormatID(f.used), nil
	}
	if f.used > len(f.IDs) {
		return "", ErrTooFewIDs
	}
	return f.IDs[f.used-1], nil
}

// pin fixes the order of the ticket's keys so far. Python appends a key to
// a ticket when it is first assigned; the codec appends new keys in field
// order. Encoding after each assignment makes the next new key land after
// the last, as Python's would.
func (f *finisher) pin() { ticket.Encode(f.Ticket) }

func (f *finisher) move(to model.Column, r model.Reason, detail, rule string) {
	ticket.Move(f.Ticket, string(to), r, detail, rule, "", f.Now)
}

func (f *finisher) run(s model.Stage) (Outcome, error) {
	t := f.Ticket
	a, _ := t.Agent.Get()
	host := a.Host
	if host == "" {
		host = model.LocalHost
	}
	info, _ := model.StageInfoOf(s)
	f.out = Outcome{Ticket: t, DeleteBranch: !info.Branch}

	t.Reports.Set(string(s), f.Report)
	t.AgentLastHost = ticket.Some(host)
	f.pin()
	t.Agent = ticket.Nil[ticket.Agent]()
	ticket.RecordFinish(t, s, a.Name, f.Now)

	r := ReadReport(f.Report)
	if q := textx.Strip(r.Or("question", "")); info.MayAsk && r.Is("status", "question") && q != "" {
		q = textx.Clip(q, MaxQuestion)
		t.Question = ticket.Some(ticket.Question{Text: q, Stage: string(s), Agent: a.Name, At: clock.Stamp(f.Now)})
		f.pin()
		f.move(model.ColNeedsHuman, model.ReasonAgentQuestion, string(s)+" asks: "+textx.Clip(q, 200), "")
		return f.out, nil
	}
	var err error
	switch s {
	case model.StageTriage:
		f.triage(r)
	case model.StageTask:
		f.task(r)
	case model.StageManage:
		f.manage(r)
	case model.StageDesign:
		f.design(r)
	case model.StageProspect:
		f.prospect(r)
	case model.StageVote:
		err = f.vote(r, a.Name)
	case model.StageWork:
		f.work(r)
	case model.StageReview:
		err = f.review(r)
	case model.StageSecurity:
		f.security(r)
	case model.StageVerify:
		err = f.verify(r)
	default:
		return Outcome{}, fmt.Errorf("%w: %s", ErrWrongStage, s)
	}
	if err != nil {
		return Outcome{}, err
	}
	return f.out, nil
}

// triage is finish_triage. Accepting rewrites the ticket in
// triage's own words, which is the injection boundary for untrusted doors.
func (f *finisher) triage(r Report) {
	t := f.Ticket
	why := textx.Prefix(r.Str("reason"), 200)
	decision, _ := r.Value("decision")
	d, _ := decision.Str()
	if r.Is("decision", "reject") {
		f.move(model.ColRejected, model.ReasonTriageRejected, "triage: "+why, "triage rejected it")
		return
	}
	if d != "accept" || decision.Kind() != jsonx.KindString || !r.Truthy("task") {
		rule := "triage could not decide"
		if r.Is("decision", "accept") {
			rule = "triage accepted it without writing a task"
		}
		f.move(model.ColNeedsHuman, model.ReasonTriageUnsure, "triage unsure: "+why, rule)
		return
	}
	t.Original = ticket.Some(ticket.Original{Title: t.Title, Body: t.Body})
	f.pin()
	if title := textx.Prefix(textx.Fold(r.Or("title", t.Title)), 120); title != "" {
		t.Title = title
	}
	task, _ := r.Value("task")
	t.Body = jsonx.PyStr(task)
	door := t.Door + " tickets"
	if info, ok := model.DoorOf(t.Door); ok {
		door = info.Label
	}
	if f.Config.AutoAccept(t.Door) {
		f.move(model.ColReady, model.ReasonTriageAccepted, "triage accepted",
			"triage accepted; "+door+" start straight away ([triage.auto_accept] "+t.Door+" = true)")
		return
	}
	f.move(model.ColNeedsHuman, model.ReasonTriageAccepted, "triage accepted; move to ready to start work",
		"triage accepted; "+door+" wait for a person ([triage.auto_accept] "+t.Door+" = false)")
}

// task: the result comes back to a person, or, for an Ask's task, to the
// ticket it was asked on (deliver_answer).
func (f *finisher) task(r Report) {
	t := f.Ticket
	t.Result = ticket.Some(r.Or("result", ""))
	f.pin()
	state := "blocked"
	if r.Is("status", "done") {
		state = "done"
	}
	summary := r.Str("summary")
	if state == "done" && AskedOn(t, f.Parent) {
		f.out.AnswerParent = &ParentAnswer{Parent: f.Parent.ID, Task: t.ID, Text: textx.Clip(summary, 300)}
		f.move(model.ColDone, model.ReasonAnsweredOn, "answered on "+t.Parent.Or(""), "")
		return
	}
	if n := len(ReportedFindings(r)); state == "done" && n > 0 && isFactoryProject(f.Config, t.Project) {
		// a finding task's findings are checked before they reach anyone
		f.move(model.ColVerifying, model.ReasonFindingsReported,
			fmt.Sprintf("task done with %d findings: %s; a verifier checks them next", n, textx.Prefix(summary, 150)), "")
		return
	}
	f.move(model.ColNeedsHuman, model.Reason("task_"+state), "task "+state+": "+textx.Prefix(summary, 200), "")
}

// AskedOn is asked_on: parent is the open ticket whose Ask filed
// task t, by a typed asked event naming t or, on old history, an asked event
// whose detail starts "<t>:". A task with a parent is not always a question.
func AskedOn(t, parent *ticket.Ticket) bool {
	pid, ok := t.Parent.Get()
	if !ok || parent == nil || string(parent.ID) != pid {
		return false
	}
	if id, ok := ticket.ParseID(pid); !ok || string(id) != pid {
		return false
	}
	if parent.Column == string(model.ColDone) || parent.Column == string(model.ColRejected) {
		return false
	}
	for _, e := range parent.History {
		if e.Event != "asked" {
			continue
		}
		if a, ok := e.Data.(ticket.Asked); ok {
			if a.Ticket == string(t.ID) {
				return true
			}
			continue
		}
		if strings.HasPrefix(e.Detail, string(t.ID)+":") {
			return true
		}
	}
	return false
}

// Deliver records an Ask's answer on the ticket it was asked on, which the
// pipeline has just loaded under its lock: a typed answered event. It
// reports false, and changes nothing, when the ticket closed meanwhile.
func Deliver(parent *ticket.Ticket, a ParentAnswer, now time.Time) bool {
	if parent.Column == string(model.ColDone) || parent.Column == string(model.ColRejected) {
		return false
	}
	ticket.Event(parent, "answered", string(a.Task)+": "+a.Text, "", ticket.Answered{Ticket: string(a.Task), Text: a.Text}, now)
	return true
}

// manage is finish_manage and clean_drafts, with the
// operator's typed actions beside the drafts. Only a report with
// status done records anything: a blocked run's proposals are dropped. A
// rerun keeps the earlier items that were applied, refused or requested
// (a tapped item is never lost to a reply) and drops the rest. Each item
// is recorded new, for the tick to classify; an action's tier comes from
// the model's table, never from the report, and a kind not in it is
// refused here.
func (f *finisher) manage(r Report) {
	t := f.Ticket
	t.Result = ticket.Some(r.Or("result", r.Or("summary", "")))
	f.pin()
	summary := textx.Prefix(r.Str("summary"), 200)
	if !r.Is("status", "done") {
		f.move(model.ColNeedsHuman, model.ReasonManagerBlocked, "manager blocked: "+summary, "")
		return
	}
	// the run's number counts the manager's starts, which no rerun trims,
	// so that no two runs share one (an item's land branch is named by it)
	run := max(1, ticket.Starts(t, model.StageManage))
	for _, d := range t.Drafts {
		run = max(run, d.Run+1)
	}
	for _, a := range t.Actions {
		run = max(run, a.Run+1)
	}
	drafts := []ticket.Draft{}
	for _, v := range r.List("drafts") {
		d, ok := v.Object()
		if !ok {
			continue
		}
		dr := Report{o: d}
		patch, _ := dr.Value("patch")
		if !jsonx.Truthy(patch) || textx.Strip(jsonx.PyStr(patch)) == "" {
			continue
		}
		drafts = append(drafts, ticket.Draft{Title: textx.Clip(dr.Or("title", "Untitled"), 120),
			Why: textx.Clip(dr.Or("why", ""), 2000), Patch: textx.Prefix(jsonx.PyStr(patch), MaxPatch), Run: run, State: model.ItemNew})
	}
	drafts = drafts[:min(len(drafts), MaxDrafts)]
	actions := reportActions(r, run)
	keptDrafts, keptActions := keptItems(t, MaxKeptItems-len(drafts)-len(actions))
	t.Drafts = append(append([]ticket.Draft{}, keptDrafts...), drafts...)
	f.pin()
	t.Actions = append(keptActions, actions...)
	if len(t.Actions) == 0 {
		t.Actions = nil
	}
	f.pin()
	applying := 0
	for _, d := range keptDrafts {
		if d.State == model.ItemRequested {
			applying++
		}
	}
	for _, a := range keptActions {
		if a.State == model.ItemRequested {
			applying++
		}
	}
	switch {
	case len(actions) > 0:
		f.move(model.ColNeedsHuman, model.ReasonManagerDrafted,
			fmt.Sprintf("manager proposed %d draft(s) and %d action(s): %s", len(drafts), len(actions), summary), "")
	case len(drafts) > 0:
		f.move(model.ColNeedsHuman, model.ReasonManagerDrafted, fmt.Sprintf("manager drafted %d change(s): %s", len(drafts), summary), "")
	case applying > 0:
		// a tapped item is still to apply: the ask says so, and no notice
		// offers to close the ticket over it
		f.move(model.ColNeedsHuman, model.ReasonManagerDrafted,
			fmt.Sprintf("manager drafted nothing new; %d tapped change(s) still to apply: %s", applying, summary), "")
	default:
		f.move(model.ColNeedsHuman, model.ReasonManagerAnswered, "manager answered: "+summary, "")
	}
}

// reportActions are the report's actions, at most MaxActions: objects with
// a string kind, and an optional target, hours (a host pause's, 1 to 168,
// 24 when not given) and why. A kind not in model.OperatorActions is kept,
// refused, so that the maintainer sees what was asked.
func reportActions(r Report, run int) []ticket.Action {
	var out []ticket.Action
	for _, v := range r.List("actions") {
		if len(out) == MaxActions {
			break
		}
		o, ok := v.Object()
		if !ok {
			continue
		}
		ar := Report{o: o}
		kv, _ := ar.Value("kind")
		kind, ok := kv.Str()
		if kind = textx.Strip(kind); !ok || kind == "" {
			continue
		}
		tv, _ := ar.Value("target")
		target, _ := tv.Str()
		target = textx.Strip(target)
		a := ticket.Action{Kind: textx.Clip(kind, 60), Target: textx.Clip(target, 120),
			Why: textx.Clip(ar.Or("why", ""), 2000), Run: run}
		info, known := model.ActionOf(kind)
		switch {
		case !known:
			a.Tier, a.State, a.Note = string(model.TierNever), model.ItemRefused, "not an action the manager may propose"
		case info.Target == "" && target != "":
			a.Tier, a.State, a.Note = string(info.Tier), model.ItemRefused, kind+" takes no target"
		case info.Target != "" && !targetName(target):
			a.Tier, a.State, a.Note = string(info.Tier), model.ItemRefused, kind+" needs a "+info.Target+" name as its target"
		default:
			a.Tier, a.State = string(info.Tier), model.ItemNew
			if info.Kind == model.ActionPauseHost {
				hv, _ := ar.Value("hours")
				a.Hours = pauseHours(hv)
			}
		}
		out = append(out, a)
	}
	return out
}

// targetName reports a target that can be a host or project name: 1 to 64
// letters, digits, '-', '_' and '.', not starting with '.' or '-'. It is
// only the shape; the tick still checks the name against the config.
func targetName(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '.' || s[0] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// pauseHours is a host pause's hours: a number clamped to 1..168, 24 when
// the report gives none.
func pauseHours(v jsonx.Value) int {
	f, ok := v.Float64()
	if !ok || math.IsNaN(f) {
		return 24
	}
	return int(min(max(f, 1), 168))
}

// keptItems are the ticket's items a rerun keeps: applied, refused and
// requested ones, in their order. When they are more than room, the oldest
// settled ones (applied or refused, by run) go first; a requested one is
// never dropped.
func keptItems(t *ticket.Ticket, room int) ([]ticket.Draft, []ticket.Action) {
	keep := func(state string) bool {
		return state == model.ItemApplied || state == model.ItemRefused || state == model.ItemRequested
	}
	var ds []ticket.Draft
	var as []ticket.Action
	for _, d := range t.Drafts {
		if keep(ticket.DraftState(d)) {
			ds = append(ds, d)
		}
	}
	for _, a := range t.Actions {
		if keep(ticket.ActionState(a)) {
			as = append(as, a)
		}
	}
	for len(ds)+len(as) > max(room, 0) {
		// the oldest settled item: the lowest run, a draft before an action
		di, ai := -1, -1
		for i, d := range ds {
			if d.State != model.ItemRequested && (di < 0 || d.Run < ds[di].Run) {
				di = i
			}
		}
		for i, a := range as {
			if a.State != model.ItemRequested && (ai < 0 || a.Run < as[ai].Run) {
				ai = i
			}
		}
		switch {
		case di >= 0 && (ai < 0 || ds[di].Run <= as[ai].Run):
			ds = append(ds[:di], ds[di+1:]...)
		case ai >= 0:
			as = append(as[:ai], as[ai+1:]...)
		default:
			return ds, as // only requested items are left
		}
	}
	return ds, as
}

// design is finish_design.
func (f *finisher) design(r Report) {
	t := f.Ticket
	proposed := []ticket.Proposed{}
	for _, v := range r.Items("tickets") {
		p, ok := v.Object()
		if !ok {
			continue
		}
		pr := Report{o: p}
		if textx.Strip(pr.Str("title")) == "" || textx.Strip(pr.Str("body")) == "" {
			continue
		}
		proposed = append(proposed, ticket.Proposed{Title: textx.Prefix(pr.Str("title"), 120), Body: pr.Str("body")})
	}
	t.Design = ticket.Some(r.Or("design", ""))
	f.pin()
	t.Proposed = proposed[:min(len(proposed), MaxProposedTickets)]
	f.pin()
	summary := r.Str("summary")
	if !r.Is("status", "done") || t.Design.Or("") == "" || len(t.Proposed) == 0 {
		f.move(model.ColNeedsHuman, model.ReasonDesignerBlocked, "designer blocked: "+textx.Prefix(summary, 200), "")
		return
	}
	f.move(model.ColNeedsHuman, model.ReasonDesignReady,
		fmt.Sprintf("design ready (%d tickets proposed): %s; factory approve or redirect", len(t.Proposed), textx.Prefix(summary, 160)), "")
}

// prospect is finish_prospect: proposals with every field
// written, for the blind panel.
func (f *finisher) prospect(r Report) {
	t := f.Ticket
	proposals := []ticket.Proposal{}
	for _, v := range r.Items("proposals") {
		o, ok := v.Object()
		if !ok {
			continue
		}
		pr := Report{o: o}
		var fields [5]string
		complete := true
		for i, k := range []string{"title", "noticed", "change", "why", "cost"} {
			fields[i] = pr.Str(k)
			if textx.Strip(fields[i]) == "" {
				complete = false
			}
			fields[i] = textx.Prefix(fields[i], 2000)
		}
		if complete {
			proposals = append(proposals, ticket.Proposal{Title: fields[0], Noticed: fields[1], Change: fields[2], Why: fields[3], Cost: fields[4]})
		}
	}
	t.Proposals = proposals[:min(len(proposals), MaxProposals)]
	f.pin()
	t.Votes = []ticket.Vote{}
	f.pin()
	if len(t.Proposals) == 0 {
		f.move(model.ColDone, model.ReasonProspectEmpty, "prospecting found nothing worth proposing: "+textx.Prefix(r.Str("summary"), 150), "")
		return
	}
	f.move(model.ColVoting, model.ReasonProposalsReady, fmt.Sprintf("%d proposals; blind panel next", len(t.Proposals)), "")
}

// vote is finish_vote: this voter's valid scores (an index of a
// proposal, a score of 1 to 5; anything else is ignored) join the votes;
// the last voter's reveals the panel.
func (f *finisher) vote(r Report, voter string) error {
	t := f.Ticket
	var scores jsonx.Object
	for _, v := range r.Items("scores") {
		o, ok := v.Object()
		if !ok {
			continue
		}
		iv, hasI := o.Lookup("index")
		sv, hasS := o.Lookup("score")
		if !hasI || !hasS {
			continue
		}
		i, okI := pyInt(iv)
		score, okS := pyInt(sv)
		if !okI || !okS || i < 1 || i > int64(len(t.Proposals)) || score < 1 || score > 5 {
			continue
		}
		scores.Set(fmt.Sprint(i), jsonx.ObjectOf(jsonx.Member{Key: "score", Value: jsonx.Int(score)},
			jsonx.Member{Key: "reason", Value: jsonx.String(textx.Prefix(Report{o: o}.Str("reason"), 300))}))
	}
	if scores == nil {
		scores = jsonx.Object{}
	}
	t.Votes = append(t.Votes, ticket.Vote{Voter: voter, Host: t.AgentLastHost.Or(model.LocalHost), Scores: scores})
	if t.Votes[len(t.Votes)-1].Host == "" {
		t.Votes[len(t.Votes)-1].Host = model.LocalHost
	}
	voters := f.Config.Panel.Voters
	if len(t.Votes) < voters {
		ticket.Event(t, "vote", fmt.Sprintf("%d/%d in", len(t.Votes), voters), "",
			ticket.Voted{K: len(t.Votes), N: voters}, f.Now)
		return nil
	}
	ids := make([]ticket.ID, panel.Needs(t))
	for k := range ids {
		id, err := f.id()
		if err != nil {
			return err
		}
		ids[k] = id
	}
	filed, err := panel.Reveal(t, ids, f.Now)
	if err != nil {
		return fmt.Errorf("stage: reveal: %w", err)
	}
	f.out.File = append(f.out.File, filed...)
	return nil
}

// work: done with commits goes to review.
func (f *finisher) work(r Report) {
	if !r.Is("status", "done") {
		f.move(model.ColNeedsHuman, model.ReasonWorkerBlocked, "worker blocked: "+textx.Prefix(r.Str("summary"), 200), "")
		return
	}
	if !f.Facts.BranchExists || f.Facts.CommitsAhead == 0 {
		f.move(model.ColNeedsHuman, model.ReasonNoCommits, "worker reported done but the branch has no commits", "")
		return
	}
	f.move(model.ColReview, model.ReasonWorkDone, "", "")
}

// committed reports a reviewer that moved the branch: review and security
// must leave the head where it was when they started.
func (f *finisher) committed(s model.Stage) bool {
	before := f.Ticket.HeadBeforeReview
	if s == model.StageSecurity {
		before = f.Ticket.HeadBeforeSecurity
	}
	h, ok := before.Get()
	if ok && h == f.Facts.Head {
		return false
	}
	f.move(model.ColNeedsHuman, model.ReasonReviewerCommitted, string(s)+" agent changed the branch; reviewers must not commit", "")
	return true
}

// review: pre-existing bugs become their own tickets; a pass goes to
// security; changes go back to the worker until its attempts run out
// (counted since the last passed review).
func (f *finisher) review(r Report) error {
	t := f.Ticket
	if f.committed(model.StageReview) {
		return nil
	}
	if err := f.filePreExisting(r); err != nil {
		return err
	}
	t.ReviewedHead = ticket.Some(f.Facts.Head)
	f.pin()
	switch {
	case r.Is("verdict", "pass"):
		f.move(model.ColSecurity, model.ReasonReviewPassed, "", "")
	case ticket.WorkRounds(t) >= model.MaxWorkAttempts:
		f.move(model.ColNeedsHuman, model.ReasonReviewExhausted, "review still asks for changes after the last attempt", "")
	default:
		f.move(model.ColReady, model.ReasonReviewChanges, "review asked for changes", "")
	}
	return nil
}

// filePreExisting is file_pre_existing: bugs a reviewer found that
// the change did not introduce go to triage as tickets from a peer, instead
// of being a reason to send the change back. Only the first MaxPreExisting
// entries count, whatever they hold.
func (f *finisher) filePreExisting(r Report) error {
	t := f.Ticket
	found := r.List("pre_existing")
	for _, v := range found[:min(len(found), MaxPreExisting)] {
		o, ok := v.Object()
		if !ok {
			continue
		}
		fr := Report{o: o}
		title := fr.Or("title", "")
		if textx.Strip(title) == "" {
			continue
		}
		id, err := f.id()
		if err != nil {
			return err
		}
		n := ticket.New(id, ticket.NewIn{
			Project: t.Project, Title: textx.Clip(textx.Strip(title), 150),
			Body: "Found while reviewing " + string(t.ID) + " (not introduced by it).\n\n" + textx.Clip(fr.Or("detail", ""), 3000),
			Door: string(model.DoorPeer), Column: string(model.ColTriage),
		}, f.Now)
		// new_ticket saved it, then the parent was added and it was saved again
		n.Updated = clock.Stamp(f.Now)
		ticket.Encode(n)
		n.Parent = ticket.Some(string(t.ID))
		f.out.File = append(f.out.File, n)
		ticket.Event(t, "filed", string(n.ID)+": pre-existing, found in review: "+textx.Clip(title, 120), "",
			ticket.Filed{Ticket: string(n.ID)}, f.Now)
	}
	return nil
}

// security is decided from the answers, not the agent's verdict
// (security_outcome).
func (f *finisher) security(r Report) {
	t := f.Ticket
	if f.committed(model.StageSecurity) {
		return
	}
	sr := security.Report{Verdict: ""}
	if v, ok := r.Value("verdict"); ok {
		sr.Verdict, _ = v.Str()
	}
	if list, ok := r.Value("answers"); ok {
		if a, isList := list.Array(); isList {
			sr.Answers = make([]security.Answer, len(a))
			for i, e := range a {
				o, isObj := e.Object()
				if !isObj {
					continue
				}
				ar := Report{o: o}
				sr.Answers[i] = security.Answer{Object: true, Concern: ar.Str("concern"), Answer: ar.Str("answer")}
			}
		}
	}
	res := security.Outcome(t.SecurityTier.Or(4), t.SecurityQuestions, sr)
	if res.Pass {
		f.move(model.ColGate, model.ReasonSecurityPassed, "", res.Rule)
		return
	}
	f.move(model.ColNeedsHuman, model.ReasonSecurityFailed, res.Detail, res.Rule)
}
