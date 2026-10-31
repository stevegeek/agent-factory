package ops

import (
	"context"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// closeOut ends a ticket from the desk with no agent run: moved to column
// as `factory move` moves it, with note as the result of one that is done.
// An open question of the agent's is closed with it, and a bot/* branch
// with no PR is deleted (PRActions). The questions asked about the ticket
// close after it is saved (closeQuestions).
func (c *call) closeOut(ctx context.Context, t *ticket.Ticket, column, note string) error {
	what := "dropped: "
	if column == string(model.ColDone) {
		what = "closed: "
	}
	if err := c.move(ctx, t, column, what+textx.Prefix(note, 150)); err != nil {
		return err
	}
	if column == string(model.ColDone) {
		t.Result = ticket.Some(note)
	}
	if q, ok := t.Question.Get(); ok && q.Answered == "" {
		q.Answered = clockStamp(c)
		t.Question = ticket.Some(q)
	}
	branch := t.Branch.Or("")
	if pr, ok := t.PR.Get(); strings.HasPrefix(branch, "bot/") && (!ok || pr.Number == 0) && c.s.PR != nil {
		if err := c.s.PR.Prune(ctx, c.cfg, t); err != nil {
			c.print("warning: " + branch + " could not be deleted: " + err.Error())
		}
		c.wake = true
	}
	c.closed = t.ID
	return nil
}

// closeQuestions closes the open "Question on" tasks filed about parent,
// each under its own lock, with a move that names the parent. A task an
// agent is still answering stays open. It returns the lines to print.
func (s *Service) closeQuestions(ctx context.Context, parent ticket.ID) []string {
	ts, _ := s.Board.Reader.List()
	prefix := "Question on " + string(parent) + ":"
	var out []string
	for _, t := range ts {
		if t.Parent.Or("") != string(parent) || !strings.HasPrefix(t.Title, prefix) || model.Closed(t.Column) {
			continue
		}
		err := s.change(ctx, string(t.ID), func(c *call, t *ticket.Ticket) error {
			if model.Closed(t.Column) {
				return nil
			}
			return c.move(ctx, t, string(model.ColDone), "not needed: "+string(parent)+" is closed")
		})
		if err != nil {
			s.logf("%s: question %s left open: %v", parent, t.ID, err)
			out = append(out, string(t.ID)+" stays open: "+err.Error())
			continue
		}
		out = append(out, string(t.ID)+" closed")
	}
	return out
}
