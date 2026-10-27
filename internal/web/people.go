package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/desk"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/push"
)

// People: everyone who logs in may do everything, here too: add a person,
// give anyone a new password, change anyone's desk, remove anyone. What is
// per person is the desk, and the tickets passed to them or mentioning them.

const (
	passTimeout = answerTimeout
	pushTimeout = 30 * time.Second
)

func (s *Server) peopleView(r *http.Request) *desk.PeopleView {
	var projects, people []string
	if cfg := s.cfg(); cfg != nil {
		people = cfg.People()
		for _, p := range cfg.Projects {
			projects = append(projects, p.Key)
		}
	}
	me := ""
	var mine []string
	if u := s.viewer(r); u != nil {
		me, mine = u.Name, u.Desk
	}
	var users []desk.Person
	for _, u := range s.auth.Users().List() {
		users = append(users, desk.Person{Name: u.Name, Email: u.Email, Added: u.Added, Desk: u.Desk})
	}
	return desk.People(me, mine, users, projects, people, s.now())
}

// desk projects from a form's ticked boxes; none ticked is every project.
func (s *Server) deskFrom(r *http.Request) []string {
	var known []string
	if cfg := s.cfg(); cfg != nil {
		for _, p := range cfg.Projects {
			known = append(known, p.Key)
		}
	}
	var out []string
	for _, p := range r.PostForm["desk"] {
		if slices.Contains(known, p) {
			out = append(out, p)
		}
	}
	return out
}

// setDesk is the viewer choosing their own desk projects.
func (s *Server) setDesk(w http.ResponseWriter, r *http.Request) {
	me := s.viewer(r)
	if me == nil {
		s.flashRedirect(w, r, "/settings", "error", "Add yourself as a person first: the main secret has no desk of its own.")
		return
	}
	s.saveDesk(w, r, me.Name)
}

func (s *Server) saveDesk(w http.ResponseWriter, r *http.Request, name string) {
	desk := s.deskFrom(r)
	if err := s.auth.Users().SetDesk(name, desk); err != nil {
		s.flashRedirect(w, r, "/settings", "error", sentence(err.Error()))
		return
	}
	s.logf("%s set %s's desk to %s", s.who(r), name, strings.Join(desk, ", "))
	what := "every project"
	if len(desk) > 0 {
		what = strings.Join(desk, ", ")
	}
	s.flashRedirect(w, r, "/settings", "ok", name+"'s desk: "+what+".")
}

// addUser adds a person.
func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	if err := s.auth.Users().Add(name, email, r.PostFormValue("password"), s.deskFrom(r)); err != nil {
		s.flashRedirect(w, r, "/settings", "error", sentence(err.Error()))
		return
	}
	s.logf("%s added %s (%s)", s.who(r), name, email)
	msg := name + " can log in now."
	if s.viewer(r) == nil {
		msg += " The main secret no longer logs in: log in as yourself."
	}
	s.flashRedirect(w, r, "/settings", "ok", msg)
}

// changeUser is a person's new password, new desk, or removal.
func (s *Server) changeUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	users := s.auth.Users()
	if users.Get(name) == nil {
		http.NotFound(w, r)
		return
	}
	switch r.PostFormValue("do") {
	case "password":
		if err := users.SetPassword(name, r.PostFormValue("password")); err != nil {
			s.flashRedirect(w, r, "/settings", "error", sentence(err.Error()))
			return
		}
		s.logf("%s gave %s a new password", s.who(r), name)
		s.flashRedirect(w, r, "/settings", "ok", name+" has a new password; their sessions have ended.")
	case "desk":
		s.saveDesk(w, r, name)
	case "remove":
		if err := users.Remove(name); err != nil {
			s.flashRedirect(w, r, "/settings", "error", sentence(err.Error()))
			return
		}
		s.logf("%s removed %s", s.who(r), name)
		s.flashRedirect(w, r, "/settings", "ok", name+" is removed; their sessions have ended.")
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

// assign passes a ticket to a person.
func (s *Server) assign(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	t := findTicket(s.visible(r), id)
	if t == nil {
		http.NotFound(w, r)
		return
	}
	back := present.TicketHref(id, "")
	if s.svc.Passer == nil {
		s.flashRedirect(w, r, back, "error", id+": passing is off on this server.")
		return
	}
	to := r.PostFormValue("to")
	var line string
	err := s.call(r, passTimeout, func(ctx context.Context) (err error) {
		line, err = s.svc.Passer.Pass(ctx, t.ID, to, s.actor(r), r.PostFormValue("note"))
		return err
	})
	if err != nil {
		s.logf("%s pass to %q by %s: %v", id, to, s.who(r), err)
		s.flashRedirect(w, r, back, "error", id+": "+sentence(err.Error()))
		return
	}
	s.logf("%s passed to %q by %s", id, to, s.who(r))
	s.nudge(r, to, id, t.Title, s.who(r)+" passed you "+id)
	s.mentioned(r, id, t.Title, r.PostFormValue("note"))
	s.flashRedirect(w, r, back, "ok", sentence(line))
}

// seenMention puts away the viewer's nudges on a ticket up to the time the
// page showed.
func (s *Server) seenMention(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := s.viewer(r)
	at := r.PostFormValue("at")
	if !validID(id) || me == nil || present.ParseTime(at).IsZero() {
		http.NotFound(w, r)
		return
	}
	if err := s.auth.Users().Dismiss(me.Name, id, at); err != nil {
		s.flashRedirect(w, r, present.TicketHref(id, ""), "error", sentence(err.Error()))
		return
	}
	s.flashRedirect(w, r, "/", "ok", id+" is off your desk.")
}

// mentioned notifies each person text from r mentions, but the writer.
func (s *Server) mentioned(r *http.Request, id, title, text string) {
	for _, p := range desk.Mentions(text, s.deskViewer(r).People) {
		s.nudge(r, p, id, title, s.who(r)+" mentioned you on "+id)
	}
}

// nudge notifies one person's own devices of a ticket put in front of them,
// in the background: a slow push service never holds the page.
func (s *Server) nudge(r *http.Request, to, id, title, line string) {
	if s.push == nil || to == "" || to == s.who(r) {
		return
	}
	m := push.Message{Title: line, Body: title, URL: present.TicketHref(id, ""), Tag: "nudge-" + id}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		s.push.SendTo(ctx, to, m)
	}()
}
