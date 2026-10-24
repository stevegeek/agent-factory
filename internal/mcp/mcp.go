// Package mcp is MCP at /mcp: streamable HTTP, one JSON-RPC message per
// POST, answered with JSON (no event stream, no sessions). The same tokens,
// guard and slots as /api/v1: every tool is one ticketapi verb, run
// in-process, and the verb decides what the caller may do. This package
// only translates.
//
// It is Foreman's mcp.go with the `factory ticket` subprocess replaced by
// ticketapi.Service.Do; the request a verb sees is built exactly as the
// CLI's stdin was. serverInfo.version is buildinfo.Version, set by the
// linker, never debug.BuildInfo (builds use -buildvcs=false).
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/guard"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

// Path is where MCP is mounted.
const Path = "/mcp"

// DefaultWait is how long a verb waits for a ticket the tick holds.
const DefaultWait = 10 * time.Second

// versions are the protocol revisions this server speaks, newest first.
func versions() []string { return []string{"2025-06-18", "2025-03-26", "2024-11-05"} }

// noHeader is what a request after initialize without MCP-Protocol-Version
// speaks, as the spec says.
const noHeader = "2025-03-26"

const instructions = "Start with my_work: answers to your questions come first, then questions put to you, then your tickets. " +
	"The tickets skill has the workflow: start_work before working a ticket, post_update at milestones, ask_person instead of guessing, finish_work with a result a person can check."

// JSON-RPC and MCP error codes.
const (
	rpcParse          = -32700
	rpcInvalid        = -32600
	rpcNoMethod       = -32601
	rpcBadParams      = -32602
	rpcInternal       = -32603
	rpcNoSuchResource = -32002
)

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

// Options configures the server.
type Options struct {
	Guard   *guard.Guard // shared with /api/v1
	Service Service
	// Version is serverInfo.version; "" is buildinfo.Version.
	Version string
	Wait    time.Duration // a verb's wait for a ticket lock (DefaultWait)
	Log     Logger        // nil: silent
}

// Server serves /mcp.
type Server struct {
	guard   *guard.Guard
	svc     Service
	version string
	wait    time.Duration
	log     Logger
	tools   []*tool
	byName  map[string]*tool
}

// New builds the server.
func New(o Options) *Server {
	s := &Server{guard: o.Guard, svc: o.Service, version: o.Version, wait: o.Wait, log: o.Log, tools: tools(),
		byName: map[string]*tool{}}
	if s.version == "" {
		s.version = buildinfo.Version
	}
	if s.wait <= 0 {
		s.wait = DefaultWait
	}
	for _, t := range s.tools {
		s.byName[t.name] = t
	}
	return s
}

func (s *Server) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
	// answered as this HTTP status and API error instead of a JSON-RPC
	// answer (auth and rate, as for /api/v1)
	status  int
	apiCode string
	header  map[string]string
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func rpcNull() json.RawMessage { return json.RawMessage("null") }

func rpcFail(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	guard.WriteJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := s.guard.Admit(w, r, false)
	if name == "" {
		return
	}
	if r.Method != http.MethodPost {
		// no server-initiated stream (GET) and no sessions to end (DELETE)
		w.Header().Set("Allow", http.MethodPost)
		guard.WriteError(w, http.StatusMethodNotAllowed, "bad_request", "MCP here is POST only: one JSON-RPC message, answered with JSON", "")
		return
	}
	if !acceptsJSON(r.Header.Values("Accept")) {
		guard.WriteError(w, http.StatusNotAcceptable, "bad_request", "this server answers with application/json only", "")
		return
	}
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		guard.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the body is at most %d KiB", guard.BodyLimit>>10), "")
		return
	}
	if err != nil {
		guard.WriteError(w, http.StatusBadRequest, "bad_request", "the body could not be read", "")
		return
	}
	data := bytes.TrimSpace(b.Bytes())
	if len(data) > 0 && data[0] == '[' {
		rpcFail(w, http.StatusBadRequest, rpcNull(), rpcInvalid, "batches are not supported: send one message per request")
		return
	}
	if !json.Valid(data) {
		rpcFail(w, http.StatusBadRequest, rpcNull(), rpcParse, "the body is not JSON")
		return
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(data, &msg) != nil || msg == nil {
		rpcFail(w, http.StatusBadRequest, rpcNull(), rpcInvalid, "the body is one JSON-RPC message, an object")
		return
	}
	var jsonrpc, method string
	if json.Unmarshal(msg["jsonrpc"], &jsonrpc) != nil || jsonrpc != "2.0" {
		rpcFail(w, http.StatusBadRequest, rpcNull(), rpcInvalid, `jsonrpc must be "2.0"`)
		return
	}
	id, hasID := msg["id"]
	_, hasMethod := msg["method"]
	if !hasMethod {
		_, res := msg["result"]
		_, rerr := msg["error"]
		if hasID && (res || rerr) {
			// a client's answer to a request this server never sends:
			// accepted, nothing to do
			w.WriteHeader(http.StatusAccepted)
			return
		}
		rpcFail(w, http.StatusBadRequest, rpcNull(), rpcInvalid, "a message needs a method")
		return
	}
	if json.Unmarshal(msg["method"], &method) != nil || method == "" {
		rpcFail(w, http.StatusBadRequest, rpcNull(), rpcInvalid, "method is a string")
		return
	}
	if !hasID {
		// notifications/initialized, notifications/cancelled and the rest:
		// nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !rpcIDOK(id) {
		rpcFail(w, http.StatusBadRequest, rpcNull(), rpcInvalid, "id is a string or a number")
		return
	}
	params := msg["params"]
	if len(params) > 0 && string(params) != "null" && params[0] != '{' {
		rpcFail(w, http.StatusOK, id, rpcBadParams, "params is an object")
		return
	}
	version := noHeader
	if method != "initialize" {
		if hs := r.Header.Values("MCP-Protocol-Version"); len(hs) > 0 {
			if len(hs) > 1 || !slices.Contains(versions(), hs[0]) {
				rpcFail(w, http.StatusBadRequest, id, rpcInvalid, "unsupported MCP-Protocol-Version; this server speaks "+strings.Join(versions(), ", "))
				return
			}
			version = hs[0]
		}
	}
	result, rerr := s.dispatch(r.Context(), name, version, method, params)
	if rerr != nil && rerr.status != 0 {
		for k, v := range rerr.header {
			w.Header().Set(k, v)
		}
		guard.WriteError(w, rerr.status, rerr.apiCode, rerr.Message, "")
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Error: rerr}
	if rerr == nil {
		if resp.Result, err = json.Marshal(result); err != nil {
			resp.Result, resp.Error = nil, &rpcError{Code: rpcInternal, Message: "internal error"}
		}
	}
	guard.WriteJSON(w, http.StatusOK, resp)
}

// acceptsJSON: no Accept, or one that takes application/json. A client that
// takes only an event stream can't read this server's answers.
func acceptsJSON(values []string) bool {
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			t, _, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err == nil && (t == "application/json" || t == "application/*" || t == "*/*") {
				return true
			}
		}
	}
	return false
}

// rpcIDOK: MCP's ids are strings or integers, never null.
func rpcIDOK(id json.RawMessage) bool {
	if len(id) > 0 && id[0] == '"' {
		var s string
		return json.Unmarshal(id, &s) == nil
	}
	if string(id) == "null" {
		return false
	}
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(id))
	d.UseNumber()
	return d.Decode(&n) == nil
}

func (s *Server) dispatch(ctx context.Context, name, version, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		v := p.ProtocolVersion
		if !slices.Contains(versions(), v) {
			v = versions()[0] // the client decides whether it can speak this
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities": map[string]any{"tools": map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false, "subscribe": false}},
			"serverInfo":   map[string]any{"name": "factory-tickets", "version": s.version},
			"instructions": instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.toolsList(ctx, name, version)
	case "tools/call":
		return s.toolsCall(ctx, name, params)
	case "resources/list":
		return s.resourcesList(ctx, name)
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []map[string]any{{"uriTemplate": "ticket://{id}", "name": "ticket",
			"description": "A ticket as Markdown: the same text as get_ticket", "mimeType": "text/markdown"}}}, nil
	case "resources/read":
		return s.resourcesRead(ctx, name, params)
	}
	return nil, &rpcError{Code: rpcNoMethod, Message: "method not found: " + clip(method, 80)}
}

// refusal is a verb's refusal, as the verb words it.
type refusal struct {
	Code, Message, Field string
}

// known are the refusal codes the verbs answer with. Any other is a failure
// of the factory's own, so drift between the two shows.
func known(code string) bool {
	switch code {
	case ticketapi.BadRequest, ticketapi.Unauthorized, ticketapi.Forbidden, ticketapi.FactoryTicket,
		ticketapi.NotOnFactoryTicket, ticketapi.NotFound, ticketapi.Conflict, ticketapi.IdempotencyMismatch,
		ticketapi.TooMany, ticketapi.Busy, ticketapi.StorageFull:
		return true
	}
	return false
}

// call runs a verb for the caller in a slot: its answer, its refusal, or the
// JSON-RPC error when there is neither. The request is in as the CLI read
// it on stdin (json.Marshal of in: keys sorted at every depth).
func (s *Server) call(ctx context.Context, verb, name string, in map[string]any) (map[string]json.RawMessage, *refusal, *rpcError) {
	input, err := json.Marshal(in)
	if err != nil {
		s.logf("mcp %s as %s: %v", verb, name, err)
		return nil, nil, &rpcError{Code: rpcInternal, Message: "internal error"}
	}
	var out jsonx.Object
	req, err := ticketapi.Request(input, 0)
	if err == nil {
		err = s.guard.Run(ctx, name, func(ctx context.Context) error {
			var e error
			out, e = s.svc.Do(ctx, ticketapi.ForToken(name), verb, req, s.wait)
			return e
		})
	}
	var refused *ticketapi.Refused
	switch {
	case errors.Is(err, guard.ErrBusy):
		return nil, &refusal{Code: ticketapi.Busy, Message: "the factory is busy"}, nil
	case errors.As(err, &refused):
		if !known(refused.Code) {
			s.logf("mcp %s as %s: a refusal this server does not know: %s %q", verb, name, refused.Code, refused.Message)
			return nil, nil, &rpcError{Code: rpcInternal, Message: "internal error"}
		}
		if refused.Code == ticketapi.Unauthorized {
			// revoked or expired since the guard read the store
			return nil, nil, &rpcError{Message: "a valid bearer token is needed", status: http.StatusUnauthorized,
				apiCode: "unauthorized", header: map[string]string{"WWW-Authenticate": `Bearer realm="factory"`}}
		}
		return nil, &refusal{Code: refused.Code, Message: refused.Message, Field: refused.Field}, nil
	case err != nil:
		s.logf("mcp %s as %s: %v", verb, name, err)
		return nil, nil, &rpcError{Code: rpcInternal, Message: "internal error"}
	}
	return rawAnswer(out), nil, nil
}

// rawAnswer is a verb's answer as Foreman read the CLI's JSON line: each
// member's value as the CLI printed it, by key.
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

// whoami is the token's projects and capabilities.
func (s *Server) whoami(ctx context.Context, name string) (projects, can []string, rerr *rpcError) {
	out, ref, rerr := s.call(ctx, "whoami", name, map[string]any{})
	if rerr != nil {
		return nil, nil, rerr
	}
	if ref != nil {
		return nil, nil, &rpcError{Code: rpcInternal, Message: ref.Message}
	}
	json.Unmarshal(out["projects"], &projects)
	json.Unmarshal(out["can"], &can)
	return projects, can, nil
}

func (s *Server) toolsList(ctx context.Context, name, version string) (any, *rpcError) {
	_, can, rerr := s.whoami(ctx, name)
	if rerr != nil {
		return nil, rerr
	}
	list := []map[string]any{}
	for _, t := range s.tools {
		if t.allowed(can) {
			list = append(list, t.describe(version))
		}
	}
	return map[string]any{"tools": list}, nil
}

func (s *Server) toolsCall(ctx context.Context, name string, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, &rpcError{Code: rpcBadParams, Message: "params are {name, arguments}"}
	}
	tl := s.byName[p.Name]
	if tl == nil {
		return nil, &rpcError{Code: rpcBadParams, Message: "unknown tool: " + clip(p.Name, 80)}
	}
	args := map[string]any{}
	if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
		d := json.NewDecoder(bytes.NewReader(p.Arguments))
		d.UseNumber()
		var v any
		if d.Decode(&v) != nil {
			return nil, &rpcError{Code: rpcBadParams, Message: "arguments is an object"}
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, &rpcError{Code: rpcBadParams, Message: "arguments is an object"}
		}
		args = obj
	}
	if msg := tl.schema.check(args, ""); msg != "" {
		return nil, &rpcError{Code: rpcBadParams, Message: tl.name + ": " + msg}
	}
	in, msg := tl.input(args)
	if msg != "" {
		return nil, &rpcError{Code: rpcBadParams, Message: tl.name + ": " + msg}
	}
	if tl.write {
		if wait := s.guard.TakeWrite(name); wait > 0 {
			return nil, &rpcError{Message: "this token is writing too often; slow down", status: http.StatusTooManyRequests,
				apiCode: "too_many", header: map[string]string{"Retry-After": strconv.Itoa(int(math.Ceil(wait.Seconds())))}}
		}
	}
	if tl.md {
		in["format"] = "md"
	}
	out, ref, rerr := s.call(ctx, tl.verb, name, in)
	if rerr != nil {
		return nil, rerr
	}
	if ref != nil {
		return toolResult(notDone(tl, ref), true), nil
	}
	return toolResult(tl.reply(out), false), nil
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// notDone is a refusal as the agent reads it: what the verb said, and what
// to do next.
func notDone(tl *tool, r *refusal) string {
	text := "**Not done:** " + strings.TrimSpace(r.Message)
	if field := r.Field; field != "" {
		if arg, ok := tl.args[field]; ok {
			field = arg
		}
		text += " (" + field + ")"
	}
	hint := tl.hints[r.Code]
	if hint == "" {
		hint = refusalHint(r.Code)
	}
	if hint != "" {
		text += "\n" + hint
	}
	return text + "\n"
}

func refusalHint(code string) string {
	switch code {
	case ticketapi.BadRequest:
		return "Fix that argument and call again."
	case ticketapi.Forbidden:
		return "This token may not do that. Don't look for another way; ask the maintainer if it should."
	case ticketapi.FactoryTicket:
		return "Only the factory's pipeline moves a factory ticket. post_update to comment on it instead."
	case ticketapi.NotOnFactoryTicket:
		return "A factory ticket takes no questions. post_update to comment on it instead."
	case ticketapi.NotFound:
		return "Check the id with my_work or search_tickets."
	case ticketapi.Conflict:
		return "get_ticket to see where it stands now."
	case ticketapi.IdempotencyMismatch:
		return "That key filed a different ticket. Use a new key for new work."
	case ticketapi.TooMany:
		return "A limit is reached. Wait for an answer, or finish what is open."
	case ticketapi.Busy:
		return "Try again in a minute."
	case ticketapi.StorageFull:
		return "The ticket store is full. Tell the maintainer, and stop filing."
	}
	return ""
}

// ---------------------------------------------------------------- resources

// re: go
var projectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s *Server) resourcesList(ctx context.Context, name string) (any, *rpcError) {
	projects, _, rerr := s.whoami(ctx, name)
	if rerr != nil {
		return nil, rerr
	}
	res := []map[string]any{}
	for _, p := range projects {
		res = append(res, map[string]any{"uri": "project://" + p, "name": p, "description": "Open tickets on " + p,
			"mimeType": "text/markdown"})
	}
	out, ref, rerr := s.call(ctx, "list", name, map[string]any{"assignee": "me", "limit": 50})
	if rerr != nil {
		return nil, rerr
	}
	if ref != nil {
		return nil, &rpcError{Code: rpcInternal, Message: ref.Message}
	}
	var tickets []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	json.Unmarshal(out["tickets"], &tickets)
	for _, t := range tickets {
		res = append(res, map[string]any{"uri": "ticket://" + t.ID, "name": t.ID, "description": strings.Join(strings.Fields(t.Title), " "),
			"mimeType": "text/markdown"})
	}
	return map[string]any{"resources": res}, nil
}

func (s *Server) resourcesRead(ctx context.Context, name string, params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(params, &p) != nil || p.URI == "" {
		return nil, &rpcError{Code: rpcBadParams, Message: "params are {uri}"}
	}
	missing := &rpcError{Code: rpcNoSuchResource, Message: "resource not found", Data: map[string]string{"uri": clip(p.URI, 200)}}
	var verb string
	var in map[string]any
	switch scheme, rest, _ := strings.Cut(p.URI, "://"); scheme {
	case "ticket":
		if !ticketID.MatchString(rest) {
			return nil, missing
		}
		verb, in = "get", map[string]any{"id": rest, "format": "md"}
	case "project":
		if !projectName.MatchString(rest) {
			return nil, missing
		}
		projects, _, rerr := s.whoami(ctx, name)
		if rerr != nil {
			return nil, rerr
		}
		if !slices.Contains(projects, rest) {
			return nil, missing
		}
		verb, in = "list", map[string]any{"project": rest, "format": "md"}
	default:
		return nil, missing
	}
	out, ref, rerr := s.call(ctx, verb, name, in)
	if rerr != nil {
		return nil, rerr
	}
	if ref != nil {
		if ref.Code == ticketapi.NotFound {
			return nil, missing
		}
		return nil, &rpcError{Code: rpcInternal, Message: ref.Message}
	}
	var text string
	json.Unmarshal(out["markdown"], &text)
	return map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": "text/markdown", "text": text}}}, nil
}

// clip is at most n characters of s, trimmed, with an ellipsis when cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
