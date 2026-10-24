package model

// Tier is a rights tier: who may make a change the manager proposes. The
// tick decides it from the change itself, never from the manager's claim.
type Tier string

// The rights tiers.
const (
	TierAuto  Tier = "auto"  // the tick applies it by itself
	TierTap   Tier = "tap"   // it waits for the maintainer's "Apply it"
	TierNever Tier = "never" // only the maintainer, by hand; the tick refuses it
)

// TierLabel is how the desk names a tier. An unknown tier reads as one
// that needs a tap, never as one that applies by itself.
func TierLabel(t Tier) string {
	switch t {
	case TierAuto:
		return "applies by itself"
	case TierNever:
		return "only you, by hand"
	}
	return "needs your tap"
}

// ActionKind is a typed operation the manager may propose beside its
// config drafts.
type ActionKind string

// The action kinds. A restart is not one on purpose.
const (
	ActionPauseHost     ActionKind = "pause_host"
	ActionResumeHost    ActionKind = "resume_host"
	ActionPauseProject  ActionKind = "pause_project"
	ActionEnableProject ActionKind = "enable_project"
	ActionDeploy        ActionKind = "deploy"
	ActionRebuild       ActionKind = "rebuild"
)

// ActionInfo is one action kind with its label, its fixed rights tier and
// what its target names ("host", "project", or "" for none).
type ActionInfo struct {
	Kind   ActionKind
	Label  string
	Tier   Tier
	Target string
}

// OperatorActions are the action kinds the manager may propose. Any other
// kind is refused.
func OperatorActions() []ActionInfo {
	return []ActionInfo{
		{ActionPauseHost, "pause a host", TierAuto, "host"},
		{ActionResumeHost, "resume a host", TierAuto, "host"},
		{ActionPauseProject, "pause a project", TierAuto, "project"},
		{ActionDeploy, "deploy main", TierAuto, ""},
		{ActionEnableProject, "switch a project on", TierTap, "project"},
		{ActionRebuild, "rebuild the images", TierTap, ""},
	}
}

// ActionOf is the row of one action kind, matched exactly.
func ActionOf(kind string) (ActionInfo, bool) {
	for _, a := range OperatorActions() {
		if string(a.Kind) == kind {
			return a, true
		}
	}
	return ActionInfo{}, false
}

// The states of an operator item (a manager's draft or action) on its
// ticket.
const (
	ItemNew       = "new"       // recorded by the finish, not yet classified by the tick
	ItemWaiting   = "waiting"   // waits for the maintainer's tap
	ItemRequested = "requested" // tapped; the tick applies it on its next pass
	ItemApplied   = "applied"
	ItemRefused   = "refused"
	ItemFailed    = "failed"  // the tick tried and failed; Apply it again retries
	ItemByHand    = "by_hand" // the factory does not land drafts; the maintainer applies it
)
