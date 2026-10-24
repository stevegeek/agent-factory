package ticketapi

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

var _ = flag.Bool("update", false, "rewrite golden files")

// testConfig is test_tickets.CONFIG.
const testConfig = `
[board]
people = ["alice", "bob"]

[projects.sandbox]
mode = "board"

[projects.other]
mode = "board"

[projects.fac]
path = "/nonexistent"
github = "o/fac"
`

// setClock is a clock a test moves (the FACTORY_TEST_NOW seam's at()).
type setClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *setClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *setClock) set(stamp string) {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	c.t = t.UTC()
	c.mu.Unlock()
}

// fx is a scratch factory root with a config, a token store and the
// service: test_tickets.TicketCase.
type fx struct {
	t      *testing.T
	env    *testkit.Env
	clock  *setClock
	board  *store.Board
	tokens *tokens.Store
	svc    *Service
}

func newFx(t *testing.T, cfg string) *fx {
	t.Helper()
	env := testkit.Root(t)
	path := env.WriteConfig(t, cfg)
	clk := &setClock{}
	clk.set("2026-10-17T08:00:00+00:00")
	board := newBoard(env.Root, env.Home)
	board.Clock = clk
	tok := &tokens.Store{Path: filepath.Join(env.Root, "cfg", "api-tokens.json"), Clock: clk}
	svc := &Service{Board: board, Tokens: tok, Config: &config.Source{Path: path}, Clock: clk}
	return &fx{t: t, env: env, clock: clk, board: board, tokens: tok, svc: svc}
}

// newBoard is the board under root, with the paths paths.FromEnv gives it.
// The test-import rule (archtest) keeps package paths out of these tests,
// so the fields the board uses are set here, as FromEnv sets them.
func newBoard(root, home string) *store.Board {
	b := &store.Board{}
	p := &b.Paths
	p.Root, p.Home = root, home
	p.Board = filepath.Join(root, "board")
	p.Tickets = filepath.Join(p.Board, "tickets")
	p.Locks = filepath.Join(p.Tickets, ".locks")
	p.Index = filepath.Join(p.Board, "index.json")
	p.IDLock = filepath.Join(p.Tickets, ".id.lock")
	p.LastID = filepath.Join(p.Tickets, ".last-id")
	p.IndexLock = filepath.Join(p.Board, ".index.lock")
	p.CreateLock = filepath.Join(p.Board, ".create.lock")
	p.Logs = filepath.Join(root, "logs")
	p.FactoryLog = filepath.Join(p.Logs, "factory.log")
	p.LogLock = filepath.Join(p.Logs, ".log.lock")
	p.Config = filepath.Join(root, "config.toml")
	p.TickLock = filepath.Join(root, ".tick.lock")
	p.TickAgain = filepath.Join(root, ".tick.again")
	p.Tokens = filepath.Join(root, "cfg", "api-tokens.json")
	b.Reader = &boardread.Reader{Paths: b.Paths}
	return b
}

func (f *fx) writeConfig(cfg string) {
	f.t.Helper()
	f.env.WriteConfig(f.t, cfg)
	// a new mtime even on a coarse clock
	future := time.Now().Add(time.Duration(len(cfg)) * time.Second)
	os.Chtimes(f.svc.Config.Path, future, future)
}

// token is `factory tokens add NAME --can C --project P...`.
func (f *fx) token(name string, can string, projects ...string) {
	f.t.Helper()
	if len(projects) == 0 {
		projects = []string{"sandbox", "fac"}
	}
	if can == "" {
		can = "read,file,talk,work,plan"
	}
	_, err := f.tokens.Add(context.Background(), tokens.AddIn{Name: name, Projects: projects, Can: can,
		People: []string{"alice", "bob"}, Known: []string{"sandbox", "other", "fac"}})
	if err != nil {
		f.t.Fatalf("token %s: %v", name, err)
	}
}

// setToken changes one key of a token's record by hand.
func (f *fx) setToken(name, key, value string) {
	f.t.Helper()
	err := f.tokens.Change(context.Background(), func(file *tokens.File) error { return file.Set(name, key, parse(f.t, value)) })
	if err != nil {
		f.t.Fatal(err)
	}
}

func parse(t testing.TB, s string) jsonx.Value {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return v
}

func callerOf(as string) Caller {
	if as == "" {
		return ForPerson()
	}
	return ForToken(as)
}

// do is `factory ticket VERB --json [--as AS]` with req on stdin.
func (f *fx) do(verb, req, as string) (jsonx.Object, error) {
	f.t.Helper()
	return f.svc.Do(context.Background(), callerOf(as), verb, parse(f.t, req), 5*time.Second)
}

func (f *fx) ok(verb, req, as string) jsonx.Object {
	f.t.Helper()
	out, err := f.do(verb, req, as)
	if err != nil {
		f.t.Fatalf("%s %s as %q: %v", verb, req, as, err)
	}
	return out
}

func (f *fx) refused(verb, req, as string) string {
	f.t.Helper()
	_, err := f.do(verb, req, as)
	var r *Refused
	if !errors.As(err, &r) {
		f.t.Fatalf("%s %s as %q: %v, want a refusal", verb, req, as, err)
	}
	return r.Code
}

// code is "ok" or the refusal's code.
func (f *fx) code(verb, req, as string) string {
	f.t.Helper()
	_, err := f.do(verb, req, as)
	if err == nil {
		return "ok"
	}
	var r *Refused
	if !errors.As(err, &r) {
		f.t.Fatalf("%s %s as %q: %v", verb, req, as, err)
	}
	return r.Code
}

// boardTicket is TicketCase.board_ticket: a sandbox ticket filed by the
// maintainer.
func (f *fx) boardTicket(extra string) jsonx.Value {
	f.t.Helper()
	req := `{"project": "sandbox", "title": "Fix the date picker"`
	if extra != "" {
		req += ", " + extra
	}
	return f.ok("create", req+"}", "").Get("ticket")
}

// factoryTicket is new_ticket(project, title, body, door, column).
func (f *fx) factoryTicket(project, title, door, column string) *ticket.Ticket {
	f.t.Helper()
	t, err := f.board.FileTicket(context.Background(), ticket.NewIn{Project: project, Title: title, Body: "b", Door: door, Column: column})
	if err != nil {
		f.t.Fatal(err)
	}
	return t
}

func (f *fx) load(id string) jsonx.Value {
	f.t.Helper()
	t, err := f.board.Reader.Load(ticket.ID(id))
	if err != nil {
		f.t.Fatal(err)
	}
	return t.Tree()
}

func (f *fx) index() boardread.Index {
	f.t.Helper()
	ix, err := f.board.RefreshIndex(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return ix
}

func (f *fx) countTickets() int {
	f.t.Helper()
	ts, _ := f.board.Reader.List()
	return len(ts)
}

func (f *fx) limits(edit func(*Limits)) {
	l := DefaultLimits()
	edit(&l)
	f.svc.Limits = &l
}

// str is the string at a path of keys and indexes ("0") in v.
func str(v jsonx.Value, path ...string) string {
	v = at(v, path...)
	if s, ok := v.Str(); ok {
		return s
	}
	return string(jsonx.Format{Indent: -1}.Encode(v))
}

func at(v jsonx.Value, path ...string) jsonx.Value {
	for _, p := range path {
		if a, ok := v.Array(); ok {
			var i int
			fmt.Sscan(p, &i)
			if i < 0 {
				i += len(a)
			}
			if i < 0 || i >= len(a) {
				return jsonx.Null()
			}
			v = a[i]
			continue
		}
		v = v.Get(p)
	}
	return v
}

func enc(v jsonx.Value) string { return string(jsonx.Format{Indent: -1}.Encode(v)) }

func ids(v jsonx.Value) string {
	a, _ := v.Array()
	var out []string
	for _, x := range a {
		out = append(out, str(x, "id"))
	}
	return strings.Join(out, ",")
}

func length(v jsonx.Value) int {
	if a, ok := v.Array(); ok {
		return len(a)
	}
	o, _ := v.Object()
	return len(o)
}

func obj(o jsonx.Object) jsonx.Value { return o.Value() }
