package web

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// §6.3: after "Open the PR" the ticket says the tick is doing it.
func TestTheTicketSaysAnIntentIsPending(t *testing.T) {
	h := newHarness(t, false)
	id := string(h.fixtures()[0].ID)
	h.change(id, func(tk *ticket.Ticket) {
		pr, _ := tk.PR.Get()
		pr.OpenRequested = ticket.Some("2026-10-18T10:00:00+00:00")
		tk.PR = ticket.Some(pr)
	})
	body := h.do("GET", "/t/"+id, nil, h.login()).Body.String()
	if !strings.Contains(body, "Opening the PR; it shows here when it is open") {
		t.Errorf("no pending line on /t/%s", id)
	}
}
