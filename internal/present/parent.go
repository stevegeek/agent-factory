package present

import "github.com/stevegeek/agent-factory/internal/ticket"

// ParentOf is the ticket t was filed from, "" for none or one not in all.
func ParentOf(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) string {
	id := t.Parent.Or("")
	if id == "" {
		if f, ok := t.FoundIn.Get(); ok {
			id = f.Ticket
		}
	}
	if id == "" || id == string(t.ID) || all[ticket.ID(id)] == nil {
		return ""
	}
	return id
}
