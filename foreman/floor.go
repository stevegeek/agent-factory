package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type FloorEvent struct {
	At                         time.Time
	Time                       string
	Class, Label, Line, Detail string
	T                          *Ticket
	Story                      Story
	Earlier                    []string
}

type FloorDay struct {
	Name   string
	Events []FloorEvent
}

type Running struct {
	T       *Ticket
	Role    string
	Host    string
	Elapsed string
}

type Floor struct {
	Start      time.Time
	Title      string
	Summary    string
	Running    []Running
	Days       []FloorDay
	Prev, Next string
	Asks       int
}

// weekStart is the Monday 00:00 (local time) of the week holding d.
func weekStart(d time.Time) time.Time {
	d = d.In(time.Local)
	off := (int(d.Weekday()) + 6) % 7
	return time.Date(d.Year(), d.Month(), d.Day()-off, 0, 0, 0, 0, time.Local)
}

func BuildFloor(ts []*Ticket, start, now time.Time) Floor {
	start = weekStart(start)
	end := start.AddDate(0, 0, 7)
	all := byID(ts)
	f := Floor{Start: start, Prev: start.AddDate(0, 0, -7).Format("2006-01-02")}
	f.Title = "Week of " + start.Format("2 January")
	if !now.Before(start) && now.Before(end) {
		f.Title = "This week"
	}
	if !end.After(weekStart(now)) {
		f.Next = end.Format("2006-01-02")
	}
	touched := map[string]bool{}
	var opened, merged, kept, dropped, closedChecks int
	var evs []FloorEvent
	for _, t := range append(append([]*Ticket{}, ts...), droppedProposals(ts)...) {
		var story *Story
		for i, h := range t.History {
			at := parseTime(h.At)
			if p := all[t.Parent]; h.Event == "panel" && p != nil && !panelTime(p).IsZero() {
				at = panelTime(p) // a proposal filed later by a migration belongs to its panel's day
			}
			if at.Before(start) || !at.Before(end) {
				continue
			}
			if t.ID != "" {
				touched[t.ID] = true
			}
			cls, label, detail := floorOutcome(t, i)
			if cls == "" {
				continue
			}
			switch label {
			case "PR opened":
				opened++
			case "Merged":
				merged++
			case "Closed":
				if strings.Contains(h.Detail, "triage: ") {
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
				s := BuildStory(t, all)
				story = &s
			}
			line := t.Title
			if t.ID != "" {
				line = t.ID + " " + t.Title
			}
			evs = append(evs, FloorEvent{At: at, Class: cls, Label: label, Line: line, Detail: detail, T: t, Story: *story})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.After(evs[j].At) })
	// one line per ticket per day: its latest outcome, naming what came before it
	type slot struct{ day, ev int }
	seen := map[*Ticket]slot{}
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
	parts := []string{plural(len(touched), "ticket")}
	if opened+merged > 0 {
		parts = append(parts, fmt.Sprintf("%s opened, %d merged", plural(opened, "PR"), merged))
	}
	if kept+dropped > 0 {
		parts = append(parts, fmt.Sprintf("%s, %d dropped by the panel", plural(kept+dropped, "proposal"), dropped))
	}
	if closedChecks > 0 {
		parts = append(parts, plural(closedChecks, "check")+" closed with nothing to do")
	}
	f.Summary = strings.Join(parts, " · ")
	for _, t := range ts {
		if t.Agent != nil {
			r := Running{T: t, Role: roles[t.Agent.Stage], Host: t.Agent.Host}
			if r.Host == "local" {
				r.Host = ""
			}
			if s := parseTime(t.Agent.Started); !s.IsZero() {
				r.Elapsed = age(now.Sub(s))
			}
			f.Running = append(f.Running, r)
		}
	}
	f.Asks = len(Desk(ts))
	return f
}

// floorOutcome: the pill and one-line detail for a history entry worth a line on the floor.
func floorOutcome(t *Ticket, i int) (cls, label, detail string) {
	h := t.History[i]
	// what waited on the maintainer reads as waiting only while it still does
	then := func(waiting string) string {
		if w := thenWhat(t, i); w != "" {
			return w
		}
		return waiting
	}
	if h.Event == "panel" {
		rule := h.Rule
		if rule == "" && t.Panel != nil {
			rule = panelRule(t.Panel.Scores)
		}
		scores := ""
		if t.Panel != nil {
			var s []string
			for _, x := range t.Panel.Scores {
				s = append(s, fmt.Sprint(x))
			}
			scores = "scored " + strings.Join(s, " · ") + " · "
		}
		if strings.HasPrefix(rule, "dropped") {
			return "dropped", "Dropped", scores + "the panel dropped it; it never reached your desk"
		}
		return "kept", "Kept", scores + then("waiting on your desk")
	}
	if h.Event == "created" && t.Column == "needs_human" && t.Project == noticeProject && strings.Contains(h.Detail, "column=needs_human") {
		return "waiting", "Notice", "the factory needed you to look at something · " + then("waiting on your desk")
	}
	if h.Event != "move" {
		return "", "", ""
	}
	head, d, _ := strings.Cut(h.Detail, ": ")
	_, to, _ := strings.Cut(head, " -> ")
	human := strings.HasPrefix(d, "by human")
	switch {
	case to == "pr_open":
		n := ""
		if m := prRef.FindStringSubmatch(d); m != nil {
			n = "#" + m[1]
		}
		if strings.HasPrefix(d, "draft") {
			return "pr", "PR opened", "draft PR " + n + " · needs your review"
		}
		return "pr", "PR opened", "PR " + n + " · auto-merge eligible"
	case strings.HasPrefix(d, "PR merged"):
		return "merged", "Merged", strings.TrimPrefix(d, "PR ")
	case strings.HasPrefix(d, "PR closed"):
		return "closed", "Closed", "the PR was closed without merging"
	case to == "ready_for_pr":
		if d == "auto-merge eligible" {
			return "kept", "Gate passed", "auto-merge eligible"
		}
		return "kept", "Gate passed", "needs a maintainer's review"
	case to == "needs_human" && !human:
		switch {
		case strings.HasPrefix(d, "triage accepted"):
			return "waiting", "Accepted", "triage accepted it · " + then("waits for you to start it")
		case strings.HasPrefix(d, "triage unsure"):
			return "waiting", "Unsure", "triage could not decide · " + then("waiting on your desk")
		case strings.HasPrefix(d, "design ready"):
			return "waiting", "Design ready", plural(len(t.Proposed), "ticket") + " proposed · " + then("waiting on your desk")
		case strings.HasPrefix(d, "task done"):
			return "info", "Answered", clip(strings.TrimPrefix(d, "task done: "), 140)
		case strings.HasPrefix(d, "gate:"):
			return "failed", "Gate failed", strings.TrimSpace(strings.TrimPrefix(d, "gate:"))
		}
		return "failed", "Failed", clip(d, 140)
	case to == "rejected":
		if strings.HasPrefix(d, "triage: ") {
			return "closed", "Closed", clip(strings.TrimPrefix(d, "triage: "), 140)
		}
		return "closed", "Dropped", "by you"
	case to == "done":
		switch {
		case strings.HasPrefix(d, "panel kept"):
			n := 0
			if t.PanelResult != nil {
				n = len(t.PanelResult.Kept)
			}
			return "kept", fmt.Sprintf("%d kept", n), d
		case strings.HasPrefix(d, "proposal approved"), strings.HasPrefix(d, "design approved"):
			return "kept", "Approved", strings.SplitN(d, "; ", 2)[len(strings.SplitN(d, "; ", 2))-1]
		case strings.HasPrefix(d, "prospecting found nothing"):
			return "closed", "Nothing found", "the prospector found nothing worth proposing"
		}
		return "info", "Done", "closed by you"
	case human && to == "ready":
		return "info", "Started", "you moved it to Ready"
	case human:
		return "info", "Moved by you", "you moved it to " + columnLabel(to)
	case strings.HasPrefix(d, "redirected"):
		return "info", "Redirected", "the design went back to the designer"
	case d == "review asked for changes":
		return "waiting", "Changes asked", "the reviewer sent it back to the worker"
	}
	return "", "", ""
}

// thenWhat: what became of an entry that waited on the maintainer, or "" while it still waits.
func thenWhat(t *Ticket, i int) string {
	for _, h := range t.History[i+1:] {
		if h.Event != "move" {
			continue
		}
		head, d, _ := strings.Cut(h.Detail, ": ")
		_, to, _ := strings.Cut(head, " -> ")
		human := strings.HasPrefix(d, "by human")
		switch {
		case strings.HasPrefix(d, "design approved"):
			return "you approved it"
		case strings.HasPrefix(d, "proposal approved"):
			return "you filed it"
		case strings.HasPrefix(d, "redirected"):
			return "you sent it back"
		case human && to == "ready":
			return "you started it"
		case human && to == "triage":
			return "you asked triage again"
		case human && to == "rejected":
			return "you dropped it"
		case human && to == "done":
			return "you closed it"
		case human:
			return "you moved it to " + columnLabel(to)
		}
		return "it moved on to " + columnLabel(to)
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

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}
