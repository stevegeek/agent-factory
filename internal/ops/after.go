package ops

import (
	"strings"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// AnswerAfter sets the tickets this one waits for, from the note: ids
// separated by commas or spaces, or none to clear the list.
const AnswerAfter = "after"

// parseAfter reads ticket ids given as repeated values, each of which may
// hold several separated by commas or spaces.
func parseAfter(list []string) ([]ticket.ID, error) {
	var out []ticket.ID
	for _, v := range list {
		for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
			id, ok := ticket.ParseID(f)
			if !ok {
				return nil, refuse(BadRequest, "%q is not a ticket id", f)
			}
			out = append(out, id)
		}
	}
	return out, nil
}

// after checks the ids the ticket id is to wait for against the board.
func (s *Service) after(id ticket.ID, list []string) ([]ticket.ID, error) {
	ids, err := parseAfter(list)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	ts, _ := s.Board.Reader.List()
	ids, err = ticket.CheckAfter(id, ids, ticket.ByID(ts))
	if err != nil {
		return nil, refuse(BadRequest, "%s", err.Error())
	}
	return ids, nil
}

// setAfter replaces the list of tickets t waits for.
func (c *call) setAfter(t *ticket.Ticket, note string) error {
	if closedColumn(t.Column) {
		return refuse(Conflict, "a closed ticket waits for nothing")
	}
	ids, err := c.s.after(t.ID, []string{note})
	if err != nil {
		return err
	}
	t.After = ids
	if len(ids) == 0 {
		c.print(string(t.ID) + " waits for nothing")
		return nil
	}
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = string(id)
	}
	c.print(string(t.ID) + " waits for " + strings.Join(names, ", "))
	return nil
}
