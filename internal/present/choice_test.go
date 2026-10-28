package present

import (
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestTheStoryShowsTheChoiceAndItsEffect(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	tk := ticket.New("T0050", ticket.NewIn{Project: "widgets", Title: "Rename", Body: "b", Door: "human", Column: "working"}, at)
	ticket.Move(tk, "needs_human", model.ReasonAgentQuestion, "work asks: Which name?", "", "", at)
	ticket.Event(tk, "chose", "chose “It is fine”: closes the ticket", "",
		ticket.OptionChosen{By: "sam", Keys: []string{"done"}, Effect: ticket.EffectClose}, at.Add(time.Minute))
	ticket.Move(tk, "done", model.ReasonQuestionClosed, "closed by the answer: It is fine", "", "sam", at.Add(time.Minute))
	got := fmtSteps(BuildStory(tk, map[ticket.ID]*ticket.Ticket{tk.ID: tk}))
	if !strings.Contains(got, "You: Chose “It is fine”: closes the ticket.") {
		t.Errorf("story lacks the choice:\n%s", got)
	}
	if strings.Contains(got, "closed by the answer") {
		t.Errorf("the close is told twice:\n%s", got)
	}
}
