package api

import (
	"os"
	"strings"
	"testing"
	"time"
)

// These are Foreman's tests that ran the real Python CLI behind the API,
// now in-process against ticketapi.Service on a temp root, with their
// assertions kept.

func TestAPIEndToEnd(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
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
	if !ticketID.MatchString(id) || w.Header().Get("Location") != "/api/v1/tickets/"+id || out["created"] != true {
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

// Capabilities, other projects and factory tickets: every refusal comes
// from the verbs, mapped to its status.
func TestAPIRefusals(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	reader := h.mint("reader-agent", "read", "sandbox")
	mine := ticketOf(want(t, h.call("POST", "/api/v1/tickets", agent, map[string]any{"project": "sandbox", "title": "Board work"}), 201, "create"))["id"].(string)
	hidden, _ := ticketOf(h.person("create", map[string]any{"project": "other", "title": "Not yours"}))["id"].(string)
	if hidden == "" {
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
	// every verb on a ticket in a project the token does not name: not
	// found, never forbidden
	for _, v := range []struct {
		method, suffix string
		body           any
	}{{"GET", "", nil}, {"PATCH", "", map[string]any{"title": "y"}}, {"POST", "/notes", map[string]any{"text": "hi"}},
		{"POST", "/move", map[string]any{"to": "next"}}, {"POST", "/claim", map[string]any{}},
		{"POST", "/ask", map[string]any{"question": "?"}}, {"POST", "/answer", map[string]any{"n": 1, "answer": "x"}},
		{"POST", "/split", map[string]any{"children": []map[string]any{{"title": "a"}}}}, {"POST", "/send", map[string]any{"project": "fac"}}} {
		cases = append(cases, c{"hidden " + v.method + v.suffix, v.method, "/api/v1/tickets/" + hidden + v.suffix, agent, v.body, 404, "not_found"})
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
	// a hidden ticket is not listed, not even when its project is asked for
	// by name
	for _, q := range []string{"", "?project=other", "?include_closed=true"} {
		if w := h.call("GET", "/api/v1/tickets"+q, agent, nil); strings.Contains(w.Body.String(), hidden) {
			t.Errorf("list%s shows a hidden ticket: %s", q, w.Body.String())
		}
	}
	if w := h.call("GET", "/api/v1/work?project=other", agent, nil); strings.Contains(w.Body.String(), hidden) {
		t.Errorf("work shows a hidden ticket: %s", w.Body.String())
	}
	if ps := want(t, h.call("GET", "/api/v1/projects", reader, nil), 200, "projects"); projectNames(ps) != "sandbox" {
		t.Errorf("a token sees its own projects only: %v", ps)
	}
}

// The guard on the real token store: revoked and expired tokens, the
// tailnet when Foreman listens beyond loopback (TestAPIAuth; the Host
// check, the security headers and "a token is no login" are web's, tested
// with the APIs mounted there).
func TestAPIAuth(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
	good := h.mint("example-agent", "read", "sandbox")
	revoked := h.mint("gone-agent", "read", "sandbox")
	h.revoke("gone-agent")
	expiring := h.mint("old-agent", "read", "sandbox")
	h.expire("old-agent", "2020-01-01T00:00:00+00:00")
	for _, k := range []struct {
		name   string
		token  string
		opts   []reqOpt
		status int
	}{
		{"no header", "", nil, 401},
		{"basic", "", []reqOpt{header("Authorization", "Basic "+good)}, 401},
		{"two headers", good, []reqOpt{header("Authorization", "Bearer "+good)}, 401},
		{"wrong token", "fct_not-a-real-token", nil, 401},
		{"revoked", revoked, nil, 401},
		{"expired", expiring, nil, 401},
		{"a good token", good, nil, 200},
		{"lower-case scheme", "", []reqOpt{header("Authorization", "bearer "+good)}, 200},
		{"foreign Origin", good, []reqOpt{header("Origin", "https://evil.example")}, 403},
		{"own Origin", good, []reqOpt{header("Origin", "https://example.com")}, 200},
		{"funnel", good, []reqOpt{header("Tailscale-Funnel-Request", "?1")}, 403},
		{"proxied from the internet", good, []reqOpt{header("X-Forwarded-For", "203.0.113.9")}, 403},
		{"off the tailnet", good, []reqOpt{remote("192.0.2.1:5555")}, 403},
	} {
		w := h.call("GET", "/api/v1/me", k.token, nil, k.opts...)
		if w.Code != k.status {
			t.Errorf("%s: %d, want %d: %s", k.name, w.Code, k.status, w.Body.String())
		}
		if k.status == 401 && w.Header().Get("WWW-Authenticate") != `Bearer realm="factory"` {
			t.Errorf("%s: no WWW-Authenticate", k.name)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: cached", k.name)
		}
	}
	// a token revoked after the guard let it in: the verb says so, as 401
	h.revoke("example-agent")
	if w := h.call("GET", "/api/v1/me", good, nil); w.Code != 401 {
		t.Errorf("revoked: %d", w.Code)
	}

	open := newHarness(t, nil, harnessOpt{open: true})
	tok := open.mint("example-agent", "read", "sandbox")
	if w := open.call("GET", "/api/v1/me", tok, nil, remote("100.64.0.5:5555")); w.Code != 200 {
		t.Errorf("listening on the tailnet, a tailnet peer may call: %d", w.Code)
	}
	if w := open.call("GET", "/api/v1/me", tok, nil, remote("[fd7a:115c:a1e0::5]:5555")); w.Code != 200 {
		t.Errorf("a tailnet IPv6 peer: %d", w.Code)
	}
	if w := open.call("GET", "/api/v1/me", tok, nil, remote("192.0.2.1:5555")); w.Code != 403 {
		t.Errorf("listening on the tailnet, anyone else is refused: %d", w.Code)
	}
}

func TestAPIStorePermissions(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
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

func TestAPIStaleTokensDontLockOutOthers(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
	good := h.mint("example-agent", "read", "sandbox")
	old := h.mint("example-agent-2", "read", "sandbox")
	h.expire("example-agent-2", "2020-01-01T00:00:00+00:00")
	far := header("X-Forwarded-For", "100.64.0.66")
	for i := 0; i <= 10; i++ {
		if w := h.call("GET", "/api/v1/me", old, nil, far); w.Code != 401 {
			t.Fatalf("an expired token, call %d: %d", i, w.Code)
		}
	}
	for i := 0; i < 10; i++ {
		h.call("GET", "/api/v1/me", "fct_revoked", nil)
	}
	if w := h.call("GET", "/api/v1/me", "fct_revoked", nil); w.Code != 429 {
		t.Errorf("the bad token itself waits: %d", w.Code)
	}
	if w := h.call("GET", "/api/v1/me", good, nil); w.Code != 200 {
		t.Errorf("another local token still gets in: %d", w.Code)
	}
}

// A config change is seen on the next call: the service reads it afresh
// (§3.4), so a project removed from config is gone at once.
func TestAPIFollowsTheConfig(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
	agent := h.mint("example-agent", "read,file", "sandbox", "other")
	if ps := want(t, h.call("GET", "/api/v1/projects", agent, nil), 200, "projects"); projectNames(ps) != "sandbox,other" {
		t.Fatalf("projects: %v", ps)
	}
	cfg := strings.Replace(apiConfig, "[projects.other]\nmode = \"board\"\n", "", 1)
	path := h.env.WriteConfig(t, cfg)
	future := time.Now().Add(time.Hour)
	os.Chtimes(path, future, future)
	if ps := want(t, h.call("GET", "/api/v1/projects", agent, nil), 200, "projects after"); projectNames(ps) != "sandbox" {
		t.Errorf("a project taken out of config is gone at once: %v", ps)
	}
}
