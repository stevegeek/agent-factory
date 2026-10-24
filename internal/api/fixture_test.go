package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/guard"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

// The archtest import table lets these tests import only api's own row
// (ticketapi, guard, model, jsonx, clock) and testkit. The real service is
// built from store, tokens and config values all the same: their types are
// reached through the fields of ticketapi.Service and guard.Options, and
// filled in by name, as paths.FromEnv and `factory tokens add` fill them.

// apiConfig is Foreman's api_test config (test_tickets.CONFIG's board).
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

// setClock is a clock a test moves.
type setClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *setClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *setClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *logs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// harness is a token store, a guard over it, the API over a service, and
// the clock they share.
type harness struct {
	t     *testing.T
	env   *testkit.Env
	store string
	clock *setClock
	log   *logs
	recs  []map[string]any
	guard *guard.Guard
	h     http.Handler
	// the real service, when the harness has one
	svc *ticketapi.Service
}

type harnessOpt struct {
	open        bool
	all, writes guard.Rate
	slotWait    time.Duration
	timeout     time.Duration
	concurrency int
}

func newHarness(t *testing.T, svc Service, o harnessOpt) *harness {
	t.Helper()
	env := testkit.Root(t)
	h := &harness{t: t, env: env, store: filepath.Join(env.Home, ".config", "factory", "api-tokens.json"),
		clock: &setClock{t: time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)}, log: &logs{}}
	h.writeStore()
	if o.all == (guard.Rate{}) {
		// room for a whole workflow in one test; the rate tests put the
		// real limits back
		o.all, o.writes = guard.Rate{PerMin: 6000, Burst: 1000}, guard.Rate{PerMin: 6000, Burst: 1000}
	}
	opts := guard.Options{Hosts: []string{"example.com"}, Open: o.open, All: o.all, Writes: o.writes,
		SlotWait: o.slotWait, Timeout: o.timeout, Concurrency: o.concurrency, Clock: h.clock, Log: h.log}
	tokens := newValue(reflect.ValueOf(&opts).Elem().FieldByName("Tokens"))
	setFields(tokens.Elem(), map[string]any{"Path": h.store, "Clock": h.clock, "Log": h.log})
	reflect.ValueOf(&opts).Elem().FieldByName("Tokens").Set(tokens)
	h.guard = guard.New(opts)
	if svc == nil {
		h.svc = realService(t, env, apiConfig, h.clock, tokens)
		svc = h.svc
	}
	h.h = New(Options{Guard: h.guard, Service: svc, Log: h.log})
	return h
}

// newValue is a new value for a pointer-typed field.
func newValue(field reflect.Value) reflect.Value { return reflect.New(field.Type().Elem()) }

func setFields(v reflect.Value, fields map[string]any) {
	for name, x := range fields {
		f := v.FieldByName(name)
		if !f.IsValid() {
			panic("no field " + name + " in " + v.Type().String())
		}
		f.Set(reflect.ValueOf(x))
	}
}

// realService is ticketapi.Service on the board under env, its config and
// the token store: what Foreman wires.
func realService(t *testing.T, env *testkit.Env, cfg string, clk *setClock, tokens reflect.Value) *ticketapi.Service {
	t.Helper()
	cfgPath := env.WriteConfig(t, cfg)
	svc := &ticketapi.Service{Clock: clk}
	sv := reflect.ValueOf(svc).Elem()
	board := newValue(sv.FieldByName("Board"))
	paths := board.Elem().FieldByName("Paths")
	root, home := env.Root, env.Home
	b := filepath.Join(root, "board")
	tk := filepath.Join(b, "tickets")
	logsDir := filepath.Join(root, "logs")
	for name, v := range map[string]string{
		"Root": root, "Bin": filepath.Join(root, "bin"), "Board": b, "Tickets": tk, "Locks": filepath.Join(tk, ".locks"),
		"Index": filepath.Join(b, "index.json"), "IDLock": filepath.Join(tk, ".id.lock"), "LastID": filepath.Join(tk, ".last-id"),
		"IndexLock": filepath.Join(b, ".index.lock"), "CreateLock": filepath.Join(b, ".create.lock"),
		"Logs": logsDir, "FactoryLog": filepath.Join(logsDir, "factory.log"), "LogLock": filepath.Join(logsDir, ".log.lock"),
		"Config": cfgPath, "State": filepath.Join(root, "state.json"), "TickLock": filepath.Join(root, ".tick.lock"),
		"TickAgain": filepath.Join(root, ".tick.again"), "Home": home,
		"Tokens": tokens.Elem().FieldByName("Path").String(),
	} {
		paths.FieldByName(name).SetString(v)
	}
	board.Elem().FieldByName("Clock").Set(reflect.ValueOf(clock.Clock(clk)))
	reader := newValue(board.Elem().FieldByName("Reader"))
	reader.Elem().FieldByName("Paths").Set(paths)
	board.Elem().FieldByName("Reader").Set(reader)
	sv.FieldByName("Board").Set(board)
	source := newValue(sv.FieldByName("Config"))
	source.Elem().FieldByName("Path").SetString(cfgPath)
	sv.FieldByName("Config").Set(source)
	sv.FieldByName("Tokens").Set(tokens)
	return svc
}

func (h *harness) writeStore() {
	h.t.Helper()
	recs := h.recs
	if recs == nil {
		recs = []map[string]any{}
	}
	data, _ := json.MarshalIndent(map[string]any{"version": 1, "tokens": recs}, "", "  ")
	if err := os.WriteFile(h.store, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
	// a new mtime even on a coarse clock
	future := time.Now().Add(time.Duration(len(h.recs)+1) * time.Second)
	os.Chtimes(h.store, future, future)
}

// mint is `factory tokens add NAME --can CAN --project P...`: read always,
// a record as the CLI writes it, and the secret.
func (h *harness) mint(name, can string, projects ...string) string {
	h.t.Helper()
	secret := "fct_secret-for-" + name
	sum := sha256.Sum256([]byte(secret))
	caps := []string{"read"}
	for _, c := range []string{"file", "talk", "work", "plan"} {
		if strings.Contains(","+can+",", ","+c+",") {
			caps = append(caps, c)
		}
	}
	h.recs = append(h.recs, map[string]any{"name": name, "sha256": hex.EncodeToString(sum[:]), "projects": projects,
		"can": caps, "added": "2026-10-17T08:00:00+00:00", "expires": nil, "tickets": nil})
	h.writeStore()
	return secret
}

// revoke is `factory tokens revoke NAME`.
func (h *harness) revoke(name string) {
	var keep []map[string]any
	for _, r := range h.recs {
		if r["name"] != name {
			keep = append(keep, r)
		}
	}
	h.recs = keep
	h.writeStore()
}

func (h *harness) expire(name, at string) {
	for _, r := range h.recs {
		if r["name"] == name {
			r["expires"] = at
		}
	}
	h.writeStore()
}

// person runs a verb as the maintainer (`factory ticket VERB --json`) and
// gives its answer.
func (h *harness) person(verb string, req map[string]any) map[string]any {
	h.t.Helper()
	data, _ := json.Marshal(req)
	v, err := jsonx.Parse(data)
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := h.svc.Do(context.Background(), ticketapi.ForPerson(), verb, v, time.Second)
	if err != nil {
		h.t.Fatalf("ticket %s as the maintainer: %v", verb, err)
	}
	var m map[string]any
	if err := json.Unmarshal(ticketapi.Reply(out), &m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

type reqOpt func(*http.Request)

func header(k, v string) reqOpt    { return func(r *http.Request) { r.Header.Add(k, v) } }
func remote(addr string) reqOpt    { return func(r *http.Request) { r.RemoteAddr = addr } }
func contentType(ct string) reqOpt { return func(r *http.Request) { r.Header.Set("Content-Type", ct) } }

func (h *harness) call(method, target, token string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
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

// ---------------------------------------------------------------- the fake service

// fakeCall is one verb call the fake saw: what the CLI's argv and stdin
// were.
type fakeCall struct {
	caller ticketapi.Caller
	verb   string
	in     map[string]any
	raw    string // the request as the verb got it
	wait   time.Duration
}

// fake answers as its mode says and records every call (Foreman's fake
// factory script).
type fake struct {
	mu      sync.Mutex
	mode    string
	calls   []fakeCall
	release chan struct{}
}

func newFake() *fake { return &fake{mode: "ok"} }

func (f *fake) setMode(m string) {
	f.mu.Lock()
	f.mode = m
	f.mu.Unlock()
}

func (f *fake) taken() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func obj(pairs ...any) jsonx.Object {
	var o jsonx.Object
	for i := 0; i < len(pairs); i += 2 {
		o = append(o, jsonx.Member{Key: pairs[i].(string), Value: pairs[i+1].(jsonx.Value)})
	}
	return o
}

func (f *fake) Do(ctx context.Context, c ticketapi.Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error) {
	raw := string(jsonx.Format{Indent: -1, Compact: true}.Encode(req))
	var in map[string]any
	json.Unmarshal([]byte(raw), &in)
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{caller: c, verb: verb, in: in, raw: raw, wait: wait})
	mode, release := f.mode, f.release
	f.mu.Unlock()
	ticket := obj("id", jsonx.String("T0007")).Value()
	switch {
	case mode == "ok":
		return obj("created", jsonx.Bool(true), "ticket", ticket), nil
	case mode == "replay":
		return obj("created", jsonx.Bool(false), "ticket", ticket), nil
	case strings.HasPrefix(mode, "refuse:"):
		return nil, &ticketapi.Refused{Code: strings.TrimPrefix(mode, "refuse:"), Message: "no", Field: "id"}
	case mode == "crash":
		return nil, errors.New("Traceback (most recent call last): boom")
	case mode == "panic":
		panic("Traceback: boom")
	case mode == "hang":
		<-ctx.Done()
		return nil, ctx.Err()
	case mode == "block":
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return obj("name", jsonx.String("x")), nil
	case mode == "slow":
		time.Sleep(time.Second)
		return obj("name", jsonx.String("x")), nil
	}
	return nil, errors.New("unknown mode " + mode)
}

// newFakeHarness is the API over the fake, with three tokens on sandbox.
func newFakeHarness(t *testing.T, o harnessOpt) (*harness, *fake, []string) {
	f := newFake()
	h := newHarness(t, f, o)
	tokens := []string{h.mint("example-agent", "read", "sandbox"), h.mint("example-agent-2", "read", "sandbox"),
		h.mint("example-agent-3", "read", "sandbox")}
	return h, f, tokens
}
