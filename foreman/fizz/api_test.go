package fizz

import (
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

const acct = "6182510"

type harness struct {
	t     *testing.T
	st    *Store
	api   *API
	token string
	clock time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := Open(filepath.Join(t.TempDir(), "boards"))
	h := &harness{t: t, st: st, clock: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}
	st.Now = func() time.Time { h.clock = h.clock.Add(time.Second); return h.clock }
	if _, err := st.Init(acct, "Projects", "Stephen", "s@example.com"); err != nil {
		t.Fatal(err)
	}
	a, _ := st.Account(acct)
	tok, err := st.AddToken("stephen", acct, a.UserNamed("Stephen").ID)
	if err != nil {
		t.Fatal(err)
	}
	h.token = tok
	h.api = NewAPI(st)
	return h
}

func (h *harness) req(method, target, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.api.ServeHTTP(w, r)
	return w
}

// do: an authorised request that must answer want; the JSON body is decoded.
func (h *harness) do(method, path, body string, want int) any {
	h.t.Helper()
	w := h.req(method, "http://example.com/fizzy"+path, body, h.token)
	if w.Code != want {
		h.t.Fatalf("%s %s: %d, want %d: %s", method, path, w.Code, want, w.Body)
	}
	var v any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			h.t.Fatalf("%s %s: not JSON: %s", method, path, w.Body)
		}
	}
	return v
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func arr(v any) []any          { a, _ := v.([]any); return a }

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasKeys(t *testing.T, what string, m map[string]any, want ...string) {
	t.Helper()
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("%s: no %q in %v", what, k, keys(m))
		}
	}
}

// board makes a board with columns Doing and Review.
func (h *harness) board() (string, string, string) {
	b := obj(h.do("POST", "/"+acct+"/boards.json", `{"name":"Thriceforged"}`, 201))
	id := b["id"].(string)
	c1 := obj(h.do("POST", "/"+acct+"/boards/"+id+"/columns.json", `{"name":"Doing"}`, 201))
	c2 := obj(h.do("POST", "/"+acct+"/boards/"+id+"/columns.json", `{"name":"Review","color":"var(--color-card-4)"}`, 201))
	return id, c1["id"].(string), c2["id"].(string)
}

func (h *harness) card(board, title string) int {
	c := obj(h.do("POST", "/"+acct+"/cards.json", `{"board_id":"`+board+`","title":"`+title+`"}`, 201))
	return int(c["number"].(float64))
}

// ---------------------------------------------------------------- store

func TestStoreRoundTripAndNumbers(t *testing.T) {
	h := newHarness(t)
	a, err := h.st.Account(acct)
	if err != nil || a.Name != "Projects" || len(a.Users) != 2 || a.Users[0].Role != "owner" || a.Users[1].Role != "system" {
		t.Fatalf("account %+v %v", a, err)
	}
	b := &Board{ID: NewID(), Name: "B", CreatedAt: h.st.Stamp(), Columns: []*Column{{ID: NewID(), Name: "Doing"}}}
	if err := h.st.SaveBoard(acct, b); err != nil {
		t.Fatal(err)
	}
	got, err := h.st.Board(acct, b.ID)
	if err != nil || !reflect.DeepEqual(got, b) {
		t.Fatalf("board round trip: %+v %v", got, err)
	}
	var nums []int
	for range 3 {
		n, err := h.st.NextNumber(a)
		if err != nil {
			t.Fatal(err)
		}
		c := &Card{ID: NewID(), Number: n, BoardID: b.ID, Title: "t", Steps: []*Step{{ID: NewID(), Content: "s"}},
			Comments: []*Comment{{ID: NewID(), Body: "<p>x</p>"}}, Assignees: []string{}, Tags: []string{}}
		if err := h.st.SaveCard(acct, c); err != nil {
			t.Fatal(err)
		}
		back, _ := h.st.Card(acct, n)
		if !reflect.DeepEqual(back, c) {
			t.Fatalf("card round trip\n got %+v\nwant %+v", back, c)
		}
		nums = append(nums, n)
	}
	if !reflect.DeepEqual(nums, []int{1, 2, 3}) {
		t.Fatalf("numbers %v", nums)
	}
	h.st.DeleteCard(acct, 3)
	if n, _ := h.st.NextNumber(a); n != 4 {
		t.Errorf("a deleted card's number came back: %d", n)
	}
	// nothing half-written is left behind, and the files are private
	filepath.Walk(h.st.Dir, func(p string, info os.FileInfo, err error) error {
		if strings.Contains(filepath.Base(p), ".tmp-") {
			t.Errorf("temp file left: %s", p)
		}
		if !info.IsDir() && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable by others: %v", p, info.Mode())
		}
		return nil
	})
	if id := NewID(); len(id) != 25 || !ValidID(id) {
		t.Errorf("id %q", id)
	}
}

func TestStoreRefusesTraversal(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"../tokens", "..", "a/b", "A", "x.json", ""} {
		if _, err := h.st.Board(acct, id); err != ErrNotFound {
			t.Errorf("board %q: %v", id, err)
		}
	}
	for _, a := range []string{"../boards", "..", "6182510/..", "abc", ""} {
		if _, err := h.st.Account(a); err != ErrNotFound {
			t.Errorf("account %q: %v", a, err)
		}
	}
	for _, n := range []string{"0", "-1", "1.json", "../1", "01", "9999999999"} {
		if CardNumber(n) != 0 {
			t.Errorf("card number %q accepted", n)
		}
	}
	if err := h.st.SaveBoard(acct, &Board{ID: "../../evil"}); err == nil {
		t.Error("saved a board with a path in its id")
	}
}

// ---------------------------------------------------------------- auth

func TestAuth(t *testing.T) {
	h := newHarness(t)
	check := func(name, token string, want int) {
		t.Helper()
		w := h.req("GET", "http://example.com/fizzy/"+acct+"/boards.json", "", token)
		if w.Code != want {
			t.Fatalf("%s: %d, want %d", name, w.Code, want)
		}
		if want == 401 {
			var e map[string]any
			if json.Unmarshal(w.Body.Bytes(), &e) != nil || e["error"] != "Unauthorized" || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("%s: body %s", name, w.Body)
			}
		}
	}
	check("no token", "", 401)
	check("wrong token", "foreman_"+strings.Repeat("0", 64), 401)
	check("right token", h.token, 200)
	r := httptest.NewRequest("GET", "http://example.com/fizzy/"+acct+"/boards.json", nil)
	r.Header.Set("Authorization", "Basic "+h.token)
	w := httptest.NewRecorder()
	h.api.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("a non-bearer scheme got %d", w.Code)
	}
	if err := h.st.RevokeToken("stephen"); err != nil {
		t.Fatal(err)
	}
	check("revoked token", h.token, 401)

	// a token only reaches its own account
	h.st.Init("42", "Other", "Someone", "")
	a, _ := h.st.Account("42")
	other, _ := h.st.AddToken("other", "42", a.Users[0].ID)
	if w := h.req("GET", "http://example.com/fizzy/"+acct+"/boards.json", "", other); w.Code != 404 {
		t.Errorf("another account's token: %d", w.Code)
	}
	if _, err := h.st.AddToken("other", "42", a.Users[0].ID); err == nil {
		t.Error("two tokens with one name")
	}
	data, _ := os.ReadFile(filepath.Join(h.st.Dir, "tokens.json"))
	if strings.Contains(string(data), other) {
		t.Error("the token is stored in the clear")
	}
}

// ---------------------------------------------------------------- endpoints

func TestEndpointsTheCLIUses(t *testing.T) {
	h := newHarness(t)
	h.do("GET", "/my/identity.json", "", 200)
	id := obj(h.do("GET", "/my/identity.json", "", 200))
	accts := arr(id["accounts"])
	if len(accts) != 1 || obj(accts[0])["slug"] != "/"+acct || obj(obj(accts[0])["user"])["name"] != "Stephen" {
		t.Fatalf("identity %v", id)
	}
	board, doing, _ := h.board()

	bl := arr(h.do("GET", "/"+acct+"/boards.json", "", 200))
	if len(bl) != 1 {
		t.Fatalf("boards %v", bl)
	}
	hasKeys(t, "board", obj(bl[0]), "id", "name", "all_access", "auto_postpone_period_in_days", "created_at", "creator", "url")
	h.do("GET", "/"+acct+"/boards/"+board, "", 200)
	if b := obj(h.do("GET", "/"+acct+"/boards/"+board+".json", "", 200)); b["name"] != "Thriceforged" {
		t.Errorf("board show %v", b)
	}
	cols := arr(h.do("GET", "/"+acct+"/boards/"+board+"/columns.json", "", 200))
	if len(cols) != 2 || obj(cols[1])["name"] != "Review" {
		t.Fatalf("columns %v", cols)
	}
	hasKeys(t, "column", obj(cols[0]), "id", "name", "color", "created_at", "cards_url")
	if c := obj(obj(cols[1])["color"]); c["name"] != "Lime" || c["value"] != "var(--color-card-4)" {
		t.Errorf("color %v", c)
	}
	h.do("GET", "/"+acct+"/boards/"+board+"/columns/"+doing, "", 200)

	// create: 201, the card, and where it lives
	w := h.req("POST", "http://example.com/fizzy/"+acct+"/cards.json",
		`{"board_id":"`+board+`","title":"First","description":"<p>Some <strong>bold</strong> text</p>"}`, h.token)
	if w.Code != 201 || w.Header().Get("Location") != "http://example.com/fizzy/"+acct+"/cards/1.json" {
		t.Fatalf("create %d %v %s", w.Code, w.Header(), w.Body)
	}
	var card map[string]any
	json.Unmarshal(w.Body.Bytes(), &card)
	hasKeys(t, "card", card, "id", "number", "title", "description", "description_html", "status", "closed",
		"postponed", "golden", "created_at", "last_active_at", "url", "comments_url", "board", "creator", "assignees", "tags", "steps")
	if card["number"] != 1.0 || card["status"] != "published" || card["description"] != "Some bold text" || len(card["id"].(string)) != 25 {
		t.Errorf("card %v", card)
	}
	if _, ok := card["column"]; ok {
		t.Error("a card in Maybe? has a column")
	}
	// Rails-style wrapped bodies work too
	h.do("POST", "/"+acct+"/cards.json", `{"card":{"board_id":"`+board+`","title":"Second","description":"plain & simple"}}`, 201)
	two := obj(h.do("GET", "/"+acct+"/cards/2", "", 200))
	if two["description_html"] != "<p>plain &amp; simple</p>" || two["description"] != "plain & simple" {
		t.Errorf("plain text description %q %q", two["description_html"], two["description"])
	}
	h.do("POST", "/"+acct+"/cards.json", `{"board_id":"`+board+`","title":"  "}`, 422)
	h.do("POST", "/"+acct+"/cards.json", `{"board_id":"nosuch","title":"x"}`, 404)
	h.do("POST", "/"+acct+"/cards.json", `not json`, 400)

	list := arr(h.do("GET", "/"+acct+"/cards.json", "", 200))
	if len(list) != 2 || obj(list[0])["number"] != 2.0 {
		t.Fatalf("list, latest first: %v", list)
	}
	if _, ok := obj(list[0])["steps"]; ok {
		t.Error("a listed card has steps; only show has them")
	}
	upd := obj(h.do("PATCH", "/"+acct+"/cards/2", `{"title":"Second, renamed"}`, 200))
	if upd["title"] != "Second, renamed" || upd["description"] != "plain & simple" {
		t.Errorf("update %v", upd)
	}

	// comments
	cm := obj(h.do("POST", "/"+acct+"/cards/1/comments.json", `{"body":"hello"}`, 201))
	hasKeys(t, "comment", cm, "id", "body", "card", "creator", "created_at", "updated_at", "url")
	if b := obj(cm["body"]); b["plain_text"] != "hello" || b["html"] != "<p>hello</p>" {
		t.Errorf("comment body %v", b)
	}
	cid := cm["id"].(string)
	h.do("GET", "/"+acct+"/cards/1/comments/"+cid, "", 200)
	if e := obj(h.do("PATCH", "/"+acct+"/cards/1/comments/"+cid, `{"body":"edited"}`, 200)); obj(e["body"])["plain_text"] != "edited" {
		t.Errorf("comment update %v", e)
	}
	if l := arr(h.do("GET", "/"+acct+"/cards/1/comments.json", "", 200)); len(l) != 1 {
		t.Errorf("comments %v", l)
	}
	h.do("DELETE", "/"+acct+"/cards/1/comments/"+cid, "", 204)
	h.do("GET", "/"+acct+"/cards/1/comments/"+cid, "", 404)

	// steps
	st := obj(h.do("POST", "/"+acct+"/cards/1/steps.json", `{"content":"write tests"}`, 201))
	sid := st["id"].(string)
	if s := obj(h.do("PATCH", "/"+acct+"/cards/1/steps/"+sid, `{"completed":true}`, 200)); s["completed"] != true || s["content"] != "write tests" {
		t.Errorf("step %v", s)
	}
	if s := obj(h.do("PATCH", "/"+acct+"/cards/1/steps/"+sid, `{"completed":false}`, 200)); s["completed"] != false {
		t.Errorf("step %v", s)
	}
	if steps := arr(obj(h.do("GET", "/"+acct+"/cards/1.json", "", 200))["steps"]); len(steps) != 1 {
		t.Errorf("card show steps %v", steps)
	}
	h.do("GET", "/"+acct+"/cards/1/steps/"+sid, "", 200)
	if l := arr(h.do("GET", "/"+acct+"/cards/1/steps.json", "", 200)); len(l) != 1 {
		t.Errorf("steps %v", l)
	}
	h.do("DELETE", "/"+acct+"/cards/1/steps/"+sid, "", 204)

	// people, gold, state
	users := arr(h.do("GET", "/"+acct+"/users.json", "", 200))
	if len(users) != 1 || obj(users[0])["name"] != "Stephen" {
		t.Fatalf("users (no system user) %v", users)
	}
	hasKeys(t, "user", obj(users[0]), "id", "name", "email_address", "role", "active", "avatar_url", "created_at", "url")
	me := obj(users[0])["id"].(string)
	h.do("GET", "/"+acct+"/users/"+me, "", 200)
	h.do("POST", "/"+acct+"/cards/2/assignments.json", `{"assignee_id":"`+me+`"}`, 204)
	if l := arr(h.do("GET", "/"+acct+"/cards.json?assignee_ids[]="+me, "", 200)); len(l) != 1 {
		t.Errorf("assignee filter %v", l)
	}
	h.do("POST", "/"+acct+"/cards/2/self_assignment.json", "", 204) // toggles off
	if l := arr(h.do("GET", "/"+acct+"/cards.json?assignment_status=unassigned", "", 200)); len(l) != 2 {
		t.Errorf("unassigned %v", l)
	}
	h.do("POST", "/"+acct+"/cards/2/assignments.json", `{"assignee_id":"nobody"}`, 404)
	h.do("POST", "/"+acct+"/cards/2/goldness.json", "", 204)
	if l := arr(h.do("GET", "/"+acct+"/cards.json?indexed_by=golden", "", 200)); len(l) != 1 {
		t.Errorf("golden %v", l)
	}
	h.do("DELETE", "/"+acct+"/cards/2/goldness.json", "", 204)
	h.do("POST", "/"+acct+"/cards/2/not_now.json", "", 204)
	if l := arr(h.do("GET", "/"+acct+"/cards.json?indexed_by=not_now", "", 200)); len(l) != 1 {
		t.Errorf("not now %v", l)
	}
	if l := arr(h.do("GET", "/"+acct+"/boards/"+board+"/columns/not_now.json", "", 200)); len(l) != 1 {
		t.Errorf("board postponed %v", l)
	}
	h.do("POST", "/"+acct+"/cards/1/closure.json", "", 204)
	for _, q := range []string{"indexed_by=closed", "closure=today", "closer_ids[]=" + me} {
		if l := arr(h.do("GET", "/"+acct+"/cards.json?"+q, "", 200)); len(l) != 1 {
			t.Errorf("%s: %v", q, l)
		}
	}
	if l := arr(h.do("GET", "/"+acct+"/cards.json", "", 200)); len(l) != 0 {
		t.Errorf("open cards should be none: %v", l)
	}
	h.do("DELETE", "/"+acct+"/cards/1/closure.json", "", 204)
	if c := obj(h.do("GET", "/"+acct+"/cards/1", "", 200)); c["closed"] != false {
		t.Errorf("reopen %v", c)
	}

	// search, sort, terms, tags
	if l := arr(h.do("GET", "/"+acct+"/search.json?q=renamed", "", 200)); len(l) != 1 || obj(l[0])["number"] != 2.0 {
		t.Errorf("search %v", l)
	}
	if l := arr(h.do("GET", "/"+acct+"/search.json?q=1", "", 200)); len(l) != 1 || obj(l[0])["number"] != 1.0 {
		t.Errorf("search by number %v", l)
	}
	if l := arr(h.do("GET", "/"+acct+"/cards.json?terms[]=BOLD&terms[]=some", "", 200)); len(l) != 1 {
		t.Errorf("terms %v", l)
	}
	if l := arr(h.do("GET", "/"+acct+"/cards.json?indexed_by=everything&sorted_by=oldest", "", 200)); len(l) != 2 || obj(l[0])["number"] != 1.0 {
		t.Errorf("oldest %v", l)
	}
	h.do("GET", "/"+acct+"/cards.json?sorted_by=sideways", "", 400)
	h.do("GET", "/"+acct+"/cards.json?indexed_by=nope", "", 400)
	if l := arr(h.do("GET", "/"+acct+"/tags.json", "", 200)); len(l) != 0 {
		t.Errorf("tags %v", l)
	}

	h.do("DELETE", "/"+acct+"/cards/2", "", 204)
	h.do("GET", "/"+acct+"/cards/2", "", 404)
	h.do("GET", "/"+acct+"/nothing.json", "", 404)
}

func TestTriageWritesSystemComments(t *testing.T) {
	h := newHarness(t)
	board, doing, _ := h.board()
	n := h.card(board, "Move me")
	h.do("POST", "/"+acct+"/cards/1/triage.json", `{"column_id":"`+doing+`"}`, 204)
	c := obj(h.do("GET", "/"+acct+"/cards/1", "", 200))
	if obj(c["column"])["name"] != "Doing" {
		t.Fatalf("column %v", c["column"])
	}
	if l := arr(h.do("GET", "/"+acct+"/cards.json?column_ids[]="+doing, "", 200)); len(l) != 1 {
		t.Errorf("column filter %v", l)
	}
	if l := arr(h.do("GET", "/"+acct+"/cards.json?indexed_by=maybe", "", 200)); len(l) != 0 {
		t.Errorf("maybe %v", l)
	}
	h.do("POST", "/"+acct+"/cards/1/triage.json", `{"column_id":"elsewhere"}`, 404)
	h.do("DELETE", "/"+acct+"/cards/1/triage.json", "", 204)
	h.do("DELETE", "/"+acct+"/cards/1/triage.json", "", 204) // already there: no second note
	h.do("POST", "/"+acct+"/cards/1/closure.json", "", 204)
	h.do("DELETE", "/"+acct+"/cards/1/closure.json", "", 204)
	h.do("POST", "/"+acct+"/cards/1/not_now.json", "", 204)
	if l := arr(h.do("GET", "/"+acct+"/cards.json?indexed_by=maybe", "", 200)); len(l) != 0 {
		t.Errorf("a postponed card is not in maybe: %v", l)
	}
	var got []string
	for _, cm := range arr(h.do("GET", "/"+acct+"/cards/1/comments.json", "", 200)) {
		cr := obj(obj(cm)["creator"])
		if cr["role"] != "system" {
			t.Errorf("move comment by %v", cr)
		}
		got = append(got, obj(obj(cm)["body"])["plain_text"].(string))
	}
	want := []string{"Moved to Doing by Stephen.", "Moved back to Maybe? by Stephen.", "Closed by Stephen.",
		"Reopened by Stephen.", "Moved to Not Now by Stephen."}
	if !reflect.DeepEqual(got, want) || n != 1 {
		t.Fatalf("system comments\n got %q\nwant %q", got, want)
	}
}

func TestTagToggleCreatesTags(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	h.card(board, "a")
	h.card(board, "b")
	h.do("POST", "/"+acct+"/cards/1/taggings.json", `{"tag_title":"bug"}`, 204)
	h.do("POST", "/"+acct+"/cards/2/taggings.json", `{"tag_title":"#Bug"}`, 204) // the same tag
	tags := arr(h.do("GET", "/"+acct+"/tags.json", "", 200))
	if len(tags) != 1 || obj(tags[0])["title"] != "bug" {
		t.Fatalf("tags %v", tags)
	}
	hasKeys(t, "tag", obj(tags[0]), "id", "title", "created_at", "url")
	tag := obj(tags[0])["id"].(string)
	if l := arr(h.do("GET", "/"+acct+"/cards.json?tag_ids[]="+tag, "", 200)); len(l) != 2 {
		t.Errorf("tag filter %v", l)
	}
	if c := obj(h.do("GET", "/"+acct+"/cards/1", "", 200)); !reflect.DeepEqual(c["tags"], []any{"bug"}) {
		t.Errorf("card tags %v", c["tags"])
	}
	h.do("POST", "/"+acct+"/cards/1/taggings.json", `{"tag_title":"bug"}`, 204) // toggles off
	if c := obj(h.do("GET", "/"+acct+"/cards/1", "", 200)); len(arr(c["tags"])) != 0 {
		t.Errorf("toggle off %v", c["tags"])
	}
	h.do("POST", "/"+acct+"/cards/1/taggings.json", `{"tag_title":"  "}`, 422)
}

func TestPaginationLink(t *testing.T) {
	h := newHarness(t)
	h.api.PerPage = 2
	board, _, _ := h.board()
	for range 5 {
		h.card(board, "c")
	}
	next := "http://example.com/fizzy/" + acct + "/cards.json?board_ids[]=" + board
	var seen []float64
	for pages := 0; next != ""; pages++ {
		if pages > 5 {
			t.Fatal("pages never end")
		}
		w := h.req("GET", next, "", h.token)
		if w.Code != 200 {
			t.Fatalf("%s: %d", next, w.Code)
		}
		var l []map[string]any
		json.Unmarshal(w.Body.Bytes(), &l)
		for _, c := range l {
			seen = append(seen, c["number"].(float64))
		}
		next = ""
		if link := w.Header().Get("Link"); link != "" {
			if !strings.HasSuffix(link, `>; rel="next"`) || !strings.HasPrefix(link, "<http://example.com/fizzy/") {
				t.Fatalf("link %q", link)
			}
			next = strings.TrimSuffix(strings.TrimPrefix(link, "<"), `>; rel="next"`)
			if !strings.Contains(next, "board_ids") {
				t.Errorf("the next page lost the filter: %s", next)
			}
		}
	}
	if !reflect.DeepEqual(seen, []float64{5, 4, 3, 2, 1}) {
		t.Errorf("pages gave %v", seen)
	}
	for range 3 {
		h.do("POST", "/"+acct+"/cards/1/comments.json", `{"body":"x"}`, 201)
	}
	w := h.req("GET", "http://example.com/fizzy/"+acct+"/cards/1/comments.json?page=2", "", h.token)
	if w.Code != 200 || w.Header().Get("Link") != "" || !strings.Contains(w.Body.String(), `"plain_text":"x"`) {
		t.Errorf("comments page 2: %d %q %s", w.Code, w.Header().Get("Link"), w.Body)
	}
	if w := h.req("GET", "http://example.com/fizzy/"+acct+"/cards.json?page=0", "", h.token); w.Code != 400 {
		t.Errorf("page 0: %d", w.Code)
	}
	// behind tailscale serve the links are https
	r := httptest.NewRequest("GET", "http://box.ts.net/fizzy/"+acct+"/cards.json", nil)
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set("X-Forwarded-Proto", "https")
	rw := httptest.NewRecorder()
	h.api.ServeHTTP(rw, r)
	if !strings.HasPrefix(rw.Header().Get("Link"), "<https://box.ts.net/fizzy/") {
		t.Errorf("link behind a proxy %q", rw.Header().Get("Link"))
	}
}

func TestPathsAndBodiesAreBounded(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	h.card(board, "a")
	for _, p := range []string{
		"/" + acct + "/cards/..%2F..%2Ftokens",
		"/" + acct + "/cards/..",
		"/" + acct + "/cards/1%2F..%2F2",
		"/" + acct + "/boards/..%2F..%2Ftokens",
		"/" + acct + "/boards/" + board + "%2F..%2F..%2Faccount",
		"/" + acct + "/boards/" + board + "/columns/..%2F..",
		"/" + acct + "/users/..%2F..%2Ftokens",
		"/..%2F" + acct + "/boards.json",
		"/" + acct + "/cards/1/comments/..%2F..",
		"/" + acct + "/cards/1/steps/%2E%2E",
	} {
		w := h.req("GET", "http://example.com/fizzy"+p, "", h.token)
		if w.Code != 404 && w.Code != 400 && w.Code != 301 && w.Code != 307 {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "sha256") || strings.Contains(w.Body.String(), "last_number") {
			t.Errorf("%s leaked a file: %s", p, w.Body)
		}
	}
	big := `{"body":"` + strings.Repeat("x", 2<<20) + `"}`
	if w := h.req("POST", "http://example.com/fizzy/"+acct+"/cards/1/comments.json", big, h.token); w.Code != 413 {
		t.Errorf("a 2 MB body: %d", w.Code)
	}
}

func TestRichText(t *testing.T) {
	cases := []struct{ in, html, plain, md string }{
		{"hello", "<p>hello</p>", "hello", "hello"},
		{"a & b\n\nc <x", "<p>a &amp; b</p><p>c &lt;x</p>", "a & b\n\nc <x", "a & b\n\nc <x"},
		{"<p>Some <strong>bold</strong> and <a href=\"https://x.test/?a=1&amp;b=2\">a link</a></p><ul><li>one</li><li>two</li></ul><pre>x  y\n z</pre>",
			"", "Some bold and a link\n\n- one\n- two\n\nx  y\n z",
			"Some **bold** and [a link](https://x.test/?a=1&b=2)\n\n- one\n- two\n\n```\nx  y\n z\n```"},
		{"<h1>Title</h1><p>line<br>break <code>c</code></p>", "", "Title\n\nline\nbreak c", "## Title\n\nline\nbreak `c`"},
	}
	for _, c := range cases {
		h := RichText(c.in)
		if c.html != "" && h != c.html {
			t.Errorf("RichText(%q) = %q", c.in, h)
		}
		if p := PlainText(h, false); p != c.plain {
			t.Errorf("PlainText(%q)\n got %q\nwant %q", h, p, c.plain)
		}
		if m := PlainText(h, true); m != c.md {
			t.Errorf("PlainText md (%q)\n got %q\nwant %q", h, m, c.md)
		}
	}
}
