// Package api is the ticket API at /api/v1 (ARCHITECTURE.md §2.3, §5.6;
// inventory-foreman §11), for scripts and agents with a token made by
// `factory tokens add`. The guard checks who is calling; each route names
// one ticketapi verb, which runs in-process on the shared slots, and the
// verb decides the rest. This package only translates: HTTP in, the verb's
// request; the verb's answer or refusal, HTTP out.
//
// It is Foreman's api.go with the `factory ticket VERB --as NAME --wait 10
// --json` subprocess replaced by ticketapi.Service.Do. The request a verb
// sees is built exactly as the CLI's stdin was, and the answer is encoded
// exactly as Foreman relayed the CLI's, so the HTTP contract is unchanged.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/guard"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

// Prefix is where the API is mounted.
const Prefix = "/api/v1/"

// DefaultWait is how long a verb waits for a ticket the tick holds (the
// CLI's --wait 10 for API callers).
const DefaultWait = 10 * time.Second

// re: go
var ticketID = regexp.MustCompile(`^T\d{4,6}$`)

// Service runs a ticket verb: *ticketapi.Service.
type Service interface {
	Do(ctx context.Context, c ticketapi.Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error)
}

// Logger is Foreman's journal.
type Logger interface {
	Printf(format string, args ...any)
}

// Options configures the API.
type Options struct {
	Guard   *guard.Guard // shared with /mcp
	Service Service
	Wait    time.Duration // a verb's wait for a ticket lock (DefaultWait)
	Log     Logger        // nil: silent
}

// API serves /api/v1/.
type API struct {
	guard  *guard.Guard
	svc    Service
	wait   time.Duration
	log    Logger
	routes map[string]map[string]route
}

// New builds the API.
func New(o Options) *API {
	a := &API{guard: o.Guard, svc: o.Service, wait: o.Wait, log: o.Log, routes: routes()}
	if a.wait <= 0 {
		a.wait = DefaultWait
	}
	return a
}

func (a *API) logf(format string, args ...any) {
	if a.log != nil {
		a.log.Printf(format, args...)
	}
}

// statusOf is the HTTP status for each refusal code a verb answers with;
// false for a code this API does not know.
func statusOf(code string) (int, bool) {
	switch code {
	case ticketapi.BadRequest:
		return http.StatusBadRequest, true
	case ticketapi.Unauthorized:
		return http.StatusUnauthorized, true
	case ticketapi.Forbidden, ticketapi.FactoryTicket, ticketapi.NotOnFactoryTicket:
		return http.StatusForbidden, true
	case ticketapi.NotFound:
		return http.StatusNotFound, true
	case ticketapi.Conflict:
		return http.StatusConflict, true
	case ticketapi.IdempotencyMismatch:
		return http.StatusUnprocessableEntity, true
	case ticketapi.TooMany:
		return http.StatusTooManyRequests, true
	case ticketapi.Busy:
		return http.StatusServiceUnavailable, true
	case ticketapi.StorageFull:
		return http.StatusInsufficientStorage, true
	}
	return 0, false
}

type queryKind int

const (
	qString queryKind = iota
	qInt
	qBool
	qTime
)

// route is one endpoint. The verb is fixed here, never taken from the
// request.
type route struct {
	verb    string
	status  int                  // on success
	query   map[string]queryKind // the query parameters it takes (GET only)
	rename  map[string]string    // query name -> request key
	body    bool                 // takes a JSON body
	created bool                 // 201 and Location only when the verb says it filed one
}

func routes() map[string]map[string]route {
	list := map[string]queryKind{"project": qString, "column": qString, "assignee": qString, "label": qString,
		"kind": qString, "q": qString, "updated_since": qTime, "include_closed": qBool, "limit": qInt, "cursor": qString,
		"format": qString}
	return map[string]map[string]route{
		"me":       {"GET": {verb: "whoami", status: 200}},
		"projects": {"GET": {verb: "projects", status: 200}},
		"work":     {"GET": {verb: "mine", status: 200, query: map[string]queryKind{"project": qString, "limit": qInt, "format": qString}}},
		"tickets": {"GET": {verb: "list", status: 200, query: list, rename: map[string]string{"q": "text"}},
			"POST": {verb: "create", status: 200, body: true, created: true}},
		"tickets/{id}": {"GET": {verb: "get", status: 200, query: map[string]queryKind{"format": qString}},
			"PATCH": {verb: "update", status: 200, body: true}},
		"tickets/{id}/notes":  {"POST": {verb: "note", status: 201, body: true}},
		"tickets/{id}/move":   {"POST": {verb: "move", status: 200, body: true}},
		"tickets/{id}/claim":  {"POST": {verb: "claim", status: 200, body: true}},
		"tickets/{id}/ask":    {"POST": {verb: "ask", status: 201, body: true}},
		"tickets/{id}/answer": {"POST": {verb: "answer", status: 200, body: true}},
		"tickets/{id}/split":  {"POST": {verb: "split", status: 201, body: true}},
		"tickets/{id}/send":   {"POST": {verb: "send", status: 201, body: true}},
	}
}

// match is the route pattern for a path under /api/v1/ and the ticket id in
// it ("" when none).
func (a *API) match(path string) (pattern, id string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, Prefix), "/")
	switch {
	case len(parts) == 1:
		_, ok = a.routes[parts[0]]
		return parts[0], "", ok
	case parts[0] == "tickets" && (len(parts) == 2 || len(parts) == 3):
		pattern = "tickets/{id}"
		if len(parts) == 3 {
			pattern += "/" + parts[2]
		}
		_, ok = a.routes[pattern]
		return pattern, parts[1], ok
	}
	return "", "", false
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := a.guard.Admit(w, r, r.Method != http.MethodGet)
	if name == "" {
		return
	}
	pattern, id, ok := a.match(r.URL.Path)
	if !ok {
		guard.WriteError(w, http.StatusNotFound, "not_found", "no such endpoint", "")
		return
	}
	methods := a.routes[pattern]
	rt, ok := methods[r.Method]
	if !ok {
		var allow []string
		for m := range methods {
			allow = append(allow, m)
		}
		sort.Strings(allow)
		w.Header().Set("Allow", strings.Join(allow, ", "))
		guard.WriteError(w, http.StatusMethodNotAllowed, "bad_request", "that method is not allowed here", "")
		return
	}
	if strings.Contains(pattern, "{id}") && !ticketID.MatchString(id) {
		guard.WriteError(w, http.StatusNotFound, "not_found", "no ticket "+id, "id")
		return
	}
	input, status, msg, field := request(r, rt, id)
	if status != 0 {
		code := "bad_request"
		if status == http.StatusRequestEntityTooLarge {
			code = "too_large"
		}
		guard.WriteError(w, status, code, msg, field)
		return
	}
	a.answer(w, r, rt, name, input)
}

// request is the verb's input, from the path id, the query and the body, as
// the JSON the CLI read on its stdin: the body's members (a body may not name
// the id), then the query's typed values, the Idempotency-Key and the path
// id, with the top-level keys sorted.
func request(r *http.Request, rt route, id string) (input []byte, status int, msg, field string) {
	in := map[string]json.RawMessage{}
	if rt.body {
		var b bytes.Buffer
		_, err := b.ReadFrom(r.Body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is at most %d KiB", guard.BodyLimit>>10), ""
		}
		if err != nil {
			return nil, http.StatusBadRequest, "the body could not be read", ""
		}
		if data := b.Bytes(); len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &in); err != nil || in == nil {
				return nil, http.StatusBadRequest, "the body is one JSON object", ""
			}
		}
		if _, ok := in["id"]; ok && id != "" {
			return nil, http.StatusBadRequest, "the ticket is named by the path, not the body", "id"
		}
	}
	for k, vs := range r.URL.Query() {
		kind, ok := rt.query[k]
		if !ok {
			return nil, http.StatusBadRequest, "unknown query parameter " + strconv.Quote(k), k
		}
		if len(vs) != 1 {
			return nil, http.StatusBadRequest, k + " is given once", k
		}
		v := vs[0]
		if v == "" {
			continue
		}
		key := k
		if n, ok := rt.rename[k]; ok {
			key = n
		}
		switch kind {
		case qInt:
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, http.StatusBadRequest, k + " must be an integer", k
			}
			in[key], _ = json.Marshal(n)
		case qBool:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, http.StatusBadRequest, k + " must be true or false", k
			}
			in[key], _ = json.Marshal(b)
		case qTime:
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return nil, http.StatusBadRequest, k + " must be an RFC 3339 time", k
			}
			in[key], _ = json.Marshal(v)
		default:
			in[key], _ = json.Marshal(v)
		}
	}
	if rt.verb == "create" {
		if keys := r.Header.Values("Idempotency-Key"); len(keys) > 1 {
			return nil, http.StatusBadRequest, "Idempotency-Key is given once", "idempotency_key"
		} else if len(keys) == 1 {
			in["idempotency_key"], _ = json.Marshal(keys[0])
		}
	}
	if id != "" {
		in["id"], _ = json.Marshal(id)
	}
	input, err := json.Marshal(in)
	if err != nil {
		return nil, http.StatusBadRequest, "the body is one JSON object", ""
	}
	return input, 0, "", ""
}

// answer runs the verb in a slot and relays what it says.
func (a *API) answer(w http.ResponseWriter, r *http.Request, rt route, name string, input []byte) {
	var out jsonx.Object
	req, err := ticketapi.Request(input, 0)
	if err == nil {
		err = a.guard.Run(r.Context(), name, func(ctx context.Context) error {
			var e error
			out, e = a.svc.Do(ctx, ticketapi.ForToken(name), rt.verb, req, a.wait)
			return e
		})
	}
	var refused *ticketapi.Refused
	switch {
	case errors.Is(err, guard.ErrBusy):
		w.Header().Set("Retry-After", "5")
		guard.WriteError(w, http.StatusServiceUnavailable, "busy", "the factory is busy; try again in a few seconds", "")
	case errors.As(err, &refused):
		status, known := statusOf(refused.Code)
		if !known {
			// drift between the verbs and this table shows as a failure
			a.logf("api %s as %s: a refusal this API does not know: %s %q", rt.verb, name, refused.Code, refused.Message)
			guard.Internal(w)
			return
		}
		switch refused.Code {
		case ticketapi.Busy:
			w.Header().Set("Retry-After", "30")
		case ticketapi.Unauthorized:
			// revoked or expired since the guard read the store
			w.Header().Set("WWW-Authenticate", `Bearer realm="factory"`)
		}
		guard.WriteError(w, status, refused.Code, refused.Message, refused.Field)
	case err != nil:
		a.logf("api %s as %s: %v", rt.verb, name, err)
		guard.Internal(w)
	default:
		raw := rawAnswer(out)
		status := rt.status
		if rt.created && string(raw["created"]) == "true" {
			status = http.StatusCreated
			var t struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw["ticket"], &t) == nil && ticketID.MatchString(t.ID) {
				w.Header().Set("Location", Prefix+"tickets/"+t.ID)
			}
		}
		guard.WriteJSON(w, status, raw)
	}
}

// rawAnswer is a verb's answer as Foreman read the CLI's JSON line: each member's
// value as the CLI printed it (json.dumps), by key. Encoding the map sorts
// the keys and compacts the values, as Foreman's relay did.
func rawAnswer(out jsonx.Object) map[string]json.RawMessage {
	raw := make(map[string]json.RawMessage, len(out))
	for _, m := range out {
		if m.Key == "ok" {
			continue
		}
		raw[m.Key] = jsonx.Format{Indent: -1}.Encode(m.Value)
	}
	return raw
}
