package model

// Stage is one agent role's run on a ticket.
type Stage string

// The stages, in the order of TEMPLATES.
const (
	StageTriage   Stage = "triage"
	StageWork     Stage = "work"
	StageReview   Stage = "review"
	StageSecurity Stage = "security"
	StageTask     Stage = "task"
	StageDesign   Stage = "design"
	StageProspect Stage = "prospect"
	StageVote     Stage = "vote"
	StageManage   Stage = "manage"
	StageVerify   Stage = "verify"
)

// StageInfo is the static part of a stage (§2.4.1). Prompts and finishers
// are attached by name in package stage.
type StageInfo struct {
	Name          Stage
	Template      string // the default Scion template; [templates] overrides it (TEMPLATES)
	Role          string // the memory role; "" for voters and the verifier, who start blind (MEMORY_ROLES)
	Suffix        byte   // the letter in agent names: 'w' in t0042-w1 (AGENT_SUFFIX)
	WaitColumn    Column // where it waits to run (STAGE_FOR_COLUMN); "" when its kind decides
	RunningColumn Column // where the ticket sits while it runs (RUNNING_COLUMN)
	RetryColumn   Column // where a retry sends the ticket (RETRY_FOR_STAGE)
	Branch        bool   // gets a bot/ branch; false for NO_BRANCH_STAGES
	MayAsk        bool   // may stop with status "question" (QUESTION_STAGES)
	Label         string // STAGE_LABELS
}

// Stages are the stages in order.
func Stages() []StageInfo {
	return []StageInfo{
		{StageTriage, "triage", "triage", 't', ColTriage, ColTriage, ColTriage, false, true, "Triage"},
		{StageWork, "worker", "worker", 'w', ColReady, ColWorking, ColReady, true, true, "Worker"},
		{StageReview, "reviewer", "reviewer", 'r', ColReview, ColReview, ColReview, true, false, "Reviewer"},
		{StageSecurity, "security", "security", 's', ColSecurity, ColSecurity, ColSecurity, true, false, "Security"},
		{StageTask, "task", "task", 'k', "", ColWorking, ColReady, false, true, "Task agent"},
		{StageDesign, "designer", "designer", 'd', "", ColWorking, ColReady, false, true, "Designer"},
		{StageProspect, "prospector", "prospector", 'p', "", ColWorking, ColReady, false, false, "Prospector"},
		{StageVote, "voter", "", 'v', "", ColVoting, ColVoting, false, false, "Voter"},
		{StageManage, "manager", "manager", 'm', "", ColWorking, ColReady, false, true, "Manager"},
		// the verifier re-checks a finding task's findings, independently
		// of the agent that reported them: no memory, no branch, no commits
		{StageVerify, "verifier", "", 'f', ColVerifying, ColVerifying, ColVerifying, false, false, "Verifier"},
	}
}

// StageInfoOf is the row of one stage.
func StageInfoOf(s Stage) (StageInfo, bool) {
	for _, info := range Stages() {
		if info.Name == s {
			return info, true
		}
	}
	return StageInfo{}, false
}

// StageForColumn is the stage that runs next from a waiting column for a
// change (STAGE_FOR_COLUMN).
func StageForColumn(c Column) (Stage, bool) {
	for _, info := range Stages() {
		if info.WaitColumn != "" && info.WaitColumn == c {
			return info.Name, true
		}
	}
	return "", false
}

// NextStage is the stage a factory ticket of kind runs next from column,
// if any (stage_for, F:155, without its board check): one in verifying is
// verified, whatever its kind; task, intent and manage run their one stage
// from ready; a prospect is prospected from ready and voted on in voting;
// a proposal waits for a person; a change follows its column
// (StageForColumn). The gate column runs no stage: the tick gates it.
func NextStage(kind, column string) (Stage, bool) {
	if column == string(ColVerifying) {
		// a finding task's findings, whatever its kind
		return StageVerify, true
	}
	if s, ok := SingleStage(kind); ok {
		if column != string(ColReady) {
			return "", false
		}
		return s, true
	}
	switch kind {
	case string(KindProspect):
		switch column {
		case string(ColReady):
			return StageProspect, true
		case string(ColVoting):
			return StageVote, true
		}
		return "", false
	case string(KindProposal):
		return "", false
	}
	return StageForColumn(Column(column))
}

// StageOfColumn is the stage whose run a ticket was in when it left column
// c (Foreman's stageOfColumn): the report to read for a stuck ticket.
func StageOfColumn(c Column) (Stage, bool) {
	switch c {
	case ColWorking:
		return StageWork, true
	case ColReview:
		return StageReview, true
	case ColSecurity:
		return StageSecurity, true
	case ColTriage:
		return StageTriage, true
	case ColVoting:
		return StageVote, true
	case ColVerifying:
		return StageVerify, true
	}
	return "", false
}
