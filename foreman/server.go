package main

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/foreman/fizz"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	board       *Board
	auth        *Auth
	runner      *Runner
	push        *Pusher     // nil: notifications are off
	projects    []string    // what a new ticket may name, besides the board's own projects
	fz          *fizz.Store // project boards; nil: none
	fzAPI       *fizz.API
	pages       map[string]*template.Template
	now         func() time.Time
	assetV      string
	looks       shortCache // agent screens, for a few seconds
	recipeCache shortCache
}

func NewServer(board *Board, auth *Auth, runner *Runner) *Server {
	s := &Server{board: board, auth: auth, runner: runner, now: time.Now, pages: map[string]*template.Template{},
		looks: shortCache{ttl: 3 * time.Second}, recipeCache: shortCache{ttl: time.Minute}}
	h := sha256.New()
	fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := staticFS.ReadFile(p)
			h.Write(b)
		}
		return nil
	})
	s.assetV = hex.EncodeToString(h.Sum(nil))[:10]
	funcs := template.FuncMap{
		"asset": func(name string) string { return "/static/" + name + "?v=" + s.assetV },
		"low":   func(n int) bool { return n < 4 },
		"inc":   func(n int) int { return n + 1 },
		"join":  strings.Join,
		"md":    Markdown,
		"mdi":   MarkdownLine,
		"mdt":   MarkdownText,
	}
	for _, page := range []string{"desk", "ticket", "floor", "boards", "board", "login", "settings", "new", "live", "talk", "pboard", "pcard"} {
		s.pages[page] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/parts.html", "templates/"+page+".html"))
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	// the installable app: the manifest and the service worker must be readable before login,
	// and the worker must come from the root to cover every page
	mux.HandleFunc("GET /manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		data, _ := staticFS.ReadFile("static/sw.js")
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(data)
	})
	if s.fzAPI != nil {
		// the Fizzy-compatible API: bearer tokens, never the login session
		mux.Handle(fizz.Mount+"/", s.fzAPI)
	}
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", s.desk)
	app.HandleFunc("GET /t/{id}", s.ticket)
	app.HandleFunc("POST /t/{id}/act", s.act)
	app.HandleFunc("GET /floor", s.floor)
	app.HandleFunc("GET /boards", s.boards)
	app.HandleFunc("GET /settings", s.settings)
	app.HandleFunc("GET /new", s.newTicket)
	app.HandleFunc("POST /new", s.fileTicket)
	app.HandleFunc("POST /push/subscribe", s.pushSubscribe)
	app.HandleFunc("POST /push/unsubscribe", s.pushUnsubscribe)
	app.HandleFunc("POST /push/test", s.pushTest)
	app.HandleFunc("GET /b/{project}", s.boardPage)
	app.HandleFunc("GET /t/{id}/talk", s.talk)
	app.HandleFunc("GET /live", s.maintainerOnly(s.livePage))
	app.HandleFunc("GET /live/log", s.maintainerOnly(s.liveLog))
	app.HandleFunc("GET /live/{id}/screen", s.maintainerOnly(s.liveScreen))
	app.HandleFunc("GET /t/{id}/logs/{name}", s.maintainerOnly(s.agentLog))
	// project boards are the maintainer's until they have owners of their own
	app.HandleFunc("GET /pb/{account}/{board}", s.maintainerOnly(s.projectBoard))
	app.HandleFunc("GET /pc/{account}/{number}", s.maintainerOnly(s.projectCard))
	app.HandleFunc("POST /pc/{account}/{number}/send", s.maintainerOnly(s.sendCard))
	app.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if err := s.auth.Revoke(); err != nil {
			log.Printf("logout: %v", err)
		}
		ClearSession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.Handle("/", s.auth.Require(s.live(app)))
	return headers(s.auth.CheckHost(mux))
}

// liveHeader carries the board version a page was rendered from. foreman.js polls the page's own
// URL with it: 204 while nothing changed, else the page as it is now, to update in place.
const liveHeader = "X-Foreman-Live"

func (s *Server) live(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(liveHeader); v != "" && r.Method == http.MethodGet && v == boardVersion(s.board.Load()) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; "+
			"connect-src 'self'; font-src 'self'; manifest-src 'self'; worker-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// not no-referrer: under it browsers send "Origin: null" on this site's own form posts
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- who is asking, and what they may see

// viewer: nil for the maintainer (everything); else the user, whose projects bound what they see.
// A session whose user has gone sees nothing (the session check already refuses it).
func (s *Server) viewer(r *http.Request) *User {
	name := s.auth.UserOf(identityOf(r))
	if name == maintainer {
		return nil
	}
	if u := s.auth.users.Get(name); u != nil {
		return u
	}
	return &User{Name: name}
}

func (s *Server) isMaintainer(r *http.Request) bool { return s.viewer(r) == nil }

// visible: the board as this person may see it.
func (s *Server) visible(r *http.Request) []*Ticket {
	ts := s.board.Load()
	u := s.viewer(r)
	if u == nil {
		return ts
	}
	var out []*Ticket
	for _, t := range ts {
		if u.Sees(t.Project) {
			out = append(out, t)
		}
	}
	return out
}

// maintainerOnly: pages that show the whole factory (live screens, logs) are the maintainer's.
func (s *Server) maintainerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isMaintainer(r) {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}
}

// ---------------------------------------------------------------- page data

type Page struct {
	Title     string
	Tab       string
	CSRF      string
	DeskCount int
	Running   int
	Who       string // "" for the maintainer; else the user's name (no Live tab, no manager)
	Live      *LiveView
	Talk      *TalkView
	Flash     *Flash
	Desk      *DeskView
	Ticket    *TicketView
	Floor     *Floor
	Boards    []BoardSummary
	Board     *BoardView
	Projects  []ProjectSummary
	PBoard    *PBoardView
	PCard     *PCardView
	Next      string
	Error     string
	Version   string
	Settings  *SettingsView
	New       *NewView
}

type SettingsView struct {
	PushKey string // empty: notifications are off on this server
	Devices int
}

type Chip struct {
	Label string
	N     int
	On    bool
	Href  string
}

type QueueItem struct {
	ID, Kind, Label, Glyph, Question, Title, Project, Age, Href string
	Action                                                      string // what the ask wants, in a few words
	Responded                                                   string // "You asked 5 min ago": set when the item waits on the factory, not on you
	Answered                                                    bool   // back on the desk because your question was answered
	Current                                                     bool
}

type DeskView struct {
	Filters []Chip
	Items   []QueueItem // still waiting for you
	Done    []QueueItem // you have responded; back up top when the factory does anything new
	Kind    string
	Open    bool
	Total   int
}

type Pick struct {
	N           int
	Title, Body string
}

type TicketView struct {
	T                       *Ticket
	Ask                     *Ask
	Kind, KindLabel, Glyph  string
	Headline, Subtitle, Why string
	Age, Filter             string
	Facts                   []Fact
	Answers                 []Answer
	Picks                   []Pick
	NoteLabel, NoteSub      string
	Callout                 *Callout
	Next                    string
	CanNote                 bool
	Story                   Story
	More                    []More
	Children                []string
	ChildrenLabel           string
	ParentID                string
	NoteFirst               bool
	Since                   string
	Answer                  *AnswerView // the latest answer to a question asked while this ask waits
	Logs                    []string    // archived agent runs, by name
}

type AnswerView struct {
	ID, Question, Lead, Full string
}

// latestAnswer: the newest of your questions on t that has an answer, asked since the ask arrived (an
// answer about an earlier decision stays in the story).
func latestAnswer(t *Ticket, a *Ask, all map[string]*Ticket) *AnswerView {
	for i := len(t.History) - 1; i >= 0; i-- {
		h := t.History[i]
		if h.Event != "asked" {
			continue
		}
		if a != nil && parseTime(h.At).Before(parseTime(a.Since)) {
			return nil
		}
		id, q, _ := strings.Cut(h.Detail, ": ")
		if c := all[id]; c != nil && c.Result != "" {
			lead := c.report("task").str("summary")
			if lead == "" {
				lead = mdLead(c.Result, 600)
			}
			return &AnswerView{ID: id, Question: q, Lead: lead, Full: c.Result}
		}
	}
	return nil
}

type More struct {
	Summary, Text string
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *Page, ts []*Ticket) {
	if id := identityOf(r); id != "" {
		p.CSRF = s.auth.CSRF(id)
		if u := s.viewer(r); u != nil {
			p.Who = u.Name
		}
	}
	if ts != nil {
		p.DeskCount = Waiting(Desk(ts))
		p.Running = running(ts)
		p.Version = boardVersion(ts)
	}
	// a poll must not use up the message meant for the next page the person opens
	if p.Flash == nil && r.Header.Get(liveHeader) == "" {
		p.Flash = takeFlash(w, r, s.auth)
	}
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Push-Url", r.URL.RequestURI())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func deskView(items []DeskItem, kind, current string, now time.Time) *DeskView {
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Ask.Kind]++
	}
	if counts[kind] == 0 {
		kind = ""
	}
	v := &DeskView{Kind: kind, Total: len(items)}
	v.Filters = append(v.Filters, Chip{Label: "All", N: len(items), On: kind == "", Href: "/"})
	for _, k := range askOrder {
		if counts[k] > 0 {
			v.Filters = append(v.Filters, Chip{Label: askKinds[k].Label, N: counts[k], On: kind == k, Href: "/?kind=" + k})
		}
	}
	for _, it := range items {
		if kind != "" && it.Ask.Kind != kind {
			continue
		}
		q := QueueItem{ID: it.T.ID, Kind: it.Ask.Kind, Label: kindLabel(it.Ask.Kind), Glyph: kindGlyph(it.Ask.Kind),
			Question: Headline(it.T, it.Ask), Title: it.T.Title, Action: Action(it.T, it.Ask), Project: it.T.Project,
			Age: ageSince(it.Ask.Since, now), Href: ticketHref(it.T.ID, kind), Current: it.T.ID == current, Answered: it.Answered}
		switch it.Responded {
		case "asked":
			q.Responded = "You asked " + ageSince(it.RespondedAt, now) + " ago · the answer comes here"
		case "note":
			q.Responded = "You left a note " + ageSince(it.RespondedAt, now) + " ago · still yours to decide"
		}
		if q.Responded != "" {
			v.Done = append(v.Done, q)
			continue
		}
		v.Items = append(v.Items, q)
	}
	return v
}

func kindLabel(k string) string {
	if i, ok := askKinds[k]; ok {
		return i.Label
	}
	return capFirst(k)
}

func kindGlyph(k string) string {
	if i, ok := askKinds[k]; ok {
		return i.Glyph
	}
	return "?"
}

func ticketHref(id, kind string) string {
	if kind != "" {
		return "/t/" + id + "?kind=" + url.QueryEscape(kind)
	}
	return "/t/" + id
}

func ageSince(at string, now time.Time) string {
	t := parseTime(at)
	if t.IsZero() {
		return ""
	}
	return age(now.Sub(t))
}

func (s *Server) ticketView(t *Ticket, ts []*Ticket, filter string) *TicketView {
	all := byID(ts)
	a := AskFor(t)
	v := &TicketView{T: t, Ask: a, Filter: filter, Story: BuildStory(t, all), Facts: Facts(t, a, all),
		Answers: Answers(t, a), Callout: PRCallout(t), Children: t.Children, ChildrenLabel: "Filed", ParentID: t.Parent, CanNote: !t.closed()}
	if pr := t.PanelResult; pr != nil {
		// a prospect's proposals, kept and dropped, each a ticket of its own
		v.ChildrenLabel, v.Children = "Proposals", nil
		for _, id := range append(append([]string{}, pr.Kept...), pr.DroppedTickets...) {
			if validID(id) {
				v.Children = append(v.Children, id)
			}
		}
	}
	if a != nil {
		v.Kind, v.KindLabel, v.Glyph = a.Kind, kindLabel(a.Kind), kindGlyph(a.Kind)
		v.Headline, v.Why, v.Since = Headline(t, a), a.Why, a.Since
		if a.Kind == "notice" && t.Result != "" {
			// an answer is a Markdown document: its opening paragraph, not a run of its first sentences
			v.Why = mdLead(t.Result, 320)
		}
		v.Next = waitingWhat(t, a)
		v.Age = ageSince(a.Since, s.now())
		if !strings.Contains(v.Headline, t.Title) {
			v.Subtitle = t.Title
		}
		v.Answer = latestAnswer(t, a, all)
	} else {
		v.Kind = "closed"
		v.KindLabel = ticketColumnLabel(t)
		v.Headline = t.Title
		v.Age = ageSince(t.Updated, s.now())
	}
	switch {
	case a != nil && a.Kind == "design":
		v.NoteLabel = "Note for the designer"
		for i, p := range t.Proposed {
			v.Picks = append(v.Picks, Pick{N: i + 1, Title: p.Title, Body: p.Body})
		}
	case a != nil && a.Kind == "question":
		v.NoteLabel = "Your answer"
	default:
		v.NoteLabel = "A note or a question"
	}
	switch {
	case a != nil:
		v.NoteSub = "Ask the agent: one reads the change and answers on your desk in a few minutes; nothing changes. " +
			"Save note: kept for the next agent that works on it; nothing happens now."
	case t.Agent != nil:
		v.NoteSub = "Ask the agent: an answer comes to your desk. Save note: kept for the next agent, not the one running now."
	default:
		v.NoteSub = "Ask the agent: an answer comes to your desk. Save note: kept for the next agent that works on it."
	}
	for _, x := range v.Answers {
		if x.NeedsNote && x.NoteFor != "" {
			v.NoteSub = "“" + x.Label + "” sends this note. " + v.NoteSub
			break
		}
	}
	for _, x := range v.Answers {
		// on a phone the field must come before the button that needs it
		v.NoteFirst = v.NoteFirst || x.NeedsNote
	}
	if s.runner != nil {
		v.Logs = agentLogs(s.runner.Root, t.ID)
	}
	if t.Design != "" {
		v.More = append(v.More, More{"Read the design", t.Design})
	}
	if t.Result != "" {
		v.More = append(v.More, More{"Read the answer", t.Result})
	}
	if p := t.Proposal; p != nil {
		v.More = append(v.More, More{"Read the proposal", "Noticed: " + p.Noticed + "\n\nChange: " + p.Change + "\n\nWhy: " + p.Why + "\n\nCost: " + p.Cost})
	}
	if t.Body != "" && t.Proposal == nil {
		v.More = append(v.More, More{"Read the ticket", t.Body})
	}
	if t.Original != nil && t.Original.Body != "" {
		v.More = append(v.More, More{"Read the original report (untrusted)", t.Original.Title + "\n\n" + t.Original.Body})
	}
	if len(t.Notes) > 0 {
		var b strings.Builder
		for _, n := range t.Notes {
			fmt.Fprintf(&b, "%s  %s\n\n", parseTime(n.At).Local().Format("2 Jan 15:04"), n.Text)
		}
		v.More = append(v.More, More{plural(len(t.Notes), "note") + " for the agents", strings.TrimSpace(b.String())})
	}
	return v
}

// ---------------------------------------------------------------- handlers

func (s *Server) desk(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	items := Desk(ts)
	kind := r.URL.Query().Get("kind")
	dv := deskView(items, kind, "", s.now())
	p := &Page{Title: "Desk", Tab: "desk", Desk: dv}
	if len(dv.Items) > 0 {
		// wide screens show the first waiting ask beside the queue; phones show only the queue
		first := dv.Items[0].ID
		dv.Items[0].Current = true
		for _, t := range ts {
			if t.ID == first {
				p.Ticket = s.ticketView(t, ts, dv.Kind)
			}
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
	t := byID(ts)[id]
	if t == nil {
		http.NotFound(w, r)
		return
	}
	tv := s.ticketView(t, ts, r.URL.Query().Get("kind"))
	if tv.Ask != nil {
		dv := deskView(Desk(ts), r.URL.Query().Get("kind"), id, s.now())
		dv.Open = true
		tv.Filter = dv.Kind
		s.render(w, r, "desk", &Page{Title: id, Tab: "desk", Desk: dv, Ticket: tv}, ts)
		return
	}
	s.render(w, r, "ticket", &Page{Title: id, Tab: "boards", Ticket: tv}, ts)
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	filter := r.PostFormValue("filter")
	if _, ok := askKinds[filter]; !ok {
		filter = ""
	}
	back := ticketHref(id, filter)
	ts := s.visible(r)
	t := byID(ts)[id]
	if t == nil {
		http.NotFound(w, r)
		return
	}
	a := AskFor(t)
	answer := r.PostFormValue("answer")
	if a != nil && answer != "note" && answer != "ask" && answer != "talk" && (r.PostFormValue("ask") != a.Kind || r.PostFormValue("since") != a.Since) {
		s.flashRedirect(w, r, back, "error", id+" changed since you opened it. Look again before answering.")
		return
	}
	var picks []int
	for _, p := range r.PostForm["pick"] {
		n, err := strconv.Atoi(p)
		if err != nil {
			s.flashRedirect(w, r, back, "error", "bad pick")
			return
		}
		picks = append(picks, n)
	}
	cmds, err := Plan(t, a, answer, r.PostFormValue("note"), picks)
	if err != nil {
		s.flashRedirect(w, r, back, "error", id+": "+capFirst(err.Error())+".")
		return
	}
	out, err := s.runner.Run(cmds)
	if err != nil {
		log.Printf("%s %s: %v", id, answer, err)
		s.flashRedirect(w, r, back, "error", id+": "+err.Error())
		return
	}
	log.Printf("%s %s: ran %d command(s)", id, answer, len(cmds))
	msg := id + ": " + answerLabel(t, a, answer) + "."
	if out != "" {
		msg += " " + clip(strings.Join(strings.Fields(out), " "), 200)
	}
	next := "/"
	if filter != "" {
		next = "/?kind=" + filter
	}
	// work down the desk: on to the next ask still waiting for you (a note or a question moves this
	// one down to "You've responded" until the factory does something new with it)
	for _, it := range Desk(s.visible(r)) {
		if it.T.ID != id && it.Responded == "" && (filter == "" || it.Ask.Kind == filter) {
			next = ticketHref(it.T.ID, filter)
			break
		}
	}
	if a == nil {
		next = back
	}
	if r.PostFormValue("view") == "talk" {
		next = "/t/" + id + "/talk"
	}
	s.flashRedirect(w, r, next, "ok", msg)
}

func answerLabel(t *Ticket, a *Ask, answer string) string {
	if answer == "note" {
		if a != nil {
			return "Note saved for the next agent on it; the decision is still yours (under You've responded)"
		}
		return "Note saved for the next agent on it"
	}
	if answer == "ask" {
		return "Question sent; an agent answers in a few minutes, on your desk and in How it got here"
	}
	if answer == "talk" {
		if a != nil && a.Kind == "question" {
			return "Answer sent; the agent carries on with it now"
		}
		return "Sent; an agent answers here in a few minutes"
	}
	for _, x := range Answers(t, a) {
		if x.Value == answer {
			return x.Label
		}
	}
	return answer
}

func (s *Server) floor(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	at := s.now()
	if wk := r.URL.Query().Get("week"); wk != "" {
		d, err := time.ParseInLocation("2006-01-02", wk, time.Local)
		if err != nil {
			http.Error(w, "week must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		at = d
	}
	f := BuildFloor(ts, at, s.now())
	s.render(w, r, "floor", &Page{Title: "Floor", Tab: "floor", Floor: &f}, ts)
}

type BoardSummary struct {
	Name, Href                       string
	Open, Asks, PRs, Closed, Running int
}

type Lane struct {
	Column, Label string
	Tickets       []LaneTicket
}

type LaneTicket struct {
	ID, Title, Ask, AskLabel, Agent string
}

type BoardView struct {
	Name   string
	Lanes  []Lane
	Closed []Lane
}

func summaries(ts []*Ticket) []BoardSummary {
	m := map[string]*BoardSummary{}
	for _, t := range ts {
		b := m[t.Project]
		if b == nil {
			b = &BoardSummary{Name: t.Project, Href: "/b/" + url.PathEscape(t.Project)}
			m[t.Project] = b
		}
		if t.closed() {
			b.Closed++
		} else {
			b.Open++
		}
		if AskFor(t) != nil {
			b.Asks++
		}
		if t.Column == "pr_open" {
			b.PRs++
		}
		if t.Agent != nil {
			b.Running++
		}
	}
	var out []BoardSummary
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) boards(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	var projects []ProjectSummary
	if s.isMaintainer(r) {
		projects = s.projectSummaries()
	}
	s.render(w, r, "boards", &Page{Title: "Boards", Tab: "boards", Boards: summaries(ts), Projects: projects}, ts)
}

func (s *Server) boardPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("project")
	ts := s.visible(r)
	bv := &BoardView{Name: name}
	found := false
	for _, col := range columns {
		lane := Lane{Column: col, Label: columnLabel(col)}
		if col == "needs_human" {
			lane.Label = "Needs you"
		}
		for i := len(ts) - 1; i >= 0; i-- {
			t := ts[i]
			if t.Project != name || t.Column != col {
				continue
			}
			found = true
			lt := LaneTicket{ID: t.ID, Title: t.Title}
			if a := AskFor(t); a != nil {
				lt.Ask, lt.AskLabel = a.Kind, kindLabel(a.Kind)
			}
			if t.Agent != nil {
				lt.Agent = roles[t.Agent.Stage] + " running"
			}
			lane.Tickets = append(lane.Tickets, lt)
		}
		if len(lane.Tickets) == 0 {
			continue
		}
		if col == "done" || col == "rejected" {
			bv.Closed = append(bv.Closed, lane)
		} else {
			bv.Lanes = append(bv.Lanes, lane)
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "board", &Page{Title: name, Tab: "boards", Board: bv}, ts)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.Identity(r) != "" {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", &Page{Title: "Log in", Next: safeNext(r.URL.Query().Get("next"))}, nil)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	next := safeNext(r.PostFormValue("next"))
	if !sameOrigin(r) {
		http.Error(w, "cross-site login refused", http.StatusForbidden)
		return
	}
	if !s.auth.Allowed(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, r, "login", &Page{Title: "Log in", Next: next, Error: "Too many wrong tries. Wait a few minutes."}, nil)
		return
	}
	user, ok := s.auth.LoginAs(ip, r.PostFormValue("secret"))
	if !ok {
		log.Printf("login failed from %s", ip)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login", &Page{Title: "Log in", Next: next, Error: "That is not the secret."}, nil)
		return
	}
	s.auth.SetSessionFor(w, r, user)
	if user != maintainer {
		log.Printf("%s logged in", user)
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// ---------------------------------------------------------------- filing a ticket from Foreman

type NewView struct {
	Projects                   []string
	Project, Kind, Title, Body string
	Error                      string
	Recipes                    []Recipe
}

// Recipe: a standing intent from the factory's config (`factory recipes`), to start a ticket from.
type Recipe struct {
	Name, Title, Body, Kind string
	Projects                []string
	Scheduled               bool
}

// recipes: the factory's recipes, read through its CLI (Foreman has no TOML parser), for a minute at a time.
func (s *Server) recipes() []Recipe {
	text := s.recipeCache.get("all", func() string {
		out, err := s.runner.Output("recipes", "--json")
		if err != nil {
			return "[]"
		}
		return out
	})
	var raw []struct {
		Name, Title, Body, Kind string
		Projects                []string
		Scheduled               bool
	}
	if json.Unmarshal([]byte(text), &raw) != nil {
		return nil
	}
	var out []Recipe
	for _, r := range raw {
		if _, ok := newKinds[r.Kind]; ok {
			out = append(out, Recipe(r))
		}
	}
	return out
}

var newKinds = map[string][]string{"change": nil, "intent": {"--intent"}, "task": {"--task"}, "manage": {"--manage"}}

// projectChoices: the configured projects, then any other on the board, then the no-repository project.
func (s *Server) projectChoices(ts []*Ticket, u *User) []string {
	seen := map[string]bool{noticeProject: true}
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
		add(t.Project)
	}
	if u != nil {
		return out
	}
	return append(out, noticeProject)
}

func (s *Server) newTicket(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	v := &NewView{Projects: s.projectChoices(ts, s.viewer(r)), Kind: "change", Recipes: s.recipes()}
	if len(v.Projects) > 0 {
		v.Project = v.Projects[0]
	}
	s.render(w, r, "new", &Page{Title: "New ticket", Tab: "new", New: v}, ts)
}

func (s *Server) fileTicket(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	v := &NewView{Projects: s.projectChoices(ts, s.viewer(r)), Project: r.PostFormValue("project"), Kind: r.PostFormValue("kind"),
		Title: strings.TrimSpace(r.PostFormValue("title")), Body: strings.TrimSpace(r.PostFormValue("body"))}
	again := func(msg string) {
		v.Error = msg
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, "new", &Page{Title: "New ticket", Tab: "new", New: v}, ts)
	}
	known := false
	for _, p := range v.Projects {
		known = known || p == v.Project
	}
	flags, okKind := newKinds[v.Kind]
	switch {
	case !known:
		again("Pick one of the projects.")
		return
	case !okKind:
		again("Pick a kind.")
		return
	case v.Project == noticeProject && v.Kind != "task" && v.Kind != "manage":
		again("The factory project has no repository: it only takes questions and requests for the manager.")
		return
	case v.Kind == "manage" && s.viewer(r) != nil:
		again("Only the maintainer talks to the manager.")
		return
	case v.Kind == "manage" && v.Project != noticeProject:
		again("Requests for the manager go on the factory project.")
		return
	case v.Title == "" || len(v.Title) > 200 || strings.ContainsAny(v.Title, "\x00\r\n"):
		again("The title is needed, on one line, at most 200 characters.")
		return
	case len(v.Body) > 8000 || strings.ContainsRune(v.Body, 0):
		again("The description is at most 8000 characters.")
		return
	}
	// options first, then "--": a title or description starting with "-" is never read as an option
	args := append([]string{"add", "--body=" + v.Body}, flags...)
	args = append(args, "--", v.Project, v.Title)
	out, err := s.runner.Run([][]string{args})
	if err != nil {
		log.Printf("new ticket: %v", err)
		again(err.Error())
		return
	}
	id := strings.TrimSpace(out)
	if i := strings.LastIndex(id, "\n"); i >= 0 {
		id = id[i+1:]
	}
	if !validID(id) {
		s.flashRedirect(w, r, "/", "ok", "Filed: "+clip(v.Title, 80))
		return
	}
	log.Printf("%s filed from Foreman (%s, %s)", id, v.Project, v.Kind)
	s.flashRedirect(w, r, "/t/"+id, "ok", id+": filed. "+map[string]string{
		"change": "A worker picks it up on the next tick.", "intent": "A designer picks it up on the next tick.",
		"task":   "An agent answers it; the answer lands on your desk.",
		"manage": "The manager answers it, with any config change as a draft for you to apply."}[v.Kind])
}

// ---------------------------------------------------------------- installable app and notifications

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	fmt.Fprint(w, `{"name":"Foreman","short_name":"Foreman","description":"The factory's desk: what needs you, and what happened.",`+
		`"start_url":"/","scope":"/","display":"standalone","background_color":"#111519","theme_color":"#111519",`+
		`"icons":[{"src":"/static/icons/icon-192.png","sizes":"192x192","type":"image/png"},`+
		`{"src":"/static/icons/icon-512.png","sizes":"512x512","type":"image/png"},`+
		`{"src":"/static/icons/icon-512.png","sizes":"512x512","type":"image/png","purpose":"maskable"}]}`)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	v := &SettingsView{}
	if s.push != nil {
		v.PushKey, v.Devices = s.push.PublicKey(), s.push.Count()
	}
	s.render(w, r, "settings", &Page{Title: "Settings", Tab: "settings", Settings: v}, ts)
}

func jsonReply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		jsonReply(w, http.StatusNotFound, map[string]string{"error": "notifications are off on this server"})
		return
	}
	var sub Subscription
	if err := json.Unmarshal([]byte(r.PostFormValue("subscription")), &sub); err != nil {
		jsonReply(w, http.StatusBadRequest, map[string]string{"error": "bad subscription"})
		return
	}
	if u := s.viewer(r); u != nil {
		sub.User = u.Name
	}
	if err := s.push.Add(sub); err != nil {
		jsonReply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("push: a device subscribed (%s)", hostOf(sub.Endpoint))
	jsonReply(w, http.StatusOK, map[string]int{"devices": s.push.Count()})
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.push != nil {
		s.push.Remove(r.PostFormValue("endpoint"))
	}
	n := 0
	if s.push != nil {
		n = s.push.Count()
	}
	jsonReply(w, http.StatusOK, map[string]int{"devices": n})
}

func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		jsonReply(w, http.StatusNotFound, map[string]string{"error": "notifications are off on this server"})
		return
	}
	n := s.push.Send(Message{Title: "Foreman", Body: "Notifications work. You'll hear when something needs you.", URL: "/",
		Tag: "test", Badge: Waiting(Desk(s.visible(r)))})
	jsonReply(w, http.StatusOK, map[string]int{"sent": n, "devices": s.push.Count()})
}

// ---------------------------------------------------------------- flash: one message across a redirect

type Flash struct {
	Kind, Text string
}

const flashCookie = "foreman_flash"

func (s *Server) flashRedirect(w http.ResponseWriter, r *http.Request, to, kind, text string) {
	v := base64.RawURLEncoding.EncodeToString([]byte(kind + "\x00" + clip(text, 600)))
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: v + "." + s.auth.mac("flash", v), Path: "/", MaxAge: 60,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func takeFlash(w http.ResponseWriter, r *http.Request, a *Auth) *Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	v, sig, ok := strings.Cut(c.Value, ".")
	if !ok || sig != a.mac("flash", v) {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil
	}
	kind, text, _ := strings.Cut(string(raw), "\x00")
	return &Flash{Kind: kind, Text: text}
}
