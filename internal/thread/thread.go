package thread

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Related is one ticket in a ticket's thread or among its descendants.
type Related struct {
	ID, Title, Column, Kind string
	// How is how it came from the ticket above it: "Finding 2", "Question",
	// "Proposal", "Follow-up", or a link's rel.
	How                  string
	Open, Waits, Current bool
}

// RelGroup is the descendants of a ticket that came the same way.
type RelGroup struct {
	Label      string
	Tickets    []Related
	Open, Done int
}

// Relations are a ticket's place among the tickets filed from one another:
// the thread from its root down to it, and what was filed from it or links
// to it. Only tickets in the set it was built from appear.
type Relations struct {
	Thread []Related // root first, the ticket itself last; empty without a parent
	Groups []RelGroup
	// Lead is the header's words before the parent's id: "Finding 2 of",
	// "Question on", "Filed from"
	Lead, ParentID, ParentTitle string
}

// how is how c came from parent p, and the group that lists it.
func how(c, p *ticket.Ticket) (string, int) {
	if f, ok := c.FoundIn.Get(); ok && f.Ticket == string(p.ID) {
		return fmt.Sprintf("Finding %d", f.N), 0
	}
	if strings.HasPrefix(c.Title, "Question on "+string(p.ID)) || slices.Contains(askedOn(p), string(c.ID)) {
		return "Question", 1
	}
	switch model.Kind(p.KindName()) {
	case model.KindProposal, model.KindIntent, model.KindProspect:
		return "Proposal", 2
	}
	return "Follow-up", 3
}

// askedOn are the tasks filed for the questions asked on t.
func askedOn(t *ticket.Ticket) []string {
	var out []string
	for _, h := range t.History {
		if h.Event == string(model.EventAsked) {
			if id, _ := present.AskOf(h); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

func related(t *ticket.Ticket, how string) Related {
	return Related{ID: string(t.ID), Title: t.Title, Column: present.TicketColumnLabel(t), Kind: kindWord(t), How: how,
		Open: !model.Closed(t.Column), Waits: present.AskFor(t) != nil}
}

func kindWord(t *ticket.Ticket) string {
	if t.Board() {
		return "item"
	}
	if k := t.KindName(); k != "" {
		return k
	}
	return string(model.KindChange)
}

// Relate resolves t's relations from its parent and children, the finding
// it was filed from, the questions asked on it, its proposals and its
// links, among all.
func Relate(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) Relations {
	var r Relations
	r.Thread = []Related{related(t, "")}
	r.Thread[0].Current = true
	seen := map[string]bool{string(t.ID): true}
	for c := t; ; {
		pid := present.ParentOf(c, all)
		if pid == "" || seen[pid] {
			break
		}
		seen[pid] = true
		p := all[ticket.ID(pid)]
		r.Thread[0].How, _ = how(c, p)
		r.Thread = append([]Related{related(p, "")}, r.Thread...)
		c = p
	}
	if len(r.Thread) == 1 {
		r.Thread = nil
	} else {
		p := r.Thread[len(r.Thread)-2]
		r.ParentID, r.ParentTitle = p.ID, p.Title
		switch h := r.Thread[len(r.Thread)-1].How; {
		case strings.HasPrefix(h, "Finding"):
			r.Lead = h + " of"
		case h == "Question":
			r.Lead = "Question on"
		default:
			r.Lead = "Filed from"
		}
	}
	labels := []string{"Filed from its findings", "Questions asked on it", "Proposals and designs", "Follow-ups", "Links"}
	groups := make([][]Related, len(labels))
	listed := map[string]bool{string(t.ID): true}
	add := func(id string, g int, h string) {
		c := all[ticket.ID(id)]
		if c == nil || listed[id] {
			return
		}
		listed[id] = true
		groups[g] = append(groups[g], related(c, h))
	}
	kids := slices.Concat(t.Children, askedOn(t))
	for _, f := range t.Findings {
		kids = append(kids, f.Filed)
	}
	if pr, ok := t.PanelResult.Get(); ok {
		kids = slices.Concat(kids, pr.Kept, pr.DroppedTickets)
	}
	ids := slices.SortedFunc(maps.Keys(all), cmpID)
	for _, id := range ids {
		if present.ParentOf(all[id], all) == string(t.ID) {
			kids = append(kids, string(id))
		}
	}
	slices.SortFunc(kids, func(a, b string) int { return cmpID(ticket.ID(a), ticket.ID(b)) })
	for _, id := range kids {
		if c := all[ticket.ID(id)]; c != nil {
			h, g := how(c, t)
			if g != 0 {
				h = "" // the group's label says it
			}
			add(id, g, h)
		}
	}
	for _, l := range t.Links {
		add(l.Ticket, 4, l.Rel)
	}
	for _, id := range ids {
		for _, l := range all[id].Links {
			if l.Ticket == string(t.ID) {
				add(string(id), 4, l.Rel+" (from it)")
			}
		}
	}
	for i, rs := range groups {
		if len(rs) == 0 {
			continue
		}
		g := RelGroup{Label: labels[i], Tickets: rs}
		for _, x := range rs {
			if x.Open {
				g.Open++
			} else {
				g.Done++
			}
		}
		r.Groups = append(r.Groups, g)
	}
	return r
}

func cmpID(a, b ticket.ID) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(string(a), string(b))
}
