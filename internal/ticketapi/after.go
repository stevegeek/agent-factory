package ticketapi

import (
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// cleanAfter checks the tickets id is to wait for: each one the caller
// sees, not id itself, and none that already waits for id.
func (cx *call) cleanAfter(id string, v jsonx.Value) ([]ticket.ID, error) {
	var ids []ticket.ID
	for _, s := range strsOf(v) {
		if !isTicketID(s) {
			return nil, bad("after", "no ticket %s", s)
		}
		if _, err := cx.visible(jstr(s)); err != nil {
			return nil, bad("after", "no ticket %s", s)
		}
		ids = append(ids, ticket.ID(s))
	}
	if len(ids) == 0 {
		return nil, nil
	}
	ts, _ := cx.s.Board.Reader.List()
	ids, err := ticket.CheckAfter(ticket.ID(id), ids, ticket.ByID(ts))
	if err != nil {
		return nil, bad("after", "%s", err.Error())
	}
	return ids, nil
}

// afterOnly is whether a person's update names nothing but the tickets
// it waits for: the one change the API makes to a factory ticket.
func (cx *call) afterOnly(o jsonx.Object) bool {
	_, ok := o.Lookup("after")
	return ok && cx.c.person && len(o) == 2
}

func afterStrs(ids []ticket.ID) jsonx.Value {
	ss := make([]string, len(ids))
	for i, id := range ids {
		ss[i] = string(id)
	}
	return strs(ss)
}
