package present

import (
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// afterFacts are the tickets t waits for, each with its column, and the
// open tickets that wait for t.
func afterFacts(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) []Fact {
	var f []Fact
	if len(t.After) > 0 {
		var parts []string
		tone := ""
		for _, id := range t.After {
			col := "missing"
			if d, ok := all[id]; ok {
				col = ColumnLabel(d.Column)
				if d.Column != string(model.ColDone) && !closed(t) {
					tone = "warn"
				}
			}
			parts = append(parts, string(id)+" ("+col+")")
		}
		f = append(f, Fact{Label: "Waiting for", Value: strings.Join(parts, ", "), Tone: tone})
	}
	var blocks []string
	for id, o := range all {
		if slices.Contains(o.After, t.ID) && !closed(o) {
			blocks = append(blocks, string(id))
		}
	}
	if len(blocks) > 0 {
		slices.Sort(blocks)
		f = append(f, Fact{Label: "Blocks", Value: strings.Join(blocks, ", ")})
	}
	return f
}
