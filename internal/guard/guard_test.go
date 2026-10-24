package guard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stevegeek/agent-factory/internal/tokens"
)

// moving is a clock a test sets.
type moving struct {
	mu sync.Mutex
	t  time.Time
}

func (m *moving) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t
}

func (m *moving) add(d time.Duration) {
	m.mu.Lock()
	m.t = m.t.Add(d)
	m.mu.Unlock()
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// harness: a token store in a 0700 directory, and a guard over it.
type harness struct {
	t     *testing.T
	store string
	clock *moving
	log   *logs
	g     *Guard
	recs  []map[string]any
}

func newHarness(t *testing.T, opt Options) *harness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "conf")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: filepath.Join(dir, "api-tokens.json"),
		clock: &moving{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}, log: &logs{}}
	h.write()
	opt.Tokens = &tokens.Store{Path: h.store, Clock: h.clock, Log: h.log}
	if opt.Clock == nil {
		opt.Clock = h.clock
	}
	opt.Log = h.log
	if opt.Hosts == nil {
		opt.Hosts = []string{"example.com"}
	}
	h.g = New(opt)
	return h
}

func (h *harness) write() {
	h.t.Helper()
	recs := h.recs
	if recs == nil {
		recs = []map[string]any{}
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "tokens": recs})
	if err := os.WriteFile(h.store, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
	// a new mtime even on a coarse clock
	future := time.Now().Add(time.Duration(len(h.recs)+1) * time.Second)
	os.Chtimes(h.store, future, future)
}

// mint adds a record for name, as `factory tokens add` writes one, and
// gives its secret.
func (h *harness) mint(name string, expires any) string {
	secret := "fct_secret-for-" + name
	sum := sha256.Sum256([]byte(secret))
	h.recs = append(h.recs, map[string]any{"name": name, "sha256": hex.EncodeToString(sum[:]),
		"projects": []string{"sandbox"}, "can": []string{"read"}, "added": "2026-10-01T00:00:00+00:00",
		"expires": expires, "tickets": nil})
	h.write()
	return secret
}

type opt func(*http.Request)

func header(k, v string) opt { return func(r *http.Request) { r.Header.Add(k, v) } }
func remote(a string) opt    { return func(r *http.Request) { r.RemoteAddr = a } }

// admit runs one request through Admit and gives the response and the name.
func (h *harness) admit(method, token string, write bool, opts ...opt) (*httptest.ResponseRecorder, string) {
	var body io.Reader
	if method != "GET" {
		body = strings.NewReader("{}")
	}
	r := httptest.NewRequest(method, "/api/v1/me", body)
	r.RemoteAddr = "127.0.0.1:5555"
	if method != "GET" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	name := h.g.Admit(w, r, write)
	return w, name
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON (%d): %s", w.Code, w.Body.String())
	}
	return out.Error.Code
}

// Who may call, from where, with what: TestAPIAuth's guard rows (the Host
// check, the security headers and the session cookie are web's and are
// tested there).
func TestAdmitAuth(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.mint("example-agent", nil)
	revoked := h.mint("gone-agent", nil)
	h.recs = h.recs[:1]
	h.write()
	expired := h.mint("old-agent", "2020-01-01T00:00:00+00:00")
	for _, k := range []struct {
		name   string
		token  string
		opts   []opt
		status int
	}{
		{"no header", "", nil, 401},
		{"basic", "", []opt{header("Authorization", "Basic "+good)}, 401},
		{"bearer without a token", "", []opt{header("Authorization", "Bearer")}, 401},
		{"bearer and spaces", "", []opt{header("Authorization", "Bearer a b")}, 401},
		{"two headers", good, []opt{header("Authorization", "Bearer "+good)}, 401},
		{"wrong token", "fct_not-a-real-token", nil, 401},
		{"revoked", revoked, nil, 401},
		{"expired", expired, nil, 401},
		{"a good token", good, nil, 200},
		{"lower-case scheme", "", []opt{header("Authorization", "bearer "+good)}, 200},
		{"foreign Origin", good, []opt{header("Origin", "https://evil.example")}, 403},
		{"null Origin", good, []opt{header("Origin", "null")}, 403},
		{"plain http Origin", good, []opt{header("Origin", "http://example.com")}, 403},
		{"Origin with a path", good, []opt{header("Origin", "https://example.com/x")}, 403},
		{"two Origins", good, []opt{header("Origin", "https://example.com"), header("Origin", "https://example.com")}, 403},
		{"own Origin", good, []opt{header("Origin", "https://example.com")}, 200},
		{"own Origin, upper case", good, []opt{header("Origin", "https://EXAMPLE.com")}, 200},
		{"funnel", good, []opt{header("Tailscale-Funnel-Request", "?1")}, 403},
		{"proxied from the internet", good, []opt{header("X-Forwarded-For", "203.0.113.9")}, 403},
		{"proxied from the tailnet", good, []opt{header("X-Forwarded-For", "100.64.0.9")}, 200},
		{"proxied from loopback", good, []opt{header("X-Forwarded-For", "127.0.0.1")}, 200},
		{"off the tailnet", good, []opt{remote("192.0.2.1:5555")}, 403},
		{"a tailnet peer while on loopback", good, []opt{remote("100.64.0.5:5555")}, 403},
	} {
		w, name := h.admit("GET", k.token, false, k.opts...)
		status := w.Code
		if name != "" {
			status = 200
			if w.Body.Len() != 0 {
				t.Errorf("%s: admitted, yet answered: %s", k.name, w.Body.String())
			}
		}
		if status != k.status {
			t.Errorf("%s: %d, want %d: %s", k.name, status, k.status, w.Body.String())
		}
		if k.status == 401 && w.Header().Get("WWW-Authenticate") != `Bearer realm="factory"` {
			t.Errorf("%s: no WWW-Authenticate", k.name)
		}
		if k.status != 200 && (w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store") {
			t.Errorf("%s: a JSON refusal: %v", k.name, w.Header())
		}
		if k.status == 200 && name != "example-agent" {
			t.Errorf("%s: name %q", k.name, name)
		}
	}

	open := newHarness(t, Options{Open: true})
	tok := open.mint("example-agent", nil)
	for _, k := range []struct {
		addr string
		ok   bool
	}{{"100.64.0.5:5555", true}, {"[fd7a:115c:a1e0::5]:5555", true}, {"192.0.2.1:5555", false}, {"[2001:db8::1]:5555", false}} {
		if _, name := open.admit("GET", tok, false, remote(k.addr)); (name != "") != k.ok {
			t.Errorf("listening on the tailnet, %s: admitted %v", k.addr, name != "")
		}
	}
}

// A store others can read holds no tokens, and the guard never changes it
// (TestAPIStorePermissions).
func TestAdmitStorePermissions(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.mint("example-agent", nil)
	if err := os.Chmod(h.store, 0o644); err != nil {
		t.Fatal(err)
	}
	if w, _ := h.admit("GET", good, false); w.Code != 401 {
		t.Errorf("a store others can read holds no tokens: %d", w.Code)
	}
	if info, _ := os.Stat(h.store); info.Mode().Perm() != 0o644 {
		t.Errorf("the guard must not change the store: %v", info.Mode())
	}
	if !strings.Contains(h.log.all(), "readable by others") {
		t.Errorf("the refusal is logged: %q", h.log.all())
	}
	os.Chmod(h.store, 0o600)
	if _, name := h.admit("GET", good, false); name == "" {
		t.Errorf("back at 0600")
	}
	os.Chmod(filepath.Dir(h.store), 0o777)
	defer os.Chmod(filepath.Dir(h.store), 0o700)
	if _, name := h.admit("GET", good, false); name != "" {
		t.Errorf("a store in a directory others can write holds no tokens")
	}
}

// Ten bad tokens from one address lock it out for the window; another
// address still gets in (TestAPIGuessingAndRate, first half).
func TestAdmitLockout(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.mint("example-agent", nil)
	guesser := header("X-Forwarded-For", "100.64.0.66")
	for i := 0; i < FailLimit; i++ {
		if w, _ := h.admit("GET", "fct_guess", false, guesser); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	w, _ := h.admit("GET", good, false, guesser)
	if w.Code != 429 || w.Header().Get("Retry-After") != "900" || errCode(t, w) != "too_many" {
		t.Errorf("the guessing address waits, even with a good token: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	if _, name := h.admit("GET", good, false, header("X-Forwarded-For", "100.64.0.67")); name == "" {
		t.Errorf("another address still gets in")
	}
	h.clock.add(FailWindow + time.Second)
	if _, name := h.admit("GET", good, false, guesser); name == "" {
		t.Errorf("the block ends with the window")
	}
	// a malformed header counts too: it presented something
	for i := 0; i < FailLimit; i++ {
		h.admit("GET", "", false, header("X-Forwarded-For", "100.64.0.68"), header("Authorization", "Basic x"))
	}
	if w, _ := h.admit("GET", good, false, header("X-Forwarded-For", "100.64.0.68")); w.Code != 429 {
		t.Errorf("malformed headers count as bad tokens: %d", w.Code)
	}
	// no header at all is not a guess
	for i := 0; i < FailLimit+1; i++ {
		h.admit("GET", "", false, header("X-Forwarded-For", "100.64.0.69"))
	}
	if _, name := h.admit("GET", good, false, header("X-Forwarded-For", "100.64.0.69")); name == "" {
		t.Errorf("calls without a token never lock out")
	}
}

// An expired token is stale, not a guess; on loopback a bad token locks
// out only itself (TestAPIStaleTokensDontLockOutOthers).
func TestAdmitStaleTokensDontLockOutOthers(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.mint("example-agent", nil)
	old := h.mint("example-agent-2", "2020-01-01T00:00:00+00:00")
	far := header("X-Forwarded-For", "100.64.0.66")
	for i := 0; i <= FailLimit; i++ {
		if w, _ := h.admit("GET", old, false, far); w.Code != 401 {
			t.Fatalf("an expired token, call %d: %d", i, w.Code)
		}
	}
	for i := 0; i < FailLimit; i++ {
		h.admit("GET", "fct_revoked", false)
	}
	if w, _ := h.admit("GET", "fct_revoked", false); w.Code != 429 {
		t.Errorf("the bad token itself waits: %d", w.Code)
	}
	if _, name := h.admit("GET", good, false); name == "" {
		t.Errorf("another local token still gets in")
	}
}

// Bug 4: the bad-token map is bounded. Expired keys go on every failure,
// and past the cap a new key is not tracked.
func TestFailMapIsBounded(t *testing.T) {
	h := newHarness(t, Options{})
	for i := 0; i < 50; i++ {
		h.admit("GET", "fct_guess-"+strconv.Itoa(i), false)
	}
	if n := h.g.failKeys(); n != 50 {
		t.Fatalf("each local bad token is its own key: %d", n)
	}
	h.clock.add(FailWindow + time.Second)
	h.admit("GET", "fct_one-more", false)
	if n := h.g.failKeys(); n != 1 {
		t.Errorf("expired keys should be pruned: %d left", n)
	}
	for i := 0; i < maxFailKeys+100; i++ {
		h.g.failed("k" + strconv.Itoa(i))
	}
	if n := h.g.failKeys(); n > maxFailKeys {
		t.Errorf("the map grew past its cap: %d", n)
	}
	if !h.g.allowed("k" + strconv.Itoa(maxFailKeys+50)) {
		t.Errorf("an untracked key is allowed")
	}
}

// The rate per token: 120 a minute with a burst of 30; writes also 30 a
// minute with a burst of 10; a refused call draws nothing; MCP's late write
// check shares the bucket (TestAPIGuessingAndRate, second half;
// TestMCPWriteRate).
func TestAdmitRate(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.mint("example-agent", nil)
	for i := 0; i < int(DefaultWrites().Burst); i++ {
		if _, name := h.admit("POST", good, true); name == "" {
			t.Fatalf("write %d refused", i)
		}
	}
	w, _ := h.admit("POST", good, true)
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" || errCode(t, w) != "too_many" {
		t.Errorf("past the write burst: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if _, name := h.admit("GET", good, false); name == "" {
		t.Errorf("reads go on while writes wait")
	}
	if wait := h.g.TakeWrite("example-agent"); wait <= 0 {
		t.Errorf("a late write check draws on the same bucket")
	}
	h.clock.add(2 * time.Second)
	if _, name := h.admit("POST", good, true); name == "" {
		t.Errorf("a write after the wait")
	}
	h.clock.add(time.Hour)
	for i := 0; i < int(DefaultAll().Burst); i++ {
		h.admit("GET", good, false)
	}
	if w, _ := h.admit("GET", good, false); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Errorf("past the burst: %d", w.Code)
	}
	// each token has its own buckets
	other := h.mint("other-agent", nil)
	if _, name := h.admit("GET", other, false); name != "other-agent" {
		t.Errorf("another token's calls are its own")
	}

	custom := newHarness(t, Options{Writes: Rate{PerMin: 1, Burst: 2}, All: Rate{PerMin: 6000, Burst: 1000}})
	tok := custom.mint("example-agent", nil)
	custom.admit("POST", tok, true)
	if wait := custom.g.TakeWrite("example-agent"); wait != 0 {
		t.Errorf("the second write: %v", wait)
	}
	if wait := custom.g.TakeWrite("example-agent"); wait <= 0 {
		t.Errorf("the third write waits")
	}
}

// POST and PATCH need a JSON body, and the body is capped at 256 KiB
// (TestAPIBodyAndRoutes, TestMCPAuth).
func TestAdmitBody(t *testing.T) {
	h := newHarness(t, Options{})
	good := h.mint("example-agent", nil)
	for _, k := range []struct {
		method, ct string
		status     int
	}{
		{"POST", "text/plain", 415}, {"PATCH", "", 415}, {"POST", "application/json; charset=utf-8", 200},
		{"POST", "application/json", 200}, {"PUT", "text/plain", 200}, {"DELETE", "", 200},
	} {
		r := httptest.NewRequest(k.method, "/api/v1/tickets", strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("Authorization", "Bearer "+good)
		if k.ct != "" {
			r.Header.Set("Content-Type", k.ct)
		}
		w := httptest.NewRecorder()
		status := 200
		if h.g.Admit(w, r, true) == "" {
			status = w.Code
		}
		if status != k.status {
			t.Errorf("%s %q: %d", k.method, k.ct, status)
		}
		if status == 415 && errCode(t, w) != "bad_request" {
			t.Errorf("415's code: %s", w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/api/v1/tickets", strings.NewReader(strings.Repeat("a", BodyLimit+1)))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Authorization", "Bearer "+good)
	r.Header.Set("Content-Type", "application/json")
	if h.g.Admit(httptest.NewRecorder(), r, true) == "" {
		t.Fatal("refused")
	}
	_, err := io.ReadAll(r.Body)
	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) {
		t.Errorf("the body is capped: %v", err)
	}
}

// No store at all, or no token source, holds no tokens.
func TestAdmitWithoutTokens(t *testing.T) {
	g := New(Options{})
	r := httptest.NewRequest("GET", "/api/v1/me", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Authorization", "Bearer fct_x")
	w := httptest.NewRecorder()
	if g.Admit(w, r, false) != "" || w.Code != 401 {
		t.Errorf("no store: %d", w.Code)
	}
}

// Two calls at a time, one per token, a wait of up to 10 s for a slot, then
// busy (TestAPIConcurrency, TestMCPBusy).
func TestRunSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := New(Options{Concurrency: 2})
		release := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 3)
		for i, name := range []string{"a", "b", "c"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i == 2 {
					time.Sleep(time.Second) // the first two hold both slots
				}
				errs[i] = g.Run(context.Background(), name, func(ctx context.Context) error {
					<-release
					return nil
				})
			}()
		}
		time.Sleep(DefaultSlotWait + 2*time.Second)
		close(release)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || !errors.Is(errs[2], ErrBusy) {
			t.Errorf("two at a time, the third waits and then gives up: %v", errs)
		}

		// a waiting call takes the slot once one is free
		for i, name := range []string{"a", "b", "c"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = g.Run(context.Background(), name, func(ctx context.Context) error {
					time.Sleep(time.Second)
					return nil
				})
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || errs[2] != nil {
			t.Errorf("within the wait, all go through: %v", errs)
		}

		// one token holds one slot: its second call waits for its first
		short := New(Options{Concurrency: 2, SlotWait: 300 * time.Millisecond})
		for i, name := range []string{"a", "a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i > 0 {
					time.Sleep(100 * time.Millisecond)
				}
				errs[i] = short.Run(context.Background(), name, func(ctx context.Context) error {
					time.Sleep(time.Second)
					return nil
				})
			}()
		}
		wg.Wait()
		if errs[0] != nil || !errors.Is(errs[1], ErrBusy) || errs[2] != nil {
			t.Errorf("a token's second call waits for its first, others don't: %v", errs)
		}
	})
}

// A call runs on a context the client cannot cancel, with its own 30 s
// deadline (§5.6); what it returns is passed back.
func TestRunDetaches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := New(Options{})
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		var deadline time.Time
		var live error
		want := errors.New("refused")
		err := g.Run(parent, "a", func(ctx context.Context) error {
			deadline, _ = ctx.Deadline()
			live = ctx.Err()
			return want
		})
		if !errors.Is(err, want) {
			t.Errorf("the call's error: %v", err)
		}
		if live != nil {
			t.Errorf("a client that hung up cut the call short: %v", live)
		}
		if got := time.Until(deadline); got != DefaultTimeout {
			t.Errorf("deadline in %v, want %v", got, DefaultTimeout)
		}
		var hit error
		g2 := New(Options{Timeout: time.Second})
		g2.Run(context.Background(), "a", func(ctx context.Context) error {
			<-ctx.Done()
			hit = ctx.Err()
			return hit
		})
		if !errors.Is(hit, context.DeadlineExceeded) {
			t.Errorf("the timeout: %v", hit)
		}
	})
}

// A panic in a call is recovered: the caller learns nothing of it, the
// journal gets the stack, and the slots are free again (§5.6).
func TestRunRecoversPanics(t *testing.T) {
	l := &logs{}
	g := New(Options{Concurrency: 1, Log: l})
	err := g.Run(context.Background(), "a", func(ctx context.Context) error { panic("boom") })
	if !errors.Is(err, ErrPanic) || strings.Contains(err.Error(), "boom") {
		t.Errorf("panic: %v", err)
	}
	if !strings.Contains(l.all(), "boom") || !strings.Contains(l.all(), "guard_test.go") {
		t.Errorf("the panic and its stack are logged: %q", l.all())
	}
	if err := g.Run(context.Background(), "a", func(ctx context.Context) error { return nil }); err != nil {
		t.Errorf("the slots are free after a panic: %v", err)
	}
}

// Error answers: {"error": {code, message?, field?}}, JSON, not cached.
func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, 404, "not_found", "no ticket T1", "id")
	if w.Code != 404 || w.Body.String() != `{"error":{"code":"not_found","field":"id","message":"no ticket T1"}}`+"\n" ||
		w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%d %q %v", w.Code, w.Body.String(), w.Header())
	}
	w = httptest.NewRecorder()
	WriteError(w, 400, "bad_request", "", "")
	if w.Body.String() != `{"error":{"code":"bad_request"}}`+"\n" {
		t.Errorf("no message, no field: %q", w.Body.String())
	}
	w = httptest.NewRecorder()
	Internal(w)
	if w.Code != 500 || errCode(t, w) != "internal" || !strings.Contains(w.Body.String(), "it is logged") {
		t.Errorf("internal: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	WriteJSON(w, 200, func() {})
	if w.Code != 500 || errCode(t, w) != "internal" {
		t.Errorf("an answer that cannot be written: %d %s", w.Code, w.Body.String())
	}
}
