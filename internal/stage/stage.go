// Package stage is the behaviour of each stage: what its agent is told
// (Prompt) and what its report does to the ticket (Finish). The static
// part of a stage, its template, column, suffix and so on, is
// model.Stages(); this package attaches the behaviour by name.
//
// Both halves are pure. The pipeline gathers the facts first (git heads,
// the tier, the memory a run has, the parent of an Ask's task, the ids of
// the tickets an outcome files), calls the stage, and saves what comes
// back. Nothing here reads a file, runs a command or asks for the time.
//
// The pipeline's order around a finish is Python's finish_stage:
// remove the agent (keeping its branch when KeepBranch says so), settle its
// memory on the ticket, then Finish, which records the report, clears the
// agent, records the finished event and decides the move.
package stage

import (
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/security"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Behaviour is one stage's prompt and finisher.
type Behaviour struct {
	// Prompt is the whole prompt start_stage sends: the stage's own text,
	// the project facts, the question note and the memory prompt.
	Prompt func(PromptIn) string
	// Finish decides what the stage's report does to the ticket.
	Finish func(FinishIn) (Outcome, error)
}

// For is the behaviour of stage s; the zero Behaviour for a name that is
// not a stage.
func For(s model.Stage) Behaviour {
	var p func(PromptIn) string
	switch s {
	case model.StageTriage:
		p = triagePrompt
	case model.StageWork:
		p = workPrompt
	case model.StageReview:
		p = reviewPrompt
	case model.StageSecurity:
		p = securityPrompt
	case model.StageTask:
		p = taskPrompt
	case model.StageDesign:
		p = designPrompt
	case model.StageProspect:
		p = prospectPrompt
	case model.StageVote:
		p = votePrompt
	case model.StageManage:
		p = managePrompt
	case model.StageVerify:
		p = verifyPrompt
	default:
		return Behaviour{}
	}
	return Behaviour{
		Prompt: func(in PromptIn) string { return compose(s, in, p(in)) },
		Finish: func(in FinishIn) (Outcome, error) { return finishAs(s, in) },
	}
}

// ForTicket is stage_for: the stage a ticket runs next from where
// it waits, if any. A board ticket runs none; a factory ticket's is
// model.NextStage of its kind and column, which Foreman reads too.
func ForTicket(t *ticket.Ticket) (model.Stage, bool) {
	if t.Board() {
		return "", false
	}
	return model.NextStage(t.KindName(), t.Column)
}

// AgentName is the next agent's name for stage s on t: the lower-cased id,
// the stage's letter and its number of starts so far plus one
// ("t0042-w2"). It always matches the helper's job-name rule.
func AgentName(t *ticket.Ticket, s model.Stage) string {
	info, _ := model.StageInfoOf(s)
	return strings.ToLower(string(t.ID)) + "-" + string(info.Suffix) + strconv.Itoa(ticket.Starts(t, s)+1)
}

// KeepBranch reports whether removing the stage's agent keeps its branch:
// false for the stages that work without one (NO_BRANCH_STAGES).
func KeepBranch(s model.Stage) bool {
	info, _ := model.StageInfoOf(s)
	return info.Branch
}

// BeginIn is what the pipeline found out before a stage starts.
type BeginIn struct {
	Branch string // work: the branch to use when the ticket has none yet (pick_branch)
	Head   string // review and security: where the branch is now
	Tier   int    // security: the tier the classifier gave the change
}

// Begin records on t what start_stage records before it builds the prompt:
// a worker's branch; for review and security the head they start
// from, which a reviewer must leave where it is; for security the tier and
// the questions asked, frozen so that the answers are judged against them.
func Begin(t *ticket.Ticket, s model.Stage, in BeginIn) {
	switch s {
	case model.StageWork:
		if t.Branch.Or("") == "" {
			t.Branch = ticket.Some(in.Branch)
		}
	case model.StageReview:
		t.HeadBeforeReview = ticket.Some(in.Head)
	case model.StageSecurity:
		t.HeadBeforeSecurity = ticket.Some(in.Head)
		t.SecurityTier = ticket.Some(in.Tier)
		t.SecurityQuestions = security.Angles(in.Tier)
	}
}
