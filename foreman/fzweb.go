package main

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/stevegeek/agent-factory/foreman/fizz"
)

// Project boards in the web app, read-only: the boards the fizzy CLI writes through /fizzy.

// SetProjectBoards serves the store's boards in the Boards tab and its API under /fizzy.
func (s *Server) SetProjectBoards(st *fizz.Store) {
	s.fz = st
	s.fzAPI = fizz.NewAPI(st)
}

type ProjectSummary struct {
	Name, Href          string
	Open, Maybe, Closed int
}

type PCardItem struct {
	Number int
	Title  string
	Href   string
	Golden bool
	Tags   []string
}

type PLane struct {
	Label string
	Cards []PCardItem
}

type PBoardView struct {
	Name   string
	Lanes  []PLane
	Folded []PLane
}

type PComment struct {
	Who, When, Body string
	System          bool
}

type PCardView struct {
	Number                                  int
	Title, Board, BoardHref, State, Created string
	Creator, Description                    string
	Golden                                  bool
	Tags, Assignees                         []string
	Steps                                   []*fizz.Step
	Comments                                []PComment
}

func (s *Server) projectSummaries() []ProjectSummary {
	if s.fz == nil {
		return nil
	}
	s.fz.Mu.Lock()
	defer s.fz.Mu.Unlock()
	var out []ProjectSummary
	for _, acct := range s.fz.Accounts() {
		boards, err := s.fz.Boards(acct)
		if err != nil {
			log.Printf("project boards: %v", err)
			continue
		}
		cards, err := s.fz.Cards(acct)
		if err != nil {
			log.Printf("project boards: %v", err)
		}
		for _, b := range boards {
			p := ProjectSummary{Name: b.Name, Href: "/pb/" + acct + "/" + b.ID}
			for _, c := range cards {
				switch {
				case c.BoardID != b.ID:
				case c.Closed:
					p.Closed++
				case c.Postponed:
				case c.ColumnID == "":
					p.Maybe++
					p.Open++
				default:
					p.Open++
				}
			}
			out = append(out, p)
		}
	}
	return out
}

func cardItem(acct string, a *fizz.Account, c *fizz.Card) PCardItem {
	it := PCardItem{Number: c.Number, Title: c.Title, Href: "/pc/" + acct + "/" + strconv.Itoa(c.Number), Golden: c.Golden}
	for _, id := range c.Tags {
		if t := a.Tag(id); t != nil {
			it.Tags = append(it.Tags, t.Title)
		}
	}
	return it
}

func (s *Server) projectBoard(w http.ResponseWriter, r *http.Request) {
	acct := r.PathValue("account")
	if s.fz == nil {
		http.NotFound(w, r)
		return
	}
	s.fz.Mu.Lock()
	a, err := s.fz.Account(acct)
	var b *fizz.Board
	var cards []*fizz.Card
	if err == nil {
		b, err = s.fz.Board(acct, r.PathValue("board"))
	}
	if err == nil {
		cards, err = s.fz.Cards(acct)
	}
	s.fz.Mu.Unlock()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// newest activity first in every lane, as Fizzy shows them
	for i, j := 0, len(cards)-1; i < j; i, j = i+1, j-1 {
		cards[i], cards[j] = cards[j], cards[i]
	}
	v := &PBoardView{Name: b.Name}
	lane := func(label string, keep func(*fizz.Card) bool) PLane {
		l := PLane{Label: label}
		for _, c := range cards {
			if c.BoardID == b.ID && keep(c) {
				l.Cards = append(l.Cards, cardItem(acct, a, c))
			}
		}
		return l
	}
	open := func(c *fizz.Card) bool { return !c.Closed && !c.Postponed }
	v.Lanes = append(v.Lanes, lane("Maybe?", func(c *fizz.Card) bool { return open(c) && b.Column(c.ColumnID) == nil }))
	for _, col := range b.Columns {
		id := col.ID
		v.Lanes = append(v.Lanes, lane(col.Name, func(c *fizz.Card) bool { return open(c) && c.ColumnID == id }))
	}
	v.Folded = append(v.Folded, lane("Not now", func(c *fizz.Card) bool { return c.Postponed && !c.Closed }),
		lane("Done", func(c *fizz.Card) bool { return c.Closed }))
	ts := s.board.Load()
	s.render(w, r, "pboard", &Page{Title: b.Name, Tab: "boards", PBoard: v}, ts)
}

func (s *Server) projectCard(w http.ResponseWriter, r *http.Request) {
	acct := r.PathValue("account")
	n := fizz.CardNumber(r.PathValue("number"))
	if s.fz == nil || n == 0 {
		http.NotFound(w, r)
		return
	}
	s.fz.Mu.Lock()
	a, err := s.fz.Account(acct)
	var c *fizz.Card
	var b *fizz.Board
	if err == nil {
		c, err = s.fz.Card(acct, n)
	}
	if err == nil {
		b, _ = s.fz.Board(acct, c.BoardID)
	}
	s.fz.Mu.Unlock()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := func(id string) string {
		if u := a.User(id); u != nil {
			return u.Name
		}
		return "someone"
	}
	v := &PCardView{Number: c.Number, Title: c.Title, Golden: c.Golden, Steps: c.Steps,
		Creator: name(c.CreatorID), Created: stampLabel(c.CreatedAt), Description: fizz.PlainText(c.Description, true)}
	switch {
	case c.Closed:
		v.State = "Done"
	case c.Postponed:
		v.State = "Not now"
	default:
		v.State = "Maybe?"
	}
	if b != nil {
		v.Board, v.BoardHref = b.Name, "/pb/"+acct+"/"+b.ID
		if col := b.Column(c.ColumnID); col != nil && !c.Closed && !c.Postponed {
			v.State = col.Name
		}
	}
	for _, id := range c.Tags {
		if t := a.Tag(id); t != nil {
			v.Tags = append(v.Tags, t.Title)
		}
	}
	for _, id := range c.Assignees {
		v.Assignees = append(v.Assignees, name(id))
	}
	for _, cm := range c.Comments {
		u := a.User(cm.CreatorID)
		v.Comments = append(v.Comments, PComment{Who: name(cm.CreatorID), When: stampLabel(cm.CreatedAt),
			Body: fizz.PlainText(cm.Body, true), System: u != nil && u.Role == "system"})
	}
	ts := s.board.Load()
	s.render(w, r, "pcard", &Page{Title: "#" + strconv.Itoa(c.Number) + " " + c.Title, Tab: "boards", PCard: v}, ts)
}

func stampLabel(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2 Jan 2006 15:04")
}
