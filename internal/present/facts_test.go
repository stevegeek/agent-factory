package present

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func factLine(fs []Fact) string {
	var out []string
	for _, f := range fs {
		s := f.Label + "=" + f.Value
		if f.Tone != "" {
			s += "(" + f.Tone + ")"
		}
		if f.URL != "" {
			s += "<" + f.URL + ">"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

func TestFactsOfTheFixtures(t *testing.T) {
	ts := fixtures(t)
	all := ByID(ts)
	for id, want := range map[string]string{
		"T0001": "From=issue #17 (GitHub)<https://github.com/example/widgets/issues/17>; " +
			"Pull request=#23 (draft)<https://github.com/example/widgets/pull/23>; Size=40 lines, 2 files(warn); " +
			"Changes behaviour=yes(warn); Review=pass; Security=Tier 2 (input reaches a sink): 2 questions, none above low",
		"T0006": "Panel scores=; Panel=kept: 3 of 3 voters scored 4 or more (2 needed); mean 4.3 (3.5 needed)",
		"T0007": "Retry from=the gate; Changes behaviour=yes(warn); Review=pass; Security=2 questions, none above low",
	} {
		x := all[ticket.ID(id)]
		got := factLine(Facts(x, AskFor(x), all))
		if !strings.HasPrefix(got, want) {
			t.Errorf("%s facts\n got %s\nwant %s", id, got, want)
		}
	}
	if v := Facts(find(ts, "T0006"), AskFor(find(ts, "T0006")), all)[0].Votes; !reflect.DeepEqual(v, []int{4, 5, 4}) {
		t.Errorf("panel votes %v", v)
	}
	// a ticket with nothing to say says where it is
	if got := factLine(Facts(find(ts, "T0011"), nil, all)); got != "Column=Working" {
		t.Errorf("running ticket facts %q", got)
	}
}

func TestFactsOfAGateAndAProspect(t *testing.T) {
	failed := tk(t, `{"id": "T0060", "column": "needs_human", "gate": {"checks_pass": false, "files": ["a", "b"], "changed_lines": 12,
		"within_size": true, "auto_eligible": false, "blockers": []}, "pr": {"number": 4, "url": "javascript:x", "draft": false}}`)
	if got := factLine(Facts(failed, nil, nil)); got != "Pull request=#4; Tests and lint=fail(bad); Size=12 lines, 2 files(good)" {
		t.Errorf("failed gate: %s", got)
	}
	passed := tk(t, `{"id": "T0061", "column": "ready_for_pr", "gate": {"checks_pass": true, "files": ["a"], "changed_lines": 3,
		"within_size": true, "auto_eligible": false, "blockers": ["x"]}}`)
	if got := factLine(Facts(passed, nil, nil)); got != "Size=3 lines, 1 file(good); Auto-merge=no: needs a maintainer(warn)" {
		t.Errorf("passed gate: %s", got)
	}
	prospect := tk(t, `{"id": "T0062", "kind": "prospect", "column": "done", "proposals": [{}, {}, {}],
		"panel_result": {"kept": ["T0063"], "dropped": ["x", "y"]}}`)
	if got := factLine(Facts(prospect, nil, nil)); got != "Proposals=3; Kept=1(good); Dropped=2" {
		t.Errorf("prospect: %s", got)
	}
}

func TestNoteButtonAndDraftReasons(t *testing.T) {
	x := find(fixtures(t), "T0001")
	c := PRCallout(x)
	if c == nil || c.Tone != "warn" || c.Title != "Why this PR is a draft" {
		t.Fatalf("callout %+v", c)
	}
	want := []string{"1 file outside the paths that may merge automatically (docs, tests, config)",
		"40 changed lines; the limit for automatic approval is 10"}
	if !reflect.DeepEqual(c.Lines, want) {
		t.Errorf("draft reasons\n got %q\nwant %q", c.Lines, want)
	}
	if got := DraftReasons([]string{"reviewer says runtime behaviour changes (yes)", "files outside allowed paths: a, b, c", "odd one"}); !reflect.DeepEqual(got,
		[]string{"The reviewer says it changes how the code behaves at runtime",
			"3 files outside the paths that may merge automatically (docs, tests, config)", "Odd one"}) {
		t.Errorf("reasons %q", got)
	}
	y := ticket.Clone(x)
	pr := y.PR.V
	pr.Draft = ticket.Some(false)
	y.PR = ticket.Some(pr)
	if c := PRCallout(y); c == nil || c.Tone != "good" || c.Title != "Passed every check for automatic approval" {
		t.Errorf("a PR that is not a draft passed: %+v", c)
	}
	y.Column = "ready_for_pr"
	if PRCallout(y) != nil {
		t.Errorf("only an open PR has a callout")
	}
}

func TestSafeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r/pull/1": "https://github.com/o/r/pull/1",
		" http://example.com/x ":        "http://example.com/x",
		"javascript:alert(1)":           "",
		"//example.com":                 "",
		"https://":                      "",
		"ftp://example.com":             "",
		"data:text/html,x":              "",
	} {
		if got := SafeURL(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// The label half of Foreman's TestLabelsAndAnswers, and the model coverage of
// TestForemanCoversTheDispatchersModel: every column, board column and stage
// has a label, from the one model.
func TestLabels(t *testing.T) {
	if l := TicketColumnLabel(tk(t, `{"id": "T0001", "column": "rejected", "kind": "proposal"}`)); l != "Dropped" {
		t.Errorf("dropped proposal label %q", l)
	}
	if l := TicketColumnLabel(tk(t, `{"id": "T0001", "column": "rejected"}`)); l != "Closed" {
		t.Errorf("rejected change label %q", l)
	}
	if ColumnLabel("rejected") != "Closed" {
		t.Errorf("rejected reads %q", ColumnLabel("rejected"))
	}
	if ColumnLabel("nowhere") != "nowhere" {
		t.Errorf("an unknown column reads as itself")
	}
	for _, c := range model.Columns() {
		if ColumnLabel(string(c.Name)) == "" || ColumnLabel(string(c.Name)) == string(c.Name) && c.Label != string(c.Name) {
			t.Errorf("column %q has no label", c.Name)
		}
	}
	for _, c := range model.BoardColumns() {
		if ColumnLabel(string(c.Name)) != c.Label {
			t.Errorf("board column %q has no label of its own", c.Name)
		}
	}
	for _, s := range model.Stages() {
		if RoleLabel(string(s.Name)) == "" || RoleLabel(string(s.Name)) != s.Label {
			t.Errorf("stage %q has no label", s.Name)
		}
	}
	if RoleLabel("oddity") != "Oddity" {
		t.Errorf("an unknown stage is named by itself: %q", RoleLabel("oddity"))
	}
	for i, want := range model.TierNames() {
		x := tk(t, `{"id": "T0001", "security_tier": `+string(rune('0'+i))+`, "reports": {"security": {"answers": []}}}`)
		if got := SecurityLine(x); got != "Tier "+string(rune('0'+i))+" ("+want+"): 0 questions." {
			t.Errorf("tier %d: %q", i, got)
		}
	}
}

// Agents write reports, so their fields are read loosely, as Foreman did.
func TestReportsReadLoosely(t *testing.T) {
	x := tk(t, `{"id": "T0001", "reports": {"work": {"summary": "  padded  ", "n": 3, "f": 2.5, "b": true, "o": {"a": 1},
		"l": [1, 2], "z": null}, "odd": "not an object", "gone": null}}`)
	for key, want := range map[string]string{"summary": "padded", "n": "3", "f": "2.5", "b": "true", "o": "", "l": "", "z": "", "missing": ""} {
		if got := ReportField(x, "work", key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
	if reportOf(x, "odd") != nil || reportOf(x, "gone") != nil || reportOf(x, "none") != nil {
		t.Errorf("a report that is not an object is not there")
	}
	if r := parseReport([]byte(`{"summary": " s ", "n": 1e21, "answers": [{"concern": "low"}, 3]}`)); r.str("summary") != "s" ||
		r.str("n") != "1e+21" || len(r.list("answers")) != 2 || r.list("answers")[0]["concern"] != "low" {
		t.Errorf("archived report %+v", r)
	}
	if parseReport([]byte(`[1]`)) != nil || parseReport([]byte(`nope`)) != nil || parseReport([]byte(`null`)) != nil {
		t.Errorf("an archived report that is not an object is not there")
	}
}

func TestIDLess(t *testing.T) {
	if !IDLess("T0999", "T1000") || !IDLess("T9999", "T10000") || IDLess("T10000", "T9999") || IDLess("T0001", "T0001") {
		t.Errorf("id order")
	}
}

func TestSecurityLine(t *testing.T) {
	x := tk(t, `{"id": "T0001", "reports": {"security": {"verdict": "fail", "answers": [
		{"concern": "LOW"}, {"concern": "High"}, "not an object", {"concern": 3}]}}}`)
	if got := SecurityLine(x); got != "4 questions, worst high. Verdict: fail." {
		t.Errorf("security %q", got)
	}
}
