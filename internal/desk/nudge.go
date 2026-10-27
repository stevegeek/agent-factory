package desk

import (
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// A nudge is a ticket put in front of one person by someone else: a note,
// comment, question or answer that mentions @them, or the ticket passed to
// them. It stays on their desk, whatever its project, until they put it
// away or write on the ticket themselves.

// mentionRe is an @name: not inside an email address or a longer word.
var mentionRe = regexp.MustCompile(`(?:^|[^\w@.])@([a-z][a-z0-9-]{0,31})\b`) // re: go

// Mentions are the people of names that text mentions as @name.
func Mentions(text string, people []string) []string {
	var out []string
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		if slices.Contains(people, m[1]) && !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Nudge is the newest thing that put a ticket in front of the viewer.
type Nudge struct {
	T      *ticket.Ticket
	Passed bool   // passed to them; else a mention
	By     string // who
	Text   string // what they wrote, or the pass's line
	At     string
}

// writing is one piece of text a person put on a ticket.
type writing struct{ by, at, text string }

// writings are the texts people wrote on t, each with who and when: its
// notes, comments, and board questions and answers.
func writings(t *ticket.Ticket) []writing {
	noteBy := map[int]string{}
	for _, h := range t.History {
		if d, ok := h.Data.(ticket.Noted); ok {
			noteBy[d.Note] = d.By
		}
	}
	var out []writing
	for i, n := range t.Notes {
		out = append(out, writing{noteBy[i], n.At, n.Text})
	}
	for _, c := range t.Comments {
		out = append(out, writing{c.By, c.At, c.Text})
	}
	for _, q := range t.Questions {
		out = append(out, writing{q.By, q.At, q.Text})
		if a, ok := q.Answer.Get(); ok {
			out = append(out, writing{q.AnsweredBy.Or(""), q.AnsweredAt.Or(""), a})
		}
	}
	return out
}

// Nudges are the tickets put in front of v and not yet put away, newest
// first: one per ticket, its newest nudge.
func Nudges(ts []*ticket.Ticket, v present.Viewer) []Nudge {
	if v.By == "" {
		return nil
	}
	var out []Nudge
	for _, t := range ts {
		var best *Nudge
		var mine time.Time // the last time v wrote on t: older nudges are answered
		consider := func(n Nudge) {
			if best == nil || present.ParseTime(n.At).After(present.ParseTime(best.At)) {
				best = &n
			}
		}
		for _, w := range writings(t) {
			if w.by == v.By {
				mine = maxTime(mine, present.ParseTime(w.at))
				continue
			}
			if slices.Contains(Mentions(w.text, v.People), v.By) {
				consider(Nudge{T: t, By: w.by, Text: textx.Clip(w.text, 300), At: w.at})
			}
		}
		if t.Assignee.Or("") == v.By {
			for i := len(t.History) - 1; i >= 0; i-- {
				h := t.History[i]
				if d, ok := h.Data.(ticket.Edited); ok && slices.Contains(d.Fields, "assignee") {
					if d.By != v.By {
						consider(Nudge{T: t, Passed: true, By: d.By, Text: h.Detail, At: h.At})
					}
					break
				}
			}
		}
		if best == nil || !present.ParseTime(best.At).After(mine) {
			continue
		}
		if seen, ok := v.Dismissed[string(t.ID)]; ok && !present.ParseTime(best.At).After(present.ParseTime(seen)) {
			continue
		}
		out = append(out, *best)
	}
	sort.SliceStable(out, func(i, j int) bool { return present.ParseTime(out[i].At).After(present.ParseTime(out[j].At)) })
	return out
}

// ForYouItem is one row of the desk's For you: a nudge.
type ForYouItem struct {
	ID, Title, Project, Href string
	Passed                   bool
	By, Text, At, Age        string
}

// ForYou are the nudges as the desk lists them.
func ForYou(ns []Nudge, now time.Time) []ForYouItem {
	out := make([]ForYouItem, 0, len(ns))
	for _, n := range ns {
		out = append(out, ForYouItem{ID: string(n.T.ID), Title: n.T.Title, Project: n.T.Project, Href: present.TicketHref(string(n.T.ID), ""),
			Passed: n.Passed, By: n.By, Text: n.Text, At: n.At, Age: present.AgeSince(n.At, now)})
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
