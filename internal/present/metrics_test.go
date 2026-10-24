package present

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// metricsNow is a Wednesday: its week starts on Monday 12 October, and the
// four before it on 5 October, 28, 21 and 14 September.
var metricsNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func day(month time.Month, d, hour, min int) time.Time {
	return time.Date(2026, month, d, hour, min, 0, 0, time.UTC)
}

// metricTicket is a ticket filed by the clock into column at created.
func metricTicket(t *testing.T, id, project, kind, column string, created time.Time) *ticket.Ticket {
	t.Helper()
	stamp := created.Format(time.RFC3339)
	return tk(t, `{"id": "`+id+`", "project": "`+project+`", "title": "Ticket `+id+`", "body": "b", "door": "clock", "by": "factory",
		"column": "`+column+`", "kind": "`+kind+`", "branch": null, "agent": null,
		"created": "`+stamp+`", "history": [{"at": "`+stamp+`", "event": "created",
		"detail": "door=clock column=`+column+`", "type": "created", "door": "clock", "column": "`+column+`", "by": "factory"}]}`)
}

// metricTickets are typed histories across the five weeks, with one older
// untyped merge.
func metricTickets(t *testing.T) []*ticket.Ticket {
	t.Helper()
	const maint = "maintainer"
	// A: a PR GitHub asked changes on, reworked, then merged on the desk's word
	a := metricTicket(t, "T0101", "widgets", "change", "ready", day(10, 1, 9, 0))
	ticket.Move(a, "pr_open", model.ReasonPROpened, "draft PR #1", "", "", day(10, 6, 9, 0))
	ticket.Move(a, "ready", model.ReasonChangesRequested, "changes requested on GitHub", "", "", day(10, 6, 12, 0))
	ticket.Move(a, "working", model.ReasonStarted, "", "", "", day(10, 7, 9, 0))
	ticket.Move(a, "review", model.ReasonWorkDone, "", "", "", day(10, 7, 10, 0))
	ticket.Move(a, "ready", model.ReasonReviewChanges, "review asked for changes", "", "", day(10, 7, 11, 0))
	ticket.Move(a, "pr_open", model.ReasonPROpened, "PR #1", "", "", day(10, 13, 10, 0))
	ticket.Event(a, "pr", "you asked the factory to merge PR #1", "", ticket.PRNote{Action: ticket.PRMergeRequested, Number: 1}, day(10, 13, 11, 0))
	ticket.Move(a, "done", model.ReasonPRMerged, "PR merged by someone", "", "", day(10, 13, 11, 5))
	// B: started by robin, four weeks back
	b := metricTicket(t, "T0102", "widgets", "change", "triage", day(9, 15, 8, 0))
	ticket.Move(b, "needs_human", model.ReasonTriageAccepted, "triage accepted", "", "", day(9, 15, 9, 0))
	ticket.Move(b, "ready", model.ReasonByHuman, "by human", "", "robin", day(9, 16, 9, 0))
	// C: a design redirected, then approved
	c := metricTicket(t, "T0103", "gadgets", "intent", "ready", day(10, 12, 7, 0))
	ticket.Move(c, "needs_human", model.ReasonDesignReady, "design ready", "", "", day(10, 12, 8, 0))
	ticket.Move(c, "ready", model.ReasonRedirected, "redirected: smaller", "", maint, day(10, 12, 10, 0))
	ticket.Move(c, "needs_human", model.ReasonDesignReady, "design ready", "", "", day(10, 13, 8, 0))
	ticket.Move(c, "done", model.ReasonDesignApproved, "design approved; filed T0200", "", maint, day(10, 14, 8, 0))
	// D: a PR sent back from the desk
	d := metricTicket(t, "T0104", "gadgets", "change", "ready", day(10, 12, 7, 0))
	ticket.Move(d, "pr_open", model.ReasonPROpened, "draft PR #4", "", "", day(10, 12, 9, 0))
	ticket.Move(d, "ready", model.ReasonByHuman, "tighten the tests", "", maint, day(10, 12, 9, 30))
	// E: closed by triage
	e := metricTicket(t, "T0105", "gadgets", "change", "triage", day(10, 13, 7, 0))
	ticket.Move(e, "rejected", model.ReasonTriageRejected, "triage: nothing to do", "", "", day(10, 13, 8, 0))
	// F: a proposal dropped on the desk
	f := metricTicket(t, "T0106", "widgets", "proposal", "needs_human", day(10, 8, 8, 0))
	ticket.Move(f, "rejected", model.ReasonByHuman, "by human", "", maint, day(10, 8, 12, 0))
	// G: a PR closed on GitHub without merging
	g := metricTicket(t, "T0107", "widgets", "change", "ready", day(9, 21, 8, 0))
	ticket.Move(g, "pr_open", model.ReasonPROpened, "PR #7", "", "", day(9, 22, 9, 0))
	ticket.Move(g, "rejected", model.ReasonPRClosed, "PR closed without merging", "", "", day(9, 23, 9, 0))
	// H: opened by the factory itself: no person answered
	h := metricTicket(t, "T0108", "widgets", "change", "gating", day(10, 12, 7, 0))
	ticket.Move(h, "ready_for_pr", model.ReasonGatePassed, "auto-merge eligible", "", "", day(10, 12, 8, 0))
	ticket.Move(h, "pr_open", model.ReasonPROpened, "PR #8", "", "", day(10, 12, 8, 1))
	// I: an older board's untyped merge
	i := metricTicket(t, "T0109", "widgets", "change", "ready", day(10, 9, 8, 0))
	ticket.Move(i, "pr_open", model.ReasonPROpened, "PR #9", "", "", day(10, 9, 9, 0))
	i.History = append(i.History, ticket.Entry{At: day(10, 10, 9, 0).Format(time.RFC3339), Event: "move", Detail: "pr_open -> done: PR merged by someone"})
	// J: too old to count
	j := metricTicket(t, "T0110", "widgets", "change", "ready", day(9, 1, 8, 0))
	ticket.Move(j, "pr_open", model.ReasonPROpened, "PR #10", "", "", day(9, 1, 9, 0))
	ticket.Move(j, "done", model.ReasonPRMerged, "PR merged by someone", "", "", day(9, 2, 9, 0))
	// a board ticket is not the factory's work
	board := metricTicket(t, "T0111", "widgets", "intent", "needs_human", day(10, 12, 7, 0))
	board.Mode = ticket.Some("board")
	ticket.Move(board, "rejected", model.ReasonByHuman, "by human", "", maint, day(10, 12, 8, 0))
	return []*ticket.Ticket{a, b, c, d, e, f, g, h, i, j, board}
}

func TestMetricsCountTheWeeksFromTheHistories(t *testing.T) {
	ts := metricTickets(t)
	c := CountMetrics(ts, metricsNow, you)
	if !c.Starts[0].Equal(day(10, 12, 0, 0)) || !c.Starts[4].Equal(day(9, 14, 0, 0)) {
		t.Fatalf("weeks %v", c.Starts)
	}
	w := c.Total
	if w[0].Answered != 4 || !reflect.DeepEqual(w[0].ByKind, map[model.AskKind]int{model.AskReview: 2, model.AskDesign: 2}) {
		t.Errorf("this week's answers %d %v", w[0].Answered, w[0].ByKind)
	}
	if w[0].Median() != 90*time.Minute || w[0].Longest() != 24*time.Hour {
		t.Errorf("this week's waits %v: median %v longest %v", w[0].Waits, w[0].Median(), w[0].Longest())
	}
	if w[0].Redirects != 1 || w[0].SentBack != 1 || w[0].Merged != 1 || w[0].TriageRejected != 1 || w[0].Reworks != 0 {
		t.Errorf("this week's send-backs and outcomes %+v", w[0])
	}
	if w[1].Answered != 3 || !reflect.DeepEqual(w[1].ByKind, map[model.AskKind]int{model.AskReview: 2, model.AskProposal: 1}) {
		t.Errorf("last week's answers %d %v", w[1].Answered, w[1].ByKind)
	}
	if w[1].ChangesRequested != 1 || w[1].Reworks != 1 || w[1].Dropped != 1 || w[1].Merged != 1 || w[1].SentBack != 0 {
		t.Errorf("last week's send-backs and outcomes %+v", w[1])
	}
	if w[1].Longest() != 24*time.Hour || w[1].Median() != 4*time.Hour {
		t.Errorf("last week's waits %v", w[1].Waits)
	}
	if w[2].any() || w[3].ClosedUnmerged != 1 || w[3].Answered != 1 || w[4].Answered != 1 || w[4].ByKind[model.AskStart] != 1 {
		t.Errorf("older weeks %+v", w[2:])
	}
	if g := c.Projects["gadgets"]; g[0].Answered != 3 || g[0].TriageRejected != 1 || g[1].any() {
		t.Errorf("gadgets %+v", g[:2])
	}
	if wd := c.Projects["widgets"]; wd[0].Answered != 1 || wd[0].Merged != 1 || wd[1].Answered != 3 {
		t.Errorf("widgets %+v", wd[:2])
	}
	if len(c.Projects) != 2 {
		t.Errorf("projects %v", c.Projects)
	}
	// an earlier week shifts the window
	if back := CountMetrics(ts, day(10, 7, 0, 0), you); back.Total[0].Answered != 3 || back.Total[3].Answered != 1 {
		t.Errorf("a week back %+v", back.Total)
	}
}

// askedDeploy is a ticket filed by the clock straight into needs_human
// with a deploy field, as a held deploy files its ask.
func askedDeploy(t *testing.T, id, project string, created time.Time) *ticket.Ticket {
	t.Helper()
	stamp := created.Format(time.RFC3339)
	return tk(t, `{"id": "`+id+`", "project": "`+project+`", "title": "Deploy the factory?", "body": "b", "door": "clock", "by": "factory",
		"column": "needs_human", "branch": null, "agent": null, "deploy": {"rev": "0123456789ab", "why": []},
		"created": "`+stamp+`", "history": [{"at": "`+stamp+`", "event": "created",
		"detail": "door=clock column=needs_human", "type": "created", "door": "clock", "column": "needs_human", "by": "factory"}]}`)
}

// A Deploy? ask counts as one, whether it was filed into needs_human or
// moved there by a held deploy; a deploy field off the factory's own
// project is no Deploy? ask.
func TestMetricsCountDeployAsks(t *testing.T) {
	filed := askedDeploy(t, "T0120", model.NoticeProject, day(10, 12, 7, 0))
	ticket.Move(filed, "done", model.ReasonDeployRequested, "deploy main", "", "maintainer", day(10, 12, 8, 0))
	moved := askedDeploy(t, "T0121", model.NoticeProject, day(10, 13, 7, 0))
	ticket.Move(moved, "needs_human", model.ReasonDeployHeld, "now 0123456789ab", "", "", day(10, 13, 8, 0))
	ticket.Move(moved, "rejected", model.ReasonByHuman, "by human", "", "maintainer", day(10, 13, 9, 0))
	other := askedDeploy(t, "T0122", "widgets", day(10, 12, 7, 0))
	ticket.Move(other, "done", model.ReasonByHuman, "by human", "", "maintainer", day(10, 12, 8, 0))
	w := CountMetrics([]*ticket.Ticket{filed, moved, other}, metricsNow, you).Total[0]
	if w.ByKind[model.AskDeploy] != 2 || w.Answered != 3 {
		t.Errorf("answers %d %v", w.Answered, w.ByKind)
	}
}

func TestMetricsCountOnlyTheViewersOwnAnswers(t *testing.T) {
	ts := metricTickets(t)
	robin := CountMetrics(ts, metricsNow, Viewer{By: "robin"})
	for i, w := range robin.Total {
		want := 0
		if i == 4 {
			want = 1
		}
		if w.Answered != want {
			t.Errorf("robin's answers in week %d: %d", i, w.Answered)
		}
	}
	// the send-backs and outcomes are everyone's
	if robin.Total[0].Merged != 1 || robin.Total[1].Dropped != 1 {
		t.Errorf("robin's outcomes %+v", robin.Total[:2])
	}
	// a second person on the board: the maintainer still sees everyone's
	someone := Viewer{By: "maintainer"}
	if n := CountMetrics(ts, metricsNow, someone).Total[0].Answered; n != 3 {
		t.Errorf("the maintainer's own answers, as a plain viewer: %d", n)
	}
	if n := CountMetrics(ts, metricsNow, you).Total[4].Answered; n != 1 {
		t.Errorf("the maintainer sees robin's answer: %d", n)
	}
}

func TestMetricsLayout(t *testing.T) {
	m := BuildMetrics(CountMetrics(metricTickets(t), metricsNow, you), metricsNow, you)
	if !reflect.DeepEqual(m.Weeks, []string{"This week", "5 Oct", "28 Sep", "21 Sep", "14 Sep"}) {
		t.Errorf("weeks %v", m.Weeks)
	}
	if m.Quiet || len(m.Projects) != 2 || m.Projects[0].Name != "gadgets" || m.Projects[1].Name != "widgets" || !m.Total.Open {
		t.Fatalf("tables %+v", m)
	}
	rows := map[string]MetricRow{}
	var labels []string
	for _, g := range m.Total.Groups {
		for _, r := range g.Rows {
			rows[g.Title+"/"+r.Label] = r
			labels = append(labels, r.Label)
		}
	}
	want := []string{"Asks answered", "Review", "Start", "Design", "Proposal", "Median wait", "Longest wait",
		"Reviewer reworks", "Changes requested on GitHub", "Designs redirected", "Sent back from the desk",
		"PRs merged", "PRs closed unmerged", "Dropped by a person", "Closed by triage", "Reverts"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("rows\n got %v\nwant %v", labels, want)
	}
	if r := rows["Your attention/Asks answered"]; !reflect.DeepEqual(r.Cells, []string{"4", "3", "0", "1", "1"}) {
		t.Errorf("answered %v", r.Cells)
	}
	if r := rows["Your attention/Review"]; !r.Sub {
		t.Errorf("a kind is a breakdown")
	}
	if r := rows["Your attention/Median wait"]; !reflect.DeepEqual(r.Cells, []string{"1 h", "4 h", "–", "24 h", "24 h"}) {
		t.Errorf("median %v", r.Cells)
	}
	if r := rows["Outcomes/Reverts"]; r.Cells != nil || !strings.HasPrefix(r.NotTracked, "not tracked") {
		t.Errorf("reverts %+v", r)
	}
	if !strings.Contains(m.Total.Groups[0].Note, "by anyone") {
		t.Errorf("the maintainer's note %q", m.Total.Groups[0].Note)
	}
	if mine := BuildMetrics(CountMetrics(metricTickets(t), metricsNow, Viewer{By: "robin"}), metricsNow, Viewer{By: "robin"}); !strings.Contains(mine.Total.Groups[0].Note, "you answered") {
		t.Errorf("robin's note %q", mine.Total.Groups[0].Note)
	}
	if q := BuildMetrics(CountMetrics(nil, metricsNow, you), metricsNow, you); !q.Quiet {
		t.Errorf("no tickets is quiet")
	}
	// the floor carries them
	if f := BuildFloor(metricTickets(t), metricsNow, metricsNow, you); !reflect.DeepEqual(f.Metrics.Weeks, m.Weeks) || f.Metrics.Total.Groups == nil {
		t.Errorf("the floor's metrics %+v", f.Metrics)
	}
}

// The maintainer's tap answers the draft ask; the tick's work after it is
// no one's wait, and the notice its settle leaves is an ask of its own.
func TestMetricsTapAndTheTicksSettle(t *testing.T) {
	x := metricTicket(t, "T0130", model.NoticeProject, "manage", "working", day(10, 12, 7, 0))
	x.Drafts = []ticket.Draft{{Title: "Gate", State: model.ItemNew}}
	ticket.Move(x, "needs_human", model.ReasonManagerDrafted, "manager drafted 1 change(s): x", "", "", day(10, 12, 8, 0))
	x.Drafts[0].State = model.ItemWaiting
	ticket.SettleMove(x, "", "", day(10, 12, 8, 1))
	x.Drafts[0].State = model.ItemRequested
	ticket.SettleMove(x, "apply requested by maintainer; "+ticket.SettledDetail(ticket.Items(x)), "maintainer", day(10, 12, 10, 0))
	x.Drafts[0].State = model.ItemApplied
	ticket.SettleMove(x, "", "", day(10, 12, 14, 0))
	ticket.Move(x, "done", model.ReasonByHuman, "by human", "", "maintainer", day(10, 12, 15, 0))
	w := CountMetrics([]*ticket.Ticket{x}, metricsNow, you).Total[0]
	if w.Answered != 2 || !reflect.DeepEqual(w.ByKind, map[model.AskKind]int{model.AskDraft: 1, model.AskNotice: 1}) {
		t.Errorf("answers %d %v", w.Answered, w.ByKind)
	}
	if !reflect.DeepEqual(w.Waits, []time.Duration{2 * time.Hour, time.Hour}) {
		t.Errorf("waits %v", w.Waits)
	}
}
