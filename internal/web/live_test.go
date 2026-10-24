package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Foreman's TestLiveShowsRunningAgentsAndTheirScreens.
func TestLiveShowsRunningAgentsAndTheirScreens(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/live", nil, c).Body.String()
	for _, want := range []string{`id="T0011"`, `data-poll="/live/T0011/screen"`, "Speed up widgets", `>Live <span class="n">1</span>`, `href="/logs"`} {
		if !strings.Contains(body, want) {
			t.Errorf("live page lacks %q", want)
		}
	}
	if strings.Contains(body, `data-poll="/live/log"`) {
		t.Errorf("the factory log is still on Live")
	}
	if !strings.Contains(h.do("GET", "/logs", nil, c).Body.String(), `data-poll="/live/log"`) {
		t.Errorf("Logs lacks the factory log")
	}
	w := h.do("GET", "/live/T0011/screen", nil, c)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || w.Body.String() != "the agent's screen" {
		t.Fatalf("screen: %d %s %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if got := h.ops.takeLooks(); len(got) != 1 || got[0] != "T0011" {
		t.Errorf("looks: %v", got)
	}
	// cached for a few seconds: a second page costs nothing
	h.do("GET", "/live/T0011/screen", nil, c)
	if got := h.ops.takeLooks(); len(got) != 0 {
		t.Errorf("a second look within seconds looked again: %v", got)
	}
	// a ticket without an agent never looks
	if w := h.do("GET", "/live/T0001/screen", nil, c); !strings.Contains(w.Body.String(), "finished") {
		t.Errorf("no agent: %q", w.Body.String())
	}
	if got := h.ops.takeLooks(); len(got) != 0 {
		t.Errorf("look ran for a ticket with no agent: %v", got)
	}
	if w := h.do("GET", "/live/T01/screen", nil, c); w.Code != http.StatusNotFound {
		t.Errorf("bad id: %d", w.Code)
	}
	if w := h.do("GET", "/live/log", nil); w.Code != 303 {
		t.Errorf("the log needs a login: %d", w.Code)
	}
	// the ticket page links the live screen of its running agent
	if body := h.do("GET", "/t/T0011", nil, c).Body.String(); !strings.Contains(body, `<a href="/live#T0011">Watch the t0011-w1 agent live</a>`) {
		t.Errorf("the running ticket does not link its screen")
	}
}

// In flight at the top of Live: the maintainer sees every project's
// queue, agents (linked to their screens on the page) and moves, his own
// marked; another person sees only her projects, her own moves marked,
// and no screens or log. It rides the page's poll: a move changes the
// version, so the next poll brings it.
func TestLiveInFlight(t *testing.T) {
	h := newHarness(t, false)
	secret, err := h.users.Add("robin", []string{"factory"})
	if err != nil {
		t.Fatal(err)
	}
	m := h.login()
	old := version(h.fixtures())
	before := h.do("GET", "/live", nil, m).Body.String()
	if !strings.Contains(before, `id="in-flight"`) || !strings.Contains(before, "Nothing waits for an agent or the gate.") ||
		!strings.Contains(before, `href="#T0011"`) {
		t.Fatalf("in flight before: no section, a waiting ticket, or no link to the running agent's screen")
	}
	h.change("T0002", func(tk *ticket.Ticket) {
		ticket.Move(tk, "ready", model.ReasonByHuman, "go", "", "maintainer", testNow.Add(-5*time.Minute))
	})
	h.change("T0008", func(tk *ticket.Ticket) {
		ticket.Move(tk, "ready", model.ReasonByHuman, "", "", "robin", testNow.Add(-2*time.Minute))
	})
	if w := h.doWith("GET", "/live", nil, map[string]string{liveHeader: old}, m); w.Code != http.StatusOK {
		t.Errorf("a poll after the moves: %d", w.Code)
	}
	body := h.do("GET", "/live", nil, m).Body.String()
	if !(strings.Index(body, "<h5>Running") < strings.Index(body, "<h5>Waiting to start") && strings.Index(body, "<h5>Waiting to start") < strings.Index(body, "<h5>Just moved")) {
		t.Errorf("in flight not in the order running, waiting, just moved")
	}
	waiting := body[strings.Index(body, "Waiting to start"):strings.Index(body, "<h5>Just moved")]
	for _, want := range []string{`href="/t/T0002"`, "waits for Worker", "waiting 5 min", `href="/t/T0008"`, "waits for Task agent"} {
		if !strings.Contains(waiting, want) {
			t.Errorf("the maintainer's waiting lacks %q", want)
		}
	}
	moved := body[strings.Index(body, "Just moved"):]
	if !strings.Contains(moved, `class="fl-row mine"`) || !strings.Contains(moved, "<b>you</b>: go") || !strings.Contains(moved, "robin") {
		t.Errorf("the maintainer's moves: his own not marked, or robin's missing")
	}
	if strings.Index(moved, "T0008") > strings.Index(moved, "T0002") {
		t.Errorf("moves not newest first")
	}

	r := h.loginWith(secret, "/")
	body = h.do("GET", "/live", nil, r).Body.String()
	if !strings.Contains(body, `id="in-flight"`) || strings.Contains(body, "T0002") || strings.Contains(body, "T0011") || strings.Contains(body, "data-poll") {
		t.Errorf("robin's Live shows another project or the screens")
	}
	if !strings.Contains(body, `href="/t/T0008"`) || !strings.Contains(body, `class="fl-row mine"`) {
		t.Errorf("robin's Live lacks her ticket or her move")
	}
}

// A poll of a local agent's screen marks that Live is open, by the mtime
// of foreman-state/watching (the relay's only input), at most once a
// second; a ticket with no agent marks nothing.
func TestLiveScreenPollsTouchTheWatchMarker(t *testing.T) {
	h := newHarness(t, false)
	clk := testkit.NewStep(testNow, 0)
	h.srv.clock = clk
	c := h.login()
	marker := h.paths.Watching()
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	touched := func() bool {
		st, err := os.Lstat(marker)
		return err == nil && st.Mode().IsRegular() && time.Since(st.ModTime()) < time.Minute
	}
	age := func() {
		if err := os.Chtimes(marker, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if h.do("GET", "/live/T0011/screen", nil, c); !touched() {
		t.Fatalf("a screen poll did not touch the marker")
	}
	age()
	if h.do("GET", "/live/T0011/screen", nil, c); touched() {
		t.Errorf("touched twice within a second")
	}
	clk.Advance(time.Second)
	if h.do("GET", "/live/T0011/screen", nil, c); !touched() {
		t.Errorf("not touched a second later")
	}
	age()
	clk.Advance(time.Second)
	if h.do("GET", "/live/T0001/screen", nil, c); touched() {
		t.Errorf("a ticket with no agent touched the marker")
	}
	// a link in its place is never written through
	os.Remove(marker)
	target := filepath.Join(h.env.Root, "elsewhere")
	os.WriteFile(target, []byte("keep"), 0o600)
	os.Symlink(target, marker)
	clk.Advance(time.Second)
	h.do("GET", "/live/T0011/screen", nil, c)
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("wrote through a link: %q", b)
	}
}

// Bug 3: concurrent misses for one screen run one look; the cache expires;
// it never holds more than 64 screens.
func TestScreensCollapseConcurrentLooks(t *testing.T) {
	clk := testkit.NewStep(testNow, 0)
	c := newScreens(clk)
	var mu sync.Mutex
	runs := 0
	release := make(chan struct{})
	fill := func() string {
		mu.Lock()
		runs++
		mu.Unlock()
		<-release
		return "screen"
	}
	var wg sync.WaitGroup
	got := make([]string, 5)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = c.get("T0011", fill)
		}(i)
	}
	// let the goroutines queue behind the first look, then finish it
	for {
		c.mu.Lock()
		_, busy := c.inflight["T0011"]
		c.mu.Unlock()
		if busy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs != 1 || strings.Join(got, ",") != "screen,screen,screen,screen,screen" {
		t.Errorf("runs %d, got %v", runs, got)
	}
	clk.Advance(screenTTL)
	c.get("T0011", func() string { runs++; return "new" })
	if runs != 2 {
		t.Errorf("an expired screen was not looked at again")
	}
	for i := 0; i < 100; i++ {
		c.get(strings.Repeat("x", i+1), func() string { return "" })
	}
	if len(c.vals) > screenEntries {
		t.Errorf("the cache holds %d screens", len(c.vals))
	}
}

// The Live log is factory.log and Foreman's foreman-state/foreman.log,
// merged by time; a message's own newline stays with it, and a link is
// never read.
func TestTheLiveLogMergesFactoryAndForemanLogs(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	h.env.WriteFile(t, "logs/factory.log", "2026-10-18T09:00:00+00:00 tick start\n2026-10-18T09:00:02+00:00 T0001 gate pass\nsecond line\n2026-10-18T09:00:05+00:00 tick end\n")
	h.env.WriteFile(t, "foreman-state/foreman.log", "2026-10-18T09:00:01+00:00 T0002 answered: start\n2026-10-18T09:00:05+00:00 api: filed T0003\n")
	want := "2026-10-18T09:00:00+00:00 tick start\n2026-10-18T09:00:01+00:00 T0002 answered: start\n" +
		"2026-10-18T09:00:02+00:00 T0001 gate pass\nsecond line\n2026-10-18T09:00:05+00:00 tick end\n2026-10-18T09:00:05+00:00 api: filed T0003"
	if got := h.do("GET", "/live/log", nil, c).Body.String(); got != want {
		t.Errorf("live log\n%s\nwant\n%s", got, want)
	}
	os.Remove(h.env.Path("foreman-state", "foreman.log"))
	os.Symlink(h.env.Path("config.toml"), h.env.Path("foreman-state", "foreman.log"))
	if got := h.do("GET", "/live/log", nil, c).Body.String(); !strings.HasSuffix(got, "tick end") {
		t.Errorf("a linked foreman.log: %q", got)
	}
	if got := mergeLogs(2, "2026-10-18T09:00:00+00:00 a\n2026-10-18T09:00:02+00:00 c", "2026-10-18T09:00:01+00:00 b"); got != "2026-10-18T09:00:01+00:00 b\n2026-10-18T09:00:02+00:00 c" {
		t.Errorf("last two %q", got)
	}
}

// Foreman's TestAgentLogsOnlyFromTheTicketsOwnRuns.
func TestAgentLogsOnlyFromTheTicketsOwnRuns(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	h.env.WriteFile(t, "logs/T0001/t0001-w1/agent.log", "line one\nline two\n")
	h.env.WriteFile(t, "logs/factory.log", "2026 start T0001\n2026 gate pass\n")
	if w := h.do("GET", "/t/T0001/logs/t0001-w1", nil, c); w.Code != 200 || w.Body.String() != "line one\nline two" {
		t.Errorf("log: %d %q", w.Code, w.Body.String())
	}
	os.Symlink(h.env.Path("logs", "factory.log"), h.env.Path("logs", "T0001", "t0001-w1", "x"))
	os.MkdirAll(h.env.Path("logs", "T0001", "t0001-w2"), 0o755)
	os.Symlink(h.env.Path("logs", "factory.log"), h.env.Path("logs", "T0001", "t0001-w2", "agent.log"))
	for _, bad := range []string{"/t/T0001/logs/t0002-w1", "/t/T0001/logs/..", "/t/T0001/logs/t0001-w1%2F..%2F..", "/t/T0001/logs/t0001-w9",
		"/t/T0001/logs/t0001-w2", "/t/T01/logs/t01-w1"} {
		if w := h.do("GET", bad, nil, c); w.Code == 200 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := h.do("GET", "/live/log", nil, c); !strings.HasSuffix(w.Body.String(), "gate pass") {
		t.Errorf("factory log: %q", w.Body.String())
	}
	if body := h.do("GET", "/t/T0001", nil, c).Body.String(); !strings.Contains(body, `href="/t/T0001/logs/t0001-w1"`) {
		t.Errorf("the ticket page should link its agent logs")
	}
}

// Foreman's TestTailFileKeepsWholeLines.
func TestTailFileKeepsWholeLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	os.WriteFile(p, []byte("aaaa\nbbbb\ncccc\n"), 0o644)
	if got := tailFile(p, 8, 0); got != "cccc" {
		t.Errorf("tail by bytes: %q", got)
	}
	if got := tailFile(p, 100, 2); got != "bbbb\ncccc" {
		t.Errorf("tail by lines: %q", got)
	}
	os.WriteFile(p, []byte("ok \xff bad\n"), 0o644)
	if got := tailFile(p, 100, 0); got != "ok � bad" {
		t.Errorf("invalid UTF-8: %q", got)
	}
	os.Symlink(p, filepath.Join(dir, "link"))
	if got := tailFile(filepath.Join(dir, "link"), 100, 0); got != "" {
		t.Errorf("a link was followed: %q", got)
	}
	if got := tailFile(dir, 100, 0); got != "" {
		t.Errorf("a directory read: %q", got)
	}
}

// Foreman's TestAppFilesArePublicAndSettingsRender.
func TestAppFilesArePublicAndSettingsRender(t *testing.T) {
	h := newHarness(t, false)
	for _, p := range []string{"/manifest.webmanifest", "/sw.js", "/static/icons/apple-touch-icon.png", "/static/htmx.min.js"} {
		if w := h.do("GET", p, nil); w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	var m map[string]any
	if json.Unmarshal(h.do("GET", "/manifest.webmanifest", nil).Body.Bytes(), &m) != nil || m["display"] != "standalone" {
		t.Errorf("manifest %v", m)
	}
	if w := h.do("GET", "/sw.js", nil); w.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("sw.js headers %v", w.Header())
	}
	c := h.login()
	page := h.do("GET", "/settings", nil, c).Body.String()
	if !strings.Contains(page, "Add to Home Screen") || !strings.Contains(page, `rel="manifest"`) || !strings.Contains(page, "Notifications are off on this server.") {
		t.Errorf("settings page")
	}
	if w := h.do("POST", "/push/subscribe", url.Values{"subscription": {"{}"}}, c); w.Code != http.StatusForbidden {
		t.Errorf("subscribe without the CSRF token: %d", w.Code)
	}
	if w := h.do("POST", "/push/subscribe", url.Values{"csrf": {h.csrf(c)}, "subscription": {"{}"}}, c); w.Code != http.StatusNotFound {
		t.Errorf("subscribe with push off: %d", w.Code)
	}
	if w := h.do("POST", "/push/test", url.Values{"csrf": {h.csrf(c)}}, c); w.Code != http.StatusNotFound {
		t.Errorf("test with push off: %d", w.Code)
	}
	if w := h.do("POST", "/push/unsubscribe", url.Values{"csrf": {h.csrf(c)}}, c); w.Code != 200 || !strings.Contains(w.Body.String(), `"devices":0`) {
		t.Errorf("unsubscribe with push off: %d %s", w.Code, w.Body.String())
	}
}

// /healthz says when Foreman can open a file it must not (§6.2).
func TestHealthzReportsAnExposedSecret(t *testing.T) {
	h := newHarness(t, false)
	if w := h.do("GET", "/healthz", nil); w.Body.String() != healthzHead {
		t.Errorf("nothing exposed: %q", w.Body.String())
	}
	h.env.WriteFile(t, "../home/.config/factory/github-app.pem", "key")
	if w := h.do("GET", "/healthz", nil); w.Code != 200 || w.Body.String() != healthzHead+"exposed: github-app.pem\n" {
		t.Errorf("exposed: %d %q", w.Code, w.Body.String())
	}
}

// /healthz says when Foreman can connect to the user manager's bus, through
// which it could run anything outside its sandbox (§6.2).
func TestHealthzReportsAReachableBus(t *testing.T) {
	h := newHarness(t, false)
	dir, err := os.MkdirTemp("", "xdg") // short: unix socket names are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	bus := filepath.Join(dir, "bus")
	h.srv.bus = []string{bus, filepath.Join(dir, "systemd", "private")}
	if w := h.do("GET", "/healthz", nil); w.Body.String() != healthzHead {
		t.Errorf("no bus: %q", w.Body.String())
	}
	ln, err := net.Listen("unix", bus)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if w := h.do("GET", "/healthz", nil); w.Body.String() != healthzHead+"exposed: "+bus+"\n" {
		t.Errorf("bus reachable: %q", w.Body.String())
	}
}

// Every credential §6.2 seals, and the user manager's sockets, are what
// Foreman's /healthz checks.
func TestForemanChecksEverySealedPath(t *testing.T) {
	c, err := parseFlags([]string{"--root", "/r"}, map[string]string{"HOME": "/h", "XDG_RUNTIME_DIR": "/run/user/7"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/h/.config/factory/github-app.pem", "/h/.config/factory/claude-oauth-token",
		"/h/.config/factory/anthropic-api-key", "/h/.config/factory/backup-key", "/h/.config/factory/r2-credentials"}
	if !slices.Equal(c.paths.Sealed(), want) {
		t.Errorf("sealed %q", c.paths.Sealed())
	}
	if got := c.busSockets(); !slices.Equal(got, []string{"/run/user/7/bus", "/run/user/7/systemd/private"}) {
		t.Errorf("bus %q", got)
	}
}

// The board read once per request (bug 2).
func TestTheBoardIsReadOncePerRequest(t *testing.T) {
	h := newHarness(t, false)
	counting := &countingBoard{Board: h.srv.board}
	h.srv.board = counting
	hh := h.srv.Handler()
	c := h.login()
	for _, p := range []string{"/", "/t/T0001", "/floor", "/boards", "/live"} {
		counting.n = 0
		w := httptestDo(hh, "GET", p, c)
		if w != 200 || counting.n != 1 {
			t.Errorf("%s: %d, %d loads", p, w, counting.n)
		}
	}
}

type countingBoard struct {
	Board
	n int
}

func (b *countingBoard) Load() (*boardread.View, error) { b.n++; return b.Board.Load() }

func httptestDo(h http.Handler, method, target string, c *http.Cookie) int {
	r := httptest.NewRequest(method, target, nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// a ticket whose agent field is garbage still renders (lenient decode)
func TestAgentTypedFieldKeepsThePage(t *testing.T) {
	h := newHarness(t, false)
	data, _ := os.ReadFile(h.paths.TicketFile("T0001"))
	s := strings.Replace(string(data), `"runtime_behaviour": "yes"`, `"runtime_behaviour": false`, -1)
	s = strings.Replace(s, `"title": "Fix the widget finder"`, `"title": 7`, 1)
	os.WriteFile(h.paths.TicketFile("T0001"), []byte(s), 0o644)
	c := h.login()
	if w := h.do("GET", "/t/T0001", nil, c); w.Code != 200 {
		t.Errorf("a ticket with a wrong-typed field: %d", w.Code)
	}
}
