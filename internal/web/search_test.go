package web

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/ticketsearch"
)

// the search box on the desk and the boards, a GET form whose
// results link to /t/{id}; it reads notes too, filters by project, column
// and kind, and shows a person only the projects they may see.
func TestSearch(t *testing.T) {
	h := newHarness(t, false)
	ts := h.fixtures()
	if len(ts) < 2 {
		t.Fatalf("fixtures %d", len(ts))
	}
	a := ts[0]
	var b *ticket.Ticket
	for _, x := range ts {
		if x.Project != a.Project {
			b = x
			break
		}
	}
	if b == nil {
		t.Fatal("the fixtures need two projects")
	}
	h.change(string(a.ID), func(tk *ticket.Ticket) {
		tk.Notes = append(tk.Notes, ticket.Note{At: "2026-10-18T10:00:00+00:00", Text: "Mind the Zebracorn cache"})
	})
	c := h.login()
	for _, page := range []string{"/", "/boards", "/b/" + b.Project} {
		body := h.do("GET", page, nil, c).Body.String()
		if !strings.Contains(body, `action="/search"`) {
			t.Errorf("%s has no search box", page)
		}
	}
	if body := h.do("GET", "/b/"+b.Project, nil, c).Body.String(); !strings.Contains(body, `name="project" value="`+b.Project+`"`) {
		t.Errorf("a board's box does not search its project")
	}
	w := h.do("GET", "/search?q=zebracorn+CACHE", nil, c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `href="/t/`+string(a.ID)+`"`) {
		t.Fatalf("a word in a note: %d %s", w.Code, w.Body.String())
	}
	if body := h.do("GET", "/search?q=zebracorn+missing", nil, c).Body.String(); !strings.Contains(body, "No ticket matches.") {
		t.Errorf("every word must match")
	}
	if body := h.do("GET", "/search?q=zebracorn&project="+b.Project, nil, c).Body.String(); strings.Contains(body, `href="/t/`+string(a.ID)+`"`) {
		t.Errorf("the project filter let another project through")
	}
	if body := h.do("GET", "/search?column="+b.Column+"&project="+b.Project, nil, c).Body.String(); !strings.Contains(body, `href="/t/`+string(b.ID)+`"`) {
		t.Errorf("filters alone find %s", b.ID)
	}
	if body := h.do("GET", "/search?kind=nonesuch", nil, c).Body.String(); !strings.Contains(body, "No ticket matches.") {
		t.Errorf("a kind no ticket has")
	}
	if body := h.do("GET", "/search", nil, c).Body.String(); strings.Contains(body, "No ticket matches.") {
		t.Errorf("an empty search searched")
	}
	// another person searches every project too, whatever their desk
	rc := h.person("robin", b.Project)
	if body := h.do("GET", "/search?q=zebracorn", nil, rc).Body.String(); !strings.Contains(body, string(a.ID)) {
		t.Errorf("robin missed a ticket of %s: %s", a.Project, body)
	}
	if w := h.do("GET", "/search?q=x", nil); w.Code == http.StatusOK {
		t.Errorf("search without a login: %d", w.Code)
	}
}

func TestSearchByTicketID(t *testing.T) {
	h := newHarness(t, false)
	ts := h.fixtures()
	a := ts[0]
	c := h.login()
	n := ticket.ID(a.ID).Num()
	for _, q := range []string{string(a.ID), strings.ToLower(string(a.ID)), " " + string(a.ID) + " ", "t" + strconv.Itoa(n), strconv.Itoa(n)} {
		w := h.do("GET", "/search?q="+strings.ReplaceAll(q, " ", "+"), nil, c)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/t/"+string(a.ID) {
			t.Errorf("%q: %d %q", q, w.Code, w.Header().Get("Location"))
		}
	}
	// an id no one may see is just text, found nowhere
	if w := h.do("GET", "/search?q=T999999", nil, c); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No ticket matches.") {
		t.Errorf("unknown id: %d", w.Code)
	}
	// with other words the id is searched as text
	if w := h.do("GET", "/search?q="+string(a.ID)+"+"+strings.Fields(strings.ToLower(a.Title))[0], nil, c); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `href="/t/`+string(a.ID)+`"`) {
		t.Errorf("id with a word: %d", w.Code)
	}
	for _, q := range []string{"", "tt25", "T-1", "0", "T1234567", "abc"} {
		if id, ok := ticketsearch.IDQuery(q); ok {
			t.Errorf("IDQuery(%q) = %s", q, id)
		}
	}
}
