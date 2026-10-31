package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// fakePinner records the pins posted and refuses a host it does not know,
// as the verb does.
type fakePinner struct {
	mu  sync.Mutex
	got []string // "id host"
}

func (f *fakePinner) Host(_ context.Context, id, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch host {
	case "auto", "local", "helper", "any":
		f.got = append(f.got, id+" "+host)
		return nil
	}
	return errors.New("unknown host " + host + "; known: auto, local, helper")
}

func (f *fakePinner) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func pinHarness(t *testing.T) (*harness, *fakePinner) {
	t.Helper()
	h := newHarness(t, false)
	cfg, err := config.Parse([]byte(testConfig + `
[hosts.helper]
ssh = "factory@helper.example"
`))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.svc.Config = fakeConfig{cfg}
	p := &fakePinner{}
	h.srv.svc.Pins = p
	return h, p
}

func TestTheMaintainerChoosesTheHostATicketRunsOn(t *testing.T) {
	h, p := pinHarness(t)
	c := h.login()
	page := h.do("GET", "/t/T0011", nil, c).Body.String()
	for _, want := range []string{`action="/t/T0011/host"`, "Runs on any host.", `<option value="auto" selected>any</option>`,
		`<option value="local">local</option>`, `<option value="helper">helper</option>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}

	w := h.do("POST", "/t/T0011/host", url.Values{"csrf": {h.csrf(c)}, "host": {"helper"}}, c)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/t/T0011" {
		t.Fatalf("pin: %d %s", w.Code, w.Header().Get("Location"))
	}
	if f := flashOf(t, h, w); f == nil || f.Kind != "ok" || f.Text != "T0011 is pinned to helper." {
		t.Errorf("pin flash %+v", f)
	}
	w = h.do("POST", "/t/T0011/host", url.Values{"csrf": {h.csrf(c)}, "host": {"auto"}}, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "ok" || f.Text != "T0011 runs on any host." {
		t.Errorf("unpin flash %+v", f)
	}
	w = h.do("POST", "/t/T0011/host", url.Values{"csrf": {h.csrf(c)}, "host": {"any"}}, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "ok" || f.Text != "T0011 is pinned to any." {
		t.Errorf("a host named any: flash %+v", f)
	}
	if got := strings.Join(p.calls(), ", "); got != "T0011 helper, T0011 auto, T0011 any" {
		t.Errorf("the verb got %q", got)
	}

	w = h.do("POST", "/t/T0011/host", url.Values{"csrf": {h.csrf(c)}, "host": {"nowhere"}}, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || !strings.Contains(f.Text, "Unknown host nowhere") {
		t.Errorf("unknown host flash %+v", f)
	}
	if w := h.do("POST", "/t/T0011/host", url.Values{"csrf": {h.csrf(c)}}, c); w.Code != http.StatusBadRequest {
		t.Errorf("no host: %d", w.Code)
	}
	if got := len(p.calls()); got != 3 {
		t.Errorf("%d pins reached the verb", got)
	}
}

// Run on is everyone's (Foreman has no roles: the pin is recorded as its
// person's), behind the session's CSRF token and a same-origin browser.
func TestRunOnIsEveryonesBehindTheToken(t *testing.T) {
	h, p := pinHarness(t)
	m := h.person("ann")
	robin := h.person("robin", "widgets")
	if body := h.do("GET", "/t/T0011", nil, robin).Body.String(); !strings.Contains(body, "/t/T0011/host") || !strings.Contains(body, "Run on") {
		t.Errorf("robin's page does not offer Run on:\n%s", body)
	}
	if w := h.do("POST", "/t/T0011/host", url.Values{"csrf": {h.csrf(robin)}, "host": {"helper"}}, robin); w.Code != http.StatusSeeOther {
		t.Errorf("robin's pin: %d", w.Code)
	}
	if w := h.do("POST", "/t/T0011/host", url.Values{"host": {"helper"}}, m); w.Code != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", w.Code)
	}
	if w := h.do("POST", "/t/T0011/host", url.Values{"csrf": {h.csrf(robin)}, "host": {"helper"}}, m); w.Code != http.StatusForbidden {
		t.Errorf("another session's token: %d", w.Code)
	}
	if w := h.doWith("POST", "/t/T0011/host", url.Values{"csrf": {h.csrf(m)}, "host": {"helper"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"}, m); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", w.Code)
	}
	if w := h.do("POST", "/t/T0011/host", url.Values{"host": {"helper"}}); w.Code != http.StatusForbidden {
		t.Errorf("no login: %d", w.Code)
	}
	if got := p.calls(); len(got) != 1 {
		t.Fatalf("pinned %q", got)
	}
}

// A pin to a paused host is kept and shown as waiting, on the page and on
// the board's card; a closed ticket shows its pin with no choice.
func TestAPinToAPausedHostWaits(t *testing.T) {
	h, _ := pinHarness(t)
	h.change("T0011", func(tk *ticket.Ticket) { tk.Host = ticket.Some("helper") })
	h.change("T0010", func(tk *ticket.Ticket) { tk.Host = ticket.Some("helper") })
	h.writeSnapshot(sysinfo.Snapshot{Taken: testNow.Add(-time.Minute), Pauses: []sysinfo.Pause{{Host: "helper", Until: testNow.Add(3 * time.Hour)}}})
	c := h.login()
	page := h.do("GET", "/t/T0011", nil, c).Body.String()
	for _, want := range []string{"Pinned to <b>helper</b>", "helper is paused until 15:00", `<option value="helper" selected>helper</option>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
	if board := h.do("GET", "/b/widgets", nil, c).Body.String(); !strings.Contains(board, "on helper, waiting") {
		t.Errorf("the card does not show the waiting pin:\n%s", board)
	}
	closed := h.do("GET", "/t/T0010", nil, c).Body.String()
	if !strings.Contains(closed, "Pinned to <b>helper</b>") || strings.Contains(closed, "/t/T0010/host") {
		t.Errorf("a closed ticket's Run on:\n%s", closed)
	}
}
