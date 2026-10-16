package fizz

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFieldLimits(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	h.card(board, "ok")
	long := strings.Repeat("x", maxTitle+1)
	huge := strings.Repeat("y", maxText+1)
	cases := []struct{ method, path, body string }{
		{"POST", "/cards.json", `{"board_id":"` + board + `","title":"` + long + `"}`},
		{"POST", "/cards.json", `{"board_id":"` + board + `","title":"t","description":"` + huge + `"}`},
		{"PATCH", "/cards/1", `{"title":"` + long + `"}`},
		{"PATCH", "/cards/1", `{"description":"` + huge + `"}`},
		{"POST", "/cards/1/comments.json", `{"body":"` + huge + `"}`},
		{"POST", "/cards/1/steps.json", `{"content":"` + long + `"}`},
		{"POST", "/boards.json", `{"name":"` + long + `"}`},
		{"POST", "/boards/" + board + "/columns.json", `{"name":"` + long + `"}`},
		{"POST", "/cards/1/taggings.json", `{"tag_title":"` + long + `"}`},
	}
	for _, c := range cases {
		w := h.req(c.method, "http://example.com/fizzy/"+acct+c.path, c.body, h.token)
		var e map[string]any
		if w.Code != 422 || json.Unmarshal(w.Body.Bytes(), &e) != nil || !strings.Contains(e["error"].(string), "too long") {
			t.Errorf("%s %s (%d bytes): %d %.200s", c.method, c.path, len(c.body), w.Code, w.Body)
		}
	}
	// at the limit is fine
	h.do("POST", "/"+acct+"/cards/1/comments.json", `{"body":"<p>`+strings.Repeat("z", maxText-7)+`</p>"}`, 201)
}

func TestCountLimits(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	h.card(board, "full")
	card, _ := h.st.Card(acct, 1)
	for i := range maxSteps {
		card.Steps = append(card.Steps, &Step{ID: NewID(), Content: strconv.Itoa(i)})
	}
	for i := range maxComments {
		card.Comments = append(card.Comments, &Comment{ID: NewID(), Body: strconv.Itoa(i)})
	}
	h.st.SaveCard(acct, card)
	h.do("POST", "/"+acct+"/cards/1/steps.json", `{"content":"one more"}`, 422)
	h.do("POST", "/"+acct+"/cards/1/comments.json", `{"body":"one more"}`, 422)
	// a move still happens at the cap; only its note is left out
	h.do("POST", "/"+acct+"/cards/1/closure.json", "", 204)
	if c, _ := h.st.Card(acct, 1); !c.Closed || len(c.Comments) != maxComments {
		t.Errorf("closed %v with %d comments", c.Closed, len(c.Comments))
	}
	// a page of comments stays well under the budget's reach
	if l := arr(h.do("GET", "/"+acct+"/cards/1/comments.json", "", 200)); len(l) != h.api.PerPage {
		t.Errorf("comment page %d", len(l))
	}

	dir := filepath.Join(h.st.Dir, acct, "cards")
	for n := 2; n <= maxCards; n++ {
		os.WriteFile(filepath.Join(dir, strconv.Itoa(n)+".json"), []byte("{}"), 0o600)
	}
	w := h.req("POST", "http://example.com/fizzy/"+acct+"/cards.json", `{"board_id":"`+board+`","title":"too many"}`, h.token)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "as many cards") {
		t.Errorf("card 10001: %d %s", w.Code, w.Body)
	}
}

func TestFullStoreRefusesWrites(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	h.st.MaxBytes = 1
	h.st.sizeAt = time.Time{}
	w := h.req("POST", "http://example.com/fizzy/"+acct+"/cards.json", `{"board_id":"`+board+`","title":"x"}`, h.token)
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("a write to a full store: %d %s", w.Code, w.Body)
	}
	h.do("GET", "/"+acct+"/boards.json", "", 200)
}

func TestNumbersAndSoftDelete(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	h.card(board, "one")
	// a card file already at the next number (another writer, an import) is skipped
	c, _ := h.st.Card(acct, 1)
	c.Number, c.ID = 2, NewID()
	h.st.SaveCard(acct, c)
	if n := h.card(board, "three"); n != 3 {
		t.Errorf("next number %d", n)
	}
	h.do("DELETE", "/"+acct+"/cards/3", "", 204)
	h.do("GET", "/"+acct+"/cards/3", "", 404)
	h.do("DELETE", "/"+acct+"/cards/3", "", 404)
	if l := arr(h.do("GET", "/"+acct+"/cards.json", "", 200)); len(l) != 2 {
		t.Errorf("a deleted card is listed: %d", len(l))
	}
	trash, _ := os.ReadDir(filepath.Join(h.st.Dir, acct, "cards", ".trash"))
	if len(trash) != 1 || !strings.HasPrefix(trash[0].Name(), "3-") {
		t.Fatalf("trash %v", trash)
	}
	if n := h.card(board, "four"); n != 4 {
		t.Errorf("a deleted card's number came back: %d", n)
	}
	if err := h.st.RestoreCard(acct, 3); err != nil {
		t.Fatal(err)
	}
	if c := obj(h.do("GET", "/"+acct+"/cards/3", "", 200)); c["title"] != "three" {
		t.Errorf("restored %v", c)
	}
	if err := h.st.RestoreCard(acct, 3); err == nil {
		t.Error("restored over a live card")
	}
	if err := h.st.RestoreCard(acct, 99); err != ErrNotFound {
		t.Errorf("restoring what was never deleted: %v", err)
	}
}

func TestSearchTermsAreCapped(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	h.card(board, "w0 w1 w2 w3 w4 w5 w6 w7 w8 w9")
	q := strings.Repeat("w0 ", 30) + "w1 w2 w3 w4 w5 w6 w7 w8 w9 W9 nowhere"
	if l := arr(h.do("GET", "/"+acct+"/search.json?q="+strings.ReplaceAll(q, " ", "+"), "", 200)); len(l) != 1 {
		t.Errorf("only the first ten different words count; got %d hits", len(l))
	}
	f, _ := newFilter(map[string][]string{"terms[]": {q}}, nil, time.Now())
	if len(f.terms) != maxTerms {
		t.Errorf("terms %v", f.terms)
	}
}

func TestListPagesStaySmall(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	big := strings.Repeat("word ", 25000) // 125 KB
	for range 12 {
		h.do("POST", "/"+acct+"/cards.json", `{"board_id":"`+board+`","title":"big","description":"`+big+`"}`, 201)
	}
	next := "http://example.com/fizzy/" + acct + "/cards.json"
	seen := 0
	for pages := 0; next != ""; pages++ {
		if pages > 20 {
			t.Fatal("pages never end")
		}
		w := h.req("GET", next, "", h.token)
		if w.Body.Len() > 2<<20 {
			t.Errorf("a page of %d bytes", w.Body.Len())
		}
		var l []any
		json.Unmarshal(w.Body.Bytes(), &l)
		if len(l) == 0 {
			t.Fatalf("an empty page at %s", next)
		}
		seen += len(l)
		next = ""
		if link := w.Header().Get("Link"); link != "" {
			next = strings.TrimSuffix(strings.TrimPrefix(link, "<"), `>; rel="next"`)
		}
	}
	if seen != 12 {
		t.Errorf("pages held %d cards", seen)
	}
	if w := h.req("GET", "http://example.com/fizzy/"+acct+"/cards.json?page=50", "", h.token); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("past the end: %d %s", w.Code, w.Body)
	}
}

// blockingWriter holds the response until released.
type blockingWriter struct {
	*httptest.ResponseRecorder
	release chan struct{}
}

func (b blockingWriter) Write(p []byte) (int, error) {
	<-b.release
	return b.ResponseRecorder.Write(p)
}

func TestSlowClientsDoNotHoldTheLock(t *testing.T) {
	h := newHarness(t)
	board, _, _ := h.board()
	h.card(board, "a")
	quick := func() {
		t.Helper()
		done := make(chan int)
		go func() {
			w := h.req("GET", "http://example.com/fizzy/"+acct+"/cards/1", "", h.token)
			done <- w.Code
		}()
		select {
		case code := <-done:
			if code != 200 {
				t.Errorf("quick request %d", code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a request waited on a slow client")
		}
	}

	// a body still arriving
	pr, pw := io.Pipe()
	r := httptest.NewRequest("POST", "http://example.com/fizzy/"+acct+"/cards/1/comments.json", pr)
	r.Header.Set("Authorization", "Bearer "+h.token)
	slow := make(chan int)
	go func() {
		w := httptest.NewRecorder()
		h.api.ServeHTTP(w, r)
		slow <- w.Code
	}()
	pw.Write([]byte(`{"body":`))
	quick()
	pw.Write([]byte(`"late"}`))
	pw.Close()
	if code := <-slow; code != 201 {
		t.Errorf("slow post %d", code)
	}

	// an answer not yet read
	bw := blockingWriter{httptest.NewRecorder(), make(chan struct{})}
	r = httptest.NewRequest("GET", "http://example.com/fizzy/"+acct+"/cards/1", nil)
	r.Header.Set("Authorization", "Bearer "+h.token)
	go func() {
		h.api.ServeHTTP(bw, r)
		slow <- bw.Code
	}()
	time.Sleep(50 * time.Millisecond)
	quick()
	close(bw.release)
	<-slow
}

func TestLockIsAcrossOpens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "boards")
	one, two := Open(dir), Open(dir) // as two processes would: separate open files
	if err := one.Lock(); err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		if err := two.Lock(); err != nil {
			t.Error(err)
		}
		close(got)
		two.Unlock()
	}()
	select {
	case <-got:
		t.Fatal("two holders at once")
	case <-time.After(100 * time.Millisecond):
	}
	one.Unlock()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the lock was never handed on")
	}
}
