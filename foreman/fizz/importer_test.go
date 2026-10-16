package fizz

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFizzy answers like app.fizzy.do for one account, two items per page.
type fakeFizzy struct {
	handler  http.Handler
	mu       sync.Mutex
	paths    []string
	evilNext bool // a page whose next link points at another host
	redirect bool // identity answers with a redirect elsewhere
	bigCard  bool // a card numbered past nine digits
}

const (
	bA      = "03fboarda00000000000000aa"
	bB      = "03fboardb00000000000000bb"
	colDo   = "03fcolumndoing00000000000"
	colRev  = "03fcolumnreview000000000r"
	uSteve  = "03fuserstephen0000000000s"
	uAgent  = "03fuseragent000000000000a"
	uSystem = "03fusersystem00000000000y"
)

func user(id, name, role string) map[string]any {
	return map[string]any{"id": id, "name": name, "role": role, "active": true, "email_address": strings.ToLower(name) + "@example.com",
		"created_at": "2025-01-01T00:00:00.000Z", "url": "x", "avatar_url": "x"}
}

func fakeCard(n int, title string, extra map[string]any) map[string]any {
	c := map[string]any{"id": fmt.Sprintf("03fcard%018d", n), "number": n, "title": title, "status": "published",
		"description": "plain " + title, "description_html": "<p>plain <em>" + title + "</em></p>",
		"closed": false, "postponed": false, "golden": false, "created_at": fmt.Sprintf("2025-02-%02dT10:00:00.000Z", n),
		"last_active_at": fmt.Sprintf("2025-03-%02dT10:00:00.000Z", n), "board": map[string]any{"id": bA, "name": "Alpha"},
		"creator": user(uSteve, "Stephen", "owner"), "assignees": []any{}, "tags": []any{}}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func newFakeFizzy(t *testing.T) *fakeFizzy {
	f := &fakeFizzy{}
	cards := []map[string]any{
		fakeCard(3, "In Maybe", nil),
		fakeCard(5, "Doing it", map[string]any{"column": map[string]any{"id": colDo, "name": "Doing"}, "golden": true,
			"tags": []any{"bug", "urgent"}, "assignees": []any{user(uAgent, "Agent", "member")}}),
		fakeCard(7, "Finished", map[string]any{"closed": true, "column": map[string]any{"id": colRev}}),
		fakeCard(8, "Later", map[string]any{"postponed": true}),
		fakeCard(9, "Other board", map[string]any{"board": map[string]any{"id": bB}}),
	}
	page := func(w http.ResponseWriter, r *http.Request, items []any) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if p < 1 {
			p = 1
		}
		from, to := min((p-1)*2, len(items)), min(p*2, len(items))
		if to < len(items) {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(p+1))
			host := r.URL.Scheme + "://" + r.Host // the in-memory transport hands over the client request
			if f.evilNext {
				host = "http://evil.example"
			}
			w.Header().Set("Link", "<"+host+r.URL.Path+"?"+q.Encode()+`>; rel="next"`)
		}
		json.NewEncoder(w).Encode(items[from:to])
	}
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fizzy-token" {
			w.WriteHeader(401)
			return
		}
		p := strings.TrimSuffix(r.URL.Path, ".json")
		if f.redirect {
			http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
			return
		}
		if f.bigCard && p == "/"+acct+"/cards" && r.URL.Query().Get("indexed_by") == "all" {
			json.NewEncoder(w).Encode([]any{fakeCard(1_000_000_000, "Huge", nil)})
			return
		}
		switch {
		case p == "/my/identity":
			json.NewEncoder(w).Encode(map[string]any{"accounts": []any{map[string]any{"id": "x", "name": "Stephen's Fizzy", "slug": "/" + acct}}})
		case p == "/"+acct+"/users":
			page(w, r, []any{user(uSteve, "Stephen", "owner"), user(uAgent, "Agent", "member"), user(uSystem, "System", "system")})
		case p == "/"+acct+"/tags":
			page(w, r, []any{map[string]any{"id": "03ftagbug000000000000000b", "title": "bug", "created_at": "2025-01-02T00:00:00.000Z"}})
		case p == "/"+acct+"/boards":
			page(w, r, []any{
				map[string]any{"id": bA, "name": "Alpha", "all_access": true, "auto_postpone_period_in_days": 30, "created_at": "2025-01-01T00:00:00.000Z", "creator": user(uSteve, "Stephen", "owner")},
				map[string]any{"id": bB, "name": "Beta", "all_access": false, "auto_postpone_period_in_days": 7, "created_at": "2025-01-02T00:00:00.000Z", "creator": user(uSteve, "Stephen", "owner")},
			})
		case strings.HasSuffix(p, "/columns"):
			if strings.Contains(p, bA) {
				page(w, r, []any{
					map[string]any{"id": colDo, "name": "Doing", "color": map[string]any{"name": "Lime", "value": "var(--color-card-4)"}, "created_at": "2025-01-01T00:00:00.000Z"},
					map[string]any{"id": colRev, "name": "Review", "color": "var(--color-card-6)", "created_at": "2025-01-01T00:00:00.000Z"},
				})
			} else {
				page(w, r, []any{})
			}
		case p == "/"+acct+"/cards":
			q := r.URL.Query()
			var out []any
			for _, c := range cards {
				board := c["board"].(map[string]any)["id"]
				closed, postponed := c["closed"].(bool), c["postponed"].(bool)
				var in bool
				switch q.Get("indexed_by") {
				case "closed":
					in = closed
				case "not_now":
					in = postponed && !closed
				default:
					in = !closed && !postponed
				}
				if in && board == q.Get("board_ids[]") {
					out = append(out, c)
				}
			}
			page(w, r, out)
		case strings.HasPrefix(p, "/"+acct+"/cards/") && strings.HasSuffix(p, "/comments"):
			n := strings.Split(p, "/")[3]
			if n != "5" {
				page(w, r, []any{})
				return
			}
			var cms []any
			for i := 1; i <= 5; i++ {
				who := user(uAgent, "Agent", "member")
				if i == 2 {
					who = user(uSystem, "System", "system")
				}
				cms = append(cms, map[string]any{"id": fmt.Sprintf("03fcomment%015d", i), "created_at": fmt.Sprintf("2025-03-01T10:0%d:00.000Z", i),
					"updated_at": fmt.Sprintf("2025-03-01T10:0%d:00.000Z", i), "creator": who,
					"body": map[string]any{"html": fmt.Sprintf("<p>comment %d</p>", i), "plain_text": fmt.Sprintf("comment %d", i)}})
			}
			page(w, r, cms)
		case strings.HasPrefix(p, "/"+acct+"/cards/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(p, "/"+acct+"/cards/"))
			for _, c := range cards {
				if c["number"] == n {
					shown := map[string]any{}
					for k, v := range c {
						shown[k] = v
					}
					shown["steps"] = []any{}
					if n == 5 {
						shown["steps"] = []any{map[string]any{"id": "03fstep000000000000000001", "content": "one", "completed": true},
							map[string]any{"id": "03fstep000000000000000002", "content": "two", "completed": false}}
					}
					json.NewEncoder(w).Encode(shown)
					return
				}
			}
			w.WriteHeader(404)
		default:
			w.WriteHeader(404)
		}
	})
	return f
}

const fakeBase = "https://fizzy.test"

// inMemory serves requests straight from a handler: no sockets in tests.
type inMemory struct{ h http.Handler }

func (m inMemory) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	m.h.ServeHTTP(w, r)
	return w.Result(), nil
}

func (f *fakeFizzy) importer(token string, boards ...string) *Importer {
	return &Importer{Base: fakeBase, Token: token, Account: acct, Boards: boards, Client: &http.Client{Transport: inMemory{f.handler}}}
}

func TestImportFromFizzy(t *testing.T) {
	f := newFakeFizzy(t)
	st := Open(filepath.Join(t.TempDir(), "boards"))
	st.Now = func() time.Time { return time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC) }
	// init made a local Stephen first; the import maps Fizzy's Stephen onto him
	st.Init(acct, "Projects", "Stephen", "")
	before, _ := st.Account(acct)
	localSteve := before.UserNamed("Stephen").ID

	im := f.importer("fizzy-token", bA)
	n, err := im.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	if n.Boards != 1 || n.Cards != 4 || n.Comments != 5 {
		t.Errorf("counts %+v", n)
	}
	a, _ := st.Account(acct)
	if a.LastNumber != 8 {
		t.Errorf("last number %d", a.LastNumber)
	}
	if u := a.User(uAgent); u == nil || u.Name != "Agent" || u.Email != "agent@example.com" {
		t.Errorf("agent user %+v", u)
	}
	// users map by id only: Fizzy's Stephen is not taken for the local one by name
	if u := a.User(uSteve); u == nil || u.ID == localSteve || a.User(localSteve).Email != "" {
		t.Errorf("Fizzy's Stephen should be a user of his own: %+v", a.Users)
	}
	if u := a.User(uSystem); u == nil || u.Role != "system" || len(a.Users) != 5 {
		t.Errorf("users %+v", a.Users)
	}
	b, err := st.Board(acct, bA)
	if err != nil || b.Name != "Alpha" || len(b.Columns) != 2 || b.Columns[0].ColorName != "Lime" || b.Columns[1].ColorValue != "var(--color-card-6)" || b.CreatorID != uSteve {
		t.Fatalf("board %+v %v", b, err)
	}
	if _, err := st.Board(acct, bB); err != ErrNotFound {
		t.Error("imported a board not asked for")
	}
	cards, _ := st.Cards(acct)
	var nums []int
	for _, c := range cards {
		nums = append(nums, c.Number)
	}
	if !reflect.DeepEqual(nums, []int{3, 5, 7, 8}) {
		t.Fatalf("numbers %v", nums)
	}
	c5, _ := st.Card(acct, 5)
	if c5.ID != "03fcard000000000000000005" || c5.ColumnID != colDo || !c5.Golden || c5.CreatedAt != "2025-02-05T10:00:00.000Z" ||
		c5.LastActiveAt != "2025-03-05T10:00:00.000Z" || c5.Description != "<p>plain <em>Doing it</em></p>" {
		t.Errorf("card 5 %+v", c5)
	}
	if len(c5.Steps) != 2 || !c5.Steps[0].Completed || c5.Steps[1].Content != "two" {
		t.Errorf("steps %+v", c5.Steps)
	}
	if len(c5.Comments) != 5 || c5.Comments[4].Body != "<p>comment 5</p>" || c5.Comments[0].CreatorID != uAgent ||
		a.User(c5.Comments[1].CreatorID).Role != "system" {
		t.Errorf("comments %+v", c5.Comments)
	}
	if !reflect.DeepEqual(c5.Assignees, []string{uAgent}) || len(c5.Tags) != 2 || a.Tag(c5.Tags[0]).ID != "03ftagbug000000000000000b" || a.Tag(c5.Tags[1]).Title != "urgent" {
		t.Errorf("assignees %v tags %v", c5.Assignees, c5.Tags)
	}
	c7, _ := st.Card(acct, 7)
	c8, _ := st.Card(acct, 8)
	c3, _ := st.Card(acct, 3)
	if !c7.Closed || c7.ColumnID != colRev || !c8.Postponed || c8.Closed || c3.ColumnID != "" || c3.Closed {
		t.Errorf("states 3 %+v 7 %+v 8 %+v", c3, c7, c8)
	}

	// the API serves what was imported, and new cards continue the numbering
	tok, _ := st.AddToken("me", acct, localSteve)
	api := NewAPI(st)
	r := httptest.NewRequest("POST", "http://example.com/fizzy/"+acct+"/cards.json", strings.NewReader(`{"board_id":"`+bA+`","title":"new"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"number":9`) {
		t.Errorf("new card after import: %d %s", w.Code, w.Body)
	}

	// a second run refreshes in place; a local card in the way stops it
	if _, err := im.Run(st); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	c3.ID = NewID()
	st.SaveCard(acct, c3)
	if _, err := im.Run(st); err == nil || !strings.Contains(err.Error(), "#3") {
		t.Errorf("a clashing local card: %v", err)
	}

	for _, p := range f.paths {
		if strings.Contains(p, "/cards.json") && !strings.Contains(p, "board_ids") {
			t.Errorf("an unfiltered card list: %s", p)
		}
	}
}

func TestImportKeepsTheTokenOnItsHost(t *testing.T) {
	f := newFakeFizzy(t)
	f.evilNext = true
	st := Open(filepath.Join(t.TempDir(), "boards"))
	_, err := f.importer("fizzy-token").Run(st)
	if err == nil || !strings.Contains(err.Error(), "refusing to follow") {
		t.Fatalf("followed a next link to another host: %v", err)
	}
	if _, err := f.importer("wrong").Run(st); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a wrong token: %v", err)
	}
	if _, err := f.importer("fizzy-token", "nope").Run(st); err == nil {
		t.Error("a missing board was not an error")
	}

	f.evilNext = false
	f.redirect = true
	if _, err := f.importer("fizzy-token").Run(st); err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("a redirect was followed (or not reported): %v", err)
	}
	f.redirect = false

	for _, base := range []string{"http://fizzy.test", "ftp://fizzy.test", "fizzy.test"} {
		im := f.importer("fizzy-token")
		im.Base = base
		if _, err := im.Run(st); err == nil {
			t.Errorf("imported over %s", base)
		}
	}
	for _, base := range []string{"http://127.0.0.1:9", "http://localhost:9"} {
		im := f.importer("fizzy-token", bA)
		im.Base = base
		if _, err := im.Run(st); err != nil {
			t.Errorf("a loopback test server %s: %v", base, err)
		}
	}

	f.bigCard = true
	if _, err := f.importer("fizzy-token", bA).Run(Open(filepath.Join(t.TempDir(), "b"))); err == nil || !strings.Contains(err.Error(), "1000000000") {
		t.Errorf("a ten-digit card number: %v", err)
	}
}
