package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/desk"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func (s *Server) desk(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	v := s.deskViewer(r)
	dv := present.DeskPage(present.Desk(ts, v), r.URL.Query().Get("kind"), "", s.now())
	p := &Page{Title: "Desk", Tab: "desk", Desk: dv, ForYou: desk.ForYou(desk.Nudges(ts, v), s.now())}
	if len(dv.Items) > 0 {
		// wide screens show the first waiting ask beside the queue; phones
		// show only the queue
		dv.Items[0].Current = true
		if t := findTicket(ts, dv.Items[0].ID); t != nil {
			p.Ticket = s.ticketView(t, ts, dv.Kind)
			s.forPerson(r, p.Ticket)
		}
	}
	s.render(w, r, "desk", p, ts)
}

func (s *Server) ticket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	ts := s.visible(r)
	t := findTicket(ts, id)
	if t == nil {
		http.NotFound(w, r)
		return
	}
	kind := r.URL.Query().Get("kind")
	tv := s.ticketView(t, ts, kind)
	s.forPerson(r, tv)
	if tv.Ask != nil {
		v := s.deskViewer(r)
		dv := present.DeskPage(present.Desk(ts, v), kind, id, s.now())
		dv.Open = true
		tv.Filter = dv.Kind
		s.render(w, r, "desk", &Page{Title: id, Tab: "desk", Desk: dv, Ticket: tv, ForYou: desk.ForYou(desk.Nudges(ts, v), s.now())}, ts)
		return
	}
	s.render(w, r, "ticket", &Page{Title: id, Tab: "boards", Ticket: tv}, ts)
}

func knownAskKind(k string) bool {
	_, ok := model.AskKindOf(model.AskKind(k))
	return ok
}

// freeAnswer: answers that do not answer the ask, so they need no check that
// the ask is still the one the page showed.
func freeAnswer(answer string) bool {
	return answer == "note" || answer == "ask" || answer == "talk" || answer == "after"
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	filter := r.PostFormValue("filter")
	if !knownAskKind(filter) {
		filter = ""
	}
	back := present.TicketHref(id, filter)
	t := findTicket(s.visible(r), id)
	answer := r.PostFormValue("answer")
	if t == nil || t.Board() && !boardAnswer(answer) {
		// a board ticket is worked through the ticket API: here a person
		// only answers its agent's question
		http.NotFound(w, r)
		return
	}
	a := present.AskFor(t)
	if t.Board() {
		s.answerBoard(w, r, t, a, filter, back)
		return
	}
	stale := id + " changed since you opened it. Look again before answering."
	if a != nil && !freeAnswer(answer) && (r.PostFormValue("ask") != a.Kind || r.PostFormValue("since") != a.Since) {
		s.flashRedirect(w, r, back, "error", stale)
		return
	}
	var picks []int
	var as map[int]string
	for _, p := range r.PostForm["pick"] {
		n, err := strconv.Atoi(p)
		if err != nil {
			s.flashRedirect(w, r, back, "error", "bad pick")
			return
		}
		picks = append(picks, n)
		// a finding's kind, chosen beside its tick
		switch k := r.PostFormValue("as-" + strconv.Itoa(n)); k {
		case "":
		case string(model.KindChange), string(model.KindIntent):
			if as == nil {
				as = map[int]string{}
			}
			as[n] = k
		default:
			s.flashRedirect(w, r, back, "error", "bad kind")
			return
		}
	}
	if s.svc.Answers == nil {
		s.flashRedirect(w, r, back, "error", id+": answers are off on this server.")
		return
	}
	req := AnswerRequest{ID: t.ID, Value: answer, Note: r.PostFormValue("note"), Picks: picks, As: as, By: s.actor(r)}
	if a != nil {
		req.Kind, req.Since = a.Kind, a.Since
	}
	var out string
	err := s.call(r, answerTimeout, func(ctx context.Context) (err error) {
		out, err = s.svc.Answers.Answer(ctx, req)
		return err
	})
	if err != nil {
		s.logf("%s %s by %s: %v", id, answer, s.who(r), err)
		msg := id + ": " + sentence(err.Error())
		if errors.Is(err, ErrStale) {
			msg = stale
		}
		s.flashRedirect(w, r, back, "error", msg)
		return
	}
	s.logf("%s %s by %s", id, answer, s.who(r))
	s.mentioned(r, id, t.Title, req.Note)
	msg := id + ": " + s.answerLabel(t, a, answer) + "."
	if out != "" {
		msg += " " + present.Clip(strings.Join(strings.Fields(out), " "), 200)
	}
	next := deskAfter(filter)
	if a == nil {
		next = back
	}
	if r.PostFormValue("view") == "talk" {
		next = "/t/" + id + "/talk"
	}
	s.flashRedirect(w, r, next, "ok", msg)
}

// deskAfter is where an answer takes you: back to the desk, with its kind
// filter if one was on (the maintainer's choice: not on to the next ask).
func deskAfter(filter string) string {
	if filter != "" {
		return "/?kind=" + filter
	}
	return "/"
}

// optionAnswer prefixes the answer that picks a board question's option,
// by number from 1: "option-2".
const optionAnswer = "option-"

// boardAnswerWait is how long a board answer waits for the ticket's lock,
// as the ticket API's own callers wait.
const boardAnswerWait = 10 * time.Second

// boardAnswer reports an answer value that answers a board agent's
// question: an option, the typed answer, or a reply from the conversation.
func boardAnswer(v string) bool {
	return v == "answer" || v == "talk" || strings.HasPrefix(v, optionAnswer)
}

// answerBoard answers the open question of a board ticket's agent through
// the ticket API's answer verb, as the person answering, as
// answer_question does for a person: the option picked, what was typed, or
// the option with the typed text under it. The ticket API decides the rest
// (who may answer, a question answered already, the ticket's move back
// from Waiting).
func (s *Server) answerBoard(w http.ResponseWriter, r *http.Request, t *ticket.Ticket, a *ticket.Ask, filter, back string) {
	id := string(t.ID)
	q := present.Asked(t, a)
	if q == nil || r.PostFormValue("ask") != a.Kind || r.PostFormValue("since") != a.Since {
		s.flashRedirect(w, r, back, "error", id+" changed since you opened it. Look again before answering.")
		return
	}
	if s.svc.Tickets == nil {
		s.flashRedirect(w, r, back, "error", id+": answers are off on this server.")
		return
	}
	text := strings.TrimSpace(r.PostFormValue("note"))
	if n, ok := strings.CutPrefix(r.PostFormValue("answer"), optionAnswer); ok {
		opts := a.Options.Or(nil)
		i, err := strconv.Atoi(n)
		if err != nil || i < 1 || i > len(opts) {
			s.flashRedirect(w, r, back, "error", "bad option")
			return
		}
		if text != "" {
			text = opts[i-1] + "\n\n" + text
		} else {
			text = opts[i-1]
		}
	}
	if text == "" {
		s.flashRedirect(w, r, back, "error", id+": write your answer first.")
		return
	}
	req := jsonx.ObjectOf(jsonx.Member{Key: "id", Value: jsonx.String(id)}, jsonx.Member{Key: "n", Value: jsonx.Int(int64(q.N))},
		jsonx.Member{Key: "answer", Value: jsonx.String(text)})
	err := s.call(r, answerTimeout, func(ctx context.Context) error {
		_, err := s.svc.Tickets.Do(ctx, s.asPerson(r), "answer", req, boardAnswerWait)
		return err
	})
	if err != nil {
		s.logf("%s answer %d by %s: %v", id, q.N, s.who(r), err)
		s.flashRedirect(w, r, back, "error", id+": "+sentence(err.Error()))
		return
	}
	s.logf("%s answer %d by %s", id, q.N, s.who(r))
	s.mentioned(r, id, t.Title, text)
	next := deskAfter(filter)
	if r.PostFormValue("view") == "talk" {
		next = "/t/" + id + "/talk"
	}
	s.flashRedirect(w, r, next, "ok", id+": Answer sent to "+q.By+".")
}

// sentence is an error's text as the flash says it: capitalised, with a
// full stop.
func sentence(s string) string {
	s = present.CapFirst(strings.TrimSpace(s))
	if s != "" && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}

func (s *Server) answerLabel(t *ticket.Ticket, a *ticket.Ask, answer string) string {
	switch answer {
	case "note":
		if a != nil {
			return "Note saved for the next agent on it; the decision is still yours (under You've responded)"
		}
		return "Note saved for the next agent on it"
	case "ask":
		return "Question sent; an agent answers in a few minutes, on your desk and in How it got here"
	case "talk":
		if a != nil && a.Kind == string(model.AskQuestion) {
			return "Answer sent; the agent carries on with it now"
		}
		return "Sent; an agent answers here in a few minutes"
	}
	for _, x := range s.answersFor(t, a) {
		if x.Value == answer {
			return x.Label
		}
	}
	return answer
}

func (s *Server) talk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	ts := s.visible(r)
	t := findTicket(ts, id)
	if t == nil {
		http.NotFound(w, r)
		return
	}
	v := present.Conversation(t, present.ByID(ts), s.archive, s.now())
	if t.Board() && s.svc.Tickets == nil {
		v.Question = false // no reply box: board answers are off on this server
	}
	s.render(w, r, "talk", &Page{Title: id, Tab: "desk", Talk: v}, ts)
}

// archive reads an agent run's report.json beside its log, never following
// a link, at most present.MaxArchivedReport bytes; nil when there is none.
func (s *Server) archive(id ticket.ID, agent string) []byte {
	if s.paths.Logs == "" || !validID(string(id)) || !present.ValidAgentName(agent) {
		return nil
	}
	data, err := fsx.ReadNoFollow(s.paths.TicketLogs(string(id))+"/"+agent+"/report.json", present.MaxArchivedReport)
	if err != nil {
		return nil
	}
	return data
}

func (s *Server) floor(w http.ResponseWriter, r *http.Request) {
	if f, ts, ok := s.floorFor(w, r); ok {
		s.render(w, r, "floor", &Page{Title: "Floor", Tab: "floor", Floor: f}, ts)
	}
}

// floorMetrics is the floor's five weeks of counts on a page of their own,
// for the same ?week as the floor, and the tokens the agents spent up to
// now.
func (s *Server) floorMetrics(w http.ResponseWriter, r *http.Request) {
	if f, ts, ok := s.floorFor(w, r); ok {
		s.render(w, r, "metrics", &Page{Title: "Floor metrics", Tab: "floor", Floor: f, Tokens: s.tokensView(r)}, ts)
	}
}

// floorFor builds the floor for ?week (YYYY-MM-DD, default this week); a
// bad week is answered here.
func (s *Server) floorFor(w http.ResponseWriter, r *http.Request) (*present.Floor, []*ticket.Ticket, bool) {
	ts := s.visible(r)
	at := s.now()
	if wk := r.URL.Query().Get("week"); wk != "" {
		d, err := time.ParseInLocation("2006-01-02", wk, time.Local)
		if err != nil {
			http.Error(w, "week must be YYYY-MM-DD", http.StatusBadRequest)
			return nil, nil, false
		}
		at = d
	}
	f := present.BuildFloor(ts, at, s.now(), s.deskViewer(r))
	return &f, ts, true
}

func (s *Server) boards(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	p := &Page{Title: "Boards", Tab: "boards"}
	states := s.projectStates(r)
	canSwitch := s.svc.Switch != nil
	for _, b := range summaries(ts, states) {
		if st, configured := states[b.Name]; canSwitch && configured && !st.Board {
			b.Switchable = true
		}
		if b.Board {
			p.Agents = append(p.Agents, b)
		} else {
			p.Boards = append(p.Boards, b)
		}
	}
	s.render(w, r, "boards", p, ts)
}

// switchProject is the maintainer's switch on Boards (maintainerOnly; the
// session, same-origin and CSRF checks are auth.Require's): a factory
// project on, off, or back to following config.toml's `enabled`. The
// service records it in foreman-state/projects.json, logs it and wakes the
// tick; the page comes back with what happened.
func (s *Server) switchProject(w http.ResponseWriter, r *http.Request) {
	project, state := r.PostFormValue("project"), r.PostFormValue("state")
	back := map[bool]string{false: "/boards", true: "/system"}[project == config.FactorySwitch]
	if s.svc.Switch == nil {
		s.flashRedirect(w, r, back, "error", "Switching projects is off on this server.")
		return
	}
	var line string
	err := s.call(r, fileTimeout, func(ctx context.Context) (err error) {
		line, err = s.svc.Switch.SwitchProject(ctx, project, state, s.actor(r))
		return err
	})
	if err != nil {
		s.logf("switch %q to %q by %s: %v", project, state, s.who(r), err)
		s.flashRedirect(w, r, back, "error", sentence(err.Error()))
		return
	}
	s.flashRedirect(w, r, back, "ok", sentence(line))
}

// switchNote is how a project switched from Foreman reads on Boards:
// "paused here by alice at 9 Oct 10:00"; "" when it follows config.
func switchNote(p config.Project) string {
	if p.Switch == nil {
		return ""
	}
	s := "paused here"
	if p.Switch.Enabled {
		s = "switched on here"
	}
	if p.Switch.By != "" {
		s += " by " + p.Switch.By
	}
	if at, err := clock.Parse(p.Switch.At); err == nil {
		s += " at " + at.Local().Format("2 Jan 15:04")
	}
	return s
}

// projectStates are the configured projects the viewer may see, each with
// its mode and whether it is paused; nil when config can't be read.
func (s *Server) projectStates(r *http.Request) map[string]ProjectState {
	if s.svc.Config == nil {
		return nil
	}
	cfg, err := s.svc.Config.Current()
	if err != nil || cfg == nil {
		return nil
	}
	out := map[string]ProjectState{}
	for _, p := range cfg.Projects {
		switch p.Mode {
		case string(model.ModeFactory):
			out[p.Key] = ProjectState{Paused: !p.Enabled, Note: switchNote(p), Config: p.ConfigState()}
		case string(model.ModeBoard):
			out[p.Key] = ProjectState{Board: true}
		}
	}
	return out
}

func (s *Server) boardPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("project")
	ts := s.visible(r)
	projects := s.projectStates(r)
	st, known := projects[name]
	var bv *BoardView
	var ok bool
	if st.Board || !known {
		// a board project, or an unconfigured one whose tickets are board tickets
		bv, ok = boardLanes(name, ts)
	}
	if !ok && !st.Board {
		bv, ok = boardView(name, ts)
	}
	if !ok {
		if !known {
			http.NotFound(w, r)
			return
		}
		if st.Board {
			bv, _ = boardLanes(name, nil) // a configured board with no tickets yet
		} else {
			bv = &BoardView{Name: name}
		}
	}
	bv.Paused = st.Paused
	s.render(w, r, "board", &Page{Title: name, Tab: "boards", Board: bv}, ts)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.Identity(r) != "" {
		http.Redirect(w, r, auth.SafeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", s.loginView(auth.SafeNext(r.URL.Query().Get("next")), ""), nil)
}

// loginView is the login page: email and password, or the main secret
// while there are no users.
func (s *Server) loginView(next, msg string) *Page {
	return &Page{Title: "Log in", Next: next, Error: msg, FirstLogin: !s.auth.Users().Any()}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	ip := auth.ClientIP(r)
	next := auth.SafeNext(r.PostFormValue("next"))
	if !auth.SameOrigin(r) {
		http.Error(w, "cross-site login refused", http.StatusForbidden)
		return
	}
	if !s.auth.Allowed(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, r, "login", s.loginView(next, "Too many wrong tries. Wait a few minutes."), nil)
		return
	}
	user, ok := s.auth.LoginAs(ip, r.PostFormValue("email"), r.PostFormValue("password"))
	if !ok {
		s.logf("login failed from %s", ip)
		w.WriteHeader(http.StatusUnauthorized)
		msg := "That email and password do not match."
		if !s.auth.Users().Any() {
			msg = "That is not the secret."
		}
		s.render(w, r, "login", s.loginView(next, msg), nil)
		return
	}
	s.auth.SetSession(w, r, user)
	s.logf("%s logged in", user)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// logout ends the person's own sessions, on every device of theirs.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Revoke(s.auth.UserOf(auth.IdentityOf(r))); err != nil {
		s.logf("logout: %v", err)
	}
	auth.ClearSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
