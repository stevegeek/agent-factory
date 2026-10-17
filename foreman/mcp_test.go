package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// rpc: one JSON-RPC request to /mcp; params may be nil.
func (h *apiHarness) rpc(token, method string, params any, opts ...reqOpt) *httptest.ResponseRecorder {
	msg := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		msg["params"] = params
	}
	return h.call("POST", "/mcp", token, msg, opts...)
}

type rpcAnswer struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  map[string]any  `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func rpcOf(t *testing.T, w *httptest.ResponseRecorder, status int, what string) rpcAnswer {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: %d, want %d: %s", what, w.Code, status, w.Body.String())
	}
	var a rpcAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || a.JSONRPC != "2.0" {
		t.Fatalf("%s: not a JSON-RPC answer: %s", what, w.Body.String())
	}
	return a
}

func rpcResult(t *testing.T, w *httptest.ResponseRecorder, what string) map[string]any {
	t.Helper()
	a := rpcOf(t, w, 200, what)
	if a.Error != nil {
		t.Fatalf("%s: error %d %s", what, a.Error.Code, a.Error.Message)
	}
	return a.Result
}

func rpcCode(t *testing.T, w *httptest.ResponseRecorder, status int, what string) int {
	t.Helper()
	a := rpcOf(t, w, status, what)
	if a.Error == nil {
		t.Fatalf("%s: no error: %s", what, w.Body.String())
	}
	return a.Error.Code
}

// tool calls a tool and gives its text and isError.
func (h *apiHarness) tool(token, name string, args map[string]any) (string, bool) {
	h.t.Helper()
	res := rpcResult(h.t, h.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args}), name)
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		h.t.Fatalf("%s: content %v", name, res)
	}
	c := content[0].(map[string]any)
	if c["type"] != "text" {
		h.t.Fatalf("%s: %v", name, c)
	}
	isErr, ok := res["isError"].(bool)
	if !ok {
		h.t.Fatalf("%s: isError missing: %v", name, res)
	}
	return c["text"].(string), isErr
}

func (h *apiHarness) toolOK(token, name string, args map[string]any) string {
	h.t.Helper()
	text, isErr := h.tool(token, name, args)
	if isErr {
		h.t.Fatalf("%s: refused: %s", name, text)
	}
	return text
}

func (h *apiHarness) toolRefused(token, name string, args map[string]any) string {
	h.t.Helper()
	text, isErr := h.tool(token, name, args)
	if !isErr || !strings.HasPrefix(text, "**Not done:** ") {
		h.t.Fatalf("%s: want a refusal, got %v: %s", name, isErr, text)
	}
	return text
}

var firstID = regexp.MustCompile(`T\d{4,6}`)

func TestMCPHandshake(t *testing.T) {
	f := newFakeAPI(t)
	for _, v := range mcpVersions {
		res := rpcResult(t, f.rpc(f.token, "initialize", map[string]any{"protocolVersion": v, "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "test", "version": "1"}}), "initialize "+v)
		if res["protocolVersion"] != v {
			t.Errorf("initialize %s: %v", v, res["protocolVersion"])
		}
	}
	w := f.rpc(f.token, "initialize", map[string]any{"protocolVersion": "2099-01-01"}, header("MCP-Protocol-Version", "2099-01-01"))
	res := rpcResult(t, w, "initialize with a version from the future")
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("an unknown version gets the latest: %v", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok || caps["resources"] == nil || caps["prompts"] != nil {
		t.Errorf("capabilities: %v", caps)
	}
	if info, _ := res["serverInfo"].(map[string]any); info["name"] != "factory-tickets" || info["version"] != "test" {
		t.Errorf("serverInfo: %v", res["serverInfo"])
	}
	if s, _ := res["instructions"].(string); !strings.Contains(s, "my_work") || !strings.Contains(s, "tickets skill") {
		t.Errorf("instructions: %q", s)
	}
	if w.Header().Get("Mcp-Session-Id") != "" || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("no session, JSON: %v", w.Header())
	}

	// notifications and a client's answers: 202 and nothing more
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"x"}}`,
		`{"jsonrpc":"2.0","method":"notifications/anything-else"}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":-1,"message":"no"}}`,
	} {
		if w := f.call("POST", "/mcp", f.token, body); w.Code != 202 || w.Body.Len() != 0 {
			t.Errorf("%s: %d %q", body, w.Code, w.Body.String())
		}
	}
	if res := rpcResult(t, f.rpc(f.token, "ping", nil), "ping"); len(res) != 0 {
		t.Errorf("ping: %v", res)
	}
	// a string id comes back as it was sent
	w = f.call("POST", "/mcp", f.token, `{"jsonrpc":"2.0","id":"abc","method":"ping"}`)
	if a := rpcOf(t, w, 200, "string id"); string(a.ID) != `"abc"` {
		t.Errorf("id: %s", a.ID)
	}

	// the version header after initialize
	for _, v := range mcpVersions {
		rpcResult(t, f.rpc(f.token, "ping", nil, header("MCP-Protocol-Version", v)), "ping "+v)
	}
	rpcResult(t, f.rpc(f.token, "ping", nil), "no header")
	if code := rpcCode(t, f.rpc(f.token, "ping", nil, header("MCP-Protocol-Version", "2099-01-01")), 400, "bad version"); code != rpcInvalid {
		t.Errorf("bad version: %d", code)
	}
	if code := rpcCode(t, f.rpc(f.token, "ping", nil, header("MCP-Protocol-Version", "2025-06-18"), header("MCP-Protocol-Version", "2025-03-26")), 400, "two versions"); code != rpcInvalid {
		t.Errorf("two versions: %d", code)
	}

	// malformed messages
	for _, k := range []struct {
		body   string
		status int
		code   int
	}{
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, 400, rpcInvalid},
		{`[]`, 400, rpcInvalid},
		{`{"jsonrpc":"2.0","id":1,"method":"ping"`, 400, rpcParse},
		{``, 400, rpcParse},
		{`"ping"`, 400, rpcInvalid},
		{`null`, 400, rpcInvalid},
		{`{"jsonrpc":"1.0","id":1,"method":"ping"}`, 400, rpcInvalid},
		{`{"id":1,"method":"ping"}`, 400, rpcInvalid},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, 400, rpcInvalid},
		{`{"jsonrpc":"2.0","id":{},"method":"ping"}`, 400, rpcInvalid},
		{`{"jsonrpc":"2.0","id":1,"method":7}`, 400, rpcInvalid},
		{`{"jsonrpc":"2.0","id":1}`, 400, rpcInvalid},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":[1]}`, 200, rpcBadParams},
		{`{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage"}`, 200, rpcNoMethod},
		{`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`, 200, rpcNoMethod},
	} {
		if code := rpcCode(t, f.call("POST", "/mcp", f.token, k.body), k.status, k.body); code != k.code {
			t.Errorf("%s: %d, want %d", k.body, code, k.code)
		}
	}

	// transport
	for _, m := range []string{"GET", "DELETE", "PUT"} {
		w := f.call(m, "/mcp", f.token, nil, header("Accept", "text/event-stream"))
		if w.Code != 405 || w.Header().Get("Allow") != "POST" {
			t.Errorf("%s: %d %q", m, w.Code, w.Header().Get("Allow"))
		}
	}
	if w := f.rpc(f.token, "ping", nil, header("Accept", "text/event-stream")); w.Code != 406 {
		t.Errorf("an event stream only: %d", w.Code)
	}
	for _, acc := range []string{"application/json, text/event-stream", "text/event-stream, application/json;q=0.9", "*/*"} {
		rpcResult(t, f.rpc(f.token, "ping", nil, header("Accept", acc)), "Accept "+acc)
	}
	if w := f.rpc(f.token, "ping", nil, header("Mcp-Session-Id", "whatever")); w.Code != 200 {
		t.Errorf("a session id is ignored: %d", w.Code)
	}
	if got := f.argv(); len(got) != 0 {
		t.Errorf("the handshake ran the CLI: %q", got)
	}
}

func TestMCPAuth(t *testing.T) {
	f := newFakeAPI(t)
	if w := f.rpc("", "ping", nil); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no token: %d", w.Code)
	}
	if w := f.rpc("fct_wrong", "initialize", map[string]any{"protocolVersion": "2025-06-18"}); w.Code != 401 {
		t.Errorf("a wrong token: %d", w.Code)
	}
	if w := f.rpc(f.token, "ping", nil, hostIs("evil.example")); w.Code != 421 {
		t.Errorf("a wrong Host: %d", w.Code)
	}
	for _, o := range []string{"https://evil.example", "null", "http://example.com"} {
		if w := f.rpc(f.token, "ping", nil, header("Origin", o)); w.Code != 403 {
			t.Errorf("Origin %s: %d", o, w.Code)
		}
	}
	rpcResult(t, f.rpc(f.token, "ping", nil, header("Origin", "https://example.com")), "this site's Origin")
	if w := f.rpc(f.token, "ping", nil, header("Tailscale-Funnel-Request", "?1")); w.Code != 403 {
		t.Errorf("through Funnel: %d", w.Code)
	}
	if w := f.rpc(f.token, "ping", nil, remote("203.0.113.9:4000")); w.Code != 403 {
		t.Errorf("off the tailnet: %d", w.Code)
	}
	if w := f.call("POST", "/mcp", f.token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, contentType("text/plain")); w.Code != 415 {
		t.Errorf("text/plain: %d", w.Code)
	}
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", apiBody) + `"}}`
	if w := f.call("POST", "/mcp", f.token, big); w.Code != 413 {
		t.Errorf("too big: %d", w.Code)
	}
	// a token revoked since Foreman read the store
	f.mode("refuse:unauthorized")
	if w := f.rpc(f.token, "tools/list", nil); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("revoked: %d %s", w.Code, w.Body.String())
	}
}

// argsFor: arguments each tool takes, enough to reach the CLI.
var argsFor = map[string]map[string]any{
	"my_work":         {},
	"get_ticket":      {"id": "T0001"},
	"search_tickets":  {"text": "date"},
	"file_ticket":     {"project": "sandbox", "title": "x"},
	"split_ticket":    {"id": "T0001", "children": []any{map[string]any{"title": "a"}}},
	"update_ticket":   {"id": "T0001", "title": "y"},
	"start_work":      {"id": "T0001"},
	"post_update":     {"id": "T0001", "text": "hello"},
	"finish_work":     {"id": "T0001", "result": "done it"},
	"drop_ticket":     {"id": "T0001", "reason": "not needed"},
	"ask_person":      {"id": "T0001", "question": "which?"},
	"answer_question": {"id": "T0001", "question": 1, "answer": "this"},
	"send_to_factory": {"id": "T0001", "project": "fac"},
}

func TestMCPToolsList(t *testing.T) {
	h := newAPIHarness(t)
	reader := h.mint("reader-agent", "read", "sandbox")
	full := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	worker := h.mint("worker-agent", "read,work", "sandbox")
	names := func(token string, opts ...reqOpt) ([]string, []any) {
		res := rpcResult(t, h.rpc(token, "tools/list", nil, opts...), "tools/list")
		if _, ok := res["nextCursor"]; ok {
			t.Errorf("no pagination: %v", res)
		}
		tools, _ := res["tools"].([]any)
		var out []string
		for _, x := range tools {
			out = append(out, x.(map[string]any)["name"].(string))
		}
		sort.Strings(out)
		return out, tools
	}
	if got, _ := names(reader); strings.Join(got, ",") != "get_ticket,my_work,search_tickets" {
		t.Errorf("a read-only token: %v", got)
	}
	if got, _ := names(worker); slices.Contains(got, "send_to_factory") || !slices.Contains(got, "start_work") || slices.Contains(got, "post_update") {
		t.Errorf("work without file or talk: %v", got)
	}
	got, tools := names(full, header("MCP-Protocol-Version", "2025-06-18"))
	if len(got) != 13 || len(mcpTools) != 13 {
		t.Fatalf("a full token sees all 13: %v", got)
	}
	required := map[string]string{"my_work": "", "get_ticket": "id", "search_tickets": "", "file_ticket": "project,title",
		"split_ticket": "id,children", "update_ticket": "id", "start_work": "id", "post_update": "id,text",
		"finish_work": "id,result", "drop_ticket": "id,reason", "ask_person": "id,question",
		"answer_question": "id,question,answer", "send_to_factory": "id,project"}
	for _, x := range tools {
		tl := x.(map[string]any)
		name := tl["name"].(string)
		if d, _ := tl["description"].(string); len(d) < 20 {
			t.Errorf("%s: description %q", name, d)
		}
		s := tl["inputSchema"].(map[string]any)
		if s["type"] != "object" || s["additionalProperties"] != false {
			t.Errorf("%s: schema %v", name, s)
		}
		var req []string
		for _, r := range asSlice(s["required"]) {
			req = append(req, r.(string))
		}
		if strings.Join(req, ",") != required[name] {
			t.Errorf("%s: required %v", name, req)
		}
		props := s["properties"].(map[string]any)
		for _, r := range req {
			if props[r] == nil {
				t.Errorf("%s: required %s is not a property", name, r)
			}
		}
		for k, p := range props {
			schemaShape(t, name+"."+k, p.(map[string]any))
		}
		if id, ok := props["id"].(map[string]any); ok && id["pattern"] != "^T[0-9]{4,6}$" {
			t.Errorf("%s: id %v", name, id)
		}
		ann, _ := tl["annotations"].(map[string]any)
		write := mcpToolByName[name].write
		if ann == nil || ann["readOnlyHint"] != !write || (write && ann["destructiveHint"] != false) || tl["title"] == nil {
			t.Errorf("%s: annotations %v, title %v", name, ann, tl["title"])
		}
		if _, ok := argsFor[name]; !ok {
			t.Errorf("%s: no arguments in argsFor", name)
		}
	}
	_, old := names(full, header("MCP-Protocol-Version", "2024-11-05"))
	for _, x := range old {
		if tl := x.(map[string]any); tl["annotations"] != nil || tl["title"] != nil {
			t.Errorf("2024-11-05 has no annotations or titles: %v", tl)
		}
	}
	_, mid := names(full, header("MCP-Protocol-Version", "2025-03-26"))
	for _, x := range mid {
		if tl := x.(map[string]any); tl["annotations"] == nil || tl["title"] != nil {
			t.Errorf("2025-03-26 has annotations but no top-level title: %v", tl)
		}
	}
}

func asSlice(v any) []any {
	xs, _ := v.([]any)
	return xs
}

// schemaShape: a property is a JSON Schema this server's subset can say.
func schemaShape(t *testing.T, at string, s map[string]any) {
	t.Helper()
	typ := s["type"]
	if types, ok := typ.([]any); ok {
		if len(types) != 2 || types[1] != "null" {
			t.Errorf("%s: type %v", at, typ)
		}
		typ = types[0]
	}
	switch typ {
	case "string":
		if p, ok := s["pattern"].(string); ok {
			if _, err := regexp.Compile(p); err != nil {
				t.Errorf("%s: pattern %q", at, p)
			}
		}
		if _, ok := s["maxLength"]; !ok && s["pattern"] == nil && s["enum"] == nil && s["format"] == nil {
			t.Errorf("%s: an unbounded string", at)
		}
	case "integer":
		if s["minimum"] == nil || s["maximum"] == nil {
			t.Errorf("%s: an unbounded integer", at)
		}
	case "boolean":
	case "array":
		if s["maxItems"] == nil {
			t.Errorf("%s: an unbounded array", at)
		}
		schemaShape(t, at+"[]", s["items"].(map[string]any))
	case "object":
		if s["additionalProperties"] != false {
			t.Errorf("%s: open object", at)
		}
		for k, p := range s["properties"].(map[string]any) {
			schemaShape(t, at+"."+k, p.(map[string]any))
		}
	default:
		t.Errorf("%s: type %v", at, typ)
	}
}

func TestMCPAgentLoop(t *testing.T) {
	h := newAPIHarness(t)
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	helper := h.mint("helper-agent", "read,talk", "sandbox")
	var made struct {
		Ticket struct {
			ID string `json:"id"`
		} `json:"ticket"`
	}
	json.Unmarshal([]byte(h.cli(`{"project": "sandbox", "title": "Fix the date picker", "body": "It shows the wrong month."}`,
		"ticket", "create", "--json")), &made)
	id := made.Ticket.ID

	text := h.toolOK(agent, "my_work", nil)
	if !strings.Contains(text, "## Unassigned") || !strings.Contains(text, id) {
		t.Errorf("my_work: %s", text)
	}
	text = h.toolOK(agent, "start_work", map[string]any{"id": id})
	if !strings.HasPrefix(text, "You have "+id+".\n\n# "+id+" · Fix the date picker") || !strings.Contains(text, "**Doing**") {
		t.Errorf("start_work: %s", text)
	}
	h.toolOK(agent, "start_work", map[string]any{"id": id}) // again, as the holder
	text = h.toolOK(agent, "post_update", map[string]any{"id": id, "text": "Found it: the month is 0-based."})
	if !strings.HasPrefix(text, "Posted on "+id+".\n\n## Conversation") || !strings.Contains(text, "you · comment: Found it") {
		t.Errorf("post_update: %s", text)
	}
	text = h.toolOK(agent, "ask_person", map[string]any{"id": id, "question": "Which locale?", "options": []string{"en-GB", "en-US"}})
	if text != "Asked alice (question 1). "+id+" waits; the answer shows in `my_work` and `get_ticket`.\n" {
		t.Errorf("ask_person: %q", text)
	}
	if text = h.toolOK(agent, "my_work", nil); !strings.Contains(text, "## Waiting") {
		t.Errorf("my_work while waiting: %s", text)
	}
	h.cli("", "ticket", "answer", id, "1", "en-GB")
	text = h.toolOK(agent, "my_work", nil)
	if !strings.HasPrefix(text, "## Answered (your questions)\n- "+id) {
		t.Errorf("the answer comes first: %s", text)
	}
	if text = h.toolOK(agent, "get_ticket", map[string]any{"id": id}); !strings.Contains(text, "en-GB") || !strings.Contains(text, "**Doing**") {
		t.Errorf("get_ticket after the answer: %s", text)
	}
	h.toolOK(agent, "post_update", map[string]any{"id": id, "text": "Going with en-GB."})
	if text = h.toolOK(agent, "my_work", nil); strings.Contains(text, "## Answered") {
		t.Errorf("an answer is new until the agent acts: %s", text)
	}

	// a question put to the agent
	h.toolOK(helper, "ask_person", map[string]any{"id": id, "question": "Does it need a test?", "to": "example-agent"})
	if text = h.toolOK(agent, "my_work", nil); !strings.Contains(text, "## Asked of you\n- "+id) {
		t.Errorf("asked of me: %s", text)
	}
	text = h.toolOK(agent, "answer_question", map[string]any{"id": id, "question": 2, "answer": "Yes, one."})
	if !strings.HasPrefix(text, "Answered on "+id+".") {
		t.Errorf("answer_question: %s", text)
	}

	text = h.toolOK(agent, "finish_work", map[string]any{"id": id, "result": "Fixed: months count from 1. PR https://example.com/pr/1"})
	if !strings.HasPrefix(text, id+" is in Done.") || !strings.Contains(text, "## Result") {
		t.Errorf("finish_work: %s", text)
	}

	// filing, once per key
	args := map[string]any{"project": "sandbox", "title": "The year picker too", "labels": []string{"ui"}, "idempotency_key": "scan:year"}
	text = h.toolOK(agent, "file_ticket", args)
	filed := firstID.FindString(text)
	if !strings.HasPrefix(text, "Filed "+filed+".\n\n# "+filed) {
		t.Errorf("file_ticket: %s", text)
	}
	if text = h.toolOK(agent, "file_ticket", args); !strings.HasPrefix(text, "Already filed as "+filed+".") {
		t.Errorf("file_ticket again: %s", text)
	}
	text = h.toolOK(agent, "split_ticket", map[string]any{"id": filed, "children": []any{
		map[string]any{"title": "Month view"}, map[string]any{"title": "Year view", "labels": []string{"ui"}}}})
	if !regexp.MustCompile(`^Split `+filed+` into T\d+, T\d+\.`).MatchString(text) || !strings.Contains(text, "Children: ") {
		t.Errorf("split_ticket: %s", text)
	}
	text = h.toolOK(agent, "update_ticket", map[string]any{"id": filed, "title": "The year picker", "add_labels": []string{"later"}, "assignee": nil})
	if !strings.Contains(text, "# "+filed+" · The year picker") || !strings.Contains(text, "labels: later, ui") {
		t.Errorf("update_ticket: %s", text)
	}
	text = h.toolOK(agent, "search_tickets", map[string]any{"text": "view", "limit": 1})
	if !strings.Contains(text, "Year view") || !strings.Contains(text, "More: cursor=T") {
		t.Errorf("search_tickets: %s", text)
	}
	text = h.toolOK(agent, "send_to_factory", map[string]any{"id": filed, "project": "fac", "note": "the shared picker"})
	if !regexp.MustCompile(`^Sent: factory ticket T\d+ in Triage on fac; linked from ` + filed + `\.\n$`).MatchString(text) {
		t.Errorf("send_to_factory: %q", text)
	}
	text = h.toolOK(agent, "finish_work", map[string]any{"id": filed, "result": "The form half.", "outcome": "waiting", "waiting_reason": "on the design"})
	if !strings.HasPrefix(text, filed+" is in Waiting.") || !strings.Contains(text, "on the design") || !strings.Contains(text, "The form half.") {
		t.Errorf("finish_work waiting: %s", text)
	}
	text = h.toolOK(agent, "drop_ticket", map[string]any{"id": filed, "reason": "the factory does it"})
	if !strings.HasPrefix(text, filed+" is in Dropped.") {
		t.Errorf("drop_ticket: %s", text)
	}
}

func TestMCPRefusalsAreToolResults(t *testing.T) {
	// every tool, every refusal code: isError with what to do next, never a protocol error
	f := newFakeAPI(t)
	for code := range apiStatus {
		if code == "unauthorized" {
			continue // HTTP 401, as before JSON-RPC (TestMCPAuth)
		}
		f.mode("refuse:" + code)
		for _, tl := range mcpTools {
			text := f.toolRefused(f.token, tl.name, argsFor[tl.name])
			if !strings.HasPrefix(text, "**Not done:** no (id)\n") || strings.Count(text, "\n") != 2 {
				t.Errorf("%s %s: %q", tl.name, code, text)
			}
		}
	}
	f.mode("refuse:conflict")
	if text := f.toolRefused(f.token, "start_work", argsFor["start_work"]); !strings.Contains(text, "pick another with my_work") {
		t.Errorf("start_work's conflict: %s", text)
	}
	// each verb as the tool says, with the arguments it renames
	for _, k := range []struct {
		tool, verb string
		in         map[string]any
	}{
		{"my_work", "mine", map[string]any{"format": "md"}},
		{"search_tickets", "list", map[string]any{"text": "date", "format": "md"}},
		{"finish_work", "move", map[string]any{"id": "T0001", "to": "done", "result": "done it", "format": "md"}},
		{"drop_ticket", "move", map[string]any{"id": "T0001", "to": "dropped", "reason": "not needed", "format": "md"}},
		{"ask_person", "ask", map[string]any{"id": "T0001", "question": "which?", "recipient": "alice"}},
		{"answer_question", "answer", map[string]any{"id": "T0001", "n": float64(1), "answer": "this", "format": "md"}},
		{"send_to_factory", "send", map[string]any{"id": "T0001", "project": "fac"}},
	} {
		args := argsFor[k.tool]
		if k.tool == "ask_person" {
			args = map[string]any{"id": "T0001", "question": "which?", "to": "alice"}
		}
		f.argv()
		f.toolRefused(f.token, k.tool, args)
		argv := f.argv()
		if len(argv) != 1 || strings.Join(argv[0], " ") != "ticket "+k.verb+" --as example-agent --wait 10 --json" {
			t.Errorf("%s: argv %q", k.tool, argv)
		}
		got, _ := json.Marshal(f.stdin())
		wantIn, _ := json.Marshal(k.in)
		if string(got) != string(wantIn) {
			t.Errorf("%s: input %s, want %s", k.tool, got, wantIn)
		}
	}
	// failures of the factory's own: a protocol error with no detail
	for _, m := range []string{"crash", "usage", "nojson", "okexit1", "refuse:something_new"} {
		f.mode(m)
		w := f.rpc(f.token, "tools/call", map[string]any{"name": "get_ticket", "arguments": argsFor["get_ticket"]})
		if code := rpcCode(t, w, 200, m); code != rpcInternal || strings.Contains(w.Body.String(), "Traceback") || strings.Contains(w.Body.String(), "usage") {
			t.Errorf("%s: %d %s", m, code, w.Body.String())
		}
	}
}

func TestMCPRealRefusals(t *testing.T) {
	h := newAPIHarness(t)
	reader := h.mint("reader-agent", "read", "sandbox", "fac")
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	other := h.mint("other-agent", "read,file,talk,work,plan", "sandbox")
	var made struct {
		Ticket struct {
			ID string `json:"id"`
		} `json:"ticket"`
	}
	json.Unmarshal([]byte(h.cli(`{"project": "sandbox", "title": "Board work"}`, "ticket", "create", "--json")), &made)
	id := made.Ticket.ID
	json.Unmarshal([]byte(h.cli(`{"project": "other", "title": "Hidden work"}`, "ticket", "create", "--json")), &made)
	hidden := made.Ticket.ID
	json.Unmarshal([]byte(h.cli(`{"project": "fac", "title": "A change"}`, "ticket", "create", "--json")), &made)
	fac := made.Ticket.ID

	for _, tl := range mcpTools {
		args := map[string]any{}
		for k, v := range argsFor[tl.name] {
			args[k] = v
		}
		if _, ok := args["id"]; ok {
			args["id"] = id
		}
		if tl.write {
			if text := h.toolRefused(reader, tl.name, args); !strings.Contains(text, "may not") {
				t.Errorf("%s as a reader: %s", tl.name, text)
			}
		}
		if _, ok := args["id"]; ok {
			args["id"] = hidden
			if text := h.toolRefused(agent, tl.name, args); strings.Contains(text, "Hidden work") || !strings.Contains(text, "my_work or search_tickets") {
				t.Errorf("%s on a hidden ticket: %s", tl.name, text)
			}
		}
	}
	for _, name := range []string{"start_work", "update_ticket", "split_ticket", "finish_work", "drop_ticket", "send_to_factory"} {
		args := map[string]any{}
		for k, v := range argsFor[name] {
			args[k] = v
		}
		args["id"] = fac
		if text := h.toolRefused(agent, name, args); !strings.Contains(text, "pipeline") {
			t.Errorf("%s on a factory ticket: %s", name, text)
		}
	}
	if text := h.toolRefused(agent, "ask_person", map[string]any{"id": fac, "question": "x?"}); !strings.Contains(text, "post_update") {
		t.Errorf("ask_person on a factory ticket: %s", text)
	}
	h.toolOK(agent, "post_update", map[string]any{"id": fac, "text": "A comment is fine."})
	h.toolOK(other, "start_work", map[string]any{"id": id})
	if text := h.toolRefused(reader, "start_work", map[string]any{"id": id}); !strings.Contains(text, "may not") {
		t.Errorf("start_work as a reader: %s", text)
	}
	if text := h.toolRefused(agent, "answer_question", map[string]any{"id": id, "question": 9, "answer": "x"}); !strings.Contains(text, "no question 9 (question)") {
		t.Errorf("answer_question with no such question names its own argument: %s", text)
	}
	if text := h.toolRefused(agent, "ask_person", map[string]any{"id": id, "question": "x?", "to": "nobody"}); !strings.Contains(text, "(to)") {
		t.Errorf("ask_person to nobody names its own argument: %s", text)
	}
	if text := h.toolRefused(agent, "file_ticket", map[string]any{"project": "other", "title": "x"}); !strings.Contains(text, "no project other") {
		t.Errorf("file_ticket on a hidden project: %s", text)
	}
}

func TestMCPWaitingAndClosing(t *testing.T) {
	h := newAPIHarness(t)
	agent := h.mint("example-agent", "read,file,talk,work", "sandbox")
	var made struct {
		Ticket struct {
			ID string `json:"id"`
		} `json:"ticket"`
	}
	json.Unmarshal([]byte(h.cli(`{"project": "sandbox", "title": "Board work"}`, "ticket", "create", "--json")), &made)
	id := made.Ticket.ID

	// out of Waiting and back to Doing, by the holder
	h.toolOK(agent, "start_work", map[string]any{"id": id})
	h.toolOK(agent, "finish_work", map[string]any{"id": id, "result": "Half.", "outcome": "waiting", "waiting_reason": "CI is red"})
	if text := h.toolOK(agent, "start_work", map[string]any{"id": id}); !strings.Contains(text, "**Doing**") {
		t.Errorf("start_work from Waiting: %s", text)
	}

	// closing withdraws an open question, and the person is no longer asked
	h.toolOK(agent, "ask_person", map[string]any{"id": id, "question": "Which?"})
	h.toolOK(agent, "finish_work", map[string]any{"id": id, "result": "Done it"})
	text := h.toolOK(agent, "get_ticket", map[string]any{"id": id})
	if !strings.Contains(text, "1. **Withdrawn**") {
		t.Errorf("a closed ticket's question is withdrawn: %s", text)
	}
	var tk map[string]any
	data, _ := os.ReadFile(h.root + "/board/tickets/" + id + ".json")
	json.Unmarshal(data, &tk)
	if tk["ask"] != nil {
		t.Errorf("a Done ticket asks nobody: %v", tk["ask"])
	}

	// a retry is fine; a different result is not dropped silently
	h.toolOK(agent, "finish_work", map[string]any{"id": id, "result": "Done it"})
	if text := h.toolRefused(agent, "finish_work", map[string]any{"id": id, "result": "Done it again, differently"}); !strings.Contains(text, "post_update") {
		t.Errorf("a second, different result: %s", text)
	}
	h.toolOK(agent, "drop_ticket", map[string]any{"id": id, "reason": "not needed after all"})
	json.Unmarshal([]byte(h.cli(`{"id": "`+id+`"}`, "ticket", "get", "--json")), &tk)
	hist := tk["ticket"].(map[string]any)["history"].([]any)
	if last := hist[len(hist)-1].(map[string]any); last["reason"] != "dropped" {
		t.Errorf("Done to Dropped is not a reopening: %v", last)
	}
}

func TestMCPBadArguments(t *testing.T) {
	f := newFakeAPI(t)
	for _, k := range []struct {
		name string
		args any
	}{
		{"no_such_tool", map[string]any{}},
		{"get_ticket", map[string]any{}},
		{"get_ticket", map[string]any{"id": "T0001", "format": "json"}},
		{"get_ticket", map[string]any{"id": "T0001;rm"}},
		{"get_ticket", map[string]any{"id": 42}},
		{"get_ticket", map[string]any{"id": nil}},
		{"get_ticket", []any{"T0001"}},
		{"get_ticket", "T0001"},
		{"my_work", map[string]any{"limit": 0}},
		{"my_work", map[string]any{"limit": 51}},
		{"my_work", map[string]any{"limit": 2.5}},
		{"my_work", map[string]any{"limit": "5"}},
		{"search_tickets", map[string]any{"column": "nowhere"}},
		{"search_tickets", map[string]any{"updated_since": "yesterday"}},
		{"search_tickets", map[string]any{"include_closed": "yes"}},
		{"search_tickets", map[string]any{"text": strings.Repeat("a", 201)}},
		{"file_ticket", map[string]any{"project": "sandbox"}},
		{"file_ticket", map[string]any{"project": "sandbox", "title": strings.Repeat("é", 301)}},
		{"file_ticket", map[string]any{"project": "sandbox", "title": "x", "kind": "intent"}},
		{"file_ticket", map[string]any{"project": "sandbox", "title": "x", "labels": []any{"UI"}}},
		{"file_ticket", map[string]any{"project": "sandbox", "title": "x", "idempotency_key": "has space"}},
		{"split_ticket", map[string]any{"id": "T0001", "children": []any{}}},
		{"split_ticket", map[string]any{"id": "T0001", "children": []any{map[string]any{"title": "a", "kind": "task"}}}},
		{"update_ticket", map[string]any{"id": "T0001", "parent": "nope"}},
		{"finish_work", map[string]any{"id": "T0001", "result": "x", "outcome": "later"}},
		{"finish_work", map[string]any{"id": "T0001", "result": "x", "waiting_reason": "y"}},
		{"finish_work", map[string]any{"id": "T0001", "result": "x", "outcome": "waiting"}},
		{"ask_person", map[string]any{"id": "T0001", "question": "x", "options": []any{"a", "b", "c", "d", "e", "f", "g"}}},
		{"answer_question", map[string]any{"id": "T0001", "question": 0, "answer": "x"}},
		{"post_update", map[string]any{"id": "T0001", "text": strings.Repeat("a", 16*1024+1)}},
	} {
		w := f.rpc(f.token, "tools/call", map[string]any{"name": k.name, "arguments": k.args})
		if code := rpcCode(t, w, 200, k.name); code != rpcBadParams {
			t.Errorf("%s %v: %d", k.name, k.args, code)
		}
	}
	if code := rpcCode(t, f.rpc(f.token, "tools/call", map[string]any{"arguments": map[string]any{}}), 200, "no name"); code != rpcBadParams {
		t.Errorf("no name: %d", code)
	}
	if got := f.argv(); len(got) != 0 {
		t.Errorf("bad arguments reached the CLI: %q", got)
	}
	// no arguments at all is no arguments
	f.mode("refuse:forbidden")
	f.toolRefused(f.token, "my_work", nil)
}

func TestMCPWriteRate(t *testing.T) {
	f := newFakeAPI(t)
	f.mode("refuse:forbidden")
	f.guard.writes = bucketSpec{perMin: 1, burst: 2}
	f.toolRefused(f.token, "post_update", argsFor["post_update"])
	f.toolRefused(f.token, "start_work", argsFor["start_work"])
	w := f.rpc(f.token, "tools/call", map[string]any{"name": "post_update", "arguments": argsFor["post_update"]})
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || errCode(t, w) != "too_many" {
		t.Errorf("the third write: %d %q %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	// reads still go
	f.toolRefused(f.token, "get_ticket", argsFor["get_ticket"])
	rpcResult(t, f.rpc(f.token, "ping", nil), "ping")
	// and a write over /api/v1 draws on the same bucket
	if w := f.call("POST", "/api/v1/tickets/T0001/notes", f.token, map[string]any{"text": "x"}); w.Code != 429 {
		t.Errorf("the API shares the write rate: %d", w.Code)
	}
}

func TestMCPBusy(t *testing.T) {
	f := newFakeAPI(t)
	f.mode("slow")
	f.runner.slotWait = 0
	f.runner.slots <- struct{}{}
	f.runner.slots <- struct{}{}
	defer func() { <-f.runner.slots; <-f.runner.slots }()
	if text := f.toolRefused(f.token, "get_ticket", argsFor["get_ticket"]); !strings.Contains(text, "busy") || !strings.Contains(text, "Try again") {
		t.Errorf("no free slot: %s", text)
	}
}

func TestMCPResources(t *testing.T) {
	h := newAPIHarness(t)
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	ids := map[string]string{}
	for _, k := range []struct{ key, project, title string }{
		{"mine", "sandbox", "Mine"}, {"open", "sandbox", "Unassigned"}, {"done", "sandbox", "Finished"}, {"hidden", "other", "Hidden"},
	} {
		var made struct {
			Ticket struct {
				ID string `json:"id"`
			} `json:"ticket"`
		}
		json.Unmarshal([]byte(h.cli(`{"project": "`+k.project+`", "title": "`+k.title+`"}`, "ticket", "create", "--json")), &made)
		ids[k.key] = made.Ticket.ID
	}
	h.toolOK(agent, "start_work", map[string]any{"id": ids["mine"]})
	h.toolOK(agent, "start_work", map[string]any{"id": ids["done"]})
	h.toolOK(agent, "finish_work", map[string]any{"id": ids["done"], "result": "ok"})

	res := rpcResult(t, h.rpc(agent, "resources/list", nil), "resources/list")
	var uris []string
	for _, r := range asSlice(res["resources"]) {
		m := r.(map[string]any)
		if m["mimeType"] != "text/markdown" || m["name"] == "" {
			t.Errorf("resource: %v", m)
		}
		uris = append(uris, m["uri"].(string))
	}
	sort.Strings(uris)
	if want := []string{"project://fac", "project://sandbox", "ticket://" + ids["mine"]}; !slices.Equal(uris, want) {
		t.Errorf("resources: %v, want %v", uris, want)
	}
	tpl := rpcResult(t, h.rpc(agent, "resources/templates/list", nil), "templates")
	if ts := asSlice(tpl["resourceTemplates"]); len(ts) != 1 || ts[0].(map[string]any)["uriTemplate"] != "ticket://{id}" {
		t.Errorf("templates: %v", tpl)
	}

	read := func(uri string) string {
		res := rpcResult(t, h.rpc(agent, "resources/read", map[string]any{"uri": uri}), uri)
		cs := asSlice(res["contents"])
		if len(cs) != 1 {
			t.Fatalf("%s: %v", uri, res)
		}
		c := cs[0].(map[string]any)
		if c["uri"] != uri || c["mimeType"] != "text/markdown" {
			t.Errorf("%s: %v", uri, c)
		}
		return c["text"].(string)
	}
	if got, want := read("ticket://"+ids["mine"]), h.toolOK(agent, "get_ticket", map[string]any{"id": ids["mine"]}); got != want {
		t.Errorf("ticket:// is get_ticket's text:\n%s\n---\n%s", got, want)
	}
	if text := read("project://sandbox"); !strings.Contains(text, ids["mine"]) || !strings.Contains(text, ids["open"]) || strings.Contains(text, ids["done"]) {
		t.Errorf("project://sandbox: %s", text)
	}
	for _, uri := range []string{"ticket://" + ids["hidden"], "ticket://T9999", "ticket://T1;rm", "project://other",
		"project://nope", "project://../x", "file:///etc/passwd", "ticket:" + ids["mine"]} {
		w := h.rpc(agent, "resources/read", map[string]any{"uri": uri})
		if code := rpcCode(t, w, 200, uri); code != rpcNoSuchResource || strings.Contains(w.Body.String(), "Hidden") {
			t.Errorf("%s: %d %s", uri, code, w.Body.String())
		}
	}
	if code := rpcCode(t, h.rpc(agent, "resources/read", map[string]any{}), 200, "no uri"); code != rpcBadParams {
		t.Errorf("no uri: %d", code)
	}
}

// The skill names only tools the server has, and every tool, so the two can't drift.
func TestSkillMatchesTools(t *testing.T) {
	data, err := os.ReadFile("../skills/tickets/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	skill := string(data)
	if !strings.HasPrefix(skill, "---\nname: tickets\ndescription: ") {
		t.Errorf("frontmatter: %q", skill[:min(80, len(skill))])
	}
	args := map[string]bool{}
	for _, tl := range mcpTools {
		if !strings.Contains(skill, "`"+tl.name+"`") {
			t.Errorf("the skill never names %s", tl.name)
		}
		for k := range tl.schema.props {
			args[k] = true
		}
	}
	for _, m := range regexp.MustCompile("`([a-z]+(?:_[a-z]+)+)`").FindAllStringSubmatch(skill, -1) {
		if mcpToolByName[m[1]] == nil && !args[m[1]] {
			t.Errorf("the skill names %s, which is no tool or argument", m[1])
		}
	}
	for _, path := range []string{"../examples/mcp/mcp.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			MCPServers map[string]struct {
				Type, URL string
				Headers   map[string]string
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		s := cfg.MCPServers["factory"]
		if s.Type != "http" || !strings.HasSuffix(s.URL, mcpPath) || s.Headers["Authorization"] != "Bearer ${FACTORY_TOKEN}" {
			t.Errorf("%s: %+v", path, cfg)
		}
	}
}
