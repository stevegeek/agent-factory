package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// ＋ offers the board projects apart from the factory's, saying which is
// which, and a board card goes to the ticket API's create verb as the person
// filing it, with no kind: it never reaches the factory's filer.
func TestPlusFilesABoardCard(t *testing.T) {
	h := boardHarness(t)
	tickets := &fakeTickets{}
	h.srv.svc.Tickets = tickets
	c := h.login()
	csrf := h.csrf(c)

	page := h.do("GET", "/new", nil, c).Body.String()
	for _, want := range []string{"Factory projects:", "Boards: a card for that board", `<option value="notes" data-board`,
		`<option value="empty" data-board`, "A <b>board card</b>", "A <b>factory ticket</b>"} {
		if !strings.Contains(page, want) {
			t.Errorf("＋ lacks %q", want)
		}
	}
	// a board's page has a button that opens ＋ on it; a factory project's says ticket
	if b := h.do("GET", "/b/notes", nil, c).Body.String(); !strings.Contains(b, `href="/new?project=notes">＋ Add a card to this board`) {
		t.Errorf("the board page lacks its add-a-card button")
	}
	if b := h.do("GET", "/b/widgets", nil, c).Body.String(); !strings.Contains(b, `href="/new?project=widgets">＋ New ticket for this project`) {
		t.Errorf("the factory project's page lacks its new-ticket button")
	}
	// opened on a board, the factory-only choices start hidden
	board := h.do("GET", "/new?project=notes", nil, c).Body.String()
	if !strings.Contains(board, `<option value="notes" data-board selected>`) || !strings.Contains(board, `class="kinds" data-factory-only hidden`) {
		t.Errorf("＋ on a board does not start as a board card")
	}

	w := h.do("POST", "/new", url.Values{"csrf": {csrf}, "project": {"notes"}, "kind": {"change"},
		"title": {"  Tidy the date picker "}, "body": {"Make it **small**.\n"}}, c)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/b/notes" {
		t.Errorf("a board card: %d %s", w.Code, w.Header().Get("Location"))
	}
	calls := tickets.take()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "maintainer create ") ||
		!strings.Contains(calls[0], `"project": "notes"`) || !strings.Contains(calls[0], `"title": "Tidy the date picker"`) ||
		strings.Contains(calls[0], "kind") {
		t.Errorf("ticket API calls %q", calls)
	}
	if got := h.ops.takeFiled(); len(got) != 0 {
		t.Errorf("a board card went to the factory's filer: %+v", got)
	}

	// a board card still needs a title; nothing is filed without one
	if w := h.do("POST", "/new", url.Values{"csrf": {csrf}, "project": {"notes"}, "title": {""}}, c); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an untitled card: %d", w.Code)
	}
	if calls := tickets.take(); len(calls) != 0 {
		t.Errorf("an untitled card was filed: %q", calls)
	}

	// without the ticket API, boards are not offered at all
	h.srv.svc.Tickets = nil
	if page := h.do("GET", "/new", nil, c).Body.String(); strings.Contains(page, "data-board ") || strings.Contains(page, "Boards: a card") {
		t.Errorf("＋ offers boards with the ticket API off")
	}
}
