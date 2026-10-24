package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// These are Foreman's MCP tests that ran the real Python CLI, now
// in-process against ticketapi.Service on a temp root, with their
// assertions kept.

// re: go
var firstID = regexp.MustCompile(`T\d{4,6}`)

// made files a ticket as the maintainer and gives its id.
func (h *harness) made(project, title string) string {
	h.t.Helper()
	id, _ := ticketOf(h.person("create", map[string]any{"project": project, "title": title}))["id"].(string)
	if id == "" {
		h.t.Fatalf("create %s on %s", title, project)
	}
	return id
}

func TestMCPToolsList(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
	reader := h.mint("reader-agent", "read", "sandbox")
	full := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	worker := h.mint("worker-agent", "read,work", "sandbox")
	names := func(token string, opts ...reqOpt) ([]string, []any) {
		res := rpcResult(t, h.rpc(token, "tools/list", nil, opts...), "tools/list")
		if _, ok := res["nextCursor"]; ok {
			t.Errorf("no pagination: %v", res)
		}
		list, _ := res["tools"].([]any)
		var out []string
		for _, x := range list {
			out = append(out, x.(map[string]any)["name"].(string))
		}
		sort.Strings(out)
		return out, list
	}
	if got, _ := names(reader); strings.Join(got, ",") != "get_ticket,my_work,search_tickets" {
		t.Errorf("a read-only token: %v", got)
	}
	if got, _ := names(worker); slices.Contains(got, "send_to_factory") || !slices.Contains(got, "start_work") || slices.Contains(got, "post_update") {
		t.Errorf("work without file or talk: %v", got)
	}
	got, list := names(full, header("MCP-Protocol-Version", "2025-06-18"))
	if len(got) != 13 {
		t.Fatalf("a full token sees all 13: %v", got)
	}
	byName := New(Options{}).byName
	required := map[string]string{"my_work": "", "get_ticket": "id", "search_tickets": "", "file_ticket": "project,title",
		"split_ticket": "id,children", "update_ticket": "id", "start_work": "id", "post_update": "id,text",
		"finish_work": "id,result", "drop_ticket": "id,reason", "ask_person": "id,question",
		"answer_question": "id,question,answer", "send_to_factory": "id,project"}
	for _, x := range list {
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
		write := byName[name].write
		if ann == nil || ann["readOnlyHint"] != !write || (write && ann["destructiveHint"] != false) || tl["title"] == nil {
			t.Errorf("%s: annotations %v, title %v", name, ann, tl["title"])
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
	h := newHarness(t, nil, harnessOpt{})
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	helper := h.mint("helper-agent", "read,talk", "sandbox")
	id := ticketOf(h.person("create", map[string]any{"project": "sandbox", "title": "Fix the date picker", "body": "It shows the wrong month."}))["id"].(string)

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
	h.person("answer", map[string]any{"id": id, "n": 1, "answer": "en-GB"})
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

func TestMCPRealRefusals(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
	reader := h.mint("reader-agent", "read", "sandbox", "fac")
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	other := h.mint("other-agent", "read,file,talk,work,plan", "sandbox")
	id := h.made("sandbox", "Board work")
	hidden := h.made("other", "Hidden work")
	fac := h.made("fac", "A change")

	for _, tl := range tools() {
		args := map[string]any{}
		for k, v := range argsFor()[tl.name] {
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
		for k, v := range argsFor()[name] {
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
	h := newHarness(t, nil, harnessOpt{})
	agent := h.mint("example-agent", "read,file,talk,work", "sandbox")
	id := h.made("sandbox", "Board work")

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
	data, _ := os.ReadFile(filepath.Join(h.env.Root, "board", "tickets", id+".json"))
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
	got := h.person("get", map[string]any{"id": id})
	hist := ticketOf(got)["history"].([]any)
	if last := hist[len(hist)-1].(map[string]any); last["reason"] != "dropped" {
		t.Errorf("Done to Dropped is not a reopening: %v", last)
	}
}

func TestMCPResources(t *testing.T) {
	h := newHarness(t, nil, harnessOpt{})
	agent := h.mint("example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	ids := map[string]string{}
	for _, k := range []struct{ key, project, title string }{
		{"mine", "sandbox", "Mine"}, {"open", "sandbox", "Unassigned"}, {"done", "sandbox", "Finished"}, {"hidden", "other", "Hidden"},
	} {
		ids[k.key] = h.made(k.project, k.title)
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

// The skill names only tools the server has, and every tool, so the two
// can't drift.
func TestSkillMatchesTools(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "skills", "tickets", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	skill := string(data)
	if !strings.HasPrefix(skill, "---\nname: tickets\ndescription: ") {
		t.Errorf("frontmatter: %q", skill[:min(80, len(skill))])
	}
	s := New(Options{})
	args := map[string]bool{}
	for _, tl := range s.tools {
		if !strings.Contains(skill, "`"+tl.name+"`") {
			t.Errorf("the skill never names %s", tl.name)
		}
		for k := range tl.schema.props {
			args[k] = true
		}
	}
	for _, m := range regexp.MustCompile("`([a-z]+(?:_[a-z]+)+)`").FindAllStringSubmatch(skill, -1) {
		if s.byName[m[1]] == nil && !args[m[1]] {
			t.Errorf("the skill names %s, which is no tool or argument", m[1])
		}
	}
	path := filepath.Join("..", "..", "examples", "mcp", "mcp.json")
	data, err = os.ReadFile(path)
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
	if srv := cfg.MCPServers["factory"]; srv.Type != "http" || !strings.HasSuffix(srv.URL, Path) || srv.Headers["Authorization"] != "Bearer ${FACTORY_TOKEN}" {
		t.Errorf("%s: %+v", path, cfg)
	}
}
