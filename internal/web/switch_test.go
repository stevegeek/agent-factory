package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
)

// fakeSwitcher records the switches posted and answers with line or err.
type fakeSwitcher struct {
	mu   sync.Mutex
	got  []string // "project state by"
	line string
	err  error
}

func (f *fakeSwitcher) SwitchProject(_ context.Context, project, state, by string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, project+" "+state+" "+by)
	return f.line, f.err
}

func (f *fakeSwitcher) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// switchHarness has a factory project paused here over config, one paused
// in config, and a board project.
func switchHarness(t *testing.T) (*harness, *fakeSwitcher) {
	t.Helper()
	h := newHarness(t, false)
	cfg, err := config.Parse([]byte(`
[projects.busy]
path = "/y"

[projects.resting]
path = "/x"
enabled = false

[projects.notes]
mode = "board"
`))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.svc.Config = fakeConfig{cfg.WithSwitches(config.Switches{"busy": {Enabled: false, By: "alice", At: "2026-10-09T10:00:00+00:00"}})}
	sw := &fakeSwitcher{line: "busy switched on: the factory works on it"}
	h.srv.svc.Switch = sw
	return h, sw
}

// switchCard is the Boards cell of project, up to the next one.
func switchCard(body, project string) string {
	i := strings.Index(body, `href="/b/`+project+`"`)
	if i < 0 {
		return ""
	}
	card := body[i:]
	if j := strings.Index(card, `<div class="board-cell">`); j >= 0 {
		card = card[:j]
	}
	return card
}

func TestTheMaintainerSwitchesProjectsOnBoards(t *testing.T) {
	h, sw := switchHarness(t)
	c := h.login()
	body := h.do("GET", "/boards", nil, c).Body.String()
	busy := switchCard(body, "busy")
	if !strings.Contains(busy, ">paused<") || !strings.Contains(busy, `value="on">Switch on`) ||
		!strings.Contains(busy, "paused here by alice at ") || !strings.Contains(busy, " Oct ") || !strings.Contains(busy, "Follow config (enabled)") {
		t.Errorf("busy's card:\n%s", busy)
	}
	resting := switchCard(body, "resting")
	if !strings.Contains(resting, `value="on">Switch on`) || strings.Contains(resting, "Follow config") {
		t.Errorf("resting's card (paused in config, not switched):\n%s", resting)
	}
	if strings.Contains(switchCard(body, "notes"), `action="/boards/switch"`) {
		t.Error("a board project has a switch")
	}

	w := h.do("POST", "/boards/switch", url.Values{"csrf": {h.csrf(c)}, "project": {"busy"}, "state": {"on"}}, c)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/boards" {
		t.Fatalf("switch: %d %s", w.Code, w.Header().Get("Location"))
	}
	if got := sw.calls(); len(got) != 1 || got[0] != "busy on " { // "" by: the maintainer
		t.Fatalf("switched %q", got)
	}
	if f := flashOf(t, h, w); f == nil || f.Kind != "ok" || !strings.HasPrefix(f.Text, "Busy switched on") {
		t.Errorf("flash %+v", f)
	}
	// a refusal comes back as the flash
	sw.err = errors.New("notes is a board project: only a factory project is switched on or off")
	w = h.do("POST", "/boards/switch", url.Values{"csrf": {h.csrf(c)}, "project": {"notes"}, "state": {"off"}}, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || !strings.Contains(f.Text, "board project") {
		t.Errorf("flash %+v", f)
	}
}

// Nobody else sees the switch or can post it; the maintainer's post needs
// the CSRF token and a same-origin browser.
func TestOnlyTheMaintainerSwitchesProjects(t *testing.T) {
	h, sw := switchHarness(t)
	m := h.login()
	secret, _ := h.users.Add("robin", []string{"busy", "resting"})
	robin := h.loginWith(secret, "/")
	body := h.do("GET", "/boards", nil, robin).Body.String()
	if !strings.Contains(body, `href="/b/busy"`) || strings.Contains(body, "/boards/switch") || strings.Contains(body, "Follow config") {
		t.Errorf("robin's boards offer the switch:\n%s", body)
	}
	if w := h.do("POST", "/boards/switch", url.Values{"csrf": {h.csrf(robin)}, "project": {"busy"}, "state": {"on"}}, robin); w.Code != http.StatusNotFound {
		t.Errorf("robin's switch: %d", w.Code)
	}
	if w := h.do("POST", "/boards/switch", url.Values{"project": {"busy"}, "state": {"on"}}, m); w.Code != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", w.Code)
	}
	if w := h.do("POST", "/boards/switch", url.Values{"csrf": {h.csrf(robin)}, "project": {"busy"}, "state": {"on"}}, m); w.Code != http.StatusForbidden {
		t.Errorf("another session's token: %d", w.Code)
	}
	if w := h.doWith("POST", "/boards/switch", url.Values{"csrf": {h.csrf(m)}, "project": {"busy"}, "state": {"on"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"}, m); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", w.Code)
	}
	if w := h.do("POST", "/boards/switch", url.Values{"project": {"busy"}, "state": {"on"}}); w.Code != http.StatusForbidden {
		t.Errorf("no login: %d", w.Code)
	}
	if got := sw.calls(); len(got) != 0 {
		t.Fatalf("switched %q", got)
	}
}
