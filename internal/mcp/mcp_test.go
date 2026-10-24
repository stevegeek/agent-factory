package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/guard"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

// rpc is one JSON-RPC request to /mcp; params may be nil.
func (h *harness) rpc(token, method string, params any, opts ...reqOpt) *httptest.ResponseRecorder {
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
func (h *harness) tool(token, name string, args map[string]any) (string, bool) {
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

func (h *harness) toolOK(token, name string, args map[string]any) string {
	h.t.Helper()
	text, isErr := h.tool(token, name, args)
	if isErr {
		h.t.Fatalf("%s: refused: %s", name, text)
	}
	return text
}

func (h *harness) toolRefused(token, name string, args map[string]any) string {
	h.t.Helper()
	text, isErr := h.tool(token, name, args)
	if !isErr || !strings.HasPrefix(text, "**Not done:** ") {
		h.t.Fatalf("%s: want a refusal, got %v: %s", name, isErr, text)
	}
	return text
}

func TestMCPHandshake(t *testing.T) {
	h, f, tokens := newFakeHarness(t, harnessOpt{})
	tok := tokens[0]
	for _, v := range versions() {
		res := rpcResult(t, h.rpc(tok, "initialize", map[string]any{"protocolVersion": v, "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "test", "version": "1"}}), "initialize "+v)
		if res["protocolVersion"] != v {
			t.Errorf("initialize %s: %v", v, res["protocolVersion"])
		}
	}
	w := h.rpc(tok, "initialize", map[string]any{"protocolVersion": "2099-01-01"}, header("MCP-Protocol-Version", "2099-01-01"))
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
		if w := h.call("POST", "/mcp", tok, body); w.Code != 202 || w.Body.Len() != 0 {
			t.Errorf("%s: %d %q", body, w.Code, w.Body.String())
		}
	}
	if res := rpcResult(t, h.rpc(tok, "ping", nil), "ping"); len(res) != 0 {
		t.Errorf("ping: %v", res)
	}
	// a string id comes back as it was sent
	w = h.call("POST", "/mcp", tok, `{"jsonrpc":"2.0","id":"abc","method":"ping"}`)
	if a := rpcOf(t, w, 200, "string id"); string(a.ID) != `"abc"` {
		t.Errorf("id: %s", a.ID)
	}

	// the version header after initialize
	for _, v := range versions() {
		rpcResult(t, h.rpc(tok, "ping", nil, header("MCP-Protocol-Version", v)), "ping "+v)
	}
	rpcResult(t, h.rpc(tok, "ping", nil), "no header")
	if code := rpcCode(t, h.rpc(tok, "ping", nil, header("MCP-Protocol-Version", "2099-01-01")), 400, "bad version"); code != rpcInvalid {
		t.Errorf("bad version: %d", code)
	}
	if code := rpcCode(t, h.rpc(tok, "ping", nil, header("MCP-Protocol-Version", "2025-06-18"), header("MCP-Protocol-Version", "2025-03-26")), 400, "two versions"); code != rpcInvalid {
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
		if code := rpcCode(t, h.call("POST", "/mcp", tok, k.body), k.status, k.body); code != k.code {
			t.Errorf("%s: %d, want %d", k.body, code, k.code)
		}
	}

	// transport
	for _, m := range []string{"GET", "DELETE", "PUT"} {
		w := h.call(m, "/mcp", tok, nil, header("Accept", "text/event-stream"))
		if w.Code != 405 || w.Header().Get("Allow") != "POST" {
			t.Errorf("%s: %d %q", m, w.Code, w.Header().Get("Allow"))
		}
	}
	if w := h.rpc(tok, "ping", nil, header("Accept", "text/event-stream")); w.Code != 406 {
		t.Errorf("an event stream only: %d", w.Code)
	}
	for _, acc := range []string{"application/json, text/event-stream", "text/event-stream, application/json;q=0.9", "*/*"} {
		rpcResult(t, h.rpc(tok, "ping", nil, header("Accept", acc)), "Accept "+acc)
	}
	if w := h.rpc(tok, "ping", nil, header("Mcp-Session-Id", "whatever")); w.Code != 200 {
		t.Errorf("a session id is ignored: %d", w.Code)
	}
	if got := f.taken(); len(got) != 0 {
		t.Errorf("the handshake ran a verb: %+v", got)
	}
}

// serverInfo.version is the version the binary was given, and by default
// buildinfo.Version, never "dev" from debug.BuildInfo when the linker set
// one (C14).
func TestServerInfoVersion(t *testing.T) {
	if got := New(Options{}).version; got != buildinfo.Version {
		t.Errorf("the default version: %q, want %q", got, buildinfo.Version)
	}
	if got := New(Options{Version: "abc1234"}).version; got != "abc1234" {
		t.Errorf("the injected version: %q", got)
	}
}

// build.sh's -ldflags -X reaches serverInfo.version through an MCP
// initialize (C14: builds use -buildvcs=false, so the version cannot come
// from debug.BuildInfo).
func TestLinkerSetsServerInfoVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out := filepath.Join(t.TempDir(), "versionprobe")
	pkg := "github.com/stevegeek/agent-factory/internal/buildinfo"
	build := exec.CommandContext(ctx, gobin, "build", "-ldflags=-X "+pkg+".Version=abc1234", "-o", out,
		"github.com/stevegeek/agent-factory/internal/mcp/testdata/versionprobe")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	got, err := exec.CommandContext(ctx, out).Output()
	if err != nil {
		t.Fatalf("versionprobe: %v", err)
	}
	if strings.TrimSpace(string(got)) != "abc1234" {
		t.Fatalf("serverInfo.version = %q, want abc1234", got)
	}
}

func TestMCPAuth(t *testing.T) {
	h, f, tokens := newFakeHarness(t, harnessOpt{})
	tok := tokens[0]
	if w := h.rpc("", "ping", nil); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no token: %d", w.Code)
	}
	if w := h.rpc("fct_wrong", "initialize", map[string]any{"protocolVersion": "2025-06-18"}); w.Code != 401 {
		t.Errorf("a wrong token: %d", w.Code)
	}
	for _, o := range []string{"https://evil.example", "null", "http://example.com"} {
		if w := h.rpc(tok, "ping", nil, header("Origin", o)); w.Code != 403 {
			t.Errorf("Origin %s: %d", o, w.Code)
		}
	}
	rpcResult(t, h.rpc(tok, "ping", nil, header("Origin", "https://example.com")), "this site's Origin")
	if w := h.rpc(tok, "ping", nil, header("Tailscale-Funnel-Request", "?1")); w.Code != 403 {
		t.Errorf("through Funnel: %d", w.Code)
	}
	if w := h.rpc(tok, "ping", nil, remote("203.0.113.9:4000")); w.Code != 403 {
		t.Errorf("off the tailnet: %d", w.Code)
	}
	if w := h.call("POST", "/mcp", tok, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, contentType("text/plain")); w.Code != 415 {
		t.Errorf("text/plain: %d", w.Code)
	}
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", guard.BodyLimit) + `"}}`
	if w := h.call("POST", "/mcp", tok, big); w.Code != 413 {
		t.Errorf("too big: %d", w.Code)
	}
	// a token revoked since the guard read the store
	f.setMode("refuse:unauthorized")
	if w := h.rpc(tok, "tools/list", nil); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("revoked: %d %s", w.Code, w.Body.String())
	}
}

// argsFor are arguments each tool takes, enough to reach the verb.
func argsFor() map[string]map[string]any {
	return map[string]map[string]any{
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
}

func TestMCPRefusalsAreToolResults(t *testing.T) {
	// every tool, every refusal code: isError with what to do next, never a
	// protocol error
	h, f, tokens := newFakeHarness(t, harnessOpt{})
	tok := tokens[0]
	args := argsFor()
	for _, code := range []string{"bad_request", "forbidden", "factory_ticket", "not_on_factory_ticket", "not_found",
		"conflict", "idempotency_mismatch", "too_many", "busy", "storage_full"} {
		f.setMode("refuse:" + code)
		for _, tl := range tools() {
			text := h.toolRefused(tok, tl.name, args[tl.name])
			if !strings.HasPrefix(text, "**Not done:** no (id)\n") || strings.Count(text, "\n") != 2 {
				t.Errorf("%s %s: %q", tl.name, code, text)
			}
		}
	}
	f.setMode("refuse:conflict")
	if text := h.toolRefused(tok, "start_work", args["start_work"]); !strings.Contains(text, "pick another with my_work") {
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
		{"answer_question", "answer", map[string]any{"id": "T0001", "n": 1, "answer": "this", "format": "md"}},
		{"send_to_factory", "send", map[string]any{"id": "T0001", "project": "fac"}},
		{"split_ticket", "split", map[string]any{"id": "T0001", "children": []any{map[string]any{"title": "a"}}, "format": "md"}},
	} {
		a := args[k.tool]
		if k.tool == "ask_person" {
			a = map[string]any{"id": "T0001", "question": "which?", "to": "alice"}
		}
		f.taken()
		h.toolRefused(tok, k.tool, a)
		got := f.taken()
		if len(got) != 1 || got[0].verb != k.verb || got[0].caller != ticketapi.ForToken("example-agent") {
			t.Errorf("%s: calls %+v", k.tool, got)
			continue
		}
		wantIn, _ := json.Marshal(k.in)
		if got[0].raw != string(wantIn) {
			t.Errorf("%s: input %s, want %s", k.tool, got[0].raw, wantIn)
		}
		if got[0].wait != 10*time.Second {
			t.Errorf("%s: wait %v", k.tool, got[0].wait)
		}
	}
	// failures of the factory's own: a protocol error with no detail
	for _, m := range []string{"crash", "panic", "refuse:something_new"} {
		f.setMode(m)
		w := h.rpc(tok, "tools/call", map[string]any{"name": "get_ticket", "arguments": args["get_ticket"]})
		if code := rpcCode(t, w, 200, m); code != rpcInternal || strings.Contains(w.Body.String(), "Traceback") || strings.Contains(w.Body.String(), "something_new") {
			t.Errorf("%s: %d %s", m, code, w.Body.String())
		}
	}
}

func TestMCPBadArguments(t *testing.T) {
	h, f, tokens := newFakeHarness(t, harnessOpt{})
	tok := tokens[0]
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
		{"file_ticket", map[string]any{"project": "sandbox", "title": strings.Repeat(string(rune(0xe9)), 301)}},
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
		w := h.rpc(tok, "tools/call", map[string]any{"name": k.name, "arguments": k.args})
		if code := rpcCode(t, w, 200, k.name); code != rpcBadParams {
			t.Errorf("%s %v: %d", k.name, k.args, code)
		}
	}
	if code := rpcCode(t, h.rpc(tok, "tools/call", map[string]any{"arguments": map[string]any{}}), 200, "no name"); code != rpcBadParams {
		t.Errorf("no name: %d", code)
	}
	if got := f.taken(); len(got) != 0 {
		t.Errorf("bad arguments reached a verb: %+v", got)
	}
	// no arguments at all is no arguments
	f.setMode("refuse:forbidden")
	h.toolRefused(tok, "my_work", nil)
}

func TestMCPWriteRate(t *testing.T) {
	h, f, tokens := newFakeHarness(t, harnessOpt{all: guard.Rate{PerMin: 6000, Burst: 1000}, writes: guard.Rate{PerMin: 1, Burst: 2}})
	tok := tokens[0]
	args := argsFor()
	f.setMode("refuse:forbidden")
	h.toolRefused(tok, "post_update", args["post_update"])
	h.toolRefused(tok, "start_work", args["start_work"])
	w := h.rpc(tok, "tools/call", map[string]any{"name": "post_update", "arguments": args["post_update"]})
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || errCode(t, w) != "too_many" {
		t.Errorf("the third write: %d %q %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	// reads still go
	h.toolRefused(tok, "get_ticket", args["get_ticket"])
	rpcResult(t, h.rpc(tok, "ping", nil), "ping")
	// and a write over /api/v1, admitted by the same guard, draws on the
	// same bucket
	r := httptest.NewRequest("POST", "/api/v1/tickets/T0001/notes", strings.NewReader(`{"text":"x"}`))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+tok)
	aw := httptest.NewRecorder()
	if h.guard.Admit(aw, r, true) != "" || aw.Code != 429 {
		t.Errorf("the API shares the write rate: %d", aw.Code)
	}
}

func TestMCPBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, f, tokens := newFakeHarness(t, harnessOpt{slotWait: 300 * time.Millisecond})
		f.release = make(chan struct{})
		f.setMode("block")
		var wg sync.WaitGroup
		for _, tok := range tokens[1:] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.tool(tok, "get_ticket", argsFor()["get_ticket"])
			}()
		}
		synctest.Wait() // both slots held
		if text := h.toolRefused(tokens[0], "get_ticket", argsFor()["get_ticket"]); !strings.Contains(text, "busy") || !strings.Contains(text, "Try again") {
			t.Errorf("no free slot: %s", text)
		}
		close(f.release)
		wg.Wait()
	})
}

func TestMCPToolsListMatchesTools(t *testing.T) {
	if n := len(tools()); n != 13 {
		t.Fatalf("13 tools: %d", n)
	}
	seen := map[string]bool{}
	for _, tl := range tools() {
		if seen[tl.name] {
			t.Errorf("%s twice", tl.name)
		}
		seen[tl.name] = true
		if _, ok := argsFor()[tl.name]; !ok {
			t.Errorf("%s: no arguments in argsFor", tl.name)
		}
	}
	// the column enum: board columns first, then the factory's, each once
	cols := allColumns()
	if cols[0] != "inbox" || len(cols) != 18 {
		t.Errorf("columns: %v", cols)
	}
}
