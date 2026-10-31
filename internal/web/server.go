// Package web is Foreman's server: the routes, pages, templates and static
// files, the flash message, live polling, the Live tab, filing a new ticket,
// settings and the push endpoints, and Main.
//
// It holds no decision. It reads the board through a change-aware snapshot
// (boardread), formats with package present, and every write or answer goes
// to the services it declares here (Answerer, Filer, Looker), which the
// foreman binary wires to package ops in-process (web/answers.go), or, for a
// board agent's question, to the ticket API (TicketVerbs). It cannot import
// the board's writer, git, Scion or GitHub (archtest).
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/desk"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/mdhtml"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/push"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// ---------------------------------------------------------------- what web consumes

// Answer is one answer offered for an ask. Href answers are plain links;
// the rest submit the ticket's form with Value.
type Answer struct {
	Value, Label, Sub string
	Primary           bool
	Href              string // only ever an http(s) link
	NeedsNote         bool
	NoteFor           string // "to send it back": names the answer the note is needed for
	Danger            bool   // closes the ticket or the PR: shown last, in red
}

// AnswerRequest is what a person posted on a ticket's form, and who: the
// answer records its Foreman user as by.
type AnswerRequest struct {
	ID    ticket.ID
	Value string // an Answer's Value, or "note", "ask" (a question for an agent) or "talk" (a reply)
	Note  string // as typed
	Picks []int  // the design proposals, or the findings, ticked, 1-based
	// As is how each ticked finding is filed (change or intent), as chosen.
	As      map[int]string
	Options []string // the keys of the agent's question's options, chosen
	// Kind and Since are the ask the page showed ("" when none): the
	// service refuses, under the ticket's lock, an answer to an ask that has
	// moved on.
	Kind, Since string
	By          string // the user answering; "" is the maintainer
}

// ErrStale is the error an Answerer returns (wrapped) for an answer to an
// ask that has moved on since the page was rendered.
var ErrStale = errors.New("the ticket changed since the page was opened")

// Answerer is the answer service (ops.Answers and ops.Answer).
type Answerer interface {
	// Answers are the answers offered for ask a on t (a may be nil).
	Answers(t *ticket.Ticket, a *ticket.Ask) []Answer
	// Answer carries one out and returns a short line for the flash. Its
	// refusals are errors whose text a person reads.
	Answer(ctx context.Context, req AnswerRequest) (string, error)
}

// NewTicket is a ticket filed from Foreman's ＋ page.
type NewTicket struct {
	// Kind is change, intent, task or manage; By "" is the maintainer; After holds the ids it waits for.
	Project, Kind, Title, Body, By, After string
}

// Filer files a new ticket (ops.Add).
type Filer interface {
	File(ctx context.Context, n NewTicket) (ticket.ID, error)
}

// Looker reads a running agent's screen, read-only (ops.Look).
type Looker interface {
	Look(ctx context.Context, id ticket.ID, lines int) (string, error)
}

// Switcher switches a factory project on or off over config.toml's
// `enabled`, or back to following it (ops.Service.SwitchProject): state is
// "on", "off" or "follow", by who; it returns a line for the flash.
type Switcher interface {
	SwitchProject(ctx context.Context, project, state, by string) (string, error)
}

// DeployHolder holds automatic deploys or releases them
// (ops.Service.HoldDeploys): while held, a merge leaves a Deploy? ask; by
// is who, and it returns a line for the flash.
type DeployHolder interface {
	HoldDeploys(ctx context.Context, held bool, note, by string) (string, error)
}

// ConfigSource is the factory's config, re-read when it changes
// (config.Source).
type ConfigSource interface {
	Current() (*config.Config, error)
}

// Services are what web calls besides the board. Any may be nil: the pages
// then offer nothing that needs it.
type Services struct {
	Answers Answerer
	Filer   Filer
	Looker  Looker
	Passer  Passer // a ticket given to a person
	Config  ConfigSource
	Switch  Switcher     // the maintainer's project switch on Boards
	Deploys DeployHolder // the maintainer's deploy hold on System status
	Pins    HostPinner   // the maintainer's "Run on" on a ticket's page
	Tickets TicketVerbs  // a person's answer to a board agent's question (package ticketapi)
	API     http.Handler // /api/v1/, bearer tokens only (package api)
	MCP     http.Handler // /mcp, bearer tokens only (package mcp)
}

// Passer gives a ticket to a person ("" for nobody), as by, with a note
// that may be "", and returns a line for the flash.
type Passer interface {
	Pass(ctx context.Context, id ticket.ID, to, by, note string) (string, error)
}

// TicketVerbs is what web uses of the ticket API (*ticketapi.Service): the
// answer verb, which MCP's answer_question runs, for a person answering an
// agent's question on a board ticket.
type TicketVerbs interface {
	Do(ctx context.Context, c ticketapi.Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error)
}

// Board is the board as web reads it (boardread.Snapshot).
type Board interface {
	Load() (*boardread.View, error)
}

// Logger is Foreman's journal.
type Logger interface {
	Printf(format string, args ...any)
}

// Options configures a Server.
type Options struct {
	Paths    paths.Paths
	Auth     *auth.Auth
	Board    Board
	Services Services
	Push     *push.Pusher // nil: notifications are off
	Projects []string     // what a new ticket may name, besides the board's own projects
	// Sealed are files this process must not be able to open (the App key,
	// the Claude token): /healthz says when it can.
	Sealed []string
	// Bus are sockets this process must not be able to connect to (the
	// user manager's bus and private socket: through them it could run any
	// command outside its sandbox): /healthz says when it can.
	Bus   []string
	Clock clock.Clock // nil: the system clock
	Log   Logger      // nil: silent
}

// Server is Foreman's HTTP server.
type Server struct {
	paths    paths.Paths
	auth     *auth.Auth
	board    Board
	svc      Services
	push     *push.Pusher
	projects []string
	sealed   []string
	bus      []string
	clock    clock.Clock
	log      Logger
	pages    map[string]*template.Template
	assetV   string
	screens  *screens
	watch    watchMark
	started  time.Time // when New ran: /healthz says it
}

const (
	apiPrefix = "/api/v1/"
	mcpPath   = "/mcp"
	// answerTimeout outlasts the 60 s an answer waits for a ticket the tick
	// holds, so the service's own "busy" reaches the person.
	answerTimeout = 90 * time.Second
	fileTimeout   = 30 * time.Second
)

// New builds a Server; the templates are parsed once, here.
func New(opt Options) (*Server, error) {
	if opt.Auth == nil || opt.Board == nil {
		return nil, fmt.Errorf("web: a server needs Auth and Board")
	}
	s := &Server{paths: opt.Paths, auth: opt.Auth, board: opt.Board, svc: opt.Services, push: opt.Push,
		projects: opt.Projects, sealed: opt.Sealed, bus: opt.Bus, clock: opt.Clock, log: opt.Log, pages: map[string]*template.Template{}}
	if s.clock == nil {
		s.clock = clock.System{}
	}
	s.started = s.clock.Now()
	s.screens = newScreens(s.clock)
	h := sha256.New()
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		h.Write(b)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("web: static files: %w", err)
	}
	s.assetV = hex.EncodeToString(h.Sum(nil))[:10]
	funcs := template.FuncMap{
		"asset": func(name string) string { return "/static/" + name + "?v=" + s.assetV },
		"low":   func(n int) bool { return n < 4 },
		"inc":   func(n int) int { return n + 1 },
		"join":  strings.Join,
		"md":    mdhtml.Render,
		"mdi":   mdhtml.Line,
		"mdt":   mdhtml.Text,
	}
	for _, page := range []string{"desk", "ticket", "floor", "boards", "board", "login", "settings", "system", "logs", "metrics", "new", "live", "talk", "search"} {
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/parts.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("web: template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

func (s *Server) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

func (s *Server) now() time.Time { return s.clock.Now() }

// Handler is every route, behind the security headers and the Host check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /healthz", s.healthz)
	// the installable app: the manifest and the service worker must be
	// readable before login, and the worker must come from the root to
	// cover every page
	mux.HandleFunc("GET /manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /sw.js", s.serviceWorker)
	if s.svc.API != nil {
		// the ticket API: bearer tokens, never the login session
		mux.Handle(apiPrefix, s.svc.API)
	}
	if s.svc.MCP != nil {
		mux.Handle(mcpPath, s.svc.MCP)
	}
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", s.desk)
	app.HandleFunc("GET /t/{id}", s.ticket)
	app.HandleFunc("POST /t/{id}/act", s.act)
	app.HandleFunc("POST /t/{id}/host", s.pinHost)
	app.HandleFunc("GET /t/{id}/talk", s.talk)
	app.HandleFunc("GET /floor", s.floor)
	app.HandleFunc("GET /floor/metrics", s.floorMetrics)
	app.HandleFunc("GET /boards", s.boards)
	app.HandleFunc("POST /t/{id}/assign", s.assign)
	app.HandleFunc("POST /t/{id}/seen", s.seenMention)
	app.HandleFunc("POST /boards/switch", s.switchProject)
	app.HandleFunc("GET /search", s.search)
	app.HandleFunc("GET /b/{project}", s.boardPage)
	app.HandleFunc("GET /settings", s.settings)
	app.HandleFunc("POST /settings/desk", s.setDesk)
	app.HandleFunc("POST /settings/users", s.addUser)
	app.HandleFunc("POST /settings/users/{name}", s.changeUser)
	app.HandleFunc("GET /system", s.systemPage)
	app.HandleFunc("GET /system.json", s.systemJSON)
	app.HandleFunc("POST /system/deploys/hold", s.holdDeploys)
	app.HandleFunc("GET /settings/system.json", s.systemJSON) // its first address
	app.HandleFunc("GET /logs", s.logsPage)
	app.HandleFunc("GET /new", s.newTicket)
	app.HandleFunc("POST /new", s.fileTicket)
	app.HandleFunc("POST /push/subscribe", s.pushSubscribe)
	app.HandleFunc("POST /push/unsubscribe", s.pushUnsubscribe)
	app.HandleFunc("POST /push/test", s.pushTest)
	app.HandleFunc("GET /live", s.livePage)
	app.HandleFunc("GET /live/log", s.liveLog)
	app.HandleFunc("GET /live/{id}/screen", s.liveScreen)
	app.HandleFunc("GET /t/{id}/logs/{name}", s.agentLog)
	app.HandleFunc("POST /logout", s.logout)
	mux.Handle("/", s.auth.Require(s.withBoard(s.live(app))))
	return headers(s.auth.CheckHost(mux))
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; "+
			"connect-src 'self'; font-src 'self'; manifest-src 'self'; worker-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// not no-referrer: under it browsers send "Origin: null" on this
		// site's own form posts
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// healthz is "ok", then "version <commit>" (the build that answers: a
// deploy waits for the new one, since the old Foreman answers until its
// restart) and "started <RFC 3339 UTC>" (when this server started: a
// deploy of the running commit waits for a Foreman started after it
// began), plus one line for each sealed file this process can open and
// each bus socket it can connect to: under the sandboxed unit there must
// be none. The file is opened and closed, never read; the socket is
// connected to and closed, with nothing sent.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	out := "ok\nversion " + buildinfo.Version + "\nstarted " + s.started.UTC().Format(time.RFC3339) + "\n"
	// how old the tick's last sysinfo.json is, so an outside check can tell a
	// stuck tick from a live Foreman
	if snap, err := sysinfo.Read(sysinfo.File(s.paths)); err == nil && !snap.Taken.IsZero() {
		out += fmt.Sprintf("tick-age %d\n", int64(max(s.clock.Now().Sub(snap.Taken), 0)/time.Second))
	} else {
		out += "tick-age unknown\n"
	}
	for _, p := range s.sealed {
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			f.Close()
			out += "exposed: " + filepath.Base(p) + "\n"
		}
	}
	for _, p := range s.bus {
		d := net.Dialer{Timeout: time.Second}
		if c, err := d.DialContext(r.Context(), "unix", p); err == nil {
			c.Close()
			out += "exposed: " + p + "\n"
		}
	}
	fmt.Fprint(w, out)
}

// ---------------------------------------------------------------- the board, once per request

// liveHeader carries the board version a page was rendered from. foreman.js
// polls the page's own URL with it: 204 while nothing changed, else the
// page as it is now, to update in place.
const liveHeader = "X-Foreman-Live"

type boardKey struct{}

// reqBoard is the board as one request sees it: loaded once (bug 2), and
// filtered to what the viewer may see.
type reqBoard struct {
	all []*ticket.Ticket
}

func (s *Server) load() []*ticket.Ticket {
	v, err := s.board.Load()
	if err != nil {
		s.logf("%v", err)
	}
	if v == nil {
		return nil
	}
	return v.Tickets
}

func (s *Server) withBoard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rb := &reqBoard{all: s.load()}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), boardKey{}, rb)))
	})
}

// visible is the board as the request loaded it: everyone sees every board
// (only the desk is per person).
func (s *Server) visible(r *http.Request) []*ticket.Ticket { return s.whole(r) }

// whole is the whole board as the request loaded it.
func (s *Server) whole(r *http.Request) []*ticket.Ticket {
	if rb, ok := r.Context().Value(boardKey{}).(*reqBoard); ok {
		return rb.all
	}
	return s.load()
}

// fresh is the board read again, for what to show after a write.
func (s *Server) fresh(r *http.Request) []*ticket.Ticket { return s.load() }

// version is the live version of what a viewer sees.
func version(ts []*ticket.Ticket) string { return (&boardread.View{Tickets: ts}).Version() }

func (s *Server) live(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(liveHeader); v != "" && r.Method == http.MethodGet && v == version(s.visible(r)) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- who is asking

// viewer is the user asking, or nil for the main secret (only while there
// are no users). Everyone may do everything; who they are says whose
// answers they are and what is on their desk.
func (s *Server) viewer(r *http.Request) *auth.User {
	name := s.auth.UserOf(auth.IdentityOf(r))
	if name == auth.Maintainer {
		return nil
	}
	if u := s.auth.Users().Get(name); u != nil {
		return u
	}
	return &auth.User{Name: name}
}

// cfg is the config as it is now, or nil.
func (s *Server) cfg() *config.Config {
	if s.svc.Config == nil {
		return nil
	}
	cfg, err := s.svc.Config.Current()
	if err != nil {
		return nil
	}
	return cfg
}

// actor is who an answer or a new ticket from r records as by: "" for the
// main secret and for the user who is the board's first person, whom the
// services name by the first of [board] people, else the user's own name.
func (s *Server) actor(r *http.Request) string {
	u := s.viewer(r)
	if u == nil {
		return ""
	}
	if cfg := s.cfg(); cfg != nil && cfg.Maintainer() == u.Name {
		return ""
	}
	return u.Name
}

// asPerson is the ticket API's caller for a person's answer from r: the
// maintainer, or the user as a person of their own.
func (s *Server) asPerson(r *http.Request) ticketapi.Caller {
	if name := s.actor(r); name != "" {
		return ticketapi.AsPerson(name)
	}
	return ticketapi.ForPerson()
}

// deskViewer is who reads the desk: their desk projects, what is put to
// them by name, their mentions, and "You've responded" for their own
// responses only.
func (s *Server) deskViewer(r *http.Request) present.Viewer {
	v := present.Viewer{}
	cfg := s.cfg()
	if cfg != nil {
		v.People = cfg.People()
		v.By = cfg.Maintainer()
	}
	// once there are users, only they have desks: an ask put to a name no
	// user has (the default "maintainer" before [board] people names
	// anyone) stays on the desks of its project
	if users := s.auth.Users().Names(); len(users) > 0 {
		v.People = users
	}
	u := s.viewer(r)
	if u == nil {
		v.Maintainer = true
		return v
	}
	v.Maintainer = v.By == u.Name
	v.By, v.Desk, v.Dismissed = u.Name, u.Desk, u.Dismissed
	return v
}

// who names the person for the journal.
func (s *Server) who(r *http.Request) string {
	if u := s.viewer(r); u != nil {
		return u.Name
	}
	return auth.Maintainer
}

// ---------------------------------------------------------------- calls to the services

// call runs a service call on a context detached from the request (a client
// hanging up never cuts a write short), with a timeout, turning a panic
// into an error with no detail for the caller (logged with its stack).
func (s *Server) call(r *http.Request, timeout time.Duration, fn func(ctx context.Context) error) (err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			s.logf("panic in %s %s: %v\n%s", r.Method, r.URL.Path, p, debug.Stack())
			err = fmt.Errorf("the factory could not do that; it is logged")
		}
	}()
	return fn(ctx)
}

// ---------------------------------------------------------------- ids

// idRe is a ticket id as the pages accept it: "T" and 4 to 6 ASCII digits.
// Every route with an id checks it before touching anything.
var idRe = regexp.MustCompile(`^T[0-9]{4,6}$`) // re: go

func validID(id string) bool { return idRe.MatchString(id) }

func findTicket(ts []*ticket.Ticket, id string) *ticket.Ticket {
	for _, t := range ts {
		if string(t.ID) == id {
			return t
		}
	}
	return nil
}

// ---------------------------------------------------------------- pages

// Page is everything a template may show.
type Page struct {
	Title      string
	Tab        string
	CSRF       string
	DeskCount  int
	Running    int
	Who        string // the user's name; "" for the main secret
	FirstLogin bool   // there are no users yet: the main secret logs in
	Live       *LiveView
	Talk       *present.TalkView
	Flash      *Flash
	Desk       *present.DeskView
	ForYou     []desk.ForYouItem // the desk's: mentions of the viewer and tickets passed to them, from any project
	Ticket     *TicketView
	Floor      *present.Floor
	Tokens     *TokensView    // Floor metrics' Tokens section
	Boards     []BoardSummary // factory projects
	Agents     []BoardSummary // board projects, worked by agents
	Groups     []BoardGroup   // projects config puts in a group, by group name
	Board      *BoardView
	Next       string
	Error      string
	Version    string
	GitHub     string // the tick's view of GitHub (sysinfo.GitHubStatus)
	GitHubDown bool
	FactoryOff string // the whole factory is off: "by alice at 9 Oct 10:00"; "" while it runs
	CanSwitch  bool   // this server can switch projects and the factory on and off
	Settings   *SettingsView
	System     *SystemView
	New        *NewView
	Search     *SearchView
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *Page, ts []*ticket.Ticket) {
	if id := auth.IdentityOf(r); id != "" {
		p.CSRF = s.auth.CSRF(id)
		if u := s.viewer(r); u != nil {
			p.Who = u.Name
		}
	}
	if ts != nil {
		v := s.deskViewer(r)
		p.DeskCount = present.Waiting(present.Desk(ts, v)) + len(desk.Nudges(ts, v))
		p.Running = running(ts)
		p.Version = version(ts)
		p.GitHub, p.GitHubDown = sysinfo.GitHubStatus(s.paths)
	}
	if off, ok := config.FactoryOff(s.paths.ProjectSwitches()); ok { // the banner: "switched off by alice at 9 Oct 10:00"
		p.FactoryOff = "switched off" + strings.TrimPrefix(switchNote(config.Project{Switch: &off}), "paused here")
	}
	p.CanSwitch = s.svc.Switch != nil
	// a poll must not use up the message meant for the next page the person opens
	if p.Flash == nil && r.Header.Get(liveHeader) == "" {
		p.Flash = s.takeFlash(w, r)
	}
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Push-Url", r.URL.RequestURI())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		s.logf("render %s: %v", name, err)
	}
}
