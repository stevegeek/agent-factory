// Package ticketsearch finds the tickets a Foreman search asks for, and
// the choices its filters offer.
package ticketsearch

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Search: a GET form on the desk and the boards over the tickets
// the viewer may see, by text with filters for project, column and kind.
// The text rule is the ticket API's (ticketapi list: every word, case-
// folded, must appear), over the title, the body, the notes and the
// conversation (board comments and questions). Results link to /t/{id}.

// maxHits caps one search's results.
const maxHits = 200

// Query is the form.
type Query struct {
	Q, Project, Column, Kind string
}

func (q Query) Empty() bool {
	return strings.TrimSpace(q.Q) == "" && q.Project == "" && q.Column == "" && q.Kind == ""
}

// Hit is one result.
type Hit struct {
	ID, Title, Project, Column, Kind string
}

// Href is the ticket's page.
func (h Hit) Href() string { return "/t/" + h.ID }

// View is the search page.
type View struct {
	Query    Query
	Searched bool
	Hits     []Hit
	More     bool // more matched than are shown
	Projects []string
	Columns  []string
	Kinds    []string
}

// words are the words of a query as ticketapi's list splits them.
func words(q string) []string { return strings.FieldsFunc(textx.Lower(q), textx.IsSpace) }

// IDQuery is the ticket a query names on its own: "T0025", "t25" or "25".
func IDQuery(q string) (ticket.ID, bool) {
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

// Tickets is the query over ts (already what the viewer may see),
// newest first.
func Tickets(ts []*ticket.Ticket, q Query) ([]Hit, bool) {
	words := words(q.Q)
	var hits []Hit
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
		hits = append(hits, Hit{ID: string(t.ID), Title: t.Title, Project: t.Project, Column: t.Column, Kind: kindOf(t)})
	}
	return hits, more
}

// Choices are the filters' options: the viewer's projects, and the
// columns and kinds their tickets have.
func Choices(ts []*ticket.Ticket) (projects, columns, kinds []string) {
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
