package ops

import (
	"context"
	"slices"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Pass gives a ticket to a person: its assignee becomes to, one of
// [board] people ("" gives it to nobody), and it goes on their desk until
// they put it away. by is who passed it ("" is the maintainer). A note,
// when typed, goes with it: a note on a factory ticket, for its next agent,
// or a comment on a board ticket. It returns a line for the person.
func (s *Service) Pass(ctx context.Context, id, to, by, note string) (string, error) {
	note, err := cleanNote(note)
	if err != nil {
		return "", err
	}
	var line string
	err = s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := c.actAs(by); err != nil {
			return err
		}
		if to != "" && !slices.Contains(c.cfg.People(), to) {
			return refuse(BadRequest, "%s is not one of [board] people", to)
		}
		if closedColumn(t.Column) {
			return refuse(Conflict, "%s is closed", t.ID)
		}
		// an agent working a board ticket holds it by its assignee: taking
		// that away would lock the agent out of its own work
		if as := t.Assignee.Or(""); t.Board() && t.Column == string(model.BoardDoing) && as != "" && !slices.Contains(c.cfg.People(), as) {
			return refuse(Conflict, "%s is being worked by %s; wait until it leaves Doing", t.ID, as)
		}
		who := c.by()
		if t.Assignee.Or("") == to && note == "" {
			line = string(t.ID) + " is already " + orNobody(to) + "'s"
			return nil
		}
		if to == "" {
			t.Assignee = ticket.Nil[string]()
		} else {
			t.Assignee = ticket.Some(to)
		}
		detail := who + " passed it to " + orNobody(to)
		if note != "" {
			detail += ": " + textx.Clip(note, 200)
		}
		ticket.Event(t, "edit", detail, "", ticket.Edited{By: who, Fields: []string{"assignee"}}, c.now)
		if note != "" {
			if t.Board() {
				n := 0
				for _, x := range t.Comments {
					n = max(n, x.N)
				}
				if t.Comments == nil {
					t.Comments = []ticket.Comment{}
				}
				t.Comments = append(t.Comments, ticket.Comment{N: n + 1, At: clock.Stamp(c.now), By: who, Type: "comment", Text: note})
				ticket.Event(t, "comment", who+": "+textx.Clip(note, 200), "", ticket.Commented{By: who, N: n + 1}, c.now)
			} else {
				ticket.AddNote(t, note, who, nil, c.now)
			}
		}
		line = string(t.ID) + " passed to " + orNobody(to)
		return nil
	})
	return line, err
}

func orNobody(name string) string {
	if name == "" {
		return "nobody"
	}
	return name
}
