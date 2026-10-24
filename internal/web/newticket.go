package web

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// NewView is the ＋ page.
type NewView struct {
	Projects                   []string
	Boards                     []string // board projects: a card in Inbox, through the ticket API
	Project, Kind, Title, Body string
	Error                      string
	Recipes                    []config.Recipe
}

// isBoard reports a board project the page offers.
func (v *NewView) isBoard(p string) bool {
	for _, b := range v.Boards {
		if b == p {
			return true
		}
	}
	return false
}

// boardChoices are the board projects in the config as it is now that the
// viewer may file on (the maintainer every one), when filing on boards is on.
func (s *Server) boardChoices(u *auth.User) []string {
	if s.svc.Tickets == nil || s.svc.Config == nil {
		return nil
	}
	cfg, err := s.svc.Config.Current()
	if err != nil || cfg == nil {
		return nil
	}
	var out []string
	for _, p := range cfg.BoardProjects() {
		if u.Sees(p.Key) {
			out = append(out, p.Key)
		}
	}
	return out
}

// newKind reports a kind the ＋ page files. A prospecting recipe is not one.
func newKind(k string) bool {
	switch model.Kind(k) {
	case model.KindChange, model.KindIntent, model.KindTask, model.KindManage:
		return true
	}
	return false
}

// filedWords is what happens next to a ticket of each kind.
func filedWords(kind string) string {
	switch model.Kind(kind) {
	case model.KindIntent:
		return "A designer picks it up on the next tick."
	case model.KindTask:
		return "An agent answers it; the answer lands on your desk."
	case model.KindManage:
		return "The manager answers it, with any config change as a draft for you to apply."
	}
	return "A worker picks it up on the next tick."
}

// recipes are the factory's recipes, from the config as it is now, that
// file a kind the ＋ page knows.
func (s *Server) recipes() []config.Recipe {
	if s.svc.Config == nil {
		return nil
	}
	cfg, err := s.svc.Config.Current()
	if err != nil || cfg == nil {
		if err != nil {
			s.logf("recipes: %v", err)
		}
		return nil
	}
	var out []config.Recipe
	for _, r := range cfg.Recipes() {
		if newKind(r.Kind) {
			out = append(out, r)
		}
	}
	return out
}

// projectChoices are the configured projects, then any other factory
// project on the board, then the no-repository project (the maintainer's
// only).
func (s *Server) projectChoices(ts []*ticket.Ticket, u *auth.User) []string {
	seen := map[string]bool{model.NoticeProject: true}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range s.projects {
		if u.Sees(p) {
			add(p)
		}
	}
	for _, t := range ts {
		if !t.Board() { // a board project takes no factory ticket
			add(t.Project)
		}
	}
	if u != nil {
		return out
	}
	return append(out, model.NoticeProject)
}

func (s *Server) newTicket(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	u := s.viewer(r)
	v := &NewView{Projects: s.projectChoices(ts, u), Boards: s.boardChoices(u), Kind: string(model.KindChange), Recipes: s.recipes()}
	if p := r.URL.Query().Get("project"); p != "" && (v.isBoard(p) || slices.Contains(v.Projects, p)) {
		v.Project = p
	} else if len(v.Projects) > 0 {
		v.Project = v.Projects[0]
	}
	s.render(w, r, "new", &Page{Title: "New ticket", Tab: "new", New: v}, ts)
}

// formProblem is what is wrong with a new ticket as posted, or "". A board
// project's card has no kind: only its title and description count.
func formProblem(v *NewView, maintainer bool) string {
	if v.isBoard(v.Project) {
		return textProblem(v)
	}
	known := false
	for _, p := range v.Projects {
		known = known || p == v.Project
	}
	switch {
	case !known:
		return "Pick one of the projects."
	case !newKind(v.Kind):
		return "Pick a kind."
	case v.Project == model.NoticeProject && v.Kind != string(model.KindTask) && v.Kind != string(model.KindManage):
		return "The factory project has no repository: it only takes questions and requests for the manager."
	case v.Kind == string(model.KindManage) && !maintainer:
		return "Only the maintainer talks to the manager."
	case v.Kind == string(model.KindManage) && v.Project != model.NoticeProject:
		return "Requests for the manager go on the factory project."
	}
	return textProblem(v)
}

// textProblem is what is wrong with the title or description, or "".
func textProblem(v *NewView) string {
	switch {
	case v.Title == "" || len(v.Title) > 200 || strings.ContainsAny(v.Title, "\x00\r\n"):
		return "The title is needed, on one line, at most 200 characters."
	case len(v.Body) > 8000 || strings.ContainsRune(v.Body, 0):
		return "The description is at most 8000 characters."
	}
	return ""
}

func (s *Server) fileTicket(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	u := s.viewer(r)
	v := &NewView{Projects: s.projectChoices(ts, u), Boards: s.boardChoices(u), Project: r.PostFormValue("project"), Kind: r.PostFormValue("kind"),
		Title: strings.TrimSpace(r.PostFormValue("title")), Body: strings.TrimSpace(r.PostFormValue("body"))}
	again := func(msg string) {
		v.Error = msg
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, "new", &Page{Title: "New ticket", Tab: "new", New: v}, ts)
	}
	if msg := formProblem(v, s.isMaintainer(r)); msg != "" {
		again(msg)
		return
	}
	if v.isBoard(v.Project) {
		s.fileCard(w, r, v, again)
		return
	}
	if s.svc.Filer == nil {
		again("Filing is off on this server.")
		return
	}
	var id ticket.ID
	err := s.call(r, fileTimeout, func(ctx context.Context) (err error) {
		id, err = s.svc.Filer.File(ctx, NewTicket{Project: v.Project, Kind: v.Kind, Title: v.Title, Body: v.Body, By: s.actor(r)})
		return err
	})
	if err != nil {
		s.logf("new ticket by %s: %v", s.who(r), err)
		again(sentence(err.Error()))
		return
	}
	if !validID(string(id)) {
		s.flashRedirect(w, r, "/", "ok", "Filed: "+present.Clip(v.Title, 80))
		return
	}
	s.logf("%s filed from Foreman by %s (%s, %s)", id, s.who(r), v.Project, v.Kind)
	s.flashRedirect(w, r, "/t/"+string(id), "ok", string(id)+": filed. "+filedWords(v.Kind))
}

// fileCard files a card on a board project through the ticket API's create
// verb, as the person filing it (D19): it lands in Inbox for the board's
// agents, and no agent of the factory's ever takes it.
func (s *Server) fileCard(w http.ResponseWriter, r *http.Request, v *NewView, again func(string)) {
	req := jsonx.ObjectOf(jsonx.Member{Key: "project", Value: jsonx.String(v.Project)},
		jsonx.Member{Key: "title", Value: jsonx.String(v.Title)}, jsonx.Member{Key: "body", Value: jsonx.String(v.Body)})
	var res jsonx.Object
	err := s.call(r, fileTimeout, func(ctx context.Context) (err error) {
		res, err = s.svc.Tickets.Do(ctx, s.asPerson(r), "create", req, boardAnswerWait)
		return err
	})
	if err != nil {
		s.logf("new card on %s by %s: %v", v.Project, s.who(r), err)
		again(sentence(err.Error()))
		return
	}
	var id string
	if t, ok := res.Get("ticket").Object(); ok {
		id, _ = t.Get("id").Str()
	}
	if !validID(id) {
		s.flashRedirect(w, r, "/b/"+v.Project, "ok", "Filed on "+v.Project+": "+present.Clip(v.Title, 80))
		return
	}
	s.logf("%s filed on board %s from Foreman by %s", id, v.Project, s.who(r))
	s.flashRedirect(w, r, "/t/"+id, "ok", id+": filed in "+v.Project+"'s Inbox, for its agents.")
}
