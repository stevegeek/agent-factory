package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const apiConfig = `[board]
people = ["alice"]

[projects.sandbox]
mode = "board"

[projects.other]
mode = "board"

[projects.fac]
path = "/nonexistent"
github = "o/fac"
`

// apiHarness: a scratch factory root and token store, the real bin/factory, and Foreman's handler.
type apiHarness struct {
	t                    *testing.T
	root, store, factory string
	h                    http.Handler
	guard                *Guard
	runner               *APIRunner
}

func newAPIHarness(t *testing.T) *apiHarness {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed to run the factory CLI behind the API")
	}
	factory, err := filepath.Abs("../bin/factory")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "conf")
	os.Mkdir(cfg, 0o700)
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(apiConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &apiHarness{t: t, root: root, store: filepath.Join(cfg, "api-tokens.json"), factory: factory}
	h.serve(factory, false)
	return h
}

func (h *apiHarness) serve(factory string, open bool) {
	a := NewAuth([]byte(testSecret))
	a.delay = 0
	a.AllowHosts("example.com")
	s := NewServer(NewBoard(h.root), a, &Runner{Factory: factory, Root: h.root})
	h.guard = NewGuard(NewAPITokens(h.store), a.hosts, open)
	// room for a whole workflow in one test; TestAPIGuessingAndRate puts the real limits back
	h.guard.all, h.guard.writes = bucketSpec{perMin: 6000, burst: 1000}, bucketSpec{perMin: 6000, burst: 1000}
	h.runner = NewAPIRunner(factory, h.root, h.store, 2)
	s.api = NewAPI(h.guard, h.runner)
	h.h = s.Handler()
}

func (h *apiHarness) cli(stdin string, args ...string) string {
	cmd := exec.Command(h.factory, args...)
	cmd.Env = append(apiEnv(), "FACTORY_ROOT="+h.root, "FACTORY_API_TOKENS="+h.store)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("factory %v: %v %s", args, err, out)
	}
	return string(out)
}

func (h *apiHarness) mint(name, can string, projects ...string) string {
	args := []string{"tokens", "add", name, "--can", can}
	for _, p := range projects {
		args = append(args, "--project", p)
	}
	return strings.TrimSpace(h.cli("", args...))
}

type reqOpt func(*http.Request)

func header(k, v string) reqOpt    { return func(r *http.Request) { r.Header.Add(k, v) } }
func remote(addr string) reqOpt    { return func(r *http.Request) { r.RemoteAddr = addr } }
func hostIs(host string) reqOpt    { return func(r *http.Request) { r.Host = host } }
func cookie(c *http.Cookie) reqOpt { return func(r *http.Request) { r.AddCookie(c) } }
func contentType(ct string) reqOpt { return func(r *http.Request) { r.Header.Set("Content-Type", ct) } }

func (h *apiHarness) call(method, target, token string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	var payload string
	switch b := body.(type) {
	case nil:
	case string:
		payload = b
	default:
		data, _ := json.Marshal(b)
		payload = string(data)
	}
	r := httptest.NewRequest(method, target, strings.NewReader(payload))
	r.RemoteAddr = "127.0.0.1:5555"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON (%d): %s", w.Code, w.Body.String())
	}
	return out
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	e, _ := decodeJSON(t, w)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func want(t *testing.T, w *httptest.ResponseRecorder, status int, what string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: %d, want %d: %s", what, w.Code, status, w.Body.String())
	}
	return decodeJSON(t, w)
}

func ticketOf(out map[string]any) map[string]any {
	t, _ := out["ticket"].(map[string]any)
	return t
}

func TestAPIEndToEnd(t *testing.T) {
	h := newAPIHarness(t)
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	helper := h.mint("helper-agent", "read,talk", "sandbox")

	me := want(t, h.call("GET", "/api/v1/me", agent, nil), 200, "me")
	if me["name"] != "example-agent" {
		t.Errorf("me: %v", me)
	}
	ps := want(t, h.call("GET", "/api/v1/projects", agent, nil), 200, "projects")
	if names := projectNames(ps); names != "sandbox,fac" && names != "fac,sandbox" {
		t.Errorf("projects: %s", names)
	}

	body := map[string]any{"project": "sandbox", "title": "Fix the date picker", "body": "It shows the wrong month."}
	w := h.call("POST", "/api/v1/tickets", agent, body, header("Idempotency-Key", "run-1"))
	out := want(t, w, 201, "create")
	id, _ := ticketOf(out)["id"].(string)
	if !apiTicketID.MatchString(id) || w.Header().Get("Location") != "/api/v1/tickets/"+id || out["created"] != true {
		t.Fatalf("create: %v, Location %q", out, w.Header().Get("Location"))
	}
	w = h.call("POST", "/api/v1/tickets", agent, body, header("Idempotency-Key", "run-1"))
	if out = want(t, w, 200, "replay"); ticketOf(out)["id"] != id || out["created"] != false || w.Header().Get("Location") != "" {
		t.Errorf("a replay answers the same ticket: %v", out)
	}
	changed := map[string]any{"project": "sandbox", "title": "Something else"}
	if w := h.call("POST", "/api/v1/tickets", agent, changed, header("Idempotency-Key", "run-1")); w.Code != 422 || errCode(t, w) != "idempotency_mismatch" {
		t.Errorf("same key, another request: %d %s", w.Code, w.Body.String())
	}
	// the header wins over the body's key
	w = h.call("POST", "/api/v1/tickets", agent, map[string]any{"project": "sandbox", "title": "Fix the date picker",
		"body": "It shows the wrong month.", "idempotency_key": "other"}, header("Idempotency-Key", "run-1"))
	if out = want(t, w, 200, "header key"); ticketOf(out)["id"] != id {
		t.Errorf("the Idempotency-Key header should win: %v", out)
	}

	list := want(t, h.call("GET", "/api/v1/tickets?project=sandbox&q=date+picker&limit=10", agent, nil), 200, "list")
	if ts, _ := list["tickets"].([]any); len(ts) != 1 || ts[0].(map[string]any)["id"] != id {
		t.Errorf("list: %v", list)
	}
	if _, ok := list["next"]; !ok {
		t.Errorf("list carries next: %v", list)
	}
	got := want(t, h.call("GET", "/api/v1/tickets/"+id, agent, nil), 200, "get")
	if ticketOf(got)["title"] != "Fix the date picker" || ticketOf(got)["column"] != "inbox" {
		t.Errorf("get: %v", got)
	}
	md := want(t, h.call("GET", "/api/v1/tickets/"+id+"?format=md", agent, nil), 200, "get md")
	if s, _ := md["markdown"].(string); !strings.HasPrefix(s, "# "+id+" · Fix the date picker") {
		t.Errorf("markdown: %v", md)
	}

	upd := want(t, h.call("PATCH", "/api/v1/tickets/"+id, agent, map[string]any{"labels": []string{"ui"}}), 200, "update")
	if ls, _ := ticketOf(upd)["labels"].([]any); len(ls) != 1 || ls[0] != "ui" {
		t.Errorf("update: %v", upd)
	}
	cl := want(t, h.call("POST", "/api/v1/tickets/"+id+"/claim", agent, map[string]any{}), 200, "claim")
	if ticketOf(cl)["column"] != "doing" || ticketOf(cl)["assignee"] != "example-agent" {
		t.Errorf("claim: %v", cl)
	}
	want(t, h.call("POST", "/api/v1/tickets/"+id+"/notes", agent, map[string]any{"text": "Looking at the locale first."}), 201, "note")
	ask := want(t, h.call("POST", "/api/v1/tickets/"+id+"/ask", agent, map[string]any{"question": "Which locale?",
		"options": []string{"en-GB", "en-US"}, "recipient": "helper-agent"}), 201, "ask")
	if ask["n"] != float64(1) || ticketOf(ask)["column"] != "waiting" {
		t.Errorf("ask: %v", ask)
	}
	work := want(t, h.call("GET", "/api/v1/work?project=sandbox", helper, nil), 200, "work")
	groups, _ := work["groups"].(map[string]any)
	if mine, _ := groups["asked_of_me"].([]any); len(mine) != 1 || mine[0].(map[string]any)["id"] != id {
		t.Errorf("work for the recipient: %v", work)
	}
	ans := want(t, h.call("POST", "/api/v1/tickets/"+id+"/answer", helper, map[string]any{"n": 1, "answer": "en-GB"}), 200, "answer")
	if ticketOf(ans)["column"] != "doing" {
		t.Errorf("an answer takes the ticket back to doing: %v", ans)
	}
	done := want(t, h.call("POST", "/api/v1/tickets/"+id+"/move", agent, map[string]any{"to": "done", "result": "Fixed; the picker uses en-GB."}), 200, "move")
	if ticketOf(done)["column"] != "done" || ticketOf(done)["result"] == nil {
		t.Errorf("move to done: %v", done)
	}

	parent := ticketOf(want(t, h.call("POST", "/api/v1/tickets", agent, map[string]any{"project": "sandbox", "title": "Calendar"}), 201, "create parent"))["id"].(string)
	sp := want(t, h.call("POST", "/api/v1/tickets/"+parent+"/split", agent, map[string]any{"children": []map[string]any{{"title": "Week view"}, {"title": "Month view"}}}), 201, "split")
	if kids, _ := sp["children"].([]any); len(kids) != 2 {
		t.Errorf("split: %v", sp)
	}
	sent := want(t, h.call("POST", "/api/v1/tickets/"+parent+"/send", agent, map[string]any{"project": "fac", "note": "Needs the factory."}), 201, "send")
	if ft, _ := sent["factory_ticket"].(map[string]any); ft["project"] != "fac" || ft["column"] != "triage" {
		t.Errorf("send: %v", sent)
	}
}

func projectNames(out map[string]any) string {
	var names []string
	ps, _ := out["projects"].([]any)
	for _, p := range ps {
		names = append(names, p.(map[string]any)["name"].(string))
	}
	return strings.Join(names, ",")
}

// Capabilities, other projects and factory tickets: every refusal comes from the CLI, mapped to its status.
func TestAPIRefusals(t *testing.T) {
	h := newAPIHarness(t)
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	reader := h.mint("reader-agent", "read", "sandbox")
	mine := ticketOf(want(t, h.call("POST", "/api/v1/tickets", agent, map[string]any{"project": "sandbox", "title": "Board work"}), 201, "create"))["id"].(string)
	var hidden struct{ Ticket struct{ ID string } }
	json.Unmarshal([]byte(h.cli(`{"project": "other", "title": "Not yours"}`, "ticket", "create", "--json")), &hidden)
	if hidden.Ticket.ID == "" {
		t.Fatal("the maintainer's ticket in another project")
	}
	fac := ticketOf(want(t, h.call("POST", "/api/v1/tickets", agent, map[string]any{"project": "fac", "title": "A change"}), 201, "create on a factory project"))
	if fac["column"] != "triage" {
		t.Errorf("a token's factory ticket starts in triage: %v", fac)
	}
	facID := fac["id"].(string)

	type c struct {
		name, method, path, token string
		body                      any
		status                    int
		code                      string
	}
	cases := []c{
		{"read-only create", "POST", "/api/v1/tickets", reader, map[string]any{"project": "sandbox", "title": "x"}, 403, "forbidden"},
		{"read-only note", "POST", "/api/v1/tickets/" + mine + "/notes", reader, map[string]any{"text": "hi"}, 403, "forbidden"},
		{"read-only move", "POST", "/api/v1/tickets/" + mine + "/move", reader, map[string]any{"to": "next"}, 403, "forbidden"},
		{"read-only claim", "POST", "/api/v1/tickets/" + mine + "/claim", reader, map[string]any{}, 403, "forbidden"},
		{"read-only update", "PATCH", "/api/v1/tickets/" + mine, reader, map[string]any{"title": "y"}, 403, "forbidden"},
		{"read-only split", "POST", "/api/v1/tickets/" + mine + "/split", reader, map[string]any{"children": []map[string]any{{"title": "a"}}}, 403, "forbidden"},
		{"read-only send", "POST", "/api/v1/tickets/" + mine + "/send", reader, map[string]any{"project": "fac"}, 403, "forbidden"},
		{"create in another project", "POST", "/api/v1/tickets", agent, map[string]any{"project": "other", "title": "x"}, 404, "not_found"},
		{"factory move", "POST", "/api/v1/tickets/" + facID + "/move", agent, map[string]any{"to": "done"}, 403, "factory_ticket"},
		{"factory claim", "POST", "/api/v1/tickets/" + facID + "/claim", agent, map[string]any{}, 403, "factory_ticket"},
		{"factory update", "PATCH", "/api/v1/tickets/" + facID, agent, map[string]any{"title": "y"}, 403, "factory_ticket"},
		{"factory split", "POST", "/api/v1/tickets/" + facID + "/split", agent, map[string]any{"children": []map[string]any{{"title": "a"}}}, 403, "factory_ticket"},
		{"factory send", "POST", "/api/v1/tickets/" + facID + "/send", agent, map[string]any{"project": "fac"}, 403, "factory_ticket"},
		{"factory ask", "POST", "/api/v1/tickets/" + facID + "/ask", agent, map[string]any{"question": "?"}, 403, "not_on_factory_ticket"},
		{"factory answer", "POST", "/api/v1/tickets/" + facID + "/answer", agent, map[string]any{"n": 1, "answer": "x"}, 403, "forbidden"},
		{"bad kind on a factory project", "POST", "/api/v1/tickets", agent, map[string]any{"project": "fac", "title": "x", "kind": "task"}, 400, "bad_request"},
		{"unknown key", "POST", "/api/v1/tickets/" + mine + "/notes", agent, map[string]any{"text": "hi", "by": "alice"}, 400, "bad_request"},
		{"answer nothing asked", "POST", "/api/v1/tickets/" + mine + "/answer", agent, map[string]any{"n": 1, "answer": "x"}, 400, "bad_request"},
	}
	// every verb on a ticket in a project the token does not name: not found, never forbidden
	for _, v := range []struct {
		method, suffix string
		body           any
	}{{"GET", "", nil}, {"PATCH", "", map[string]any{"title": "y"}}, {"POST", "/notes", map[string]any{"text": "hi"}},
		{"POST", "/move", map[string]any{"to": "next"}}, {"POST", "/claim", map[string]any{}},
		{"POST", "/ask", map[string]any{"question": "?"}}, {"POST", "/answer", map[string]any{"n": 1, "answer": "x"}},
		{"POST", "/split", map[string]any{"children": []map[string]any{{"title": "a"}}}}, {"POST", "/send", map[string]any{"project": "fac"}}} {
		cases = append(cases, c{"hidden " + v.method + v.suffix, v.method, "/api/v1/tickets/" + hidden.Ticket.ID + v.suffix, agent, v.body, 404, "not_found"})
	}
	for _, k := range cases {
		w := h.call(k.method, k.path, k.token, k.body)
		if w.Code != k.status || (k.code != "" && errCode(t, w) != k.code) {
			t.Errorf("%s: %d %s, want %d %s", k.name, w.Code, w.Body.String(), k.status, k.code)
		}
		if k.status == 404 && strings.Contains(w.Body.String(), "Not yours") {
			t.Errorf("%s: a hidden ticket's text leaked: %s", k.name, w.Body.String())
		}
	}
	// a hidden ticket is not listed, not even when its project is asked for by name
	for _, q := range []string{"", "?project=other", "?include_closed=true"} {
		if w := h.call("GET", "/api/v1/tickets"+q, agent, nil); strings.Contains(w.Body.String(), hidden.Ticket.ID) {
			t.Errorf("list%s shows a hidden ticket: %s", q, w.Body.String())
		}
	}
	if w := h.call("GET", "/api/v1/work?project=other", agent, nil); strings.Contains(w.Body.String(), hidden.Ticket.ID) {
		t.Errorf("work shows a hidden ticket: %s", w.Body.String())
	}
	if ps := want(t, h.call("GET", "/api/v1/projects", reader, nil), 200, "projects"); projectNames(ps) != "sandbox" {
		t.Errorf("a token sees its own projects only: %v", ps)
	}
}

func TestAPIAuth(t *testing.T) {
	h := newAPIHarness(t)
	good := h.mint("example-agent", "read", "sandbox")
	revoked := h.mint("gone-agent", "read", "sandbox")
	h.cli("", "tokens", "revoke", "gone-agent")
	expiring := h.mint("old-agent", "read", "sandbox")
	setExpiry(t, h.store, "old-agent", "2020-01-01T00:00:00+00:00")

	r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"secret": {testSecret}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sw := httptest.NewRecorder()
	h.h.ServeHTTP(sw, r)
	var session *http.Cookie
	for _, c := range sw.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session")
	}

	for _, k := range []struct {
		name   string
		token  string
		opts   []reqOpt
		status int
	}{
		{"no header", "", nil, 401},
		{"basic", "", []reqOpt{header("Authorization", "Basic "+good)}, 401},
		{"bearer without a token", "", []reqOpt{header("Authorization", "Bearer")}, 401},
		{"two headers", good, []reqOpt{header("Authorization", "Bearer "+good)}, 401},
		{"wrong token", "fct_not-a-real-token", nil, 401},
		{"revoked", revoked, nil, 401},
		{"expired", expiring, nil, 401},
		{"session cookie alone", "", []reqOpt{cookie(session)}, 401},
		{"a good token", good, nil, 200},
		{"lower-case scheme", "", []reqOpt{header("Authorization", "bearer "+good)}, 200},
		{"wrong Host", good, []reqOpt{hostIs("rebound.example")}, 421},
		{"foreign Origin", good, []reqOpt{header("Origin", "https://evil.example")}, 403},
		{"null Origin", good, []reqOpt{header("Origin", "null")}, 403},
		{"plain http Origin", good, []reqOpt{header("Origin", "http://example.com")}, 403},
		{"own Origin", good, []reqOpt{header("Origin", "https://example.com")}, 200},
		{"funnel", good, []reqOpt{header("Tailscale-Funnel-Request", "?1")}, 403},
		{"proxied from the internet", good, []reqOpt{header("X-Forwarded-For", "203.0.113.9")}, 403},
		{"proxied from the tailnet", good, []reqOpt{header("X-Forwarded-For", "100.64.0.9")}, 200},
		{"off the tailnet", good, []reqOpt{remote("192.0.2.1:5555")}, 403},
		{"a tailnet peer while on loopback", good, []reqOpt{remote("100.64.0.5:5555")}, 403},
	} {
		w := h.call("GET", "/api/v1/me", k.token, nil, k.opts...)
		if w.Code != k.status {
			t.Errorf("%s: %d, want %d: %s", k.name, w.Code, k.status, w.Body.String())
		}
		if k.status == 401 && w.Header().Get("WWW-Authenticate") != `Bearer realm="factory"` {
			t.Errorf("%s: no WWW-Authenticate", k.name)
		}
		if k.status != 421 && (w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
			w.Header().Get("Content-Security-Policy") == "") {
			t.Errorf("%s: the security headers apply: %v", k.name, w.Header())
		}
	}
	// a token is no login for the app
	if w := h.call("GET", "/", good, nil); w.Code != http.StatusSeeOther {
		t.Errorf("a bearer token must not open the app: %d", w.Code)
	}

	h.serve(h.factory, true)
	if w := h.call("GET", "/api/v1/me", good, nil, remote("100.64.0.5:5555")); w.Code != 200 {
		t.Errorf("listening on the tailnet, a tailnet peer may call: %d", w.Code)
	}
	if w := h.call("GET", "/api/v1/me", good, nil, remote("[fd7a:115c:a1e0::5]:5555")); w.Code != 200 {
		t.Errorf("a tailnet IPv6 peer: %d", w.Code)
	}
	if w := h.call("GET", "/api/v1/me", good, nil, remote("192.0.2.1:5555")); w.Code != 403 {
		t.Errorf("listening on the tailnet, anyone else is refused: %d", w.Code)
	}
}

func setExpiry(t *testing.T, store, name, at string) {
	t.Helper()
	data, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	json.Unmarshal(data, &f)
	for _, r := range f["tokens"].([]any) {
		if rec := r.(map[string]any); rec["name"] == name {
			rec["expires"] = at
		}
	}
	data, _ = json.Marshal(f)
	if err := os.WriteFile(store, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAPIGuessingAndRate(t *testing.T) {
	h := newAPIHarness(t)
	good := h.mint("example-agent", "read,talk", "sandbox")
	h.guard.all, h.guard.writes = apiAll, apiWrites
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	h.guard.now = func() time.Time { return at }
	guesser := header("X-Forwarded-For", "100.64.0.66")
	for i := 0; i < apiFailLimit; i++ {
		if w := h.call("GET", "/api/v1/me", "fct_guess", nil, guesser); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	if w := h.call("GET", "/api/v1/me", good, nil, guesser); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Errorf("the guessing address waits, even with a good token: %d", w.Code)
	}
	if w := h.call("GET", "/api/v1/me", good, nil, header("X-Forwarded-For", "100.64.0.67")); w.Code != 200 {
		t.Errorf("another address still gets in: %d", w.Code)
	}
	at = at.Add(apiFailWindow + time.Second)
	if w := h.call("GET", "/api/v1/me", good, nil, guesser); w.Code != 200 {
		t.Errorf("the block ends with the window: %d", w.Code)
	}

	// writes: a burst of 10, then wait; an id that does not parse stops before the CLI but still counts
	for i := 0; i < int(apiWrites.burst); i++ {
		if w := h.call("POST", "/api/v1/tickets/BAD/notes", good, map[string]any{"text": "x"}); w.Code != 404 {
			t.Fatalf("write %d: %d", i, w.Code)
		}
	}
	w := h.call("POST", "/api/v1/tickets/BAD/notes", good, map[string]any{"text": "x"})
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Errorf("past the write burst: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := h.call("GET", "/api/v1/tickets/BAD", good, nil); w.Code != 404 {
		t.Errorf("reads go on while writes wait: %d", w.Code)
	}
	at = at.Add(2 * time.Second)
	if w := h.call("POST", "/api/v1/tickets/BAD/notes", good, map[string]any{"text": "x"}); w.Code != 404 {
		t.Errorf("a write after the wait: %d", w.Code)
	}
	// reads: a burst of 30 in all
	for i := 0; i < int(apiAll.burst); i++ {
		h.call("GET", "/api/v1/tickets/BAD", good, nil)
	}
	if w := h.call("GET", "/api/v1/tickets/BAD", good, nil); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Errorf("past the burst: %d", w.Code)
	}
}

func TestAPIBodyAndRoutes(t *testing.T) {
	h := newAPIHarness(t)
	good := h.mint("example-agent", "read,talk", "sandbox")
	big := `{"text": "` + strings.Repeat("a", apiBody) + `"}`
	if w := h.call("POST", "/api/v1/tickets/T0001/notes", good, big); w.Code != 413 {
		t.Errorf("a body over 256 KiB: %d", w.Code)
	}
	if w := h.call("POST", "/api/v1/tickets/T0001/notes", good, `{"text": "x"}`, contentType("text/plain")); w.Code != 415 {
		t.Errorf("not JSON: %d", w.Code)
	}
	if w := h.call("POST", "/api/v1/tickets/T0001/notes", good, `{"text": "x"}`, contentType("application/json; charset=utf-8")); w.Code == 415 {
		t.Errorf("a charset is fine: %d", w.Code)
	}
	if w := h.call("POST", "/api/v1/tickets/T0001/notes", good, `[1, 2]`); w.Code != 400 {
		t.Errorf("a body that is not an object: %d", w.Code)
	}
	for _, k := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/api/v1/nothing", 404, ""},
		{"GET", "/api/v1/", 404, ""},
		{"GET", "/api/v1/tickets/T0001/extra/more", 404, ""},
		{"GET", "/api/v1/tickets/T0001/whatever", 404, ""},
		{"DELETE", "/api/v1/tickets/T0001", 405, "GET, PATCH"},
		{"GET", "/api/v1/tickets/T0001/notes", 405, "POST"},
		{"PUT", "/api/v1/tickets", 405, "GET, POST"},
		{"GET", "/api/v1/me?project=sandbox", 400, ""},
		{"GET", "/api/v1/tickets?limit=many", 400, ""},
		{"GET", "/api/v1/tickets?include_closed=maybe", 400, ""},
		{"GET", "/api/v1/tickets?updated_since=yesterday", 400, ""},
		{"GET", "/api/v1/tickets?project=a&project=b", 400, ""},
	} {
		var body any
		if k.method != "GET" {
			body = map[string]any{}
		}
		w := h.call(k.method, k.path, good, body)
		if w.Code != k.status || w.Header().Get("Allow") != k.allow || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: %d Allow %q %s", k.method, k.path, w.Code, w.Header().Get("Allow"), w.Body.String())
		}
	}
	if w := h.call("GET", "/api/v1/tickets?updated_since=2026-10-01T00:00:00Z&include_closed=true&limit=5", good, nil); w.Code != 200 {
		t.Errorf("typed query values: %d %s", w.Code, w.Body.String())
	}
}

func TestAPIStorePermissions(t *testing.T) {
	h := newAPIHarness(t)
	good := h.mint("example-agent", "read", "sandbox")
	if err := os.Chmod(h.store, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := h.call("GET", "/api/v1/me", good, nil); w.Code != 401 {
		t.Errorf("a store others can read holds no tokens: %d", w.Code)
	}
	if info, _ := os.Stat(h.store); info.Mode().Perm() != 0o644 {
		t.Errorf("Foreman must not change the store: %v", info.Mode())
	}
	os.Chmod(h.store, 0o600)
	if w := h.call("GET", "/api/v1/me", good, nil); w.Code != 200 {
		t.Errorf("back at 0600: %d", w.Code)
	}
}

// ---------------------------------------------------------------- with a fake factory

// fakeAPI: the API over a script that answers as its mode file says and records argv, stdin and env.
type fakeAPI struct {
	*apiHarness
	dir, token string
}

const fakeFactory = `#!/bin/sh
d=$(dirname "$0")
{ for a in "$@"; do printf '%s\037' "$a"; done; printf '\n'; } >> "$d/argv"
cat > "$d/stdin"
printf '%s\n%s\n' "$FACTORY_ROOT" "$FACTORY_API_TOKENS" > "$d/env"
mode=$(cat "$d/mode")
case "$mode" in
ok) echo '{"ok": true, "created": true, "ticket": {"id": "T0007"}}' ;;
replay) echo '{"ok": true, "created": false, "ticket": {"id": "T0007"}}' ;;
refuse:*) echo "{\"ok\": false, \"error\": {\"code\": \"${mode#refuse:}\", \"message\": \"no\", \"field\": \"id\"}}"; exit 1 ;;
crash) echo 'Traceback (most recent call last): boom' >&2; exit 3 ;;
usage) echo 'usage: factory' >&2; exit 2 ;;
nojson) echo 'something went wrong' >&2; exit 1 ;;
okexit1) echo '{"ok": true}'; exit 1 ;;
hang) exec sleep 5 ;;
slow) sleep 1; echo '{"ok": true, "name": "x"}' ;;
big) head -c 5000 /dev/zero | tr '\0' 'a' ;;
esac
`

func newFakeAPI(t *testing.T) *fakeAPI {
	dir := t.TempDir()
	script := filepath.Join(dir, "factory")
	os.WriteFile(script, []byte(fakeFactory), 0o755)
	cfg := filepath.Join(t.TempDir(), "conf")
	os.Mkdir(cfg, 0o700)
	store := filepath.Join(cfg, "api-tokens.json")
	token := "fct_fake-token-for-tests"
	sum := sha256.Sum256([]byte(token))
	data, _ := json.Marshal(map[string]any{"version": 1, "tokens": []map[string]any{{"name": "example-agent",
		"sha256": hex.EncodeToString(sum[:]), "projects": []string{"sandbox"}, "can": []string{"read"}, "expires": nil}}})
	os.WriteFile(store, data, 0o600)
	h := &apiHarness{t: t, root: t.TempDir(), store: store, factory: script}
	h.serve(script, false)
	f := &fakeAPI{apiHarness: h, dir: dir, token: token}
	f.mode("ok")
	return f
}

func (f *fakeAPI) mode(m string) { os.WriteFile(filepath.Join(f.dir, "mode"), []byte(m), 0o644) }

func (f *fakeAPI) argv() [][]string {
	data, _ := os.ReadFile(filepath.Join(f.dir, "argv"))
	os.Remove(filepath.Join(f.dir, "argv"))
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, strings.Split(strings.TrimSuffix(line, "\x1f"), "\x1f"))
		}
	}
	return out
}

func (f *fakeAPI) stdin() map[string]any {
	data, _ := os.ReadFile(filepath.Join(f.dir, "stdin"))
	var out map[string]any
	json.Unmarshal(data, &out)
	return out
}

func TestAPIArgvAndInput(t *testing.T) {
	f := newFakeAPI(t)
	w := f.call("POST", "/api/v1/tickets/T0001/notes", f.token, map[string]any{"text": "--as maintainer; rm -rf /"})
	want(t, w, 201, "note")
	got := f.argv()
	if len(got) != 1 || strings.Join(got[0], " ") != "ticket note --as example-agent --wait 10 --json" {
		t.Errorf("argv: %q", got)
	}
	if in := f.stdin(); in["id"] != "T0001" || in["text"] != "--as maintainer; rm -rf /" || len(in) != 2 {
		t.Errorf("stdin: %v", in)
	}
	env, _ := os.ReadFile(filepath.Join(f.dir, "env"))
	if string(env) != f.root+"\n"+f.store+"\n" {
		t.Errorf("env: %q", env)
	}
	// ids that are not ids, and a body naming the id, never reach the CLI
	for _, k := range []struct {
		path   string
		body   any
		status int
	}{
		{"/api/v1/tickets/T0001;rm/notes", map[string]any{"text": "x"}, 404},
		{"/api/v1/tickets/T0001%20--as%20alice/notes", map[string]any{"text": "x"}, 404},
		{"/api/v1/tickets/t0001/notes", map[string]any{"text": "x"}, 404},
		{"/api/v1/tickets/T0001/notes", map[string]any{"text": "x", "id": "T0002"}, 400},
		{"/api/v1/tickets/T0001/notes?id=T0002", map[string]any{"text": "x"}, 400},
	} {
		if w := f.call("POST", k.path, f.token, k.body); w.Code != k.status {
			t.Errorf("%s: %d %s", k.path, w.Code, w.Body.String())
		}
	}
	if got := f.argv(); len(got) != 0 {
		t.Errorf("the CLI ran for a bad request: %q", got)
	}
	// the query becomes typed input; q is text
	want(t, f.call("GET", "/api/v1/tickets?q=date&limit=5&include_closed=true&assignee=me", f.token, nil), 200, "list")
	if in := f.stdin(); in["text"] != "date" || in["limit"] != float64(5) || in["include_closed"] != true || in["assignee"] != "me" {
		t.Errorf("list input: %v", in)
	}
	if got := f.argv(); got[0][1] != "list" {
		t.Errorf("list verb: %q", got)
	}
	w = f.call("POST", "/api/v1/tickets", f.token, map[string]any{"project": "sandbox", "title": "x"}, header("Idempotency-Key", "k-1"))
	if w.Code != 201 || w.Header().Get("Location") != "/api/v1/tickets/T0007" || f.stdin()["idempotency_key"] != "k-1" {
		t.Errorf("create: %d %q %v", w.Code, w.Header().Get("Location"), f.stdin())
	}
	f.mode("replay")
	if w := f.call("POST", "/api/v1/tickets", f.token, map[string]any{"project": "sandbox", "title": "x"}); w.Code != 200 || w.Header().Get("Location") != "" {
		t.Errorf("replay: %d %q", w.Code, w.Header().Get("Location"))
	}
	if out := decodeJSON(t, w); out["ok"] != nil {
		t.Errorf("ok is the CLI's, not the API's: %v", out)
	}
}

func TestAPIExitMapping(t *testing.T) {
	f := newFakeAPI(t)
	for code, status := range apiStatus {
		f.mode("refuse:" + code)
		w := f.call("GET", "/api/v1/tickets/T0001", f.token, nil)
		if w.Code != status || errCode(t, w) != code {
			t.Errorf("%s: %d %s", code, w.Code, w.Body.String())
		}
		e := decodeJSON(t, w)["error"].(map[string]any)
		if e["message"] != "no" || e["field"] != "id" {
			t.Errorf("%s: the message and field are relayed: %v", code, e)
		}
		if ra := w.Header().Get("Retry-After"); (code == "busy") != (ra == "30") {
			t.Errorf("%s: Retry-After %q", code, ra)
		}
	}
	for _, m := range []string{"refuse:something_new", "crash", "usage", "nojson", "okexit1", "big", "hang"} {
		f.mode(m)
		if m == "big" {
			f.runner.maxOut = 1000
		}
		if m == "hang" {
			f.runner.timeout = 200 * time.Millisecond
		}
		w := f.call("GET", "/api/v1/tickets/T0001", f.token, nil)
		if w.Code != 500 || errCode(t, w) != "internal" || strings.Contains(w.Body.String(), "Traceback") || strings.Contains(w.Body.String(), "usage") {
			t.Errorf("%s: %d %s", m, w.Code, w.Body.String())
		}
	}
}

func TestAPIConcurrency(t *testing.T) {
	f := newFakeAPI(t)
	f.mode("slow")
	f.runner.slotWait = 300 * time.Millisecond
	codes := make([]int, 3)
	retry := make([]string, 3)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i == 2 {
				time.Sleep(100 * time.Millisecond) // the first two hold both slots
			}
			w := f.call("GET", "/api/v1/me", f.token, nil)
			codes[i], retry[i] = w.Code, w.Header().Get("Retry-After")
		}(i)
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 503 || retry[2] != "5" {
		t.Errorf("two at a time, the third waits and then gives up: %v %v", codes, retry)
	}
	// a waiting request takes the slot once one is free
	f.runner.slotWait = 3 * time.Second
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.call("GET", "/api/v1/me", f.token, nil).Code
		}(i)
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 200 {
		t.Errorf("within the wait, all go through: %v", codes)
	}
}
