package ticketsearch

import (
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestSearchMatchesConversations(t *testing.T) {
	tk := &ticket.Ticket{ID: "T0007", Title: "Plan", Body: "b", Project: "home", Column: "doing", Mode: ticket.Some("board"),
		Comments:  []ticket.Comment{{N: 1, Text: "We agreed on the blue paint"}},
		Questions: []ticket.BoardQuestion{{N: 1, Text: "Which wall?"}}}
	for q, want := range map[string]bool{"blue PAINT": true, "wall": true, "green": false, "": true} {
		hits, _ := Tickets([]*ticket.Ticket{tk}, Query{Q: q})
		if (len(hits) == 1) != want {
			t.Errorf("%q: %v", q, hits)
		}
	}
	if hits, _ := Tickets([]*ticket.Ticket{tk}, Query{Kind: "change"}); len(hits) != 0 {
		t.Errorf("a board ticket is not a change: %v", hits)
	}
}
