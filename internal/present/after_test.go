package present

import (
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestFactsShowWhatATicketWaitsForAndBlocks(t *testing.T) {
	first := &ticket.Ticket{ID: "T0001", Project: "p", Column: "review"}
	second := &ticket.Ticket{ID: "T0002", Project: "p", Column: "ready", After: []ticket.ID{"T0001"}}
	all := ticket.ByID([]*ticket.Ticket{first, second})
	find := func(fs []Fact, label string) *Fact {
		for i := range fs {
			if fs[i].Label == label {
				return &fs[i]
			}
		}
		return nil
	}
	if w := find(Facts(second, nil, all), "Waiting for"); w == nil || w.Value != "T0001 (Review)" || w.Tone != "warn" {
		t.Fatalf("waiting for %+v", w)
	}
	if b := find(Facts(first, nil, all), "Blocks"); b == nil || b.Value != "T0002" {
		t.Fatalf("blocks %+v", b)
	}
	second.Column = "done"
	if b := find(Facts(first, nil, all), "Blocks"); b != nil {
		t.Fatalf("a done ticket still blocked on: %+v", b)
	}
}
