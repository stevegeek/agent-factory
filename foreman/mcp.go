package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MCP at /mcp: streamable HTTP, one JSON-RPC message per POST, answered with JSON (no event stream, no
// sessions). The same tokens, guard and CLI as /api/v1: every tool is one `factory ticket` verb, and the
// CLI decides what the caller may do.

const mcpPath = "/mcp"

// mcpVersions: the protocol revisions this server speaks, newest first.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// mcpNoHeader: what a request after initialize without MCP-Protocol-Version speaks, as the spec says.
const mcpNoHeader = "2025-03-26"

const mcpInstructions = "Start with my_work: answers to your questions come first, then questions put to you, then your tickets. " +
	"The tickets skill has the workflow: start_work before working a ticket, post_update at milestones, ask_person instead of guessing, finish_work with a result a person can check."

const (
	rpcParse          = -32700
	rpcInvalid        = -32600
	rpcNoMethod       = -32601
	rpcBadParams      = -32602
	rpcInternal       = -32603
	rpcNoSuchResource = -32002
)

type MCP struct {
	guard   *Guard
	runner  *APIRunner
	version string
}

func NewMCP(guard *Guard, runner *APIRunner, version string) *MCP {
	return &MCP{guard: guard, runner: runner, version: version}
}

// buildVersion: the module's version, or the commit it was built from.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			return s.Value[:12]
		}
	}
	return "dev"
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
	// answered as this HTTP status and API error instead of a JSON-RPC answer (auth and rate, as for /api/v1)
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

var rpcNull = json.RawMessage("null")

func rpcFail(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	apiJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (m *MCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := m.guard.Admit(w, r, false)
	if name == "" {
		return
	}
	if r.Method != http.MethodPost {
		// no server-initiated stream (GET) and no sessions to end (DELETE)
		w.Header().Set("Allow", http.MethodPost)
		apiError(w, http.StatusMethodNotAllowed, "bad_request", "MCP here is POST only: one JSON-RPC message, answered with JSON", "")
		return
	}
	if !acceptsJSON(r.Header.Values("Accept")) {
		apiError(w, http.StatusNotAcceptable, "bad_request", "this server answers with application/json only", "")
		return
	}
	data, err := readAll(r)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		apiError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the body is at most %d KiB", apiBody>>10), "")
		return
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "the body could not be read", "")
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		rpcFail(w, http.StatusBadRequest, rpcNull, rpcInvalid, "batches are not supported: send one message per request")
		return
	}
	if !json.Valid(data) {
		rpcFail(w, http.StatusBadRequest, rpcNull, rpcParse, "the body is not JSON")
		return
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(data, &msg) != nil || msg == nil {
		rpcFail(w, http.StatusBadRequest, rpcNull, rpcInvalid, "the body is one JSON-RPC message, an object")
		return
	}
	var jsonrpc, method string
	if json.Unmarshal(msg["jsonrpc"], &jsonrpc) != nil || jsonrpc != "2.0" {
		rpcFail(w, http.StatusBadRequest, rpcNull, rpcInvalid, `jsonrpc must be "2.0"`)
		return
	}
	id, hasID := msg["id"]
	_, hasMethod := msg["method"]
	if !hasMethod {
		_, res := msg["result"]
		_, rerr := msg["error"]
		if hasID && (res || rerr) {
			// a client's answer to a request this server never sends: accepted, nothing to do
			w.WriteHeader(http.StatusAccepted)
			return
		}
		rpcFail(w, http.StatusBadRequest, rpcNull, rpcInvalid, "a message needs a method")
		return
	}
	if json.Unmarshal(msg["method"], &method) != nil || method == "" {
		rpcFail(w, http.StatusBadRequest, rpcNull, rpcInvalid, "method is a string")
		return
	}
	if !hasID {
		// notifications/initialized, notifications/cancelled and the rest: nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !rpcIDOK(id) {
		rpcFail(w, http.StatusBadRequest, rpcNull, rpcInvalid, "id is a string or a number")
		return
	}
	params := msg["params"]
	if len(params) > 0 && string(params) != "null" && params[0] != '{' {
		rpcFail(w, http.StatusOK, id, rpcBadParams, "params is an object")
		return
	}
	version := mcpNoHeader
	if method != "initialize" {
		if hs := r.Header.Values("MCP-Protocol-Version"); len(hs) > 0 {
			if len(hs) > 1 || !slices.Contains(mcpVersions, hs[0]) {
				rpcFail(w, http.StatusBadRequest, id, rpcInvalid, "unsupported MCP-Protocol-Version; this server speaks "+strings.Join(mcpVersions, ", "))
				return
			}
			version = hs[0]
		}
	}
	result, rerr := m.dispatch(name, version, method, params)
	if rerr != nil && rerr.status != 0 {
		for k, v := range rerr.header {
			w.Header().Set(k, v)
		}
		apiError(w, rerr.status, rerr.apiCode, rerr.Message, "")
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Error: rerr}
	if rerr == nil {
		if resp.Result, err = json.Marshal(result); err != nil {
			resp.Result, resp.Error = nil, &rpcError{Code: rpcInternal, Message: "internal error"}
		}
	}
	apiJSON(w, http.StatusOK, resp)
}

// acceptsJSON: no Accept, or one that takes application/json. A client that takes only an event stream
// can't read this server's answers.
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

func (m *MCP) dispatch(name, version, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		v := p.ProtocolVersion
		if !slices.Contains(mcpVersions, v) {
			v = mcpVersions[0] // the client decides whether it can speak this
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities": map[string]any{"tools": map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false, "subscribe": false}},
			"serverInfo":   map[string]any{"name": "factory-tickets", "version": m.version},
			"instructions": mcpInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return m.toolsList(name, version)
	case "tools/call":
		return m.toolsCall(name, params)
	case "resources/list":
		return m.resourcesList(name)
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []map[string]any{{"uriTemplate": "ticket://{id}", "name": "ticket",
			"description": "A ticket as Markdown: the same text as get_ticket", "mimeType": "text/markdown"}}}, nil
	case "resources/read":
		return m.resourcesRead(name, params)
	}
	return nil, &rpcError{Code: rpcNoMethod, Message: "method not found: " + clip(method, 80)}
}

// call runs a verb for the caller: its answer, its refusal, or the JSON-RPC error when there is neither.
func (m *MCP) call(verb, name string, in map[string]any) (map[string]json.RawMessage, *cliRefusal, *rpcError) {
	input, _ := json.Marshal(in)
	out, refusal, err := m.runner.Call(verb, name, input)
	switch {
	case errors.Is(err, errAPIBusy):
		return nil, &cliRefusal{Code: "busy", Message: "the factory is busy"}, nil
	case err != nil:
		return nil, nil, &rpcError{Code: rpcInternal, Message: "internal error"}
	case refusal != nil && refusal.Code == "unauthorized":
		// revoked or expired since Foreman last read the store
		return nil, nil, &rpcError{Message: "a valid bearer token is needed", status: http.StatusUnauthorized,
			apiCode: "unauthorized", header: map[string]string{"WWW-Authenticate": `Bearer realm="factory"`}}
	}
	return out, refusal, nil
}

// whoami: the token's projects and capabilities.
func (m *MCP) whoami(name string) (projects, can []string, rerr *rpcError) {
	out, refusal, rerr := m.call("whoami", name, map[string]any{})
	if rerr != nil {
		return nil, nil, rerr
	}
	if refusal != nil {
		return nil, nil, &rpcError{Code: rpcInternal, Message: refusal.Message}
	}
	json.Unmarshal(out["projects"], &projects)
	json.Unmarshal(out["can"], &can)
	return projects, can, nil
}

func (m *MCP) toolsList(name, version string) (any, *rpcError) {
	_, can, rerr := m.whoami(name)
	if rerr != nil {
		return nil, rerr
	}
	tools := []map[string]any{}
	for _, t := range mcpTools {
		if t.allowed(can) {
			tools = append(tools, t.describe(version))
		}
	}
	return map[string]any{"tools": tools}, nil
}

func (m *MCP) toolsCall(name string, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, &rpcError{Code: rpcBadParams, Message: "params are {name, arguments}"}
	}
	tool := mcpToolByName[p.Name]
	if tool == nil {
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
	if msg := tool.schema.check(args, ""); msg != "" {
		return nil, &rpcError{Code: rpcBadParams, Message: tool.name + ": " + msg}
	}
	in, msg := tool.input(args)
	if msg != "" {
		return nil, &rpcError{Code: rpcBadParams, Message: tool.name + ": " + msg}
	}
	if tool.write {
		if wait := m.guard.takeWrite(name); wait > 0 {
			return nil, &rpcError{Message: "this token is writing too often; slow down", status: http.StatusTooManyRequests,
				apiCode: "too_many", header: map[string]string{"Retry-After": strconv.Itoa(int(math.Ceil(wait.Seconds())))}}
		}
	}
	if tool.md {
		in["format"] = "md"
	}
	out, refusal, rerr := m.call(tool.verb, name, in)
	if rerr != nil {
		return nil, rerr
	}
	if refusal != nil {
		return toolResult(notDone(tool, refusal), true), nil
	}
	return toolResult(tool.reply(out), false), nil
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// notDone: a refusal as the agent reads it: what the CLI said, and what to do next.
func notDone(tool *mcpTool, r *cliRefusal) string {
	text := "**Not done:** " + strings.TrimSpace(r.Message)
	if r.Field != "" {
		text += " (" + r.Field + ")"
	}
	hint := tool.hints[r.Code]
	if hint == "" {
		hint = refusalHints[r.Code]
	}
	if hint != "" {
		text += "\n" + hint
	}
	return text + "\n"
}

var refusalHints = map[string]string{
	"bad_request":           "Fix that argument and call again.",
	"forbidden":             "This token may not do that. Don't look for another way; ask the maintainer if it should.",
	"factory_ticket":        "Only the factory's pipeline moves a factory ticket. post_update to comment on it instead.",
	"not_on_factory_ticket": "A factory ticket takes no questions. post_update to comment on it instead.",
	"not_found":             "Check the id with my_work or search_tickets.",
	"conflict":              "get_ticket to see where it stands now.",
	"idempotency_mismatch":  "That key filed a different ticket. Use a new key for new work.",
	"too_many":              "A limit is reached. Wait for an answer, or finish what is open.",
	"busy":                  "Try again in a minute.",
	"storage_full":          "The ticket store is full. Tell the maintainer, and stop filing.",
}

// ---------------------------------------------------------------- resources

var mcpProjectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (m *MCP) resourcesList(name string) (any, *rpcError) {
	projects, _, rerr := m.whoami(name)
	if rerr != nil {
		return nil, rerr
	}
	res := []map[string]any{}
	for _, p := range projects {
		res = append(res, map[string]any{"uri": "project://" + p, "name": p, "description": "Open tickets on " + p,
			"mimeType": "text/markdown"})
	}
	out, refusal, rerr := m.call("list", name, map[string]any{"assignee": "me", "limit": 50})
	if rerr != nil {
		return nil, rerr
	}
	if refusal != nil {
		return nil, &rpcError{Code: rpcInternal, Message: refusal.Message}
	}
	var tickets []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	json.Unmarshal(out["tickets"], &tickets)
	for _, t := range tickets {
		res = append(res, map[string]any{"uri": "ticket://" + t.ID, "name": t.ID, "description": t.Title,
			"mimeType": "text/markdown"})
	}
	return map[string]any{"resources": res}, nil
}

func (m *MCP) resourcesRead(name string, params json.RawMessage) (any, *rpcError) {
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
		if !apiTicketID.MatchString(rest) {
			return nil, missing
		}
		verb, in = "get", map[string]any{"id": rest, "format": "md"}
	case "project":
		if !mcpProjectName.MatchString(rest) {
			return nil, missing
		}
		projects, _, rerr := m.whoami(name)
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
	out, refusal, rerr := m.call(verb, name, in)
	if rerr != nil {
		return nil, rerr
	}
	if refusal != nil {
		if refusal.Code == "not_found" {
			return nil, missing
		}
		return nil, &rpcError{Code: rpcInternal, Message: refusal.Message}
	}
	var text string
	json.Unmarshal(out["markdown"], &text)
	return map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": "text/markdown", "text": text}}}, nil
}

// ---------------------------------------------------------------- argument schemas

// jschema: a tool's input schema, both what tools/list shows and what Go checks before the CLI sees a call,
// so the two can't drift. Only what the tools use.
type jschema struct {
	typ        string // object, string, integer, boolean, array
	nullable   bool
	desc       string
	props      map[string]*jschema
	required   []string
	maxLen     int
	pattern    *regexp.Regexp
	format     string // date-time
	enum       []string
	min, max   *int
	items      *jschema
	minN, maxN int // array length; maxN 0: no limit
}

func (s *jschema) JSON() map[string]any {
	out := map[string]any{"type": s.typ}
	if s.nullable {
		out["type"] = []string{s.typ, "null"}
	}
	if s.desc != "" {
		out["description"] = s.desc
	}
	switch s.typ {
	case "object":
		props := map[string]any{}
		for k, p := range s.props {
			props[k] = p.JSON()
		}
		out["properties"] = props
		out["additionalProperties"] = false
		if len(s.required) > 0 {
			out["required"] = s.required
		}
	case "string":
		if s.maxLen > 0 {
			out["maxLength"] = s.maxLen
		}
		if s.pattern != nil {
			out["pattern"] = s.pattern.String()
		}
		if s.format != "" {
			out["format"] = s.format
		}
		if s.enum != nil {
			out["enum"] = s.enum
		}
	case "integer":
		if s.min != nil {
			out["minimum"] = *s.min
		}
		if s.max != nil {
			out["maximum"] = *s.max
		}
	case "array":
		out["items"] = s.items.JSON()
		if s.minN > 0 {
			out["minItems"] = s.minN
		}
		if s.maxN > 0 {
			out["maxItems"] = s.maxN
		}
	}
	return out
}

// check: "" when v fits the schema, else what is wrong with it (at path).
func (s *jschema) check(v any, path string) string {
	at := func(msg string) string {
		if path == "" {
			return msg
		}
		return path + " " + msg
	}
	if v == nil {
		if s.nullable {
			return ""
		}
		return at("may not be null")
	}
	switch s.typ {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return at("must be an object")
		}
		for _, k := range sortedKeys(obj) {
			p, known := s.props[k]
			if !known {
				return "unknown argument " + strconv.Quote(clip(k, 40)) + atPath(path)
			}
			if msg := p.check(obj[k], subPath(path, k)); msg != "" {
				return msg
			}
		}
		for _, k := range s.required {
			if _, ok := obj[k]; !ok {
				return subPath(path, k) + " is needed"
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return at("must be a string")
		}
		if s.maxLen > 0 && utf8.RuneCountInString(str) > s.maxLen {
			return at(fmt.Sprintf("is at most %d characters", s.maxLen))
		}
		if s.pattern != nil && !s.pattern.MatchString(str) {
			return at("must match " + s.pattern.String())
		}
		if s.enum != nil && !slices.Contains(s.enum, str) {
			return at("is one of " + strings.Join(s.enum, ", "))
		}
		if s.format == "date-time" {
			if _, err := time.Parse(time.RFC3339, str); err != nil {
				return at("must be an RFC 3339 date-time")
			}
		}
	case "integer":
		n, ok := v.(json.Number)
		i, err := strconv.ParseInt(string(n), 10, 64)
		if !ok || err != nil {
			return at("must be an integer")
		}
		if (s.min != nil && i < int64(*s.min)) || (s.max != nil && i > int64(*s.max)) {
			return at(fmt.Sprintf("is %s", rangeWords(s.min, s.max)))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return at("must be true or false")
		}
	case "array":
		xs, ok := v.([]any)
		if !ok {
			return at("must be an array")
		}
		if len(xs) < s.minN || (s.maxN > 0 && len(xs) > s.maxN) {
			if s.maxN > 0 {
				return at(fmt.Sprintf("holds %d to %d items", s.minN, s.maxN))
			}
			return at(fmt.Sprintf("holds at least %d items", s.minN))
		}
		for i, x := range xs {
			if msg := s.items.check(x, fmt.Sprintf("%s[%d]", path, i)); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func atPath(path string) string {
	if path == "" {
		return ""
	}
	return " in " + path
}

func subPath(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func rangeWords(lo, hi *int) string {
	switch {
	case lo != nil && hi != nil:
		return fmt.Sprintf("%d to %d", *lo, *hi)
	case lo != nil:
		return fmt.Sprintf("at least %d", *lo)
	}
	return fmt.Sprintf("at most %d", *hi)
}

func intp(n int) *int { return &n }

func sStr(max int, desc string) *jschema { return &jschema{typ: "string", maxLen: max, desc: desc} }
func sInt(lo, hi int, desc string) *jschema {
	return &jschema{typ: "integer", min: intp(lo), max: intp(hi), desc: desc}
}
func sBool(desc string) *jschema { return &jschema{typ: "boolean", desc: desc} }
func sEnum(desc string, vals ...string) *jschema {
	return &jschema{typ: "string", enum: vals, desc: desc}
}
func sArr(items *jschema, lo, hi int, desc string) *jschema {
	return &jschema{typ: "array", items: items, minN: lo, maxN: hi, desc: desc}
}
func sObj(props map[string]*jschema, required ...string) *jschema {
	return &jschema{typ: "object", props: props, required: required}
}

var (
	reTicketID = regexp.MustCompile(`^T[0-9]{4,6}$`)
	reLabel    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	reIdemKey  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

func sID(desc string) *jschema {
	return &jschema{typ: "string", pattern: reTicketID, desc: desc}
}
func sProject(desc string) *jschema {
	return &jschema{typ: "string", maxLen: 64, pattern: mcpProjectName, desc: desc}
}
func sLabels(desc string) *jschema {
	return sArr(&jschema{typ: "string", pattern: reLabel}, 0, 20, desc)
}

// ---------------------------------------------------------------- the tools

// mcpTool: one tool over one CLI verb.
type mcpTool struct {
	name, title, desc string
	can               []string // every one needed for the tool to be listed
	write, idempotent bool
	schema            *jschema
	verb              string
	md                bool // ask the CLI for Markdown
	// input: the verb's request from checked arguments; a message when they don't fit together
	input func(args map[string]any) (map[string]any, string)
	reply func(out map[string]json.RawMessage) string
	hints map[string]string // by refusal code, over refusalHints
}

func (t *mcpTool) allowed(can []string) bool {
	for _, c := range t.can {
		if !slices.Contains(can, c) {
			return false
		}
	}
	return true
}

func (t *mcpTool) describe(version string) map[string]any {
	out := map[string]any{"name": t.name, "description": t.desc, "inputSchema": t.schema.JSON()}
	if version == "2024-11-05" {
		return out // annotations came in 2025-03-26
	}
	if version != "2025-03-26" {
		out["title"] = t.title
	}
	ann := map[string]any{"title": t.title, "readOnlyHint": !t.write, "openWorldHint": false}
	if t.write {
		ann["destructiveHint"] = false
		ann["idempotentHint"] = t.idempotent
	}
	out["annotations"] = ann
	return out
}

// copyArgs: the arguments as they are, renamed where the verb calls them something else.
func copyArgs(rename map[string]string) func(map[string]any) (map[string]any, string) {
	return func(args map[string]any) (map[string]any, string) {
		in := map[string]any{}
		for k, v := range args {
			if n, ok := rename[k]; ok {
				k = n
			}
			in[k] = v
		}
		return in, ""
	}
}

func outString(out map[string]json.RawMessage, key string) string {
	var s string
	json.Unmarshal(out[key], &s)
	return s
}

type slimTicket struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Column      string `json:"column"`
	ColumnLabel string `json:"column_label"`
}

func outTicket(out map[string]json.RawMessage, key string) slimTicket {
	var t slimTicket
	json.Unmarshal(out[key], &t)
	return t
}

func mdOf(out map[string]json.RawMessage) string { return outString(out, "markdown") }

// headed: a line of what happened, then the CLI's Markdown.
func headed(head func(out map[string]json.RawMessage) string) func(map[string]json.RawMessage) string {
	return func(out map[string]json.RawMessage) string { return head(out) + "\n\n" + mdOf(out) }
}

func movedTo(out map[string]json.RawMessage) string {
	t := outTicket(out, "ticket")
	return fmt.Sprintf("%s is in %s.", t.ID, t.ColumnLabel)
}

var (
	allColumns = func() []string {
		out := slices.Clone(model.BoardColumns)
		for _, c := range model.Columns {
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
		return out
	}()

	mcpTools = []*mcpTool{
		{name: "my_work", title: "My work", can: []string{"read"}, idempotent: true, verb: "mine", md: true,
			desc: "What to do next, most urgent first: answers to your questions, questions put to you, your tickets in Doing and Next, unassigned tickets in Next and Inbox, then yours that wait. Call it first.",
			schema: sObj(map[string]*jschema{
				"project": sProject("Only this project"),
				"limit":   sInt(1, 50, "At most this many tickets in each group (default 20)"),
			}),
			input: copyArgs(nil), reply: mdOf},
		{name: "get_ticket", title: "Get a ticket", can: []string{"read"}, idempotent: true, verb: "get", md: true,
			desc:   "One ticket as Markdown: its body, questions and answers, conversation, links, and what it waits on.",
			schema: sObj(map[string]*jschema{"id": sID("The ticket, such as T0042")}, "id"),
			input:  copyArgs(nil), reply: mdOf},
		{name: "search_tickets", title: "Search tickets", can: []string{"read"}, idempotent: true, verb: "list", md: true,
			desc: "Tickets in your projects, newest first, one line each. Closed ones only with include_closed or a column. Ends with `More: cursor=…` when there is another page.",
			schema: sObj(map[string]*jschema{
				"project":        sProject("Only this project"),
				"column":         sEnum("Only this column", allColumns...),
				"assignee":       sStr(64, `Only tickets assigned to this person or agent; "me" for yours`),
				"label":          &jschema{typ: "string", pattern: reLabel, desc: "Only tickets with this label"},
				"text":           sStr(200, "Every word appears in the title or body"),
				"updated_since":  &jschema{typ: "string", format: "date-time", desc: "Only tickets updated since (RFC 3339)"},
				"include_closed": sBool("Include Done, Dropped and Rejected tickets"),
				"limit":          sInt(1, 50, "At most this many (default 50)"),
				"cursor":         sID("The cursor from the last page's More line"),
			}),
			input: copyArgs(nil), reply: mdOf},
		{name: "file_ticket", title: "File a ticket", can: []string{"file"}, write: true, verb: "create", md: true,
			desc: "File a new ticket: into Inbox on a board project, or as a change into triage on a factory project. Give an idempotency_key derived from what you found, so a retry never files twice.",
			schema: sObj(map[string]*jschema{
				"project":         sProject("The project to file it on"),
				"title":           sStr(300, "A short title"),
				"body":            sStr(64*1024, "Markdown: what and why, and how to tell it is done"),
				"kind":            sEnum("item (the default on a board project), task (a question to settle) or change (the only kind on a factory project)", "item", "task", "change"),
				"labels":          sLabels("Lower-case labels"),
				"parent":          sID("File it under this ticket, on the same project"),
				"idempotency_key": &jschema{typ: "string", pattern: reIdemKey, desc: "The same key with the same request answers the ticket it filed"},
			}, "project", "title"),
			input: copyArgs(nil),
			reply: headed(func(out map[string]json.RawMessage) string {
				if string(out["created"]) == "true" {
					return "Filed " + outTicket(out, "ticket").ID + "."
				}
				return "Already filed as " + outTicket(out, "ticket").ID + "."
			})},
		{name: "split_ticket", title: "Split a ticket", can: []string{"plan"}, write: true, verb: "split", md: true,
			desc: "Split a board ticket too large for one sitting into children filed under it.",
			schema: sObj(map[string]*jschema{
				"id": sID("The ticket to split"),
				"children": sArr(sObj(map[string]*jschema{
					"title":  sStr(300, "A short title"),
					"body":   sStr(64*1024, "Markdown"),
					"labels": sLabels("Lower-case labels"),
				}, "title"), 1, 20, "The children, in order"),
				"idempotency_key": &jschema{typ: "string", pattern: regexp.MustCompile(`^[A-Za-z0-9._:-]{1,120}$`),
					desc: "The same key with the same children answers the children it filed"},
			}, "id", "children"),
			input: copyArgs(nil),
			reply: headed(func(out map[string]json.RawMessage) string {
				var kids []slimTicket
				json.Unmarshal(out["children"], &kids)
				ids := make([]string, len(kids))
				for i, k := range kids {
					ids[i] = k.ID
				}
				return fmt.Sprintf("Split %s into %s.", outTicket(out, "ticket").ID, strings.Join(ids, ", "))
			})},
		{name: "update_ticket", title: "Update a ticket", can: []string{"plan"}, write: true, idempotent: true, verb: "update", md: true,
			desc: "Change a board ticket's title, body, labels, parent or assignee. Only what you name changes.",
			schema: sObj(map[string]*jschema{
				"id":            sID("The ticket"),
				"title":         sStr(300, "A new title"),
				"body":          sStr(64*1024, "A new body (Markdown), in full"),
				"add_labels":    sLabels("Labels to add"),
				"remove_labels": sLabels("Labels to take off"),
				"parent":        &jschema{typ: "string", pattern: reTicketID, nullable: true, desc: "A new parent on the same project, or null for none"},
				"assignee":      &jschema{typ: "string", maxLen: 64, nullable: true, desc: "A person or an agent on the project, or null for nobody"},
			}, "id"),
			input: copyArgs(nil), reply: mdOf},
		{name: "start_work", title: "Start work", can: []string{"work"}, write: true, idempotent: true, verb: "claim", md: true,
			desc:   "Claim a board ticket in Inbox or Next: it becomes yours and moves to Doing. Calling it again on your own ticket is fine.",
			schema: sObj(map[string]*jschema{"id": sID("The ticket")}, "id"),
			input:  copyArgs(nil),
			reply:  headed(func(out map[string]json.RawMessage) string { return "You have " + outTicket(out, "ticket").ID + "." }),
			hints:  map[string]string{"conflict": "Someone else has it, or it is not in Inbox or Next; pick another with my_work."}},
		{name: "post_update", title: "Post an update", can: []string{"talk"}, write: true, verb: "note", md: true,
			desc:   "Post a comment on a ticket: at real milestones, one line on what changed and what is next. On a factory ticket this is the only way to say something.",
			schema: sObj(map[string]*jschema{"id": sID("The ticket"), "text": sStr(16*1024, "Markdown")}, "id", "text"),
			input:  copyArgs(nil),
			reply:  headed(func(out map[string]json.RawMessage) string { return "Posted on " + outTicket(out, "ticket").ID + "." })},
		{name: "finish_work", title: "Finish work", can: []string{"work"}, write: true, idempotent: true, verb: "move", md: true,
			desc: "Finish a board ticket with a result a person can check: what changed, where (links, PR), what you didn't do. Outcome done moves it to Done; outcome waiting posts the result as a comment and moves it to Waiting with waiting_reason, for when you are blocked on something that is not a question.",
			schema: sObj(map[string]*jschema{
				"id":             sID("The ticket"),
				"result":         sStr(64*1024, "Markdown; at most 16 KiB with outcome waiting"),
				"outcome":        sEnum("done (the default) or waiting", "done", "waiting"),
				"waiting_reason": sStr(500, "With outcome waiting: what it waits on"),
			}, "id", "result"),
			input: func(args map[string]any) (map[string]any, string) {
				to, _ := args["outcome"].(string)
				if to == "" {
					to = "done"
				}
				in := map[string]any{"id": args["id"], "to": to, "result": args["result"]}
				if r, ok := args["waiting_reason"]; ok {
					if to != "waiting" {
						return nil, "waiting_reason goes with outcome waiting"
					}
					in["reason"] = r
				}
				return in, ""
			},
			reply: headed(movedTo)},
		{name: "drop_ticket", title: "Drop a ticket", can: []string{"work"}, write: true, idempotent: true, verb: "move", md: true,
			desc:   "Drop a board ticket that won't be done, saying why.",
			schema: sObj(map[string]*jschema{"id": sID("The ticket"), "reason": sStr(500, "Why it is dropped")}, "id", "reason"),
			input: func(args map[string]any) (map[string]any, string) {
				return map[string]any{"id": args["id"], "to": "dropped", "reason": args["reason"]}, ""
			},
			reply: headed(movedTo)},
		{name: "ask_person", title: "Ask a person", can: []string{"talk"}, write: true, verb: "ask",
			desc: "Ask a question on a board ticket instead of guessing, when the choice is a person's (scope, taste, anything irreversible). The ticket waits; the answer shows first in my_work and in get_ticket. Don't wait in a loop: pick other work.",
			schema: sObj(map[string]*jschema{
				"id":       sID("The ticket"),
				"question": sStr(4*1024, "One short question"),
				"options":  sArr(sStr(100, ""), 0, 6, "2 to 4 answers to choose from, when there are some"),
				"to":       sStr(64, "A person or an agent on the project (default: the maintainer)"),
			}, "id", "question"),
			input: copyArgs(map[string]string{"to": "recipient"}),
			reply: func(out map[string]json.RawMessage) string {
				var n int
				json.Unmarshal(out["n"], &n)
				id := outTicket(out, "ticket").ID
				return fmt.Sprintf("Asked %s (question %d). %s waits; the answer shows in `my_work` and `get_ticket`.\n",
					outString(out, "recipient"), n, id)
			}},
		{name: "answer_question", title: "Answer a question", can: []string{"talk"}, write: true, verb: "answer", md: true,
			desc: "Answer a question put to you (my_work lists them under Asked of you).",
			schema: sObj(map[string]*jschema{
				"id":       sID("The ticket"),
				"question": sInt(1, 1000, "The question's number"),
				"answer":   sStr(16*1024, "Markdown"),
			}, "id", "question", "answer"),
			input: copyArgs(map[string]string{"question": "n"}),
			reply: headed(func(out map[string]json.RawMessage) string { return "Answered on " + outTicket(out, "ticket").ID + "." })},
		{name: "send_to_factory", title: "Send to the factory", can: []string{"work", "file"}, write: true, verb: "send",
			desc: "Hand a board ticket's change to a factory project's pipeline: it is filed there as a change in triage, a person approves it, and the two tickets are linked. The board ticket stays where it is.",
			schema: sObj(map[string]*jschema{
				"id":      sID("The board ticket"),
				"project": sProject("The factory project"),
				"note":    sStr(4*1024, "A note for the factory's triage"),
			}, "id", "project"),
			input: copyArgs(nil),
			reply: func(out map[string]json.RawMessage) string {
				f := outTicket(out, "factory_ticket")
				return fmt.Sprintf("Sent: factory ticket %s in %s on %s; linked from %s.\n", f.ID, columnLabel(f.Column), f.Project,
					outTicket(out, "ticket").ID)
			}},
	}
	mcpToolByName = func() map[string]*mcpTool {
		m := map[string]*mcpTool{}
		for _, t := range mcpTools {
			m[t.name] = t
		}
		return m
	}()
)
