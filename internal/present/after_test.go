package present

import (
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func findFact(fs []Fact, label string) *Fact {
	for i := range fs {
		if fs[i].Label == label {
			return &fs[i]
		}
	}
	return nil
}

func TestFactsShowWhatATicketWaitsForAndBlocks(t *testing.T) {
	first := &ticket.Ticket{ID: "T0001", Project: "p", Title: "Tighten the parser", Column: "review"}
	second := &ticket.Ticket{ID: "T0002", Project: "p", Title: "Use the parser", Column: "ready", After: []ticket.ID{"T0001"}}
	all := ticket.ByID([]*ticket.Ticket{first, second})
	if w := findFact(Facts(second, nil, all), "Waiting for"); w == nil || w.Value != "T0001 Tighten the parser (Review)" || w.Tone != "warn" || w.Ticket != "T0001" {
		t.Fatalf("waiting for %+v", w)
	}
	if b := findFact(Facts(first, nil, all), "Blocks"); b == nil || b.Value != "T0002 Use the parser" || b.Ticket != "T0002" {
		t.Fatalf("blocks %+v", b)
	}
	second.Column = "done"
	if b := findFact(Facts(first, nil, all), "Blocks"); b != nil {
		t.Fatalf("a done ticket still blocked on: %+v", b)
	}
}

func TestABoardTicketShowsWhatItWaitsFor(t *testing.T) {
	first := &ticket.Ticket{ID: "T0001", Project: "p", Mode: ticket.Some("board"), Title: "Draft", Column: string(model.BoardDone)}
	second := &ticket.Ticket{ID: "T0002", Project: "p", Mode: ticket.Some("board"), Title: "Publish", Column: "next", After: []ticket.ID{"T0001"}}
	all := ticket.ByID([]*ticket.Ticket{first, second})
	if w := findFact(Facts(second, nil, all), "Waiting for"); w == nil || w.Ticket != "T0001" || w.Tone != "" {
		t.Fatalf("waiting for %+v", w)
	}
	if b := findFact(Facts(first, nil, all), "Blocks"); b == nil || b.Ticket != "T0002" {
		t.Fatalf("blocks %+v", b)
	}
}

func TestWaitLineNamesTheTicketsStillOpen(t *testing.T) {
	done := &ticket.Ticket{ID: "T0001", Title: "Done one", Column: "done"}
	open := &ticket.Ticket{ID: "T0002", Title: "Open one", Column: "security"}
	held := &ticket.Ticket{ID: "T0003", Column: "ready", After: []ticket.ID{"T0001", "T0002", "T0009"}}
	all := ticket.ByID([]*ticket.Ticket{done, open, held})
	if got, want := WaitLine(held, all), "Waiting for T0002 Open one (Security), T0009 (missing)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	held.After = []ticket.ID{"T0001"}
	if got := WaitLine(held, all); got != "" {
		t.Fatalf("a ticket whose tickets are done: %q", got)
	}
	held.After, held.Column = []ticket.ID{"T0002"}, "rejected"
	if got := WaitLine(held, all); got != "" {
		t.Fatalf("a closed ticket: %q", got)
	}
}

func TestADeskItemSaysWhatItWaitsFor(t *testing.T) {
	ts := fixtures(t)
	items := Desk(ts, you)
	if len(items) < 2 {
		t.Fatal("the fixtures have fewer than two asks")
	}
	held, dep := items[0].T, items[1].T
	held.After = []ticket.ID{dep.ID}
	v := DeskPage(Desk(ts, you), "", "", time.Now())
	want, found := WaitLine(held, ticket.ByID(ts)), false
	for _, q := range append(v.Items, v.Done...) {
		if q.ID == string(held.ID) {
			found = true
			if want == "" || q.Wait != want {
				t.Fatalf("desk item wait %q, want %q", q.Wait, want)
			}
		}
	}
	if !found {
		t.Fatalf("%s is not on the desk", held.ID)
	}
}
