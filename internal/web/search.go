package web

import (
	"net/http"

	"github.com/stevegeek/agent-factory/internal/ticketsearch"
)

// SearchView is the search page: the form, its choices and the hits.
type SearchView = ticketsearch.View

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	v := r.URL.Query()
	text := v.Get("q")
	if rs := []rune(text); len(rs) > 200 {
		text = string(rs[:200])
	}
	q := ticketsearch.Query{Q: text, Project: v.Get("project"), Column: v.Get("column"), Kind: v.Get("kind")}
	// a ticket id on its own goes straight to that ticket, if the viewer may see it
	if id, ok := ticketsearch.IDQuery(q.Q); ok {
		for _, t := range ts {
			if t.ID == id {
				http.Redirect(w, r, "/t/"+string(id), http.StatusSeeOther)
				return
			}
		}
	}
	sv := &ticketsearch.View{Query: q}
	sv.Projects, sv.Columns, sv.Kinds = ticketsearch.Choices(ts)
	if !q.Empty() {
		sv.Searched = true
		sv.Hits, sv.More = ticketsearch.Tickets(ts, q)
	}
	s.render(w, r, "search", &Page{Title: "Search", Tab: "boards", Search: sv}, ts)
}
