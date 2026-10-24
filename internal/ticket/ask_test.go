package ticket

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
)

// askJSON is an ask as JSON, for comparing with Python's.
func askJSON(a *Ask) string {
	if a == nil {
		return "null"
	}
	tk := &Ticket{tree: jsonx.ObjectOf()}
	tk.SetAsk(a)
	return string(jsonx.Format{Indent: -1}.Encode(tk.Tree().Get("ask")))
}

func ticketFrom(t *testing.T, s string) *Ticket {
	t.Helper()
	tk, problems, err := Decode([]byte(s))
	if err != nil || len(problems) > 0 {
		t.Fatalf("%s: %v %v", s, err, problems)
	}
	return tk
}

// Every case of testdata/asks/asks.json (Python's ask_for over a table of
// tickets, olds, details and reasons) gives Python's ask, byte for byte.
func TestAskForMatchesPython(t *testing.T) {
	b, err := os.ReadFile("testdata/asks/asks.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Since   string
		Tickets []json.RawMessage
		Cases   [][]json.RawMessage
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	tickets := make([]*Ticket, len(g.Tickets))
	for i, raw := range g.Tickets {
		tickets[i] = ticketFrom(t, string(raw))
	}
	kinds := map[string]bool{}
	fails := 0
	for _, c := range g.Cases {
		var idx int
		var old, detail, reason *string
		json.Unmarshal(c[0], &idx)
		json.Unmarshal(c[1], &old)
		json.Unmarshal(c[2], &detail)
		json.Unmarshal(c[3], &reason)
		want, err := jsonx.Parse(c[4])
		if err != nil {
			t.Fatal(err)
		}
		deref := func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		}
		a := AskFor(tickets[idx], deref(old), deref(detail), g.Since, model.Reason(deref(reason)))
		if a != nil {
			kinds[a.Kind] = true
		}
		if got := askJSON(a); got != string(jsonx.Format{Indent: -1}.Encode(want)) {
			if fails++; fails <= 10 {
				t.Errorf("ticket %d old %v detail %q reason %v:\n got %s\nwant %s", idx, deref(old), deref(detail), deref(reason),
					got, jsonx.Format{Indent: -1}.Encode(want))
			}
		}
	}
	if fails > 0 {
		t.Errorf("%d of %d asks differ from Python's", fails, len(g.Cases))
	}
	// the findings ask came after Python: its cases are in findings_test.go
	if a := hunt(t).Ask.V; a.Kind == string(model.AskFindings) {
		kinds[a.Kind] = true
	}
	// so did the deploy ask: TestAskForHeldDeploy
	if a := heldDeploy(t).Ask.V; a.Kind == string(model.AskDeploy) {
		kinds[a.Kind] = true
	}
	// ticket/ask_test.go completeness: AskFor covers every AskKind
	for _, k := range model.AskKinds() {
		if !kinds[string(k.Kind)] {
			t.Errorf("no case gives ask kind %q", k.Kind)
		}
	}
}

// port: TT TypedEventTest.test_asks_from_reasons_match_asks_from_details
func TestAsksFromReasonsMatchAsksFromDetails(t *testing.T) {
	rows := []struct {
		kind, old, detail string
		reason            model.Reason
		extra             string
	}{
		{"", "triage", "triage accepted; move to ready to start work", model.ReasonTriageAccepted, ``},
		{"", "triage", "triage unsure: no idea", model.ReasonTriageUnsure, ``},
		{"", "working", "worker blocked: stuck", model.ReasonWorkerBlocked, ``},
		{"", "working", "agent t0001-w1 timed out", model.ReasonTimedOut, ``},
		{"", "working", "work asks: which one?", model.ReasonAgentQuestion, `, "question": {"text": "which one?", "stage": "work", "agent": "a", "at": "s"}`},
		{"task", "working", "task done: yes", model.ReasonTaskDone, ``},
		{"task", "working", "task blocked: no", model.ReasonTaskBlocked, ``},
		{"intent", "working", "designer blocked: hm", model.ReasonDesignerBlocked, `, "design": "d", "proposed": [{"title": "x", "body": ""}]`},
		{"intent", "working", "design ready (1 tickets proposed): s; factory approve or redirect", model.ReasonDesignReady, `, "design": "d", "proposed": [{"title": "x", "body": ""}]`},
		{"manage", "working", "manager drafted 1 change(s): s", model.ReasonManagerDrafted, `, "drafts": [{"title": "d", "why": "", "patch": ""}]`},
		{"manage", "working", "manager answered: s", model.ReasonManagerAnswered, `, "result": "r"`},
		{"manage", "working", "manager blocked: s", model.ReasonManagerBlocked, ``},
		{"", "gate", "gate: tests or lint failed on a fresh checkout", model.ReasonGateFailed, ``},
		{"", "ready", "by human", model.ReasonByHuman, ``},
		{"", "security", "security: problems found (q1 high)", model.ReasonSecurityFailed, ``},
		{"", "ready", "dispatcher error: boom", model.ReasonDispatcherError, ``},
		{"", "ready_for_pr", "draft PR https://github.com/o/r/pull/3", model.ReasonPROpened, `, "pr": {"number": 3}`},
	}
	for _, r := range rows {
		column := "needs_human"
		if r.reason == model.ReasonPROpened {
			column = "pr_open"
		}
		kind := `null`
		if r.kind != "" {
			kind = `"` + r.kind + `"`
		}
		tk := ticketFrom(t, `{"id": "T0001", "project": "p", "title": "x", "kind": `+kind+`, "column": "`+column+`", "reports": {}`+r.extra+`}`)
		typed, worded := AskFor(tk, r.old, r.detail, "s", r.reason), AskFor(tk, r.old, r.detail, "s", "")
		if askJSON(typed) != askJSON(worded) {
			t.Errorf("%s: by reason %s, by wording %s", r.reason, askJSON(typed), askJSON(worded))
		}
		if !model.IsReason(string(r.reason)) {
			t.Errorf("%q is not a move reason", r.reason)
		}
	}
}

// port: TF AskTest.test_stuck_tickets_ask_to_be_unstuck_from_where_they_stopped
func TestStuckTicketsAskToBeUnstuckFromWhereTheyStopped(t *testing.T) {
	tk := ticketFrom(t, `{"id": "T0001", "project": "gem", "title": "x", "column": "working", "reports": {"work": {"status": "blocked", "summary": "the spec contradicts itself"}}, "history": []}`)
	Move(tk, "needs_human", model.ReasonWorkerBlocked, "worker blocked: the spec contradicts itself", "", "", testNow)
	a := tk.Ask.V
	if a.Kind != "unstick" || a.RetryColumn.Or("") != "ready" || !strings.Contains(a.Question, "work step") || !strings.Contains(a.Why, "the spec contradicts itself") {
		t.Errorf("worker blocked: %+v", a)
	}
	Move(tk, "ready", model.ReasonByHuman, "by human", "", "", testNow)
	if !tk.Ask.Set || !tk.Ask.Null {
		t.Errorf("leaving needs_human keeps an ask: %+v", tk.Ask)
	}
	tk.Column = "security"
	Move(tk, "needs_human", model.ReasonSecurityFailed, "security: problems found (q1 high)", "", "", testNow)
	if a := tk.Ask.V; a.Kind != "unstick" || a.RetryColumn.Or("") != "security" {
		t.Errorf("security failed: %+v", a)
	}
	tk.Column = "gate"
	Move(tk, "needs_human", model.ReasonGateFailed, "gate: tests or lint failed on a fresh checkout", "", "", testNow)
	if a := tk.Ask.V; a.RetryColumn.Or("") != "gate" {
		t.Errorf("gate failed: %+v", a)
	}
}

// port: TF AskTest.test_pr_open_asks_for_a_review_and_settling_clears_it
func TestPROpenAsksForAReviewAndSettlingClearsIt(t *testing.T) {
	tk := ticketFrom(t, `{"id": "T0001", "project": "gem", "title": "x", "column": "ready_for_pr", "agent": null, "pr": {"number": 23}, "reports": {"work": {"summary": "Clarify the install steps."}}, "history": []}`)
	Move(tk, "pr_open", model.ReasonPROpened, "PR https://example.com/pull/23", "", "", testNow)
	if a := tk.Ask.V; a.Kind != "review" || a.Question != "Review and merge PR #23?" || a.Why != "Clarify the install steps." {
		t.Errorf("pr open: %+v", a)
	}
	Move(tk, "done", model.ReasonPRMerged, "PR merged by someone", "", "", testNow)
	if tk.Ask.Present() {
		t.Errorf("done keeps an ask: %+v", tk.Ask)
	}
}

// A PR the App pushed merges only once the maintainer approves its current
// head on GitHub: the tick records the latest review decision and a merge
// GitHub refused for want of one, and NeedsApproval reads them. A PR whose
// reviews were never read (no reviewers configured, or before the first
// tick) is not known to need one.
func TestNeedsApproval(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, c := range []struct {
		name string
		pr   string
		want bool
	}{
		{"never read", `{"number": 5}`, false},
		{"no review", `{"number": 5, "review": {"state": "none"}}`, true},
		{"changes requested", `{"number": 5, "review": {"state": "changes", "commit": "` + head + `"}}`, true},
		{"approved at the head", `{"number": 5, "review": {"state": "approved", "commit": "` + head + `"}}`, false},
		{"approved before a rework", `{"number": 5, "review": {"state": "approved", "commit": "` + strings.Repeat("b", 40) + `"}}`, true},
		{"approved, no commit", `{"number": 5, "review": {"state": "approved"}}`, true},
		{"a merge refused for an approval", `{"number": 5, "merge_blocked": "approval"}`, true},
		{"refused, then approved at the head", `{"number": 5, "merge_blocked": "approval", "review": {"state": "approved", "commit": "` + head + `"}}`, false},
	} {
		tk := ticketFrom(t, `{"id": "T0001", "column": "pr_open", "gate": {"head": "`+head+`"}, "pr": `+c.pr+`}`)
		if got := NeedsApproval(tk); got != c.want {
			t.Errorf("%s: NeedsApproval = %v", c.name, got)
		}
		tk.Column = "ready"
		if NeedsApproval(tk) {
			t.Errorf("%s: only an open PR waits for an approval", c.name)
		}
	}
}

// port: TF TriageAskTest.test_triage_outcomes_ask_and_record_the_policy
func TestTriageOutcomesAsk(t *testing.T) {
	tk := ticketFrom(t, `{"id": "T0001", "project": "gem", "title": "Fix x", "door": "github", "column": "triage", "reports": {"triage": {"decision": "accept", "reason": "real bug"}}, "history": []}`)
	Move(tk, "needs_human", model.ReasonTriageAccepted, "triage accepted; move to ready to start work", "", "", testNow)
	if a := tk.Ask.V; a.Kind != "start" || a.Why != "real bug." {
		t.Errorf("accepted: %+v", a)
	}
	tk = ticketFrom(t, `{"id": "T0001", "project": "gem", "title": "Fix x", "door": "github", "column": "triage", "reports": {"triage": {"decision": "unsure", "reason": "no steps to reproduce"}}, "history": []}`)
	Move(tk, "needs_human", model.ReasonTriageUnsure, "triage unsure: no steps to reproduce", "", "", testNow)
	if a := tk.Ask.V; a.Kind != "decide" || a.Why != "no steps to reproduce." || !a.RetryColumn.Null {
		t.Errorf("unsure: %+v", a)
	}
}

// port: TF KindAskTest.test_designs_tasks_and_notices_each_ask_their_own_question
func TestDesignsTasksAndNoticesAskTheirOwnQuestion(t *testing.T) {
	tk := ticketFrom(t, `{"id": "T0001", "project": "g", "title": "Idea", "kind": "intent", "column": "working", "design": "d",
		"proposed": [{"title": "A", "body": "a"}, {"title": "B", "body": "b"}], "reports": {"design": {"status": "done", "summary": "a small encoder"}}, "history": []}`)
	Move(tk, "needs_human", model.ReasonDesignReady, "design ready (2 tickets proposed): a small encoder; factory approve or redirect", "", "", testNow)
	if a := tk.Ask.V; a.Kind != "design" || a.Why != "a small encoder." ||
		a.Question != "Approve the design for “Idea” and its 2 proposed tickets, or send it back?" {
		t.Errorf("design: %+v", a)
	}
	tk = ticketFrom(t, `{"id": "T0002", "project": "factory", "title": "Uptime", "kind": "task", "column": "working", "result": "all good", "reports": {"task": {"summary": "s"}}, "history": []}`)
	Move(tk, "needs_human", model.ReasonTaskDone, "task done: s", "", "", testNow)
	if a := tk.Ask.V; a.Kind != "notice" || a.Why != "all good." {
		t.Errorf("task: %+v", a)
	}
	n := New("T0003", newIn(model.NoticeProject, "Update something", "A newer release is out.", "clock", "needs_human", ""), testNow)
	if a := n.Ask.V; a.Kind != "notice" || a.Why != "A newer release is out." || !a.RetryColumn.Null {
		t.Errorf("notice: %+v", a)
	}
}

// port: TF KindAskTest.test_a_blocked_designer_asks_to_be_unstuck_not_approved
func TestABlockedDesignerAsksToBeUnstuck(t *testing.T) {
	tk := ticketFrom(t, `{"id": "T0001", "project": "g", "title": "Idea", "kind": "intent", "column": "working", "design": "half a draft",
		"proposed": [{"title": "A", "body": "a"}], "reports": {"design": {"status": "blocked", "summary": "two readings of the idea"}}, "history": []}`)
	Move(tk, "needs_human", model.ReasonDesignerBlocked, "designer blocked: two readings of the idea", "", "", testNow)
	if a := tk.Ask.V; a.Kind != "unstick" || a.RetryColumn.Or("") != "ready" {
		t.Errorf("%+v", a)
	}
}

// port: TF KindAskTest.test_questions_name_their_subject
func TestQuestionsNameTheirSubject(t *testing.T) {
	tk := ticketFrom(t, `{"column": "needs_human", "title": "Fix x", "source": {"issue": 18, "url": "u"}, "reports": {}}`)
	if q := AskFor(tk, "triage", "triage accepted; move to ready", "s", "").Question; q != "Start work on issue #18, “Fix x”?" {
		t.Errorf("%q", q)
	}
	tk.Source = Opt[Source]{}
	if q := AskFor(tk, "triage", "triage unsure: vague", "s", "").Question; q != "Triage could not decide on “Fix x”: start it, or drop it?" {
		t.Errorf("%q", q)
	}
	tk.Kind = Some("proposal")
	tk.Proposal = Some(Proposal{Why: "It is slow."})
	tk.Panel = Some(Panel{Scores: []int{4, 5, 4}, Mean: 4.33})
	a := AskFor(tk, "", "", "s", "")
	if a.Question != "File “Fix x” as a change, or drop it?" || a.Why != "It is slow. The panel scored it 4 · 5 · 4, mean 4.33." {
		t.Errorf("%+v", a)
	}
}

// A board ticket asks only through an open question put to a person.
func TestBoardAsk(t *testing.T) {
	tk := ticketFrom(t, `{"id": "T0001", "mode": "board", "column": "waiting", "questions": [
		{"n": 1, "by": "a", "recipient": "b", "to_person": false, "text": "agent?", "options": [], "at": "s1", "answer": null},
		{"n": 2, "by": "a", "recipient": "maintainer", "to_person": true, "text": "Which?", "options": ["A", "B"], "at": "s2", "answer": null}]}`)
	a := BoardAsk(tk, "s")
	if a == nil || askJSON(a) != `{"kind": "question", "question": "Which?", "why": "a asks maintainer.", "since": "s2", "retry_column": null, "options": ["A", "B"], "n": 2}` {
		t.Errorf("%s", askJSON(a))
	}
	tk.Column = "dropped"
	if BoardAsk(tk, "s") != nil {
		t.Error("a dropped ticket asks")
	}
}

// heldDeploy is the Deploy? ticket a held deploy files on the notice
// project.
func heldDeploy(t *testing.T) *Ticket {
	t.Helper()
	in := newIn(model.NoticeProject, "Deploy the factory?", "", "clock", "needs_human", "")
	in.Fields = fields(t, `{"deploy": {"rev": "0123456789ab", "why": ["merged #12", "merged #13"]}}`)
	return New("T0040", in, testNow)
}

// A held deploy asks to deploy main, whatever else a notice would ask; once
// the maintainer's Deploy it has closed it, it asks nothing.
func TestAskForHeldDeploy(t *testing.T) {
	tk := heldDeploy(t)
	d, ok := tk.Deploy.Get()
	if !ok || d.Rev != "0123456789ab" || len(d.Why) != 2 || d.Requested.Set || d.Started.Set {
		t.Fatalf("deploy = %+v", tk.Deploy)
	}
	a := tk.Ask.V
	if a.Kind != string(model.AskDeploy) || a.Question != "Deploy main (now 0123456789ab) to the factory?" || a.Why != "merged #12; merged #13." {
		t.Errorf("ask = %+v", a)
	}
	Move(tk, "needs_human", model.ReasonDeployHeld, "now 0123456789ab", "", "", testNow)
	if a := tk.Ask.V; a.Kind != string(model.AskDeploy) {
		t.Errorf("after a move in place: %+v", a)
	}
	d.Requested = Some("2026-10-24T10:00:00Z")
	tk.Deploy = Some(d)
	Move(tk, "done", model.ReasonDeployRequested, "deploy main", "", "maintainer", testNow)
	if tk.Ask.Present() {
		t.Errorf("a requested deploy still asks: %+v", tk.Ask)
	}
	b := Encode(tk)
	if !strings.Contains(string(b), `"deploy": {
    "rev": "0123456789ab",
    "why": [
      "merged #12",
      "merged #13"
    ],
    "requested": "2026-10-24T10:00:00Z"`) {
		t.Errorf("encoded:\n%s", b)
	}
	back := ticketFrom(t, string(b))
	if got, _ := back.Deploy.Get(); got.Requested.Or("") != "2026-10-24T10:00:00Z" || got.Started.Set {
		t.Errorf("decoded deploy = %+v", got)
	}
}

// Only the factory's own project asks Deploy?: a deploy field on any other
// ticket is not a deploy, and its answers would close that ticket.
func TestAskForDeployOnlyOnTheNoticeProject(t *testing.T) {
	in := newIn("p", "Deploy the factory?", "", "clock", "needs_human", "")
	in.Fields = fields(t, `{"deploy": {"rev": "0123456789ab", "why": ["merged #12"]}}`)
	if a := New("T0041", in, testNow).Ask.V; a.Kind == string(model.AskDeploy) {
		t.Errorf("a project ticket asks %+v", a)
	}
}

// askAfter is the ask of a manager ticket after the finish's move (or the
// tick's settle move) with reason r.
func askAfter(t *testing.T, ds []Draft, as []Action, r model.Reason) Ask {
	t.Helper()
	tk := manageTicket(t, ds, as)
	tk.Result = Some("The result.")
	Move(tk, "needs_human", r, "manager proposed it", "", "", testNow)
	a, ok := tk.Ask.Get()
	if !ok {
		t.Fatal("no ask")
	}
	return a
}

func TestAskForWaitingItems(t *testing.T) {
	for _, r := range []model.Reason{model.ReasonManagerDrafted, model.ReasonOperatorSettled} {
		a := askAfter(t, []Draft{{Title: "Use Sonnet", State: model.ItemWaiting}, {Title: "Gate mount", State: model.ItemRefused}},
			[]Action{{Kind: "rebuild", State: model.ItemFailed, Note: "systemd-run: no user bus"}, {Kind: "pause_host", Target: "local", State: model.ItemApplied}}, r)
		if a.Kind != string(model.AskDraft) || a.Question != "Apply the manager's 2 changes?" {
			t.Errorf("%s: ask %+v", r, a)
		}
		if !strings.HasPrefix(a.Why, "failed: rebuild (systemd-run: no user bus); waiting: Use Sonnet.") {
			t.Errorf("%s: why %q", r, a.Why)
		}
	}
	if a := askAfter(t, []Draft{{Title: "x", State: model.ItemWaiting}}, nil, model.ReasonOperatorSettled); a.Question != "Apply the manager's 1 change?" {
		t.Errorf("one: %+v", a)
	}
}

func TestAskForByHandKeepsTodaysQuestion(t *testing.T) {
	a := askAfter(t, []Draft{{Title: "a", State: model.ItemByHand}, {Title: "b", State: model.ItemByHand}},
		[]Action{{Kind: "pause_host", Target: "local", State: model.ItemApplied}}, model.ReasonOperatorSettled)
	if a.Kind != string(model.AskDraft) || a.Question != "Apply the manager's 2 config changes?" {
		t.Errorf("ask %+v", a)
	}
}

// With the landing off, a draft is by hand while the actions still work: a
// waiting or failed action is still asked for, beside the by-hand drafts.
func TestAskForByHandBesideWaitingActions(t *testing.T) {
	a := askAfter(t, []Draft{{Title: "Use Sonnet", State: model.ItemByHand}},
		[]Action{{Kind: "rebuild", State: model.ItemWaiting}, {Kind: "enable_project", Target: "gem", State: model.ItemFailed, Note: "no such project"}},
		model.ReasonManagerDrafted)
	if a.Kind != string(model.AskDraft) || a.Question != "Apply the manager's 2 changes? 1 config change is yours to apply by hand." {
		t.Errorf("ask %+v", a)
	}
	if !strings.HasPrefix(a.Why, "failed: enable_project gem (no such project); waiting: rebuild; by hand: Use Sonnet.") {
		t.Errorf("why %q", a.Why)
	}
}

func TestAskForOldDraftsAreByHand(t *testing.T) {
	a := askAfter(t, []Draft{{Title: "a", Why: "w", Patch: "p"}}, nil, model.ReasonManagerDrafted)
	if a.Kind != string(model.AskDraft) || a.Question != "Apply the manager's 1 config change?" || a.Why != "manager proposed it." {
		t.Errorf("ask %+v", a)
	}
}

func TestAskForAllSettledIsANotice(t *testing.T) {
	a := askAfter(t, []Draft{{Title: "a", State: model.ItemApplied}, {Title: "b", State: model.ItemRefused}},
		[]Action{{Kind: "pause_host", Target: "local", State: model.ItemApplied}}, model.ReasonOperatorSettled)
	if a.Kind != string(model.AskNotice) || a.Question != "Read what the manager did and close it?" ||
		a.Why != "operator: 2 applied, 1 refused. The result." {
		t.Errorf("ask %+v", a)
	}
}

func TestAskForApplyingWhileNew(t *testing.T) {
	a := askAfter(t, []Draft{{Title: "a", State: model.ItemNew}},
		[]Action{{Kind: "rebuild", State: model.ItemRequested}, {Kind: "deploy", State: model.ItemApplied}}, model.ReasonManagerDrafted)
	if a.Kind != string(model.AskDraft) || a.Question != "The factory is applying the manager's 2 changes" {
		t.Errorf("ask %+v", a)
	}
}
