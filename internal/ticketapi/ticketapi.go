// Package ticketapi is the ticket API: the verbs of `factory ticket`, which
// Foreman's /api/v1 and /mcp serve to agents outside the factory
// (ARCHITECTURE.md §2.3, inventory features §15). Every decision about a
// ticket made through the API is made here: who the caller is, what they
// may see and do, the column rules of a board, the limits and quotas, and
// idempotent creates.
//
// A ticket the caller may not see is not_found, never forbidden: nothing
// says it exists. A token can never act as a person. Each call reads the
// config once, as it is now (config.Source), so a project removed, a new
// person or a changed mode takes effect on the next call.
//
// The verbs port bin/factory's t_* functions (F:3705-4189) and give the
// same answers, the same refusals with the same words, and the same ticket
// files, byte for byte (the goldens under testdata/py are Python's).
package ticketapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

// The codes a refusal carries (§8.1). api and mcp map them to statuses.
const (
	BadRequest          = "bad_request"
	NotFound            = "not_found"
	Forbidden           = "forbidden"
	Unauthorized        = "unauthorized"
	Conflict            = "conflict"
	Busy                = "busy"
	TooMany             = "too_many"
	StorageFull         = "storage_full"
	IdempotencyMismatch = "idempotency_mismatch"
	FactoryTicket       = "factory_ticket"
	NotOnFactoryTicket  = "not_on_factory_ticket"
)

// Refused is a request the factory will not carry out: a code from the
// list above, the message a caller is shown and, for a bad request, the
// field at fault ("" for none).
type Refused struct {
	Code, Message, Field string
}

func (e *Refused) Error() string { return e.Message }

// Reply is the JSON line `factory ticket --json` prints for a refusal:
// {"ok": false, "error": {code, message[, field]}}.
func (e *Refused) Reply() []byte {
	o := jsonx.Object{{Key: "code", Value: jsonx.String(e.Code)}, {Key: "message", Value: jsonx.String(e.Message)}}
	if e.Field != "" {
		o = append(o, jsonx.Member{Key: "field", Value: jsonx.String(e.Field)})
	}
	return jsonx.Format{Indent: -1}.Encode(jsonx.ObjectOf(
		jsonx.Member{Key: "ok", Value: jsonx.Bool(false)}, jsonx.Member{Key: "error", Value: o.Value()}))
}

// Reply is the JSON line `factory ticket --json` prints for an answer:
// {"ok": true, ...out}.
func Reply(out jsonx.Object) []byte {
	o := append(jsonx.Object{{Key: "ok", Value: jsonx.Bool(true)}}, out...)
	return jsonx.Format{Indent: -1}.Encode(o.Value())
}

func refuse(code, field, format string, args ...any) error {
	return &Refused{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func bad(field, format string, args ...any) error { return refuse(BadRequest, field, format, args...) }

// Request reads a request as `factory ticket --json` reads stdin: at most
// limit bytes (0 is LIMIT["request"], 1 MiB), JSON, and empty is {}. What
// it reads need not be an object: the verb refuses one that is not.
func Request(raw []byte, limit int) (jsonx.Value, error) {
	if limit <= 0 {
		limit = DefaultLimits().Request
	}
	if len(raw) > limit {
		return jsonx.Value{}, bad("", "the request is at most 1 MiB")
	}
	if len(raw) == 0 {
		return jsonx.ObjectOf(), nil
	}
	v, err := jsonx.Parse(raw)
	if err != nil {
		return jsonx.Value{}, bad("", "the request is not JSON")
	}
	return v, nil
}

// TokenSource is the token store as the API reads it: the live record of a
// token name (tokens.Store).
type TokenSource interface {
	Live(name string) (tokens.Record, bool, error)
}

// Service carries out the verbs on Board. Tokens and Config are read
// afresh on every call; Clock stamps what the verbs write (the board's own
// clock stamps saves). Limits nil is LIMIT's values.
type Service struct {
	Board  *store.Board
	Tokens TokenSource
	Config *config.Source
	Clock  clock.Clock
	Limits *Limits

	// afterRead, when set, runs between an update's first read of a ticket
	// and its locks: tests use it to change the ticket meanwhile.
	afterRead func()
}

// Caller is who asks: the maintainer (a person, with every right), or a
// token by name (`--as NAME`), whose rights its record gives.
type Caller struct {
	name   string
	token  bool
	person bool
}

// ForPerson is the maintainer, the first of [board] people: whom the CLI
// and Foreman's maintainer login act as.
func ForPerson() Caller { return Caller{person: true} }

// AsPerson is another person acting with a person's rights: a Foreman user
// answering a board agent's question, recorded by their own name (D19). A
// reserved name (factory, maintainer, …) is refused.
func AsPerson(name string) Caller { return Caller{name: name, person: true} }

// ForToken is a token's caller, by its name: Foreman checked the bearer
// token and passes on the name.
func ForToken(name string) Caller { return Caller{name: name, token: true} }

// verbs are TICKET_VERBS, in their order.
func verbs() []string {
	return []string{"create", "get", "list", "mine", "note", "move", "claim", "update", "ask", "answer", "split", "send",
		"projects", "whoami", "reindex"}
}

// Verbs are the verb names, in TICKET_VERBS' order.
func Verbs() []string { return verbs() }

// ErrUnknownVerb is a verb the API does not have (argparse's choices).
var ErrUnknownVerb = errors.New("unknown ticket verb")

// call is one verb's call: the config it read, the caller it resolved and
// how long it waits for a lock.
type call struct {
	ctx  context.Context
	s    *Service
	cfg  *config.Config
	c    *caller
	wait time.Duration
	lim  Limits
}

// Do is `factory ticket VERB --json`: the verb's answer to req, where a
// "format" key of a write is taken out first and "md" answers it with
// Markdown and a slim ticket (written_markdown). wait is how long a ticket
// lock is waited for (negative: as long as ctx allows); the API waits 10 s,
// the CLI 60 s. A *Refused is the caller's to see; any other error is the
// factory's own failure.
func (s *Service) Do(ctx context.Context, c Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error) {
	cx, err := s.begin(ctx, c, verb, wait)
	if err != nil {
		return nil, err
	}
	md := false
	if o, ok := req.Object(); ok && verb != "get" && verb != "list" && verb != "mine" {
		if f, has := o.Lookup("format"); has {
			o = o.Clone()
			o.Delete("format")
			req = o.Value()
			switch s, _ := f.Str(); {
			case f.Kind() == jsonx.KindString && s == "md":
				md = true
			case f.Kind() == jsonx.KindString && s == "json":
			default:
				return nil, bad("format", "format is json or md")
			}
		}
	}
	out, err := cx.run(verb, req)
	if err != nil || !md {
		return out, err
	}
	if _, ok := out.Lookup("ticket"); !ok {
		return out, nil
	}
	return cx.written(verb, out), nil
}

// Call is one verb on req as TICKET_VERBS[verb] runs it, without Do's
// format handling.
func (s *Service) Call(ctx context.Context, c Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error) {
	cx, err := s.begin(ctx, c, verb, wait)
	if err != nil {
		return nil, err
	}
	return cx.run(verb, req)
}

// written is written_markdown's answer to out, for a test's golden.
func (s *Service) written(ctx context.Context, verb string, c Caller, out jsonx.Object) (jsonx.Object, error) {
	cx, err := s.begin(ctx, c, verb, 0)
	if err != nil {
		return nil, err
	}
	return cx.written(verb, out), nil
}

func (s *Service) begin(ctx context.Context, c Caller, verb string, wait time.Duration) (*call, error) {
	known := false
	for _, v := range verbs() {
		known = known || v == verb
	}
	if !known {
		return nil, fmt.Errorf("%w: %q", ErrUnknownVerb, verb)
	}
	cfg, err := s.Config.Current()
	if err != nil {
		return nil, fmt.Errorf("ticket %s: %w", verb, err)
	}
	lim := DefaultLimits()
	if s.Limits != nil {
		lim = *s.Limits
	}
	cx := &call{ctx: ctx, s: s, cfg: cfg, wait: wait, lim: lim}
	if cx.c, err = cx.resolve(c); err != nil {
		return nil, err
	}
	return cx, nil
}

func (cx *call) run(verb string, req jsonx.Value) (jsonx.Object, error) {
	switch verb {
	case "create":
		return cx.create(req)
	case "get":
		return cx.get(req)
	case "list":
		return cx.list(req)
	case "mine":
		return cx.mine(req)
	case "note":
		return cx.note(req)
	case "move":
		return cx.move(req)
	case "claim":
		return cx.claim(req)
	case "update":
		return cx.update(req)
	case "ask":
		return cx.ask(req)
	case "answer":
		return cx.answer(req)
	case "split":
		return cx.split(req)
	case "send":
		return cx.send(req)
	case "projects":
		return cx.projects(req)
	case "whoami":
		return cx.whoami(req)
	}
	return cx.reindex(req)
}

func (cx *call) now() time.Time {
	if cx.s.Clock == nil {
		return time.Now().UTC()
	}
	return cx.s.Clock.Now().UTC()
}

func (cx *call) stamp() string { return clock.Stamp(cx.now()) }

// Limits are LIMIT (F:3205): the most a caller may put in a ticket, and
// the most a token may write. Byte limits count UTF-8 bytes; Title, Reason,
// Option, URL and LinkTitle count code points, as Python's len does.
type Limits struct {
	Title, Body, Labels, Links, URL, LinkTitle int
	Comment, Comments, Result, Question        int
	Options, Option, OpenQuestions, Answer     int
	Reason, Children, Depth, Note              int
	ProjectTickets, CreatesPerDay              int
	Request, Markdown, History, Conversation   int
	Store                                      int64
	TokenStore, CommentBytes                   int64
	TicketHistory, SearchFiles                 int
	SearchBytes                                int64
}

// DefaultLimits are LIMIT's values.
func DefaultLimits() Limits {
	return Limits{Title: 300, Body: 64 << 10, Labels: 20, Links: 50, URL: 2048, LinkTitle: 300,
		Comment: 16 << 10, Comments: 1000, Result: 64 << 10, Question: 4 << 10,
		Options: 6, Option: 100, OpenQuestions: 3, Answer: 16 << 10,
		Reason: 500, Children: 20, Depth: 20, Note: 4 << 10,
		ProjectTickets: 10_000, CreatesPerDay: 200,
		Store: 200 << 20, Request: 1 << 20, Markdown: 256 << 10, History: 50,
		Conversation: 50, TokenStore: 20 << 20, CommentBytes: 1 << 20,
		TicketHistory: 2000, SearchFiles: 2000, SearchBytes: 16 << 20}
}
