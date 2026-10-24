package model

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// encodeLikePython is json.dumps(m, indent=2, sort_keys=True) plus print's
// newline: encoding/json sorts map keys, and the model holds only ASCII.
func encodeLikePython(t *testing.T, m Model) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(generic); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// testdata/model.json is what `factory model` prints: a regression golden,
// written from Export and checked in. It began as the old Foreman's
// model.json, but the public review column is Go's own, so it no longer
// shows parity with Python's; a change to the model updates it in the
// same commit. The Python test checked that a checked-in copy of the model
// is current, which is all this one does now.
//
// port: TF ModelTest.test_foremans_copy_of_the_model_is_current
func TestExportIsTheModelGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "model.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := encodeLikePython(t, Export())
	if !bytes.Equal(bytes.TrimRight(got, "\n"), bytes.TrimRight(want, "\n")) {
		t.Errorf("model.Export() differs from testdata/model.json:\n got %s\nwant %s", got, want)
	}
}

func TestTablesAreComplete(t *testing.T) {
	cols := map[Column]bool{}
	for _, c := range Columns() {
		if c.Label == "" {
			t.Errorf("column %q has no label", c.Name)
		}
		if cols[c.Name] {
			t.Errorf("column %q twice", c.Name)
		}
		cols[c.Name] = true
		if !IsColumn(string(c.Name)) || ColumnLabel(string(c.Name)) != c.Label {
			t.Errorf("column %q: IsColumn/ColumnLabel disagree with the table", c.Name)
		}
	}
	if len(cols) != 14 {
		t.Errorf("%d factory columns, want 14", len(cols))
	}
	for _, c := range BoardColumns() {
		if c.Label == "" || ColumnLabel(string(c.Name)) != c.Label {
			t.Errorf("board column %q has no label of its own", c.Name)
		}
		if !IsBoardColumn(string(c.Name)) {
			t.Errorf("IsBoardColumn(%q) is false", c.Name)
		}
	}
	suffixes := map[byte]Stage{}
	for _, s := range Stages() {
		if s.Label == "" || s.Template == "" {
			t.Errorf("stage %q lacks a label or template", s.Name)
		}
		if other, dup := suffixes[s.Suffix]; dup {
			t.Errorf("stages %q and %q share suffix %q", s.Name, other, s.Suffix)
		}
		suffixes[s.Suffix] = s.Name
		if !cols[s.RunningColumn] || !cols[s.RetryColumn] {
			t.Errorf("stage %q runs in %q and retries from %q: not both columns", s.Name, s.RunningColumn, s.RetryColumn)
		}
		if s.WaitColumn != "" && !cols[s.WaitColumn] {
			t.Errorf("stage %q waits in %q, not a column", s.Name, s.WaitColumn)
		}
		if got, ok := StageInfoOf(s.Name); !ok || got.Name != s.Name {
			t.Errorf("StageInfoOf(%q) = %v, %v", s.Name, got, ok)
		}
		if s.Role == "voter" || s.Role == "verifier" || (s.Name == StageVote || s.Name == StageVerify || s.Name == StagePublic) != (s.Role == "") {
			t.Errorf("stage %q has memory role %q: only voters, the verifier and public review start blind", s.Name, s.Role)
		}
	}
	for c, s := range map[Column]Stage{ColWorking: StageWork, ColReview: StageReview, ColSecurity: StageSecurity, ColPublic: StagePublic, ColTriage: StageTriage, ColVoting: StageVote, ColVerifying: StageVerify} {
		if got, ok := StageOfColumn(c); !ok || got != s {
			t.Errorf("StageOfColumn(%q) = %q, %v; want %q", c, got, ok, s)
		}
	}
	if _, ok := StageOfColumn(ColDone); ok {
		t.Error("done has a stage")
	}
	order := map[int]bool{}
	for _, k := range AskKinds() {
		if k.Label == "" || k.Glyph == "" {
			t.Errorf("ask kind %q has no label or glyph", k.Kind)
		}
		if order[k.DeskOrder] {
			t.Errorf("ask kind %q shares desk position %d", k.Kind, k.DeskOrder)
		}
		order[k.DeskOrder] = true
		if info, ok := AskKindOf(k.Kind); !ok || info != k {
			t.Errorf("AskKindOf(%q) = %v", k.Kind, info)
		}
	}
	reasons := map[Reason]bool{}
	for _, r := range Reasons() {
		if reasons[r.Reason] {
			t.Errorf("reason %q twice", r.Reason)
		}
		reasons[r.Reason] = true
		if !IsReason(string(r.Reason)) {
			t.Errorf("IsReason(%q) is false", r.Reason)
		}
	}
	if len(reasons) != 66 {
		t.Errorf("%d reasons, want 66", len(reasons))
	}
	if IsReason("") || IsReason("nonsense") {
		t.Error("IsReason accepts a reason that is not one")
	}
	for _, k := range Kinds() {
		if k.Stage != "" {
			if _, ok := StageInfoOf(k.Stage); !ok {
				t.Errorf("kind %q runs unknown stage %q", k.Name, k.Stage)
			}
		}
	}
	for _, m := range Modes() {
		if len(m.Columns) == 0 || len(m.Kinds) == 0 {
			t.Errorf("mode %q has no columns or kinds", m.Mode)
		}
	}
	for _, d := range Doors() {
		if d.Label == "" {
			t.Errorf("door %q has no label", d.Name)
		}
		if d.Untrusted && d.PromptNote == "" {
			t.Errorf("untrusted door %q has no prompt note", d.Name)
		}
		if d.Untrusted && !d.FlagMemory {
			t.Errorf("untrusted door %q does not flag memory changes", d.Name)
		}
	}
	if n := TierNames(); len(n) != 5 || n[1] != "plain code" {
		t.Errorf("TierNames() = %q", n)
	}
	for _, e := range EventTypes() {
		if len(e.Fields) == 0 {
			t.Errorf("event type %q has no fields", e.Type)
		}
	}
}

// Foreman reads these typed events; the dispatcher must keep writing them.
func TestForemansTypedEventsAreInTheModel(t *testing.T) {
	have := map[EventType]bool{}
	for _, e := range EventTypes() {
		have[e.Type] = true
	}
	for _, e := range []EventType{EventCreated, EventMoved, EventStarted, EventFinished, EventMemory, EventNoted, EventAsked, EventAnswered} {
		if !have[e] {
			t.Errorf("typed %q is not in the model", e)
		}
	}
}

// The tables are fresh copies: a caller changing one cannot change the model.
func TestTablesAreFreshCopies(t *testing.T) {
	c := Columns()
	c[0].Label = "changed"
	s := Stages()
	s[0].Label = "changed"
	m := Modes()
	m[0].Columns[0] = "changed"
	e := EventTypes()
	e[0].Fields[0] = "changed"
	if Columns()[0].Label == "changed" || Stages()[0].Label == "changed" || Modes()[0].Columns[0] == "changed" || EventTypes()[0].Fields[0] == "changed" {
		t.Error("a table shares its backing array between calls")
	}
	x := Export()
	x.ColumnLabels["triage"] = "changed"
	if Export().ColumnLabels["triage"] == "changed" {
		t.Error("Export shares its maps between calls")
	}
}

func TestStepNamesAndRetry(t *testing.T) {
	want := map[string]string{"triage": "triage", "ready": "work", "working": "work", "voting": "panel vote", "verifying": "verify", "review": "review",
		"security": "security check", "public": "public review", "gate": "gate checks", "ready_for_pr": "PR", "pr_open": "PR"}
	for c, n := range want {
		if got, ok := StepName(c); !ok || got != n {
			t.Errorf("StepName(%q) = %q, %v; want %q", c, got, ok, n)
		}
	}
	for _, c := range []string{"needs_human", "done", "rejected", ""} {
		if _, ok := StepName(c); ok {
			t.Errorf("StepName(%q) is a step", c)
		}
	}
	if RetryColumnFor("working") != "ready" || RetryColumnFor("gate") != "gate" {
		t.Error("RetryColumnFor: working retries from ready, anything else from itself")
	}
	closed := []string{}
	for _, c := range []string{"done", "rejected", "dropped", "ready", "inbox"} {
		if Closed(c) {
			closed = append(closed, c)
		}
	}
	sort.Strings(closed)
	if len(closed) != 3 {
		t.Errorf("closed columns %v", closed)
	}
}

// The findings flow: a task with findings waits in verifying for the
// verifier, which runs there, blind, without a branch, and is retried from
// there; its findings come back to the desk as their own ask kind, and the
// tickets it files come in through an untrusted door.
func TestFindingsFlowIsInTheModel(t *testing.T) {
	info, ok := StageInfoOf(StageVerify)
	if !ok || info.WaitColumn != ColVerifying || info.RunningColumn != ColVerifying || info.RetryColumn != ColVerifying ||
		info.Branch || info.MayAsk || info.Role != "" || info.Template != "verifier" {
		t.Errorf("verify stage = %+v", info)
	}
	if s, ok := StageForColumn(ColVerifying); !ok || s != StageVerify {
		t.Errorf("StageForColumn(verifying) = %q, %v", s, ok)
	}
	if ColumnLabel(string(ColVerifying)) != "Verifying" {
		t.Errorf("verifying is labelled %q", ColumnLabel(string(ColVerifying)))
	}
	if k, ok := AskKindOf(AskFindings); !ok || k.Label != "Findings" {
		t.Errorf("findings ask = %+v, %v", k, ok)
	}
	d, ok := DoorOf(string(DoorFinding))
	if !ok || !d.Untrusted || !d.FlagMemory || d.DefaultColumn != ColTriage {
		t.Errorf("finding door = %+v, %v", d, ok)
	}
	if d, ok := DoorOf(string(DoorImport)); !ok || !d.Untrusted || !d.FlagMemory || d.PromptNote == "" {
		t.Errorf("import door = %+v, %v", d, ok)
	}
	for r, flagged := range map[Reason]bool{ReasonFindingsReported: false, ReasonVerifyRequested: false, ReasonFindingsVerified: false,
		ReasonVerifierBlocked: true, ReasonFindingsFiled: false, ReasonFindingsDismissed: false, ReasonImported: false} {
		found := false
		for _, info := range Reasons() {
			if info.Reason == r {
				found = true
				if info.Flagged != flagged {
					t.Errorf("reason %s flagged %v", r, info.Flagged)
				}
			}
		}
		if !found {
			t.Errorf("reason %s is not in the model", r)
		}
	}
}

// A held deploy asks after Open PR, before the config drafts; its reasons
// are plain.
func TestDeployAskIsInTheModel(t *testing.T) {
	var kinds []AskKind
	for _, k := range AskKinds() {
		kinds = append(kinds, k.Kind)
	}
	want := []AskKind{AskQuestion, AskUnstick, AskReview, AskOpen, AskDeploy, AskDraft, AskStart, AskDesign, AskFindings, AskProposal, AskDecide, AskNotice}
	if !slices.Equal(kinds, want) {
		t.Errorf("ask kinds %q", kinds)
	}
	for i, k := range AskKinds() {
		if k.DeskOrder != i {
			t.Errorf("%s at desk position %d, want %d", k.Kind, k.DeskOrder, i)
		}
	}
	if k, ok := AskKindOf(AskDeploy); !ok || k != (AskKindInfo{AskDeploy, "Deploy", "DP", 4}) {
		t.Errorf("deploy ask = %+v, %v", k, ok)
	}
	for _, r := range []Reason{ReasonDeployHeld, ReasonDeployRequested, ReasonDeployStarted} {
		found := false
		for _, info := range Reasons() {
			if info.Reason == r {
				found = !info.Flagged
			}
		}
		if !found {
			t.Errorf("reason %s is not a plain reason of the model", r)
		}
	}
}

// The operator's actions: each kind once, the tiers they carry, and a
// kind that is not in the table (a restart) is not an action.
func TestOperatorActionsTable(t *testing.T) {
	want := map[ActionKind]struct {
		tier   Tier
		target string
	}{
		ActionPauseHost:     {TierAuto, "host"},
		ActionResumeHost:    {TierAuto, "host"},
		ActionPauseProject:  {TierAuto, "project"},
		ActionDeploy:        {TierAuto, ""},
		ActionEnableProject: {TierTap, "project"},
		ActionRebuild:       {TierTap, ""},
	}
	seen := map[ActionKind]bool{}
	for _, a := range OperatorActions() {
		if seen[a.Kind] {
			t.Errorf("action %q twice", a.Kind)
		}
		seen[a.Kind] = true
		w, ok := want[a.Kind]
		if !ok {
			t.Errorf("action %q is not in the table", a.Kind)
			continue
		}
		if a.Tier != w.tier || a.Target != w.target || a.Label == "" {
			t.Errorf("action %q = %+v, want tier %s target %q", a.Kind, a, w.tier, w.target)
		}
		if got, ok := ActionOf(string(a.Kind)); !ok || got != a {
			t.Errorf("ActionOf(%q) = %+v, %v", a.Kind, got, ok)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("%d actions, want %d", len(seen), len(want))
	}
	for _, k := range []string{"restart", "", "PAUSE_HOST", "pause_host "} {
		if a, ok := ActionOf(k); ok {
			t.Errorf("ActionOf(%q) = %+v, want none", k, a)
		}
	}
	if names := []string{ItemNew, ItemWaiting, ItemRequested, ItemApplied, ItemRefused, ItemFailed, ItemByHand}; !slices.Equal(names,
		[]string{"new", "waiting", "requested", "applied", "refused", "failed", "by_hand"}) {
		t.Errorf("item states %q", names)
	}
}

func TestTierLabels(t *testing.T) {
	for tier, want := range map[Tier]string{TierAuto: "applies by itself", TierTap: "needs your tap", TierNever: "only you, by hand"} {
		if got := TierLabel(tier); got != want {
			t.Errorf("TierLabel(%s) = %q, want %q", tier, got, want)
		}
	}
	// an unknown tier is never presented as one that applies by itself
	if got := TierLabel("bogus"); got != TierLabel(TierTap) {
		t.Errorf("TierLabel(bogus) = %q", got)
	}
}

func TestOperatorSettledIsPlainAndLast(t *testing.T) {
	rs := Reasons()
	last := rs[len(rs)-1]
	if last != (ReasonInfo{ReasonOperatorSettled, false}) || ReasonOperatorSettled != "operator_settled" {
		t.Errorf("last reason = %+v", last)
	}
}
