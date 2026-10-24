// Package model is the one definition of the factory's board, as plain data:
// the columns, stages, kinds, doors, ask kinds, move reasons, typed event
// schema, modes, capabilities and tiers, everything `factory model` prints,
// plus the constants Foreman used to hard-code. Every table is a function
// returning a fresh slice, so no caller can change the model for another.
// Behaviour that needs other packages (prompts, finishers, answers) is
// attached by name in the package that owns it, with a completeness test
// there.
package model

// Column is a factory column: where a ticket waits or runs.
type Column string

// The factory columns, in board order (COLUMNS).
const (
	ColTriage     Column = "triage"
	ColReady      Column = "ready"
	ColWorking    Column = "working"
	ColVoting     Column = "voting"
	ColVerifying  Column = "verifying"
	ColReview     Column = "review"
	ColSecurity   Column = "security"
	ColPublic     Column = "public"
	ColGate       Column = "gate"
	ColReadyForPR Column = "ready_for_pr"
	ColPROpen     Column = "pr_open"
	ColNeedsHuman Column = "needs_human"
	ColDone       Column = "done"
	ColRejected   Column = "rejected"
)

// BoardColumn is a board project's column.
type BoardColumn string

// The board columns, in order (BOARD_COLUMNS).
const (
	BoardInbox   BoardColumn = "inbox"
	BoardNext    BoardColumn = "next"
	BoardDoing   BoardColumn = "doing"
	BoardWaiting BoardColumn = "waiting"
	BoardDone    BoardColumn = "done"
	BoardDropped BoardColumn = "dropped"
)

// Kind is a ticket kind. A factory change is written as kind null.
type Kind string

// The kinds (KINDS).
const (
	KindChange   Kind = "change"
	KindTask     Kind = "task"
	KindIntent   Kind = "intent"
	KindProspect Kind = "prospect"
	KindProposal Kind = "proposal"
	KindManage   Kind = "manage"
	KindItem     Kind = "item"
)

// Mode is a project mode.
type Mode string

// The modes (MODES).
const (
	ModeFactory Mode = "factory"
	ModeBoard   Mode = "board"
)

// Capability is what an API token may be granted.
type Capability string

// The capabilities (CAPABILITIES), in order.
const (
	CapRead Capability = "read"
	CapFile Capability = "file"
	CapTalk Capability = "talk"
	CapWork Capability = "work"
	CapPlan Capability = "plan"
)

// NoticeProject is the project of notices about the factory itself; it is a
// factory project even when the config does not name it.
const NoticeProject = "factory"

// LocalHost is this host's name in host routing, agent records and pauses.
const LocalHost = "local"

// MaxWorkAttempts is how many worker starts a ticket gets since its last
// passed review before a review that still asks for changes goes to a
// person (ticket.WorkRounds).
const MaxWorkAttempts = 2

// The panel's keep rule (KEEN_SCORE, KEEN_NEEDED, MEAN_NEEDED): a proposal
// survives with PanelKeenNeeded voters scoring PanelKeenScore or more, or a
// mean score of PanelMeanNeeded.
const (
	PanelKeenScore  = 4
	PanelKeenNeeded = 2
	PanelMeanNeeded = 3.5
)

// ColumnInfo is one column and the label people see.
type ColumnInfo struct {
	Name  Column
	Label string
}

// BoardColumnInfo is one board column and its label.
type BoardColumnInfo struct {
	Name  BoardColumn
	Label string
}

// Columns are the factory columns in board order (COLUMNS, COLUMN_LABELS).
func Columns() []ColumnInfo {
	return []ColumnInfo{
		{ColTriage, "Triage"}, {ColReady, "Ready"}, {ColWorking, "Working"}, {ColVoting, "Voting"},
		{ColVerifying, "Verifying"}, {ColReview, "Review"}, {ColSecurity, "Security"}, {ColPublic, "Public review"}, {ColGate, "the gate"}, {ColReadyForPR, "Ready for PR"},
		{ColPROpen, "PR open"}, {ColNeedsHuman, "Needs you"}, {ColDone, "Done"}, {ColRejected, "Closed"},
	}
}

// BoardColumns are the board columns in order (BOARD_COLUMNS, BOARD_COLUMN_LABELS).
func BoardColumns() []BoardColumnInfo {
	return []BoardColumnInfo{
		{BoardInbox, "Inbox"}, {BoardNext, "Next"}, {BoardDoing, "Doing"}, {BoardWaiting, "Waiting"},
		{BoardDone, "Done"}, {BoardDropped, "Dropped"},
	}
}

// IsColumn reports a factory column.
func IsColumn(s string) bool {
	for _, c := range Columns() {
		if string(c.Name) == s {
			return true
		}
	}
	return false
}

// IsBoardColumn reports a board column.
func IsBoardColumn(s string) bool {
	for _, c := range BoardColumns() {
		if string(c.Name) == s {
			return true
		}
	}
	return false
}

// ColumnLabel is the label of any column, factory or board ("done" is in
// both, with one label), or the name itself when it is not a column.
func ColumnLabel(s string) string {
	for _, c := range Columns() {
		if string(c.Name) == s {
			return c.Label
		}
	}
	for _, c := range BoardColumns() {
		if string(c.Name) == s {
			return c.Label
		}
	}
	return s
}

// Closed reports a column a ticket is finished in (CLOSED_COLUMNS).
func Closed(column string) bool {
	return column == string(ColDone) || column == string(ColRejected) || column == string(BoardDropped)
}

// AskColumns are the columns where a ticket asks a person something
// (ASK_COLUMNS).
func AskColumns() []Column { return []Column{ColNeedsHuman, ColPROpen, ColReadyForPR} }

// StepName is what the step a column stands for is called in an unstick
// question (STEP_NAMES), and whether the column is a step at all.
func StepName(column string) (string, bool) {
	switch Column(column) {
	case ColTriage:
		return "triage", true
	case ColReady, ColWorking:
		return "work", true
	case ColVoting:
		return "panel vote", true
	case ColVerifying:
		return "verify", true
	case ColReview:
		return "review", true
	case ColSecurity:
		return "security check", true
	case ColPublic:
		return "public review", true
	case ColGate:
		return "gate checks", true
	case ColReadyForPR, ColPROpen:
		return "PR", true
	}
	return "", false
}

// RetryColumnFor is where a retry sends a ticket that got stuck after
// leaving column (RETRY_COLUMN.get(old, old)): working retries from ready,
// every other step from itself.
func RetryColumnFor(column string) string {
	if column == string(ColWorking) {
		return string(ColReady)
	}
	return column
}

// KindInfo is one kind in one mode, and the single stage it runs from
// ready, if it is a single-stage kind (SINGLE_STAGE_KINDS).
type KindInfo struct {
	Name  Kind
	Mode  Mode
	Stage Stage
}

// Kinds are the kinds of each mode, in order (KINDS).
func Kinds() []KindInfo {
	return []KindInfo{
		{KindChange, ModeFactory, ""}, {KindTask, ModeFactory, StageTask}, {KindIntent, ModeFactory, StageDesign},
		{KindProspect, ModeFactory, ""}, {KindProposal, ModeFactory, ""}, {KindManage, ModeFactory, StageManage},
		{KindItem, ModeBoard, ""}, {KindTask, ModeBoard, ""},
	}
}

// SingleStage is the one stage a factory kind runs from ready (task, intent,
// manage), if it has one.
func SingleStage(k string) (Stage, bool) {
	for _, info := range Kinds() {
		if info.Mode == ModeFactory && string(info.Name) == k && info.Stage != "" {
			return info.Stage, true
		}
	}
	return "", false
}

// ModeInfo is one project mode: its columns, kinds, the capabilities a
// token may hold on it, and whether the pipeline runs its tickets.
type ModeInfo struct {
	Mode         Mode
	Columns      []string
	Kinds        []Kind
	Capabilities []Capability
	Pipeline     bool
}

// Modes are the project modes (MODES).
func Modes() []ModeInfo {
	var out []ModeInfo
	for _, m := range []Mode{ModeFactory, ModeBoard} {
		info := ModeInfo{Mode: m, Capabilities: Capabilities(), Pipeline: m == ModeFactory}
		if m == ModeFactory {
			for _, c := range Columns() {
				info.Columns = append(info.Columns, string(c.Name))
			}
		} else {
			for _, c := range BoardColumns() {
				info.Columns = append(info.Columns, string(c.Name))
			}
		}
		for _, k := range Kinds() {
			if k.Mode == m {
				info.Kinds = append(info.Kinds, k.Name)
			}
		}
		out = append(out, info)
	}
	return out
}

// Capabilities are what an API token may be granted, in order.
func Capabilities() []Capability {
	return []Capability{CapRead, CapFile, CapTalk, CapWork, CapPlan}
}

// ReservedNames are RESERVED_NAMES: no token, and no person but the
// default "maintainer", may be called these. config reads this list;
// tokens, which may not import model, keeps a copy that a cli test ties
// to it.
func ReservedNames() []string { return []string{"factory", "maintainer", "human", "system", "me"} }

// DefaultPeople is [board] people when the config names none.
func DefaultPeople() []string { return []string{"maintainer"} }

// TierNames are the security tiers' names, by tier number (TIER_NAMES).
// Python's wording wins over Foreman's ("plain code", not "inert code").
func TierNames() []string {
	return []string{"text only", "plain code", "input reaches a sink", "build or dependencies",
		"input reaches a sink, and build or dependencies"}
}
