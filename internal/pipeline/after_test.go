package pipeline

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestAReadyTicketWaitsUntilItsAfterTicketsAreDone(t *testing.T) {
	h := newHarness(t)
	first := h.file("p", "First", "review", nil)
	second := h.file("p", "Second", "ready", func(t *ticket.Ticket) { t.After = []ticket.ID{first} })
	h.tick()
	if len(h.local.started) != 0 {
		t.Fatalf("started %+v while %s was open", h.local.started, first)
	}
	h.change(first, func(t *ticket.Ticket) {
		ticket.Move(t, string(model.ColDone), model.ReasonByHuman, "", "", "maintainer", h.clock.Now())
	})
	h.tick()
	if len(h.local.started) != 1 || h.load(second).Agent.V.Name == "" {
		t.Fatalf("started %+v once %s was done", h.local.started, first)
	}
}

func TestATicketWaitingForARejectedTicketGoesToTheMaintainer(t *testing.T) {
	h := newHarness(t)
	first := h.file("p", "First", "rejected", nil)
	second := h.file("p", "Second", "ready", func(t *ticket.Ticket) { t.After = []ticket.ID{first} })
	h.tick()
	if len(h.local.started) != 0 {
		t.Fatal("a ticket waiting for a rejected one started")
	}
	got := h.load(second)
	if got.Column != string(model.ColNeedsHuman) || len(got.After) != 0 {
		t.Fatalf("column %s, after %v", got.Column, got.After)
	}
	if a, ok := got.Ask.Get(); !ok || a.Kind != string(model.AskStart) {
		t.Fatalf("ask %+v", got.Ask)
	}
}

func TestATicketWaitingForAnUnreadableTicketStaysReady(t *testing.T) {
	h := newHarness(t)
	first := h.file("p", "First", "review", nil)
	second := h.file("p", "Second", "ready", func(t *ticket.Ticket) { t.After = []ticket.ID{first} })
	if err := os.WriteFile(h.paths.TicketFile(string(first)), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.tick()
	got := h.load(second)
	if len(h.local.started) != 0 || got.Column != string(model.ColReady) || !slices.Equal(got.After, []ticket.ID{first}) {
		t.Fatalf("started %+v, column %s, after %v", h.local.started, got.Column, got.After)
	}
}

func TestADisabledProjectKeepsATicketWaitingForARejectedOne(t *testing.T) {
	h := newHarness(t)
	h.config(strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\nenabled = false\n", 1))
	first := h.file("p", "First", "rejected", nil)
	second := h.file("p", "Second", "ready", func(t *ticket.Ticket) { t.After = []ticket.ID{first} })
	h.tick()
	if got := h.load(second); got.Column != string(model.ColReady) || len(got.After) != 1 {
		t.Fatalf("column %s, after %v", got.Column, got.After)
	}
}
