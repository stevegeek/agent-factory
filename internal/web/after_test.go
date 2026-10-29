package web

import (
	"html"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestTicketPageAndBoardShowWhatATicketWaitsFor(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	var held, dep *ticket.Ticket
	for _, tk := range h.fixtures() {
		if tk.Board() || tk.Column == "done" {
			continue
		}
		if dep == nil {
			dep = tk
		} else if held == nil && tk.Project == dep.Project {
			held = tk
		}
	}
	if held == nil {
		t.Fatal("the fixtures lack two open factory tickets in one project")
	}
	h.change(string(held.ID), func(tk *ticket.Ticket) { tk.Column, tk.After = "ready", []ticket.ID{dep.ID} })
	page := html.UnescapeString(h.do("GET", "/t/"+string(held.ID), nil, c).Body.String())
	if !strings.Contains(page, `<a href="/t/`+string(dep.ID)+`">`+string(dep.ID)+" ") {
		t.Errorf("the ticket page lacks a link to %s", dep.ID)
	}
	page = h.do("GET", "/t/"+string(dep.ID), nil, c).Body.String()
	if !strings.Contains(page, "<dt>Blocks</dt>") || !strings.Contains(page, `<a href="/t/`+string(held.ID)+`">`) {
		t.Errorf("the blocking ticket's page lacks a link to %s", held.ID)
	}
	board := html.UnescapeString(h.do("GET", "/b/"+held.Project, nil, c).Body.String())
	if !strings.Contains(board, `<span class="card-meta">Waiting for `+string(dep.ID)+" ") {
		t.Errorf("the board card lacks the wait")
	}
}
