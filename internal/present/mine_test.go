package present

import (
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var people = []string{"ann", "robin"}

func stampAt(m int) string {
	return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC).Add(time.Duration(m) * time.Minute).Format(time.RFC3339)
}

// An ask put to a person by name is on their desk alone, whatever its
// project; any other is on the desks that hold its project (none named:
// every project).
func TestAnAskGoesToItsPersonOrItsProjectsDesks(t *testing.T) {
	ann := Viewer{By: "ann", People: people}
	robin := Viewer{By: "robin", People: people, Desk: []string{"widgets"}}
	plain := &ticket.Ticket{ID: "T0001", Project: "gadgets", Column: "needs_human"}
	a := &ticket.Ask{Kind: string(model.AskQuestion)}
	if !ann.Mine(plain, a) || robin.Mine(plain, a) {
		t.Errorf("an unassigned ask: on every-project desks, off robin's widgets desk")
	}
	plain.Project = "widgets"
	if !robin.Mine(plain, a) {
		t.Errorf("robin's desk project")
	}
	passed := &ticket.Ticket{ID: "T0002", Project: "gadgets", Column: "needs_human", Assignee: ticket.Some("robin")}
	if !robin.Mine(passed, a) || ann.Mine(passed, a) {
		t.Errorf("an ask on a ticket assigned to robin is robin's alone")
	}
	// an assignee who is not a person (an agent) puts it to nobody
	passed.Assignee = ticket.Some("worker-1")
	if !ann.Mine(passed, a) || robin.Mine(passed, a) {
		t.Errorf("an agent's ticket goes by project")
	}
	// a board question to a person is theirs, even over the assignee
	q := &ticket.Ticket{ID: "T0003", Project: "notes", Column: "doing", Mode: ticket.Some("board"), Assignee: ticket.Some("ann"),
		Questions: []ticket.BoardQuestion{{N: 1, By: "agent", Recipient: "robin", ToPerson: true, Text: "which?", At: stampAt(0),
			Answer: ticket.Nil[string](), AnsweredBy: ticket.Nil[string](), AnsweredAt: ticket.Nil[string]()}}}
	qa := AskFor(q)
	if qa == nil || PutTo(q, qa, people) != "robin" || !robin.Mine(q, qa) || ann.Mine(q, qa) {
		t.Errorf("the question is robin's: %+v", qa)
	}
	// a recipient who is not one of the people (an older board's
	// "maintainer") goes by project
	q.Questions[0].Recipient = "maintainer"
	q.Assignee = ticket.Nil[string]()
	if PutTo(q, qa, people) != "" || !ann.Mine(q, qa) {
		t.Errorf("an unknown recipient goes by project")
	}
}
