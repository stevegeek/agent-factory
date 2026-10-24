package boardread

import (
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// CheckBoard is `factory check --board`: every ticket file that does not
// read cleanly. That is a file that cannot be read at all, a ticket with
// fields of the wrong type, a ticket whose id is not its file's, and a
// T*.json whose name is not a ticket id (Python's pipeline reads it; the
// index and Foreman do not).
func (r *Reader) CheckBoard() []Problem {
	files, err := r.files()
	if err != nil {
		return []Problem{{File: r.Paths.Tickets, Err: err}}
	}
	var out []Problem
	for _, f := range files {
		p := f.p
		if p.Err == nil {
			switch id, ok := TicketFileName(f.name); {
			case !ok:
				p.Note = "the name is not a ticket id"
			case f.t.ID != id:
				p.Note = "it holds ticket " + string(f.t.ID) + ", not " + string(id)
			}
		}
		if p.Err != nil || len(p.Fields) > 0 || p.Note != "" {
			out = append(out, p)
		}
	}
	return out
}

// MissingAsk is a ticket waiting for a person with no stored ask.
type MissingAsk struct {
	ID     ticket.ID
	Column string
}

// CheckAsks is `factory check --asks`: every factory ticket in an ask
// column (model.AskColumns) without a stored ask, which Foreman does not
// show on the desk (it no longer derives asks, D11). Board tickets ask
// through their questions and are not counted.
func (r *Reader) CheckAsks() []MissingAsk {
	ts, _ := r.List()
	var out []MissingAsk
	for _, t := range ts {
		if t.Board() || !askColumn(t.Column) {
			continue
		}
		if a, ok := t.Ask.Get(); ok && a.Kind != "" {
			continue
		}
		out = append(out, MissingAsk{ID: t.ID, Column: t.Column})
	}
	return out
}

func askColumn(c string) bool {
	for _, a := range model.AskColumns() {
		if string(a) == c {
			return true
		}
	}
	return false
}
