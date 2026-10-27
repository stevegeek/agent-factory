package ops_test

import (
	"context"
	"slices"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Pass gives a ticket to one of [board] people, recorded as an edit by who
// passed it; its note is a note on a factory ticket and a comment on a
// board ticket.
func TestPassGivesATicketToAPerson(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	id := w.new(ticket.NewIn{Project: "widgets", Title: "One", Body: "b", Column: "ready"}, nil).ID
	line, err := w.svc.Pass(ctx, string(id), "alex", "", "over to you, @alex")
	if err != nil || line != string(id)+" passed to alex" {
		t.Fatalf("pass: %q, %v", line, err)
	}
	got := w.load(id)
	if got.Assignee.Or("") != "alex" || len(got.Notes) != 1 || got.Notes[0].Text != "over to you, @alex" {
		t.Errorf("assignee %v notes %+v", got.Assignee, got.Notes)
	}
	var edited *ticket.Edited
	for _, h := range got.History {
		if e, ok := h.Data.(ticket.Edited); ok {
			edited = &e
		}
	}
	if edited == nil || edited.By != "sam" || !slices.Equal(edited.Fields, []string{"assignee"}) {
		t.Errorf("edit %+v", edited)
	}
	// a pass to nobody, by a Foreman user
	if _, err := w.svc.Pass(ctx, string(id), "", "alex", ""); err != nil {
		t.Fatal(err)
	}
	if got := w.load(id); got.Assignee.Set && got.Assignee.Or("") != "" {
		t.Errorf("assignee after a pass to nobody: %v", got.Assignee)
	}

	board := w.new(ticket.NewIn{Project: "widgets", Title: "Card", Body: "b", Column: "ready"}, func(t *ticket.Ticket) {
		t.Mode = ticket.Some("board")
	}).ID
	if _, err := w.svc.Pass(ctx, string(board), "sam", "alex", "can you take it"); err != nil {
		t.Fatal(err)
	}
	if got := w.load(board); got.Assignee.Or("") != "sam" || len(got.Notes) != 0 || len(got.Comments) != 1 ||
		got.Comments[0].By != "alex" || got.Comments[0].Text != "can you take it" || got.Comments[0].N != 1 {
		t.Errorf("board pass: assignee %v comments %+v", got.Assignee, got.Comments)
	}

	before := w.files()
	if _, err := w.svc.Pass(ctx, string(id), "robin", "", ""); err == nil {
		t.Error("a pass to someone not in [board] people")
	}
	if _, err := w.svc.Pass(ctx, string(id), "alex", "factory", ""); err == nil {
		t.Error("a pass as the factory")
	}
	if after := w.files(); after[string(id)+".json"] != before[string(id)+".json"] {
		t.Error("a refused pass saved something")
	}
	done := w.new(ticket.NewIn{Project: "widgets", Title: "Done", Body: "b", Column: "done"}, nil).ID
	if _, err := w.svc.Pass(ctx, string(done), "alex", "", ""); err == nil {
		t.Error("a pass of a closed ticket")
	}
	// an agent working a board card holds it by its assignee
	held := w.new(ticket.NewIn{Project: "widgets", Title: "Held", Body: "b", Column: "doing"}, func(t *ticket.Ticket) {
		t.Mode, t.Assignee = ticket.Some("board"), ticket.Some("example-agent")
	}).ID
	if _, err := w.svc.Pass(ctx, string(held), "sam", "alex", ""); err == nil {
		t.Error("a pass took a card from the agent working it")
	}
}
