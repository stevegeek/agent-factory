package present

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// MetricWeeks is how many weeks the floor's metrics cover: the week shown
// and the four before it.
const MetricWeeks = 5

// MetricWeek is what one week cost the maintainer and how often the
// factory's work came back, for one project or for all of them.
type MetricWeek struct {
	// your attention: the asks answered (by the viewer, or by anyone for the
	// maintainer), by kind, and how long each waited from its since
	Answered int
	ByKind   map[model.AskKind]int
	Waits    []time.Duration
	// send-backs
	Reworks, ChangesRequested, Redirects, SentBack int
	// outcomes
	Merged, ClosedUnmerged, Dropped, TriageRejected int
}

func (w *MetricWeek) any() bool {
	return w.Answered+w.Reworks+w.ChangesRequested+w.Redirects+w.SentBack+
		w.Merged+w.ClosedUnmerged+w.Dropped+w.TriageRejected > 0
}

// Median is the middle wait (the mean of the two middle ones for an even
// count), 0 with none.
func (w *MetricWeek) Median() time.Duration {
	if len(w.Waits) == 0 {
		return 0
	}
	ws := append([]time.Duration{}, w.Waits...)
	sort.Slice(ws, func(i, j int) bool { return ws[i] < ws[j] })
	n := len(ws)
	if n%2 == 1 {
		return ws[n/2]
	}
	return (ws[n/2-1] + ws[n/2]) / 2
}

// Longest is the longest wait, 0 with none.
func (w *MetricWeek) Longest() time.Duration {
	var m time.Duration
	for _, d := range w.Waits {
		m = max(m, d)
	}
	return m
}

func (w *MetricWeek) add(o *MetricWeek) {
	w.Answered += o.Answered
	for k, n := range o.ByKind {
		w.kind(k, n)
	}
	w.Waits = append(w.Waits, o.Waits...)
	w.Reworks += o.Reworks
	w.ChangesRequested += o.ChangesRequested
	w.Redirects += o.Redirects
	w.SentBack += o.SentBack
	w.Merged += o.Merged
	w.ClosedUnmerged += o.ClosedUnmerged
	w.Dropped += o.Dropped
	w.TriageRejected += o.TriageRejected
}

func (w *MetricWeek) kind(k model.AskKind, n int) {
	if w.ByKind == nil {
		w.ByKind = map[model.AskKind]int{}
	}
	w.ByKind[k] += n
}

// MetricCounts are the weeks' counts, newest week first, per project and
// in total, read from the tickets' histories alone.
type MetricCounts struct {
	Starts   [MetricWeeks]time.Time // each week's Monday, newest first
	Total    [MetricWeeks]MetricWeek
	Projects map[string]*[MetricWeeks]MetricWeek
}

// openAsk is an ask a ticket put to a person and nobody has answered yet.
type openAsk struct {
	kind  model.AskKind
	since time.Time
}

// CountMetrics counts the floor's metrics for the week holding start and the
// four before it, as v sees them: one pass over each ticket's history.
// Board tickets are not counted, as they are not on the floor.
//
// An ask opens when a ticket arrives in a column that asks a person
// (needs_human, ready_for_pr, pr_open), its kind read from the move as the
// desk read it then, and closes at the ticket's next move. It counts as
// answered, in the week of the answer, when a person made that move, or
// when the PR it asked about was merged or closed on GitHub or had changes
// requested there, or when someone asked the factory to merge it (a pr
// merge_requested event). Only the viewer's own answers count unless the
// viewer is the maintainer, who sees everyone's; an answer whose history
// names nobody (GitHub's, a merge request, older boards) is the
// maintainer's.
func CountMetrics(ts []*ticket.Ticket, start time.Time, v Viewer) MetricCounts {
	return new(reader).countMetrics(ts, start, v)
}

func (r *reader) countMetrics(ts []*ticket.Ticket, start time.Time, v Viewer) MetricCounts {
	start = weekStart(start)
	var c MetricCounts
	for i := range c.Starts {
		c.Starts[i] = start.AddDate(0, 0, -7*i)
	}
	end := start.AddDate(0, 0, 7)
	week := func(at time.Time) int {
		if at.IsZero() || !at.Before(end) || at.Before(c.Starts[MetricWeeks-1]) {
			return -1
		}
		for i, s := range c.Starts {
			if !at.Before(s) {
				return i
			}
		}
		return -1
	}
	c.Projects = map[string]*[MetricWeeks]MetricWeek{}
	for _, t := range ts {
		if t.Board() {
			continue
		}
		var weeks *[MetricWeeks]MetricWeek
		at := func(i int) *MetricWeek {
			if weeks == nil {
				weeks = c.Projects[t.Project]
				if weeks == nil {
					weeks = new([MetricWeeks]MetricWeek)
					c.Projects[t.Project] = weeks
				}
			}
			return &weeks[i]
		}
		var open *openAsk
		answer := func(when time.Time, by string) {
			a := open
			open = nil
			if v.Maintainer || v.made(by) {
				if i := week(when); i >= 0 {
					w := at(i)
					w.Answered++
					w.kind(a.kind, 1)
					w.Waits = append(w.Waits, max(when.Sub(a.since), 0))
				}
			}
		}
		for _, h := range t.History {
			when := ParseTime(h.At)
			switch h.Event {
			case "created":
				if col := r.createdIn(h); isAskColumn(col) {
					open = &openAsk{kind: createdAskKind(t), since: when}
				}
			case "pr":
				if open != nil && open.kind == model.AskReview && r.prAction(h) == ticket.PRMergeRequested {
					answer(when, "")
				}
			case "move":
				m := r.moveOf(h)
				i := week(when)
				if i >= 0 {
					r.countMove(at(i), m, open)
				}
				settle := m.Is(model.ReasonOperatorSettled, false)
				kind, asks := model.AskKind(""), isAskColumn(m.To)
				if asks {
					kind = r.arrivalKind(t, m)
				}
				if settle {
					kind, asks = settledAsk(m.Text)
					if open != nil && asks && byOf(h) == "factory" {
						// the tick's settle refreshes the ask it keeps open:
						// the wait goes on from when it arrived, as the ask
						// the settle left
						open.kind = kind
						continue
					}
				}
				if open != nil && (!settle || byOf(h) != "factory") {
					if by, ok := r.answeredBy(h, m, open); ok {
						answer(when, by)
					}
				}
				open = nil
				if asks {
					open = &openAsk{kind: kind, since: when}
				}
			}
		}
	}
	for _, weeks := range c.Projects {
		for i := range weeks {
			c.Total[i].add(&weeks[i])
		}
	}
	return c
}

// countMove counts a move's send-back or outcome into w; open is the ask
// the move closes, if any.
func (r *reader) countMove(w *MetricWeek, m MoveFacts, open *openAsk) {
	d := m.Text
	switch {
	case m.Is(model.ReasonReviewChanges, d == "review asked for changes"):
		w.Reworks++
	case m.Is(model.ReasonChangesRequested, strings.HasPrefix(d, "changes requested")):
		w.ChangesRequested++
	case m.Is(model.ReasonRedirected, strings.HasPrefix(d, "redirected")):
		w.Redirects++
	case m.Is(model.ReasonPRMerged, strings.HasPrefix(d, "PR merged")):
		w.Merged++
	case m.Is(model.ReasonPRClosed, strings.HasPrefix(d, "PR closed")):
		w.ClosedUnmerged++
	case m.Is(model.ReasonTriageRejected, strings.HasPrefix(d, "triage: ")) && m.To == string(model.ColRejected):
		w.TriageRejected++
	case m.Human && m.To == string(model.ColRejected):
		w.Dropped++
	case m.Human && m.To == string(model.ColReady) && open != nil &&
		(open.kind == model.AskReview || open.kind == model.AskOpen || open.kind == model.AskDraft):
		// the desk's "Send back with a note"
		w.SentBack++
	}
}

// answeredBy is who answered the open ask with move h, and whether a person
// did: a person's own move, or what a person did to the PR on GitHub, which
// the history does not name ("").
func (r *reader) answeredBy(h ticket.Entry, m MoveFacts, open *openAsk) (string, bool) {
	if mv, ok := h.Data.(ticket.Moved); ok && mv.By != "" && mv.By != "factory" {
		return mv.By, true
	}
	d := m.Text
	switch {
	case m.Human,
		m.Is(model.ReasonDesignApproved, strings.HasPrefix(d, "design approved")),
		m.Is(model.ReasonProposalApproved, strings.HasPrefix(d, "proposal approved")),
		m.Is(model.ReasonRedirected, strings.HasPrefix(d, "redirected")):
		return byOf(h), true
	case open.kind == model.AskReview && (m.Is(model.ReasonPRMerged, strings.HasPrefix(d, "PR merged")) ||
		m.Is(model.ReasonPRClosed, strings.HasPrefix(d, "PR closed")) ||
		m.Is(model.ReasonChangesRequested, strings.HasPrefix(d, "changes requested"))):
		return "", true
	}
	return "", false
}

// prAction is a pr event's action ("" on older history, which never says).
func (r *reader) prAction(e ticket.Entry) string {
	if p, ok := e.Data.(ticket.PRNote); ok {
		return p.Action
	}
	return ""
}

func isAskColumn(col string) bool {
	for _, c := range model.AskColumns() {
		if col == string(c) {
			return true
		}
	}
	return false
}

// createdAskKind is the ask of a ticket filed straight into a column that
// asks a person, as ticket.AskFor reads it with no move behind it.
func createdAskKind(t *ticket.Ticket) model.AskKind {
	switch {
	case t.KindName() == string(model.KindProposal):
		return model.AskProposal
	case t.Deploy.Present() && t.Project == model.NoticeProject:
		return model.AskDeploy
	case t.KindName() == string(model.KindTask), t.Project == model.NoticeProject && !t.Kind.Present():
		return model.AskNotice
	}
	return model.AskUnstick
}

// settledAsk is what an operator settle move left a person to answer, read
// from the tally in its detail ("operator: 1 applied, 1 waits for you"):
// nothing while the factory has an item still to run (the tap, a pass that
// left one), the draft ask while one waits, failed or is by hand, else the
// notice. What follows the tally after "; " is the pass's own lines.
func settledAsk(text string) (model.AskKind, bool) {
	_, tally, ok := strings.Cut(text, "operator: ")
	if !ok {
		return model.AskDraft, true
	}
	tally, _, _ = strings.Cut(tally, ";")
	has := func(words ...string) bool {
		for _, part := range strings.Split(tally, ", ") {
			for _, w := range words {
				if _, rest, ok := strings.Cut(part, " "); ok && rest == w {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("waits for you", "wait for you", "failed", "to apply by hand"):
		return model.AskDraft, true
	case has("requested", "new"):
		return "", false
	}
	return model.AskNotice, true
}

// arrivalKind is the kind of the ask move m put to a person, as
// ticket.AskFor decided it when the move was made: read from the move's
// reason (or older history's wording), not from the ticket as it is now.
func (r *reader) arrivalKind(t *ticket.Ticket, m MoveFacts) model.AskKind {
	switch model.Column(m.To) {
	case model.ColPROpen:
		return model.AskReview
	case model.ColReadyForPR:
		return model.AskOpen
	}
	d, kind := m.Text, t.KindName()
	switch {
	case kind == string(model.KindProposal):
		return model.AskProposal
	case m.Is(model.ReasonDesignReady, strings.HasPrefix(d, "design ready")):
		return model.AskDesign
	case m.Is(model.ReasonManagerDrafted, strings.HasPrefix(d, "manager drafted")),
		m.Is(model.ReasonOperatorSettled, false):
		return model.AskDraft
	case m.Is(model.ReasonManagerAnswered, strings.HasPrefix(d, "manager answered")):
		return model.AskNotice
	case m.Is(model.ReasonFindingsVerified, strings.HasPrefix(d, "findings verified")):
		return model.AskFindings
	case m.Is(model.ReasonDeployHeld, false):
		return model.AskDeploy
	case m.Is(model.ReasonAgentQuestion, strings.Contains(d, " asks")):
		return model.AskQuestion
	case kind == string(model.KindTask) && (m.Reason == string(model.ReasonTaskDone) ||
		m.Reason == string(model.ReasonTaskBlocked) || m.Reason == "" && strings.HasPrefix(d, "task ")):
		return model.AskNotice
	case m.Is(model.ReasonTriageAccepted, strings.HasPrefix(d, "triage accepted")):
		return model.AskStart
	case m.Is(model.ReasonTriageUnsure, strings.HasPrefix(d, "triage unsure")):
		return model.AskDecide
	}
	return model.AskUnstick
}

// MetricRow is one line of a metrics table: a label and one cell per week,
// newest first, or NotTracked in place of the cells.
type MetricRow struct {
	Label      string
	Cells      []string
	Sub        bool   // a breakdown of the line above
	NotTracked string // why there are no numbers
}

// MetricGroup is one block of rows: your attention, send-backs, outcomes.
type MetricGroup struct {
	Title, Note string
	Rows        []MetricRow
}

// MetricTable is one project's metrics, or all of them, under the weeks'
// column heads.
type MetricTable struct {
	Name   string
	Weeks  []string
	Open   bool // shown unfolded: the total
	Groups []MetricGroup
}

// Metrics is the floor's metrics block: the week shown and the four
// before it, in total and per project (only the projects with something to
// count).
type Metrics struct {
	Weeks    []string // the column heads, newest first
	Total    MetricTable
	Projects []MetricTable
	Quiet    bool // nothing to count in any of the weeks
}

// BuildMetrics lays c out for the floor, as v sees it at now.
func BuildMetrics(c MetricCounts, now time.Time, v Viewer) Metrics {
	var m Metrics
	for _, s := range c.Starts {
		if !now.Before(s) && now.Before(s.AddDate(0, 0, 7)) {
			m.Weeks = append(m.Weeks, "This week")
		} else {
			m.Weeks = append(m.Weeks, s.Format("2 Jan"))
		}
	}
	m.Total = metricTable("All projects", &c.Total, v)
	m.Total.Weeks, m.Total.Open = m.Weeks, true
	names := make([]string, 0, len(c.Projects))
	for name, weeks := range c.Projects {
		for i := range weeks {
			if weeks[i].any() {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	for _, name := range names {
		t := metricTable(name, c.Projects[name], v)
		t.Weeks = m.Weeks
		m.Projects = append(m.Projects, t)
	}
	m.Quiet = len(names) == 0
	return m
}

func metricTable(name string, weeks *[MetricWeeks]MetricWeek, v Viewer) MetricTable {
	row := func(label string, f func(w *MetricWeek) string) MetricRow {
		r := MetricRow{Label: label}
		for i := range weeks {
			r.Cells = append(r.Cells, f(&weeks[i]))
		}
		return r
	}
	count := func(label string, f func(w *MetricWeek) int) MetricRow {
		return row(label, func(w *MetricWeek) string { return strconv.Itoa(f(w)) })
	}
	attention := MetricGroup{Title: "Your attention", Note: "asks you answered, and how long each waited for you"}
	if v.Maintainer {
		attention.Note = "asks answered on the desk, by anyone, and how long each waited"
	}
	attention.Rows = append(attention.Rows, count("Asks answered", func(w *MetricWeek) int { return w.Answered }))
	for _, info := range model.AskKinds() {
		seen := false
		for i := range weeks {
			seen = seen || weeks[i].ByKind[info.Kind] > 0
		}
		if seen {
			k := info.Kind
			r := count(info.Label, func(w *MetricWeek) int { return w.ByKind[k] })
			r.Sub = true
			attention.Rows = append(attention.Rows, r)
		}
	}
	attention.Rows = append(attention.Rows,
		row("Median wait", func(w *MetricWeek) string { return waitText(w, w.Median()) }),
		row("Longest wait", func(w *MetricWeek) string { return waitText(w, w.Longest()) }))
	sendBacks := MetricGroup{Title: "Send-backs", Note: "work that came back for another round", Rows: []MetricRow{
		count("Reviewer reworks", func(w *MetricWeek) int { return w.Reworks }),
		count("Changes requested on GitHub", func(w *MetricWeek) int { return w.ChangesRequested }),
		count("Designs redirected", func(w *MetricWeek) int { return w.Redirects }),
		count("Sent back from the desk", func(w *MetricWeek) int { return w.SentBack }),
	}}
	outcomes := MetricGroup{Title: "Outcomes", Rows: []MetricRow{
		count("PRs merged", func(w *MetricWeek) int { return w.Merged }),
		count("PRs closed unmerged", func(w *MetricWeek) int { return w.ClosedUnmerged }),
		count("Dropped by a person", func(w *MetricWeek) int { return w.Dropped }),
		count("Closed by triage", func(w *MetricWeek) int { return w.TriageRejected }),
		{Label: "Reverts", NotTracked: "not tracked: nothing records a merged PR's revert yet"},
	}}
	return MetricTable{Name: name, Groups: []MetricGroup{attention, sendBacks, outcomes}}
}

// waitText is a wait in words, "–" for a week with no answers.
func waitText(w *MetricWeek, d time.Duration) string {
	switch {
	case len(w.Waits) == 0:
		return "–"
	case d < time.Minute:
		return "<1 min"
	}
	return Age(d)
}
