package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/foreman/fizz"
)

// projectHarness: Foreman with a project board: one board, a column, two cards.
func projectHarness(t *testing.T) (*harness, *fizz.Store, string, string) {
	h := newHarness(t, false)
	root := t.TempDir()
	var out, errb bytes.Buffer
	if code := runCLI([]string{"boards", "init", "--root", root, "--account", "6182510"}, &out, &errb); code != 0 {
		t.Fatalf("init: %s", errb.String())
	}
	out.Reset()
	if code := runCLI([]string{"tokens", "add", "agent", "--root", root, "--user", "thriceforged-agent"}, &out, &errb); code != 0 {
		t.Fatalf("tokens add: %s", errb.String())
	}
	token := strings.TrimSpace(out.String())
	st := fizz.Open(storeDir(root))
	h.srv.SetProjectBoards(st)
	h.h = h.srv.Handler()

	api := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://example.com/fizzy/6182510"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		return w
	}
	w := api("POST", "/boards.json", `{"name":"Thriceforged"}`)
	if w.Code != 201 {
		t.Fatalf("board: %d %s", w.Code, w.Body)
	}
	b, _ := st.Boards("6182510")
	board := b[0].ID
	api("POST", "/boards/"+board+"/columns.json", `{"name":"Doing"}`)
	b, _ = st.Boards("6182510")
	api("POST", "/cards.json", `{"board_id":"`+board+`","title":"Fix <the> widget","description":"<p>Use <strong>care</strong></p><script>alert(1)</script>"}`)
	api("POST", "/cards.json", `{"board_id":"`+board+`","title":"Second"}`)
	api("POST", "/cards/1/triage.json", `{"column_id":"`+b[0].Columns[0].ID+`"}`)
	api("POST", "/cards/1/steps.json", `{"content":"write a test"}`)
	api("POST", "/cards/1/comments.json", `{"body":"<p>on it <img src=x onerror=alert(1)></p>"}`)
	api("POST", "/cards/2/closure.json", "")
	return h, st, board, token
}

func TestProjectBoardsAPIMount(t *testing.T) {
	h, _, _, token := projectHarness(t)
	// the API takes its token, not the login cookie
	cookie := h.login()
	if w := h.do("GET", "/fizzy/6182510/boards.json", nil, cookie); w.Code != 401 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("a session cookie on the API: %d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest("GET", "http://example.com/fizzy/6182510/boards.json", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Thriceforged") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("with the token: %d %v %s", w.Code, w.Header(), w.Body)
	}
	// the host allowlist still applies
	r = httptest.NewRequest("GET", "http://rebound.example/fizzy/6182510/boards.json", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("an unknown host: %d", w.Code)
	}
	// and the token is no login for the app
	r = httptest.NewRequest("GET", "http://example.com/boards", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Errorf("a token on the app: %d", w.Code)
	}
}

func TestProjectBoardPages(t *testing.T) {
	h, _, board, _ := projectHarness(t)
	c := h.login()
	w := h.do("GET", "/boards", nil, c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `href="/pb/6182510/`+board+`"`) || !strings.Contains(w.Body.String(), "1 open · 1 done") {
		t.Fatalf("boards: %d %s", w.Code, w.Body)
	}
	w = h.do("GET", "/pb/6182510/"+board, nil, c)
	body := w.Body.String()
	for _, want := range []string{"<h3>Maybe? <span>0</span></h3>", "<h3>Doing <span>1</span></h3>", "Fix &lt;the&gt; widget", "Done · 1", `href="/pc/6182510/1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("board page lacks %q", want)
		}
	}
	w = h.do("GET", "/pc/6182510/1", nil, c)
	body = w.Body.String()
	for _, want := range []string{"Fix &lt;the&gt; widget", "<strong>care</strong>", "write a test", "Moved to Doing by thriceforged-agent.", "on it", ">Doing<"} {
		if !strings.Contains(body, want) {
			t.Errorf("card page lacks %q", want)
		}
	}
	for _, bad := range []string{"<script>alert", "onerror", "<img"} {
		if strings.Contains(body, bad) {
			t.Errorf("card page carries %q", bad)
		}
	}
	for _, p := range []string{"/pc/6182510/99", "/pc/6182510/..%2F..%2Ftokens", "/pb/6182510/nosuch", "/pb/..%2F..%2Fboards/x", "/pc/abc/1"} {
		if w := h.do("GET", p, nil, c); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	// logged out: the login page, not the board
	if w := h.do("GET", "/pb/6182510/"+board, nil); w.Code != http.StatusSeeOther {
		t.Errorf("logged out: %d", w.Code)
	}
}

func TestSendACardToTheFactory(t *testing.T) {
	h, st, _, _ := projectHarness(t)
	c := h.login()
	if !strings.Contains(h.do("GET", "/pc/6182510/1", nil, c).Body.String(), `action="/pc/6182510/1/send"`) {
		t.Fatalf("the card page should offer to send it")
	}
	h.argv()
	t.Setenv("FAKE_ID", "T0099")
	csrf := h.srv.auth.CSRF(h.srv.auth.session(c.Value))
	w := h.do("POST", "/pc/6182510/1/send", url.Values{"csrf": {csrf}, "project": {"widgets"}, "kind": {"change"}}, c)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/t/T0099" {
		t.Fatalf("send: %d %s", w.Code, w.Header().Get("Location"))
	}
	// the harness logs one line per call, so a body with newlines spreads over several: read them as one
	raw, _ := os.ReadFile(h.log)
	os.Remove(h.log)
	call := string(raw)
	for _, want := range []string{"add\x1f--body=From card #1 on the Thriceforged board.", "- [ ] write a test", "--\x1fwidgets\x1fFix <the> widget"} {
		if !strings.Contains(call, want) {
			t.Errorf("the add call lacks %q:\n%q", want, call)
		}
	}
	card, _ := st.Card("6182510", 1)
	if last := card.Comments[len(card.Comments)-1]; !strings.Contains(last.Body, "Sent to the factory as T0099 (widgets).") {
		t.Errorf("the card should say where it went: %q", last.Body)
	}
	for _, bad := range []url.Values{{"project": {"nowhere"}, "kind": {"change"}}, {"project": {"widgets"}, "kind": {"manage"}},
		{"project": {"factory"}, "kind": {"change"}}} {
		bad.Set("csrf", csrf)
		if w := h.do("POST", "/pc/6182510/1/send", bad, c); w.Header().Get("Location") != "/pc/6182510/1" {
			t.Errorf("%v: %s", bad, w.Header().Get("Location"))
		}
	}
	if got := h.argv(); len(got) != 0 {
		t.Errorf("a refused send ran %v", got)
	}
}

func TestTokensCLI(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) (string, string, int) {
		var out, errb bytes.Buffer
		code := runCLI(append(args, "--root", root), &out, &errb)
		return out.String(), errb.String(), code
	}
	if _, e, code := run("tokens", "add", "x"); code == 0 || !strings.Contains(e, "boards init") {
		t.Errorf("a token before init: %d %s", code, e)
	}
	run("boards", "init", "--account", "77", "--owner", "Stephen")
	tok, _, code := run("tokens", "add", "stephen")
	if code != 0 || !strings.HasPrefix(tok, "foreman_") {
		t.Fatalf("add: %d %q", code, tok)
	}
	if _, _, code := run("tokens", "add", "stephen"); code == 0 {
		t.Error("a second token with the same name")
	}
	if _, _, code := run("tokens", "add", "../x"); code == 0 {
		t.Error("a token name with a path")
	}
	out, _, _ := run("tokens", "list")
	if !strings.Contains(out, "stephen\taccount 77\tas Stephen") || strings.Contains(out, strings.TrimSpace(tok)) {
		t.Errorf("list %q", out)
	}
	st := fizz.Open(filepath.Join(root, "boards"))
	if got, _ := st.CheckToken(strings.TrimSpace(tok)); got == nil || got.Name != "stephen" {
		t.Errorf("the token does not check: %+v", got)
	}
	if _, _, code := run("tokens", "revoke", "stephen"); code != 0 {
		t.Error("revoke")
	}
	if got, _ := st.CheckToken(strings.TrimSpace(tok)); got != nil {
		t.Error("a revoked token still checks")
	}
	if _, _, code := run("tokens", "revoke", "stephen"); code == 0 {
		t.Error("revoking twice")
	}
	if _, _, code := run("boards", "import", "--token-file", "/nonexistent"); code == 0 {
		t.Error("import without --from-fizzy")
	}
}
