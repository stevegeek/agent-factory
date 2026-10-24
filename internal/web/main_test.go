package web

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/push"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

func subscription(t *testing.T, endpoint string) string {
	t.Helper()
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	secret := make([]byte, 16)
	rand.Read(secret)
	var s push.Subscription
	s.Endpoint = endpoint
	s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	s.Keys.Auth = base64.RawURLEncoding.EncodeToString(secret)
	b, _ := json.Marshal(s)
	return string(b)
}

// Bug 7: each person's settings count their own devices, a subscription
// carries who made it, and nobody can unsubscribe another person's device.
func TestPushDevicesArePerPerson(t *testing.T) {
	h := newHarness(t, false)
	p, err := push.New(filepath.Join(h.env.Home, ".config", "factory"), "https://example.com", push.Options{Audience: h.users, Clock: testkit.Fixed{T: testNow}})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.push = p
	m := h.login()
	secret, _ := h.users.Add("robin", []string{"factory"})
	s := h.loginWith(secret, "/")
	sub := func(c *http.Cookie, endpoint string) string {
		w := h.do("POST", "/push/subscribe", url.Values{"csrf": {h.csrf(c)}, "subscription": {subscription(t, endpoint)}}, c)
		if w.Code != 200 {
			t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	sub(m, "https://web.push.apple.com/a")
	sub(m, "https://web.push.apple.com/b")
	if got := sub(s, "https://web.push.apple.com/c"); !strings.Contains(got, `"devices":1`) {
		t.Errorf("robin's count after subscribing: %s", got)
	}
	if page := h.do("GET", "/settings", nil, m).Body.String(); !strings.Contains(page, "2 devices receive notifications.") || !strings.Contains(page, p.PublicKey()) {
		t.Errorf("the maintainer's settings")
	}
	if page := h.do("GET", "/settings", nil, s).Body.String(); !strings.Contains(page, "1 device receive notifications.") {
		t.Errorf("robin's settings count everyone's devices")
	}
	w := h.do("POST", "/push/unsubscribe", url.Values{"csrf": {h.csrf(s)}, "endpoint": {"https://web.push.apple.com/a"}}, s)
	if w.Code != 200 || p.Count() != 3 {
		t.Errorf("robin unsubscribed the maintainer's device: %d devices", p.Count())
	}
	h.do("POST", "/push/unsubscribe", url.Values{"csrf": {h.csrf(s)}, "endpoint": {"https://web.push.apple.com/c"}}, s)
	if p.CountFor("robin") != 0 || p.Count() != 2 {
		t.Errorf("robin's own device stayed")
	}
	if w := h.do("POST", "/push/subscribe", url.Values{"csrf": {h.csrf(m)}, "subscription": {subscription(t, "https://169.254.169.254/x")}}, m); w.Code != http.StatusBadRequest {
		t.Errorf("a non-push endpoint: %d", w.Code)
	}
	if w := h.do("POST", "/push/subscribe", url.Values{"csrf": {h.csrf(m)}, "subscription": {"not json"}}, m); w.Code != http.StatusBadRequest {
		t.Errorf("bad json: %d", w.Code)
	}
}

func TestParseFlags(t *testing.T) {
	env := map[string]string{"HOME": "/h", "FACTORY_ROOT": "/r"}
	var errs bytes.Buffer
	c, err := parseFlags(nil, env, &errs)
	if err != nil {
		t.Fatal(err)
	}
	if c.paths.Root != "/r" || c.paths.Tickets != "/r/board/tickets" || c.listen != "127.0.0.1:8790" || c.secretFile != "/r/foreman-state/foreman-secret" ||
		c.apiTokens != "/h/.config/factory/api-tokens.json" || !c.push || !c.api || c.apiConcurrency != 2 || c.open() {
		t.Errorf("defaults %+v", c)
	}
	// the flags deploy renders from config, and the unit's own
	c, err = parseFlags([]string{"--root", "/x", "--listen", "0.0.0.0:9", "--project", "a", "--project", "b", "--host", "desk.example",
		"--trust-tailscale", "--tailscale-user", "me@example.com", "--factory", "/x/bin/factory", "--api=false", "--push=false"}, env, &errs)
	if err != nil {
		t.Fatal(err)
	}
	if c.paths.Root != "/x" || strings.Join(c.projects, ",") != "a,b" || c.hosts[0] != "desk.example" || !c.trustTS || c.tsUsersIfTrusted()[0] != "me@example.com" ||
		c.api || c.push || !c.open() {
		t.Errorf("flags %+v", c)
	}
	for _, bad := range [][]string{{"--trust-tailscale"}, {"--api-concurrency", "0"}, {"boards"}, {"tokens", "add"}, {"--nope"}} {
		if _, err := parseFlags(bad, env, &errs); err == nil {
			t.Errorf("%v passed", bad)
		}
	}
}

// Foreman's state lives in <root>/foreman-state. A host whose files
// deploy has not moved yet keeps its old secret (and its users and sessions
// beside it); once the moved secret exists it wins.
func TestTheSecretDefaultFallsBackToTheOldPath(t *testing.T) {
	e := testkit.Root(t)
	env := map[string]string{"HOME": e.Home, "FACTORY_ROOT": e.Root}
	moved := filepath.Join(e.Root, "foreman-state", "foreman-secret")
	old := filepath.Join(e.Home, ".config", "factory", "foreman-secret")
	secretOf := func() string {
		c, err := parseFlags(nil, env, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if c.apiTokens != filepath.Join(e.Home, ".config", "factory", "api-tokens.json") {
			t.Errorf("the token store moved with the secret: %s", c.apiTokens)
		}
		return c.secretFile
	}
	if got := secretOf(); got != moved {
		t.Errorf("fresh host: %s", got)
	}
	for _, f := range []string{old, moved} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(old, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := secretOf(); got != old {
		t.Errorf("unmoved host: %s", got)
	}
	var out bytes.Buffer
	if code := Main([]string{"users", "add", "robin", "--project", "p"}, env, &out, &bytes.Buffer{}, nil); code != 0 {
		t.Fatalf("users add: %d", code)
	}
	if _, err := os.Stat(auth.UsersFile(old)); err != nil {
		t.Errorf("foreman users wrote elsewhere than the old store: %v", err)
	}
	if err := os.WriteFile(moved, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := secretOf(); got != moved {
		t.Errorf("moved host: %s", got)
	}
}

func TestMainCommands(t *testing.T) {
	e := testkit.Root(t)
	env := map[string]string{"HOME": e.Home, "FACTORY_ROOT": e.Root}
	secret := filepath.Join(e.Root, "foreman-state", "foreman-secret")
	var out, errs bytes.Buffer
	if code := Main([]string{"--revoke-sessions"}, env, &out, &errs, nil); code != 0 || !strings.Contains(out.String(), "every session is revoked") {
		t.Errorf("revoke: %d %q %q", code, out.String(), errs.String())
	}
	if _, err := os.Stat(auth.SinceFile(secret)); err != nil {
		t.Errorf("no revocation file: %v", err)
	}
	out.Reset()
	if code := Main([]string{"users", "add", "robin", "--project", "p"}, env, &out, &errs, nil); code != 0 || !strings.Contains(out.String(), "robin can log in") {
		t.Errorf("users add: %d %q", code, out.String())
	}
	env["FOREMAN_SECRET_FILE"] = filepath.Join(e.Home, "other", "secret")
	out.Reset()
	if code := Main([]string{"users", "list"}, env, &out, &errs, nil); code != 0 || out.Len() != 0 {
		t.Errorf("FOREMAN_SECRET_FILE names another store: %q", out.String())
	}
	if code := Main([]string{"users", "frob"}, env, &out, &errs, nil); code != 1 {
		t.Errorf("a bad users command: %d", code)
	}
	if code := Main([]string{"--trust-tailscale"}, env, &out, &errs, nil); code != 2 {
		t.Errorf("trust without a user: %d", code)
	}
	if code := Main([]string{"-h"}, env, &out, &errs, nil); code != 0 {
		t.Errorf("help: %d", code)
	}
}

type recordLog struct{ lines []string }

func (l *recordLog) Printf(format string, args ...any) {
	l.lines = append(l.lines, format)
}

// build wires the server from the flags: the wire function sees the paths
// and the API settings, a failing wire stops the start, --api=false drops
// the token APIs, and a new secret and VAPID key are made beside each other.
func TestBuildWiresTheServer(t *testing.T) {
	e := testkit.Root(t)
	env := map[string]string{"HOME": e.Home, "FACTORY_ROOT": e.Root}
	c, err := parseFlags([]string{"--host", "desk.example", "--api-concurrency", "3"}, env, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	var seen Wiring
	ops := &fakeOps{}
	wire := func(ctx context.Context, w Wiring) (Services, error) {
		seen = w
		return Services{Answers: ops, API: http.NotFoundHandler()}, nil
	}
	log := &recordLog{}
	a, err := build(context.Background(), c, wire, log, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Paths.Root != e.Root || seen.APIConcurrency != 3 || seen.Hosts[0] != "desk.example" || seen.Open || !seen.API ||
		seen.Tokens != filepath.Join(e.Home, ".config", "factory", "api-tokens.json") {
		t.Errorf("wiring %+v", seen)
	}
	if a.pusher == nil || a.srv.svc.API == nil || a.srv.svc.Answers == nil {
		t.Errorf("app %+v", a)
	}
	for _, f := range []string{"foreman-secret", "foreman-vapid.pem"} {
		if _, err := os.Stat(filepath.Join(e.Root, "foreman-state", f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if w := httptestDoHost(a.srv.Handler(), "/healthz", "desk.example"); w != 200 {
		t.Errorf("the --host name is served: %d", w)
	}
	c.api = false
	a, err = build(context.Background(), c, wire, log, clock.System{})
	if err != nil || a.srv.svc.API != nil {
		t.Errorf("--api=false kept the API: %v", err)
	}
	if _, err := build(context.Background(), c, func(context.Context, Wiring) (Services, error) {
		return Services{}, errors.New("no ops")
	}, log, clock.System{}); err == nil || !strings.Contains(err.Error(), "no ops") {
		t.Errorf("a failing wire: %v", err)
	}
	// with no wiring the desk is read-only but up
	c.push = false
	a, err = build(context.Background(), c, nil, log, clock.System{})
	if err != nil || a.pusher != nil {
		t.Fatalf("no wire: %v", err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookie, Value: a.srv.auth.NewSession(auth.Maintainer)}
	r, _ := http.NewRequest("GET", "/", nil)
	r.Host = "127.0.0.1:8790"
	r.AddCookie(cookie)
	w := &statusWriter{h: http.Header{}}
	a.srv.Handler().ServeHTTP(w, r)
	if code := w.code; code != 200 {
		t.Errorf("read-only desk: %d", code)
	}
}

func httptestDoHost(h http.Handler, target, host string) int {
	r, _ := http.NewRequest("GET", target, nil)
	r.Host = host
	r.RemoteAddr = "127.0.0.1:1"
	w := &statusWriter{h: http.Header{}}
	h.ServeHTTP(w, r)
	return w.code
}

type statusWriter struct {
	h    http.Header
	code int
}

func (w *statusWriter) Header() http.Header { return w.h }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	return len(b), nil
}
func (w *statusWriter) WriteHeader(c int) { w.code = c }

// Every page of every ticket on a private snapshot of the live board
// renders (FACTORY_LIVE_SNAPSHOT; skipped when unset).
func TestLiveBoardRenders(t *testing.T) {
	snap := testkit.LiveSnapshot(t)
	h := newHarness(t, false, filepath.Join(snap, "board", "tickets"))
	c := h.login()
	ts := h.fixtures()
	if len(ts) == 0 {
		t.Skip("the snapshot has no tickets")
	}
	for _, p := range []string{"/", "/floor", "/boards", "/live"} {
		if w := h.do("GET", p, nil, c); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	projects := map[string]bool{}
	for _, tk := range ts {
		projects[tk.Project] = true
		for _, p := range []string{"/t/" + string(tk.ID), "/t/" + string(tk.ID) + "/talk"} {
			if w := h.do("GET", p, nil, c); w.Code != 200 || strings.Contains(w.Body.String(), "javascript:") {
				t.Errorf("%s: %d", p, w.Code)
			}
		}
	}
	for p := range projects {
		if w := h.do("GET", "/b/"+url.PathEscape(p), nil, c); w.Code != 200 && w.Code != 404 {
			t.Errorf("/b/%s: %d", p, w.Code)
		}
	}
}

// healthzHead is /healthz's first lines under the harness: ok, the build,
// the server's start at the harness's clock, and no tick seen yet.
const healthzHead = "ok\nversion dev\nstarted 2026-10-09T12:00:00Z\ntick-age unknown\n"

// /healthz names the build that answers and when it started, so a deploy
// can tell the new Foreman from the old one still running until its
// restart, even when both are of the same commit.
func TestHealthzNamesTheVersion(t *testing.T) {
	h := newHarness(t, false)
	w := h.do("GET", "/healthz", nil)
	lines := strings.Split(w.Body.String(), "\n")
	if w.Code != 200 || len(lines) < 3 || lines[0] != "ok" || lines[1] != "version dev" || lines[2] != "started "+testNow.Format(time.RFC3339) {
		t.Errorf("healthz: %d %q", w.Code, w.Body.String())
	}
}

// /healthz says how old the tick's last sysinfo.json is, so a check from
// outside can tell a stuck tick from a live Foreman.
func TestHealthzGivesTheTickAge(t *testing.T) {
	h := newHarness(t, false)
	h.writeSnapshot(sysinfo.Snapshot{Taken: testNow.Add(-312 * time.Second)})
	w := h.do("GET", "/healthz", nil)
	lines := strings.Split(w.Body.String(), "\n")
	if w.Code != 200 || len(lines) < 4 || lines[3] != "tick-age 312" {
		t.Errorf("healthz: %d %q", w.Code, w.Body.String())
	}
}

// A Taken ahead of the server's clock (a skewed host) reads as age 0, never
// a negative age a check could misread.
func TestHealthzTickAgeFromTheFutureIsZero(t *testing.T) {
	h := newHarness(t, false)
	h.writeSnapshot(sysinfo.Snapshot{Taken: testNow.Add(90 * time.Second)})
	w := h.do("GET", "/healthz", nil)
	lines := strings.Split(w.Body.String(), "\n")
	if w.Code != 200 || len(lines) < 4 || lines[3] != "tick-age 0" {
		t.Errorf("healthz: %d %q", w.Code, w.Body.String())
	}
}

// With no sysinfo.json, or one that does not read, the age is unknown.
func TestHealthzTickAgeUnknownWithoutSysinfo(t *testing.T) {
	h := newHarness(t, false)
	if w := h.do("GET", "/healthz", nil); w.Code != 200 || w.Body.String() != healthzHead {
		t.Errorf("no file: %d %q", w.Code, w.Body.String())
	}
	if err := os.WriteFile(sysinfo.File(h.paths), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := h.do("GET", "/healthz", nil); w.Code != 200 || w.Body.String() != healthzHead {
		t.Errorf("bad file: %d %q", w.Code, w.Body.String())
	}
}
