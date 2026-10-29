package present

import (
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// afterFacts are the tickets t waits for, each with its title and column,
// and the open tickets that wait for t, each a link to its page.
func afterFacts(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) []Fact {
	var f []Fact
	for _, id := range t.After {
		tone := ""
		if d, ok := all[id]; !closed(t) && (!ok || d.Column != string(model.ColDone)) {
			tone = "warn"
		}
		f = append(f, Fact{Label: "Waiting for", Value: afterName(id, all), Tone: tone, Ticket: id})
	}
	var blocks []ticket.ID
	for id, o := range all {
		if slices.Contains(o.After, t.ID) && !closed(o) {
			blocks = append(blocks, id)
		}
	}
	slices.Sort(blocks)
	for _, id := range blocks {
		f = append(f, Fact{Label: "Blocks", Value: string(id) + " " + Clip(all[id].Title, 80), Ticket: id})
	}
	return f
}

// WaitLine is "Waiting for T0002 title (Review)" for the tickets an open t
// still waits for, or "" when it waits for none.
func WaitLine(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) string {
	if closed(t) {
		return ""
	}
	var open []string
	for _, id := range t.After {
		if d, ok := all[id]; !ok || d.Column != string(model.ColDone) {
			open = append(open, afterName(id, all))
		}
	}
	if len(open) == 0 {
		return ""
	}
	return "Waiting for " + strings.Join(open, ", ")
}

func afterName(id ticket.ID, all map[ticket.ID]*ticket.Ticket) string {
	d, ok := all[id]
	if !ok {
		return string(id) + " (missing)"
	}
	return string(id) + " " + Clip(d.Title, 80) + " (" + ColumnLabel(d.Column) + ")"
}
