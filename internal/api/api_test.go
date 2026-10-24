package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stevegeek/agent-factory/internal/guard"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

// The request a verb gets is what the CLI read on stdin: the body, the
// typed query and the path id, top-level keys sorted; the caller is the
// token, never a person; bad ids and a body naming the id never reach the
// verb (Foreman's TestAPIArgvAndInput: argv, stdin and env become the
// verb, the caller, the request and the wait).
func TestAPIArgvAndInput(t *testing.T) {
	h, f, tokens := newFakeHarness(t, harnessOpt{})
	tok := tokens[0]
	w := h.call("POST", "/api/v1/tickets/T0001/notes", tok, map[string]any{"text": "--as maintainer; rm -rf /"})
	want(t, w, 201, "note")
	got := f.taken()
	if len(got) != 1 || got[0].verb != "note" || got[0].caller != ticketapi.ForToken("example-agent") || got[0].wait != 10*time.Second {
		t.Fatalf("calls: %+v", got)
	}
	if got[0].caller == ticketapi.ForPerson() {
		t.Errorf("a token never acts as a person")
	}
	if in := got[0].in; in["id"] != "T0001" || in["text"] != "--as maintainer; rm -rf /" || len(in) != 2 {
		t.Errorf("request: %v", in)
	}
	if got[0].raw != `{"id":"T0001","text":"--as maintainer; rm -rf /"}` {
		t.Errorf("the request's keys are sorted, as the CLI read them: %s", got[0].raw)
	}
	// nested values keep their order; numbers keep their text
	h.call("PATCH", "/api/v1/tickets/T0001", tok, `{"title": "y", "links": [{"url": "https://example.com", "title": "x"}], "n": 1.50}`)
	if got := f.taken(); len(got) != 1 || got[0].raw != `{"id":"T0001","links":[{"url":"https://example.com","title":"x"}],"n":1.5,"title":"y"}` {
		t.Errorf("nested order: %+v", got)
	}
	// ids that are not ids, and a body naming the id, never reach the verb
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
		if w := h.call("POST", k.path, tok, k.body); w.Code != k.status {
			t.Errorf("%s: %d %s", k.path, w.Code, w.Body.String())
		}
	}
	if got := f.taken(); len(got) != 0 {
		t.Errorf("the verb ran for a bad request: %+v", got)
	}
	// the query becomes typed input; q is text
	want(t, h.call("GET", "/api/v1/tickets?q=date&limit=5&include_closed=true&assignee=me", tok, nil), 200, "list")
	got = f.taken()
	if in := got[0].in; got[0].verb != "list" || in["text"] != "date" || in["limit"] != float64(5) || in["include_closed"] != true || in["assignee"] != "me" {
		t.Errorf("list input: %+v", got)
	}
	w = h.call("POST", "/api/v1/tickets", tok, map[string]any{"project": "sandbox", "title": "x"}, header("Idempotency-Key", "k-1"))
	got = f.taken()
	if w.Code != 201 || w.Header().Get("Location") != "/api/v1/tickets/T0007" || got[0].in["idempotency_key"] != "k-1" {
		t.Errorf("create: %d %q %+v", w.Code, w.Header().Get("Location"), got)
	}
	if w := h.call("POST", "/api/v1/tickets", tok, map[string]any{"title": "x"}, header("Idempotency-Key", "a"), header("Idempotency-Key", "b")); w.Code != 400 {
		t.Errorf("two keys: %d", w.Code)
	}
	f.setMode("replay")
	w = h.call("POST", "/api/v1/tickets", tok, map[string]any{"project": "sandbox", "title": "x"})
	if w.Code != 200 || w.Header().Get("Location") != "" {
		t.Errorf("replay: %d %q", w.Code, w.Header().Get("Location"))
	}
	if out := decodeJSON(t, w); out["ok"] != nil {
		t.Errorf("ok is the CLI's, not the API's: %v", out)
	}
	if w.Body.String() != `{"created":false,"ticket":{"id":"T0007"}}`+"\n" {
		t.Errorf("the answer as Foreman relayed it: %q", w.Body.String())
	}
}

// Every refusal code maps to its status with the message and field; a
// failure of the factory's own is a 500 with no detail (TestAPIExitMapping:
// the CLI's crash, usage, no JSON and too much output become an error or a
// panic in the verb; a hang is the 30 s deadline).
func TestAPIExitMapping(t *testing.T) {
	h, f, tokens := newFakeHarness(t, harnessOpt{timeout: 200 * time.Millisecond})
	statuses := map[string]int{"bad_request": 400, "unauthorized": 401, "forbidden": 403, "factory_ticket": 403,
		"not_on_factory_ticket": 403, "not_found": 404, "conflict": 409, "idempotency_mismatch": 422, "too_many": 429,
		"busy": 503, "storage_full": 507}
	for code, status := range statuses {
		f.setMode("refuse:" + code)
		w := h.call("GET", "/api/v1/tickets/T0001", tokens[0], nil)
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
		if wa := w.Header().Get("WWW-Authenticate"); (code == "unauthorized") != (wa != "") {
			t.Errorf("%s: WWW-Authenticate %q", code, wa)
		}
	}
	// every code the verbs can refuse with has a status
	for _, code := range []string{ticketapi.BadRequest, ticketapi.NotFound, ticketapi.Forbidden, ticketapi.Unauthorized,
		ticketapi.Conflict, ticketapi.Busy, ticketapi.TooMany, ticketapi.StorageFull, ticketapi.IdempotencyMismatch,
		ticketapi.FactoryTicket, ticketapi.NotOnFactoryTicket} {
		if _, ok := statusOf(code); !ok || statuses[code] == 0 {
			t.Errorf("%s has no status", code)
		}
	}
	for _, m := range []string{"refuse:something_new", "crash", "panic", "hang"} {
		f.setMode(m)
		w := h.call("GET", "/api/v1/tickets/T0001", tokens[0], nil)
		if w.Code != 500 || errCode(t, w) != "internal" || strings.Contains(w.Body.String(), "Traceback") || strings.Contains(w.Body.String(), "something_new") {
			t.Errorf("%s: %d %s", m, w.Code, w.Body.String())
		}
	}
	if !strings.Contains(h.log.all(), "something_new") || !strings.Contains(h.log.all(), "Traceback") {
		t.Errorf("the failures are in the journal: %s", h.log.all())
	}
}

// Two verbs at a time, one per token; the third waits up to 10 s and then
// is busy (TestAPIConcurrency).
func TestAPIConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, f, tokens := newFakeHarness(t, harnessOpt{slotWait: 300 * time.Millisecond})
		f.setMode("slow")
		codes := make([]int, 3)
		retry := make([]string, 3)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i == 2 {
					time.Sleep(100 * time.Millisecond) // the first two hold both slots
				}
				w := h.call("GET", "/api/v1/me", tokens[i], nil)
				codes[i], retry[i] = w.Code, w.Header().Get("Retry-After")
			}()
		}
		wg.Wait()
		if codes[0] != 200 || codes[1] != 200 || codes[2] != 503 || retry[2] != "5" {
			t.Errorf("two at a time, the third waits and then gives up: %v %v", codes, retry)
		}

		// a waiting request takes the slot once one is free
		long, lf, ltokens := newFakeHarness(t, harnessOpt{slotWait: 3 * time.Second})
		lf.setMode("slow")
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = long.call("GET", "/api/v1/me", ltokens[i], nil).Code
			}()
		}
		wg.Wait()
		if codes[0] != 200 || codes[1] != 200 || codes[2] != 200 {
			t.Errorf("within the wait, all go through: %v", codes)
		}

		// one token holds one slot at a time
		for i, tok := range []string{tokens[0], tokens[0], tokens[1]} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i > 0 {
					time.Sleep(100 * time.Millisecond)
				}
				codes[i] = h.call("GET", "/api/v1/me", tok, nil).Code
			}()
		}
		wg.Wait()
		if codes[0] != 200 || codes[1] != 503 || codes[2] != 200 {
			t.Errorf("a token's second call waits for its first, others don't: %v", codes)
		}
	})
}

// A client that hangs up does not cut a write short: the verb runs on a
// detached context (§5.6).
func TestAPIClientHangUpDoesNotCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, f, tokens := newFakeHarness(t, harnessOpt{})
		f.release = make(chan struct{})
		f.setMode("block")
		req := httptest.NewRequest("POST", "/api/v1/tickets/T0001/notes", strings.NewReader(`{"text": "x"}`))
		req.RemoteAddr = "127.0.0.1:5555"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tokens[0])
		ctx, cancel := context.WithCancel(req.Context())
		req = req.WithContext(ctx)
		done := make(chan int)
		go func() {
			w := httptest.NewRecorder()
			h.h.ServeHTTP(w, req)
			done <- w.Code
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		close(f.release)
		if code := <-done; code != 201 {
			t.Errorf("the note was cut short: %d", code)
		}
	})
}

func TestAPIBodyAndRoutes(t *testing.T) {
	h, _, tokens := newFakeHarness(t, harnessOpt{})
	good := tokens[0]
	big := `{"text": "` + strings.Repeat("a", guard.BodyLimit) + `"}`
	if w := h.call("POST", "/api/v1/tickets/T0001/notes", good, big); w.Code != 413 || errCode(t, w) != "too_large" {
		t.Errorf("a body over 256 KiB: %d", w.Code)
	}
	if w := h.call("POST", "/api/v1/tickets/T0001/notes", good, `{"text": "x"}`, contentType("text/plain")); w.Code != 415 {
		t.Errorf("not JSON: %d", w.Code)
	}
	if w := h.call("POST", "/api/v1/tickets/T0001/notes", good, `{"text": "x"}`, contentType("application/json; charset=utf-8")); w.Code == 415 {
		t.Errorf("a charset is fine: %d", w.Code)
	}
	for _, body := range []string{`[1, 2]`, `null`, `"x"`, `{"text": `} {
		if w := h.call("POST", "/api/v1/tickets/T0001/notes", good, body); w.Code != 400 {
			t.Errorf("a body that is not an object, %s: %d", body, w.Code)
		}
	}
	if w := h.call("POST", "/api/v1/tickets/T0001/claim", good, ""); w.Code != 200 {
		t.Errorf("an empty body is {}: %d", w.Code)
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

func TestAPIGuessingAndRate(t *testing.T) {
	h, _, tokens := newFakeHarness(t, harnessOpt{all: guard.DefaultAll(), writes: guard.DefaultWrites()})
	good := tokens[0]
	guesser := header("X-Forwarded-For", "100.64.0.66")
	for i := 0; i < guard.FailLimit; i++ {
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
	h.clock.add(guard.FailWindow + time.Second)
	if w := h.call("GET", "/api/v1/me", good, nil, guesser); w.Code != 200 {
		t.Errorf("the block ends with the window: %d", w.Code)
	}

	// writes: a burst of 10, then wait; an id that does not parse stops
	// before the verb but still counts
	for i := 0; i < int(guard.DefaultWrites().Burst); i++ {
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
	h.clock.add(2 * time.Second)
	if w := h.call("POST", "/api/v1/tickets/BAD/notes", good, map[string]any{"text": "x"}); w.Code != 404 {
		t.Errorf("a write after the wait: %d", w.Code)
	}
	for i := 0; i < int(guard.DefaultAll().Burst); i++ {
		h.call("GET", "/api/v1/tickets/BAD", good, nil)
	}
	if w := h.call("GET", "/api/v1/tickets/BAD", good, nil); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Errorf("past the burst: %d", w.Code)
	}
}

// rawAnswer encodes members as the CLI printed them, so the relay matches
// Foreman's byte for byte: sorted keys, compact, HTML characters escaped.
func TestRawAnswer(t *testing.T) {
	title := "<caf" + string(rune(0xe9)) + "> & " + string(rune(0x2028))
	out := obj("ticket", obj("title", jsonx.String(title), "id", jsonx.String("T0001")).Value(),
		"created", jsonx.Bool(true), "n", jsonx.Float(1.5))
	data, _ := json.Marshal(rawAnswer(out))
	bs := string(rune(92))
	esc := func(hex string) string { return bs + "u" + hex }
	wantText := `{"created":true,"n":1.5,"ticket":{"title":"` + esc("003c") + "caf" + esc("00e9") + esc("003e") + " " +
		esc("0026") + " " + esc("2028") + `","id":"T0001"}}`
	if string(data) != wantText {
		t.Errorf("relay: %s\nwant   %s", data, wantText)
	}
}
