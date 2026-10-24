package web

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Search (§15.3): a GET form on the desk and the boards over the tickets
// the viewer may see, by text with filters for project, column and kind.
// The text rule is the ticket API's (ticketapi list: every word, case-
// folded, must appear), over the title, the body, the notes and the
// conversation (board comments and questions). Results link to /t/{id}.

// maxHits caps one search's results.
const maxHits = 200

// SearchQuery is the form.
type SearchQuery struct {
	Q, Project, Column, Kind string
}

func (q SearchQuery) empty() bool {
	return strings.TrimSpace(q.Q) == "" && q.Project == "" && q.Column == "" && q.Kind == ""
}

// SearchHit is one result.
type SearchHit struct {
	ID, Title, Project, Column, Kind string
}

// Href is the ticket's page.
func (h SearchHit) Href() string { return "/t/" + h.ID }

// SearchView is the search page.
type SearchView struct {
	Query    SearchQuery
	Searched bool
	Hits     []SearchHit
	More     bool // more matched than are shown
	Projects []string
	Columns  []string
	Kinds    []string
}

// searchWords are the words of a query as ticketapi's list splits them.
func searchWords(q string) []string { return strings.FieldsFunc(textx.Lower(q), textx.IsSpace) }

// idQuery is the ticket a query names on its own: "T0025", "t25" or "25".
func idQuery(q string) (ticket.ID, bool) {
	q = strings.TrimSpace(q)
	digits := strings.TrimLeft(q, "Tt")
	if len(q)-len(digits) > 1 || digits == "" || len(digits) > 6 {
		return "", false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 || strings.ContainsAny(digits, "+-") {
		return "", false
	}
	return ticket.FormatID(n), true
}

// haystack is the text a search looks in, case-folded.
func haystack(t *ticket.Ticket) string {
	parts := []string{string(t.ID), t.Title, t.Body}
	for _, n := range t.Notes {
		parts = append(parts, n.Text)
	}
	for _, c := range t.Comments {
		parts = append(parts, c.Text)
	}
	for _, q := range t.Questions {
		parts = append(parts, q.Text)
		if a, ok := q.Answer.Get(); ok {
			parts = append(parts, a)
		}
	}
	return textx.Lower(strings.Join(parts, "\n"))
}

func kindOf(t *ticket.Ticket) string {
	if k := t.KindName(); k != "" {
		return k
	}
	if t.Board() {
		return string(model.KindItem)
	}
	return string(model.KindChange)
}

// searchTickets is the query over ts (already what the viewer may see),
// newest first.
func searchTickets(ts []*ticket.Ticket, q SearchQuery) ([]SearchHit, bool) {
	words := searchWords(q.Q)
	var hits []SearchHit
	more := false
	sorted := slices.Clone(ts)
	sort.SliceStable(sorted, func(i, j int) bool { return ticket.ID(sorted[i].ID).Num() > ticket.ID(sorted[j].ID).Num() })
	for _, t := range sorted {
		if q.Project != "" && t.Project != q.Project || q.Column != "" && t.Column != q.Column || q.Kind != "" && kindOf(t) != q.Kind {
			continue
		}
		if len(words) > 0 {
			hay := haystack(t)
			all := true
			for _, w := range words {
				all = all && strings.Contains(hay, w)
			}
			if !all {
				continue
			}
		}
		if len(hits) == maxHits {
			more = true
			break
		}
		hits = append(hits, SearchHit{ID: string(t.ID), Title: t.Title, Project: t.Project, Column: t.Column, Kind: kindOf(t)})
	}
	return hits, more
}

// searchChoices are the filters' options: the viewer's projects, and the
// columns and kinds their tickets have.
func searchChoices(ts []*ticket.Ticket) (projects, columns, kinds []string) {
	add := func(list *[]string, v string) {
		if v != "" && !slices.Contains(*list, v) {
			*list = append(*list, v)
		}
	}
	for _, t := range ts {
		add(&projects, t.Project)
		add(&columns, t.Column)
		add(&kinds, kindOf(t))
	}
	sort.Strings(projects)
	sort.Strings(kinds)
	order := map[string]int{}
	for i, c := range model.Columns() {
		order[string(c.Name)] = i
	}
	for i, c := range model.BoardColumns() {
		order[string(c.Name)] = 100 + i
	}
	sort.SliceStable(columns, func(i, j int) bool { return order[columns[i]] < order[columns[j]] })
	return projects, columns, kinds
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	v := r.URL.Query()
	text := v.Get("q")
	if rs := []rune(text); len(rs) > 200 {
		text = string(rs[:200])
	}
	q := SearchQuery{Q: text, Project: v.Get("project"), Column: v.Get("column"), Kind: v.Get("kind")}
	// a ticket id on its own goes straight to that ticket, if the viewer may see it
	if id, ok := idQuery(q.Q); ok {
		for _, t := range ts {
			if t.ID == id {
				http.Redirect(w, r, "/t/"+string(id), http.StatusSeeOther)
				return
			}
		}
	}
	sv := &SearchView{Query: q}
	sv.Projects, sv.Columns, sv.Kinds = searchChoices(ts)
	if !q.empty() {
		sv.Searched = true
		sv.Hits, sv.More = searchTickets(ts, q)
	}
	s.render(w, r, "search", &Page{Title: "Search", Tab: "boards", Search: sv}, ts)
}
