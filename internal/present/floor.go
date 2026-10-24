package present

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// FloorEvent is one line of the floor: one ticket's latest outcome on a day.
type FloorEvent struct {
	At                         time.Time
	Time                       string
	Class, Label, Line, Detail string
	T                          *ticket.Ticket
	Story                      Story
	Earlier                    []string
}

// FloorDay is one day of the floor, newest event first.
type FloorDay struct {
	Name   string
	Events []FloorEvent
}

// Running is an agent at work now.
type Running struct {
	T       *ticket.Ticket
	Role    string
	Doing   string // what the run is doing, when its role does not say
	Host    string
	Elapsed string
	started time.Time // In flight's order
}

// Floor is the week's log: what happened, day by day, what runs now and how
// many asks wait.
type Floor struct {
	Start      time.Time
	Title      string
	Summary    string
	Running    []Running
	Days       []FloorDay
	Prev, Next string
	Asks       int
	Metrics    Metrics
}

// weekStart is the Monday 00:00 (local time) of the week holding d.
func weekStart(d time.Time) time.Time {
	d = d.In(time.Local)
	off := (int(d.Weekday()) + 6) % 7
	return time.Date(d.Year(), d.Month(), d.Day()-off, 0, 0, 0, 0, time.Local)
}

// BuildFloor is the floor of the week holding start, as v sees it at now.
// Board tickets are not on it.
func BuildFloor(ts []*ticket.Ticket, start, now time.Time, v Viewer) Floor {
	return new(reader).floor(ts, start, now, v)
}

func (r *reader) floor(ts []*ticket.Ticket, start, now time.Time, v Viewer) Floor {
	start = weekStart(start)
	end := start.AddDate(0, 0, 7)
	all := ByID(ts)
	f := Floor{Start: start, Prev: start.AddDate(0, 0, -7).Format("2006-01-02")}
	f.Title = "Week of " + start.Format("2 January")
	if !now.Before(start) && now.Before(end) {
		f.Title = "This week"
	}
	if !end.After(weekStart(now)) {
		f.Next = end.Format("2006-01-02")
	}
	touched := map[ticket.ID]bool{}
	var opened, merged, kept, dropped, closedChecks int
	var evs []FloorEvent
	for _, t := range append(append([]*ticket.Ticket{}, ts...), r.droppedProposals(ts)...) {
		if t.Board() {
			continue
		}
		var story *Story
		for i, h := range t.History {
			at := ParseTime(h.At)
			if p := all[parentOf(t)]; h.Event == "panel" && p != nil && !r.panelTime(p).IsZero() {
				at = r.panelTime(p) // a proposal filed later by a migration belongs to its panel's day
			}
			if at.Before(start) || !at.Before(end) {
				continue
			}
			if t.ID != "" {
				touched[t.ID] = true
			}
			cls, label, detail := r.floorOutcome(t, i)
			if cls == "" {
				continue
			}
			switch label {
			case "PR opened":
				opened++
			case "Merged":
				merged++
			case "Closed":
				if r.moveOf(h).Is(model.ReasonTriageRejected, strings.Contains(h.Detail, "triage: ")) {
					closedChecks++
				}
			}
			if h.Event == "panel" {
				if cls == "kept" {
					kept++
				} else {
					dropped++
				}
				if cls == "kept" {
					continue // the prospect's line covers the ones kept
				}
			}
			if story == nil {
				s := r.story(t, all)
				story = &s
			}
			line := t.Title
			if t.ID != "" {
				line = string(t.ID) + " " + t.Title
			}
			evs = append(evs, FloorEvent{At: at, Class: cls, Label: label, Line: line, Detail: detail, T: t, Story: *story})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.After(evs[j].At) })
	// one line per ticket per day: its latest outcome, naming what came before it
	type slot struct{ day, ev int }
	seen := map[*ticket.Ticket]slot{}
	for _, e := range evs {
		e.Time = e.At.Local().Format("15:04")
		name := e.At.Local().Format("Monday 2 January")
		if len(f.Days) == 0 || f.Days[len(f.Days)-1].Name != name {
			f.Days = append(f.Days, FloorDay{Name: name})
		}
		if s, ok := seen[e.T]; ok && s.day == len(f.Days)-1 {
			last := &f.Days[s.day].Events[s.ev]
			if len(last.Earlier) == 0 || last.Earlier[0] != lowerLabel(e.Label) {
				last.Earlier = append([]string{lowerLabel(e.Label)}, last.Earlier...)
			}
			continue
		}
		d := &f.Days[len(f.Days)-1]
		d.Events = append(d.Events, e)
		seen[e.T] = slot{len(f.Days) - 1, len(d.Events) - 1}
	}
	parts := []string{Plural(len(touched), "ticket")}
	if opened+merged > 0 {
		parts = append(parts, fmt.Sprintf("%s opened, %d merged", Plural(opened, "PR"), merged))
	}
	if kept+dropped > 0 {
		parts = append(parts, fmt.Sprintf("%s, %d dropped by the panel", Plural(kept+dropped, "proposal"), dropped))
	}
	if closedChecks > 0 {
		parts = append(parts, Plural(closedChecks, "check")+" closed with nothing to do")
	}
	f.Summary = strings.Join(parts, " · ")
	for _, t := range ts {
		if a, ok := agentOf(t); ok {
			run := Running{T: t, Role: roleOf(a.Stage), Host: localAsEmpty(a.Host)}
			if s := ParseTime(a.Started); !s.IsZero() {
				run.Elapsed = Age(now.Sub(s))
			}
			f.Running = append(f.Running, run)
		}
	}
	f.Asks = Waiting(r.desk(ts, v))
	f.Metrics = BuildMetrics(r.countMetrics(ts, start, v), now, v)
	return f
}

// FloorOutcome is the pill class and label and the one-line detail for
// history entry i of t, or "" when the entry is not worth a line.
func FloorOutcome(t *ticket.Ticket, i int) (cls, label, detail string) {
	return new(reader).floorOutcome(t, i)
}

func (r *reader) floorOutcome(t *ticket.Ticket, i int) (cls, label, detail string) {
	h := t.History[i]
	// what waited on the maintainer reads as waiting only while it still does
	then := func(waiting string) string {
		if w := r.thenWhat(t, i); w != "" {
			return w
		}
		return waiting
	}
	p, hasPanel := t.Panel.Get()
	if h.Event == "panel" {
		rule := h.Rule
		if rule == "" && hasPanel {
			rule = PanelRule(p.Scores)
		}
		scores := ""
		if hasPanel {
			var s []string
			for _, x := range p.Scores {
				s = append(s, fmt.Sprint(x))
			}
			scores = "scored " + strings.Join(s, " · ") + " · "
		}
		if strings.HasPrefix(rule, "dropped") {
			return "dropped", "Dropped", scores + "the panel dropped it; it never reached your desk"
		}
		return "kept", "Kept", scores + then("waiting on your desk")
	}
	if h.Event == "created" && t.Column == string(model.ColNeedsHuman) && t.Project == model.NoticeProject &&
		r.createdIn(h) == string(model.ColNeedsHuman) {
		return "waiting", "Notice", "the factory needed you to look at something · " + then("waiting on your desk")
	}
	if h.Event != "move" {
		return "", "", ""
	}
	m := r.moveOf(h)
	to, d, human := m.To, m.Text, m.Human
	switch {
	case to == string(model.ColPROpen):
		n := ""
		if m := prRef.FindStringSubmatch(d); m != nil {
			n = "#" + m[1]
		}
		if strings.HasPrefix(d, "draft") {
			return "pr", "PR opened", "draft PR " + n + " · needs your review"
		}
		return "pr", "PR opened", "PR " + n + " · auto-merge eligible"
	case m.Is(model.ReasonPRMerged, strings.HasPrefix(d, "PR merged")):
		return "merged", "Merged", strings.TrimPrefix(d, "PR ")
	case m.Is(model.ReasonPRClosed, strings.HasPrefix(d, "PR closed")):
		return "closed", "Closed", "the PR was closed without merging"
	case to == string(model.ColReadyForPR):
		if d == "auto-merge eligible" {
			return "kept", "Gate passed", "auto-merge eligible"
		}
		return "kept", "Gate passed", "needs a maintainer's review"
	case to == string(model.ColNeedsHuman) && !human:
		switch {
		case m.Is(model.ReasonTriageAccepted, strings.HasPrefix(d, "triage accepted")):
			return "waiting", "Accepted", "triage accepted it · " + then("waits for you to start it")
		case m.Is(model.ReasonTriageUnsure, strings.HasPrefix(d, "triage unsure")):
			return "waiting", "Unsure", "triage could not decide · " + then("waiting on your desk")
		case m.Is(model.ReasonDesignReady, strings.HasPrefix(d, "design ready")):
			return "waiting", "Design ready", Plural(len(t.Proposed), "ticket") + " proposed · " + then("waiting on your desk")
		case m.Is(model.ReasonTaskDone, strings.HasPrefix(d, "task done")):
			return "info", "Answered", Clip(strings.TrimPrefix(d, "task done: "), 140)
		case m.Is(model.ReasonGateFailed, strings.HasPrefix(d, "gate:")):
			return "failed", "Gate failed", strings.TrimSpace(strings.TrimPrefix(d, "gate:"))
		}
		return "failed", "Failed", Clip(d, 140)
	case to == string(model.ColRejected):
		if m.Is(model.ReasonTriageRejected, strings.HasPrefix(d, "triage: ")) {
			return "info", "Closed", Clip(strings.TrimPrefix(d, "triage: "), 140)
		}
		return "closed", "Dropped", "by you"
	case to == string(model.ColDone):
		switch {
		case m.Is(model.ReasonPanelDone, strings.HasPrefix(d, "panel kept")):
			n := 0
			if pr, ok := t.PanelResult.Get(); ok {
				n = len(pr.Kept)
			}
			return "kept", fmt.Sprintf("%d kept", n), d
		case m.Is(model.ReasonProposalApproved, strings.HasPrefix(d, "proposal approved")),
			m.Is(model.ReasonDesignApproved, strings.HasPrefix(d, "design approved")):
			parts := strings.SplitN(d, "; ", 2)
			return "kept", "Approved", parts[len(parts)-1]
		case m.Is(model.ReasonProspectEmpty, strings.HasPrefix(d, "prospecting found nothing")):
			return "info", "Nothing found", "the prospector found nothing worth proposing"
		case m.Is(model.ReasonAnsweredOn, strings.HasPrefix(d, "answered on ")):
			return "info", "Answered", "the answer went back to " + strings.TrimPrefix(d, "answered on ")
		}
		return "info", "Done", "closed by you"
	case human && to == string(model.ColReady):
		return "info", "Started", "you moved it to Ready"
	case human:
		return "info", "Moved by you", "you moved it to " + ColumnLabel(to)
	case m.Is(model.ReasonRedirected, strings.HasPrefix(d, "redirected")):
		return "info", "Redirected", "the design went back to the designer"
	case m.Is(model.ReasonReviewChanges, d == "review asked for changes"):
		return "info", "Changes asked", "the reviewer sent it back to the worker"
	}
	return "", "", ""
}

// thenWhat is what became of an entry that waited on the maintainer, or ""
// while it still waits.
func (r *reader) thenWhat(t *ticket.Ticket, i int) string {
	for _, h := range t.History[i+1:] {
		if h.Event != "move" {
			continue
		}
		m := r.moveOf(h)
		to, d, human := m.To, m.Text, m.Human
		switch {
		case m.Is(model.ReasonDesignApproved, strings.HasPrefix(d, "design approved")):
			return "you approved it"
		case m.Is(model.ReasonProposalApproved, strings.HasPrefix(d, "proposal approved")):
			return "you filed it"
		case m.Is(model.ReasonRedirected, strings.HasPrefix(d, "redirected")):
			return "you sent it back"
		case human && to == string(model.ColReady):
			return "you started it"
		case human && to == string(model.ColTriage):
			return "you asked triage again"
		case human && to == string(model.ColRejected):
			return "you dropped it"
		case human && to == string(model.ColDone):
			return "you closed it"
		case human:
			return "you moved it to " + ColumnLabel(to)
		}
		return "it moved on to " + ColumnLabel(to)
	}
	if AskFor(t) != nil {
		return ""
	}
	return "waited for you"
}

func lowerLabel(s string) string {
	if strings.HasPrefix(s, "PR") {
		return s
	}
	return strings.ToLower(s)
}
