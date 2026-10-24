package ticketapi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// The goldens of the Python goldens tool (g_markdown): Python's ticket verbs on a
// scratch board, at a fixed clock per call. The writes are replayed here in
// the same order, each output and written Markdown compared with Python's,
// then the ticket files byte for byte, then the read calls on the board.

func readGolden(t *testing.T, rel string) jsonx.Value {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "py", rel))
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, string(data))
}

// readFile is a JSON golden at a path of its own: the written Markdown
// goldens live with mdout, which renders them.
func readFile(t *testing.T, path string) jsonx.Value {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, string(data))
}

// goldenFx is the board of g_markdown: BOARD_CONFIG and the tokens it added
// (whose sha256 is of their names).
func goldenFx(t *testing.T) *fx {
	t.Helper()
	cfg, err := os.ReadFile(filepath.Join("testdata", "py", "board", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	f := newFx(t, string(cfg))
	src, err := os.ReadFile(filepath.Join("testdata", "py", "board", "api-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(f.tokens.Path), 0o700)
	if err := os.WriteFile(f.tokens.Path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// goldenCaller is a caller as goldens.py names it: null is the maintainer,
// "bob" the second person, anything else a token.
func goldenCaller(v jsonx.Value) Caller {
	name, ok := v.Str()
	switch {
	case !ok:
		return ForPerson()
	case name == "bob" || name == "alice":
		return AsPerson(name)
	}
	return ForToken(name)
}

// outcome is refused()'s shape: {"ok": true, "out": ...} or
// {"ok": false, "error": {code, message, field}}.
func outcome(out jsonx.Object, err error) (jsonx.Value, error) {
	if err == nil {
		return jsonx.ObjectOf(jsonx.Member{Key: "ok", Value: jsonx.Bool(true)}, jsonx.Member{Key: "out", Value: out.Value()}), nil
	}
	var r *Refused
	if !errors.As(err, &r) {
		return jsonx.Value{}, err
	}
	field := jsonx.Null()
	if r.Field != "" {
		field = jsonx.String(r.Field)
	}
	return jsonx.ObjectOf(jsonx.Member{Key: "ok", Value: jsonx.Bool(false)}, jsonx.Member{Key: "error", Value: jsonx.ObjectOf(
		jsonx.Member{Key: "code", Value: jsonx.String(r.Code)}, jsonx.Member{Key: "message", Value: jsonx.String(r.Message)},
		jsonx.Member{Key: "field", Value: field})}), nil
}

func sameJSON(t *testing.T, what string, want, got jsonx.Value) {
	t.Helper()
	w, g := jsonx.EncodePython(want, 2), jsonx.EncodePython(got, 2)
	if !bytes.Equal(w, g) {
		t.Errorf("%s:\n%s", what, testkit.Diff(string(w), string(g)))
	}
}

// replay is g_markdown's writes, in order.
func replay(t *testing.T, f *fx) {
	t.Helper()
	agent, alice := ForToken("example-agent"), ForPerson()
	steps := []struct {
		verb  string
		who   Caller
		req   string
		stamp string
	}{
		{"create", agent, `{"project": "sandbox", "title": "Date picker", "labels": ["ui"], "idempotency_key": "k1", "body": "Pick a date.\n\nWith ` + "``` fences ``` inside and a `tick`." + `"}`, "2026-10-17T08:00:00+00:00"},
		{"create", alice, `{"project": "sandbox", "title": "Second  ticket\nwith a break", "links": [{"rel": "url", "url": "https://example.test/a", "title": "A ` + "`ticked`" + ` title"}]}`, "2026-10-17T08:01:00+00:00"},
		{"claim", agent, `{"id": "T0001"}`, "2026-10-17T08:02:00+00:00"},
		{"note", agent, `{"id": "T0001", "text": "Started on it."}`, "2026-10-17T08:03:00+00:00"},
		{"ask", agent, `{"id": "T0001", "question": "Which locale?", "options": ["en-GB", "en-US", "` + "`raw`" + `"]}`, "2026-10-17T08:04:00+00:00"},
		{"ask", agent, `{"id": "T0001", "question": "Two\nlines?", "recipient": "bob"}`, "2026-10-17T08:05:00+00:00"},
		{"answer", alice, `{"id": "T0001", "n": 1, "answer": "en-GB"}`, "2026-10-18T09:00:00+00:00"},
		{"update", agent, `{"id": "T0002", "links": [{"rel": "related", "ticket": "T0001"}], "add_labels": ["later", "api"]}`, "2026-10-18T09:01:00+00:00"},
		{"split", agent, `{"id": "T0001", "children": [{"title": "Child one"}, {"title": "Child two", "body": "x"}]}`, "2026-10-18T09:02:00+00:00"},
		{"send", agent, `{"id": "T0002", "project": "fac", "note": "Please look."}`, "2026-10-18T09:03:00+00:00"},
		{"move", agent, `{"id": "T0003", "to": "waiting", "reason": "Blocked on the API\nsecond line ` + "`x`" + `"}`, "2026-10-18T09:04:00+00:00"},
		{"move", agent, `{"id": "T0004", "to": "done", "result": "Shipped."}`, "2026-10-18T09:05:00+00:00"},
		{"create", agent, `{"project": "sandbox", "title": "Assigned to bob"}`, "2026-10-18T09:06:00+00:00"},
		{"list", alice, `{"project": "sandbox", "text": "assigned"}`, "2026-10-18T09:07:00+00:00"},
		{"update", alice, `{"id": "T0006", "assignee": "bob"}`, "2026-10-18T09:07:00+00:00"},
		{"move", alice, `{"id": "T0006", "to": "next"}`, "2026-10-18T09:08:00+00:00"},
		{"create", agent, `{"project": "sandbox", "title": "Inbox one"}`, "2026-10-18T09:09:00+00:00"},
	}
	files, _ := filepath.Glob(filepath.Join("..", "mdout", "testdata", "py", "written_markdown", "*.json"))
	sort.Strings(files)
	n := 0
	for _, s := range steps {
		f.clock.set(s.stamp)
		out, err := f.svc.Call(context.Background(), s.who, s.verb, parse(t, s.req), 5e9)
		if err != nil {
			t.Fatalf("%s %s: %v", s.verb, s.req, err)
		}
		if s.verb == "list" {
			if got := str(out.Value(), "tickets", "0", "id"); got != "T0006" {
				t.Fatalf("list found %s, want T0006", got)
			}
			continue
		}
		if n >= len(files) {
			t.Fatalf("more writes than goldens")
		}
		g := readFile(t, files[n])
		n++
		if str(g, "verb") != s.verb {
			t.Fatalf("golden %s is a %s, the replay a %s", files[n-1], str(g, "verb"), s.verb)
		}
		sameJSON(t, s.verb+" "+s.req, g.Get("out"), out.Value())
		res, err := f.svc.written(context.Background(), s.verb, s.who, out)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "written "+s.verb+" "+s.req, g.Get("result"), res.Value())
	}
	if n != len(files) {
		t.Fatalf("%d writes, %d goldens", n, len(files))
	}
}

// port: TT WrittenMarkdownTest.test_a_result_with_a_move_to_waiting_is_a_comment
func TestReplayMatchesPythonsWritesAndFiles(t *testing.T) {
	f := goldenFx(t)
	replay(t, f)
	want, _ := filepath.Glob(filepath.Join("testdata", "py", "board", "tickets", "T*.json"))
	got, _ := filepath.Glob(filepath.Join(f.board.Paths.Tickets, "T*.json"))
	if len(want) != len(got) || len(want) == 0 {
		t.Fatalf("%d ticket files, want %d", len(got), len(want))
	}
	for _, w := range want {
		wb, _ := os.ReadFile(w)
		gb, err := os.ReadFile(filepath.Join(f.board.Paths.Tickets, filepath.Base(w)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wb, gb) {
			t.Errorf("%s:\n%s", filepath.Base(w), testkit.Diff(string(wb), string(gb)))
		}
	}
}

// port: TT VerbTest.test_list_filters_pages_and_searches
func TestBoardCallsMatchPython(t *testing.T) {
	f := goldenFx(t)
	// the board Python left, not our replay of it: the reads are compared on
	// Python's own files
	os.MkdirAll(f.board.Paths.Tickets, 0o777)
	src, _ := filepath.Glob(filepath.Join("testdata", "py", "board", "tickets", "T*.json"))
	for _, s := range src {
		b, _ := os.ReadFile(s)
		os.WriteFile(filepath.Join(f.board.Paths.Tickets, filepath.Base(s)), b, 0o666)
	}
	g := readGolden(t, "board-calls.json")
	f.clock.set(str(g, "now"))
	calls, _ := g.Get("calls").Array()
	if len(calls) < 50 {
		t.Fatalf("%d calls", len(calls))
	}
	for _, c := range calls {
		what := str(c, "caller") + " " + str(c, "verb") + " " + enc(c.Get("req"))
		out, err := f.svc.Call(context.Background(), goldenCaller(c.Get("caller")), str(c, "verb"), c.Get("req"), 5e9)
		got, err := outcome(out, err)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		o, _ := c.Object()
		want := jsonx.Object{}
		for _, m := range o {
			if m.Key == "ok" || m.Key == "out" || m.Key == "error" {
				want = append(want, m)
			}
		}
		sameJSON(t, what, want.Value(), withoutPublicColumn(got))
	}
}

// withoutPublicColumn takes the public review column, which Python's board
// never had, out of every list in v that is exactly Go's column list, so
// that Go's answers compare with Python's. Any other list is left as it is.
func withoutPublicColumn(v jsonx.Value) jsonx.Value {
	if a, ok := v.Array(); ok {
		if isColumnList(a) {
			var out []jsonx.Value
			for _, x := range a {
				if s, _ := x.Str(); s != string(model.ColPublic) {
					out = append(out, x)
				}
			}
			return jsonx.ArrayOf(out...)
		}
		var out []jsonx.Value
		for _, x := range a {
			out = append(out, withoutPublicColumn(x))
		}
		return jsonx.ArrayOf(out...)
	}
	if o, ok := v.Object(); ok {
		var out jsonx.Object
		for _, m := range o {
			out = append(out, jsonx.Member{Key: m.Key, Value: withoutPublicColumn(m.Value)})
		}
		return out.Value()
	}
	return v
}

// isColumnList reports a list that is model.Columns' names, in order.
func isColumnList(a []jsonx.Value) bool {
	cols := model.Columns()
	if len(a) != len(cols) {
		return false
	}
	for i, x := range a {
		if s, ok := x.Str(); !ok || s != string(cols[i].Name) {
			return false
		}
	}
	return true
}

// Only Go's column list loses the public column: a list that holds
// "public" beside "security" but is not that list keeps it.
func TestWithoutPublicColumnTouchesOnlyTheColumnList(t *testing.T) {
	var cols, py []jsonx.Value
	for _, c := range model.Columns() {
		cols = append(cols, jsonx.String(string(c.Name)))
		if c.Name != model.ColPublic {
			py = append(py, jsonx.String(string(c.Name)))
		}
	}
	other := jsonx.ArrayOf(jsonx.String("security"), jsonx.String("public"))
	in := jsonx.ObjectOf(jsonx.Member{Key: "columns", Value: jsonx.ArrayOf(cols...)}, jsonx.Member{Key: "stages", Value: other})
	want := jsonx.ObjectOf(jsonx.Member{Key: "columns", Value: jsonx.ArrayOf(py...)}, jsonx.Member{Key: "stages", Value: other})
	if got := withoutPublicColumn(in); string(jsonx.Canonical(got)) != string(jsonx.Canonical(want)) {
		t.Errorf("withoutPublicColumn = %s, want %s", jsonx.Canonical(got), jsonx.Canonical(want))
	}
}

// The contract fixture's board ticket (TT ContractFixture's last part),
// re-made through the service: the same keys, in the same order, with the
// same values, once the times (which Python's stepping clock chose) are
// set aside.
//
// port: TT ContractFixture.test_fixture_is_current
func TestContractBoardTicket(t *testing.T) {
	// The module's own copy of the contract fixture.
	files, _ := filepath.Glob(filepath.Join("..", "ticket", "testdata", "legacy", "contract", "T*.json"))
	var want []byte
	for _, p := range files {
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte(`"mode": "board"`)) {
			want = b
		}
	}
	if want == nil {
		t.Fatalf("no contract board ticket among %v", files)
	}
	f := newFx(t, "[board]\npeople = [\"alice\"]\n\n[projects.p]\npath = \"/nonexistent\"\n\n[projects.sandbox]\nmode = \"board\"\n")
	// the earlier tickets of the fixture are not needed, only the id after them
	tid := str(parse(t, string(want)), "id")
	os.MkdirAll(f.board.Paths.Tickets, 0o777)
	os.WriteFile(f.board.Paths.LastID, []byte(itoa(atoiID(tid)-1)), 0o666)
	f.token("example-agent", "read,file,talk,work", "sandbox")
	agent := ForToken("example-agent")
	call := func(c Caller, verb, req string) {
		if _, err := f.svc.Call(context.Background(), c, verb, parse(t, req), 5e9); err != nil {
			t.Fatalf("%s %s: %v", verb, req, err)
		}
	}
	call(agent, "create", `{"project": "sandbox", "title": "Date picker", "labels": ["ui"], "idempotency_key": "k1"}`)
	call(agent, "claim", `{"id": "`+tid+`"}`)
	call(agent, "note", `{"id": "`+tid+`", "text": "Started on it."}`)
	call(agent, "ask", `{"id": "`+tid+`", "question": "Which locale?", "options": ["en-GB", "en-US"]}`)
	call(ForPerson(), "answer", `{"id": "`+tid+`", "n": 1, "answer": "en-GB"}`)
	call(agent, "move", `{"id": "`+tid+`", "to": "done", "result": "Uses en-GB."}`)
	got, err := os.ReadFile(f.board.Paths.TicketFile(tid))
	if err != nil {
		t.Fatal(err)
	}
	stamps := regexp.MustCompile(`"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\+00:00"`)
	norm := func(b []byte) string {
		return strings.TrimRight(stamps.ReplaceAllString(string(b), `"<time>"`), "\n")
	}
	if w, g := norm(want), norm(got); w != g {
		t.Errorf("the contract board ticket:\n%s", testkit.Diff(w, g))
	}
}

func atoiID(id string) int {
	n := 0
	for _, c := range id[1:] {
		n = n*10 + int(c-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}
