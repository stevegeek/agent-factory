package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (h *harness) loginWith(secret string) *http.Cookie {
	w := h.do("POST", "/login", url.Values{"secret": {secret}, "next": {"/"}})
	if w.Code != http.StatusSeeOther {
		h.t.Fatalf("login: %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	h.t.Fatal("no session cookie")
	return nil
}

func TestAnotherPersonSeesOnlyTheirProjects(t *testing.T) {
	h := newHarness(t, false)
	users := NewUsers(filepath.Join(t.TempDir(), "foreman-users.json"))
	h.srv.auth.users = users
	secret, err := users.Add("stef", []string{"factory"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.Add("maintainer", []string{"x"}); err == nil {
		t.Errorf("the maintainer's name is taken")
	}
	c := h.loginWith(secret)
	desk := h.do("GET", "/", nil, c).Body.String()
	if strings.Contains(desk, `href="/b/widgets"`) || strings.Contains(desk, "T0001") || !strings.Contains(desk, "stef · Log out") {
		t.Errorf("stef's desk shows another project or not her name")
	}
	if strings.Contains(desk, `href="/live"`) {
		t.Errorf("Live is the maintainer's")
	}
	for _, path := range []string{"/t/T0001", "/t/T0001/talk", "/live", "/live/log", "/b/widgets"} {
		if w := h.do("GET", path, nil, c); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
	csrf := h.srv.auth.CSRF(h.srv.auth.session(c.Value))
	if w := h.do("POST", "/t/T0001/act", url.Values{"csrf": {csrf}, "answer": {"note"}, "note": {"hi"}}, c); w.Code != http.StatusNotFound {
		t.Errorf("acting on another project's ticket: %d", w.Code)
	}
	if got := h.argv(); len(got) != 0 {
		t.Errorf("ran %v for someone who can't see the ticket", got)
	}
	newPage := h.do("GET", "/new", nil, c).Body.String()
	if strings.Contains(newPage, `value="manage"`) || strings.Contains(newPage, ">widgets<") {
		t.Errorf("the new-ticket page offers stef the manager or another project")
	}
	// the maintainer still sees everything
	m := h.login()
	if !strings.Contains(h.do("GET", "/b/widgets", nil, m).Body.String(), "widgets") {
		t.Errorf("the maintainer lost a board")
	}
	// removing her ends her session
	if err := users.Remove("stef"); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(users.path, users.mod.Add(1e9), users.mod.Add(1e9))
	if w := h.do("GET", "/", nil, c); w.Code != http.StatusSeeOther {
		t.Errorf("a removed user is still logged in: %d", w.Code)
	}
}

func TestPushReachesOnlyTheDevicesThatMaySeeIt(t *testing.T) {
	users := NewUsers(filepath.Join(t.TempDir(), "foreman-users.json"))
	users.Add("stef", []string{"quotes"})
	p := &Pusher{users: users}
	mine, hers := Subscription{Endpoint: "https://a"}, Subscription{Endpoint: "https://b", User: "stef"}
	var to []string
	deliver := func(m Message) []string {
		to = nil
		for _, s := range []Subscription{mine, hers} {
			if p.allowed(s, m) {
				to = append(to, s.Endpoint)
			}
		}
		return to
	}
	if got := deliver(Message{Project: "widgets"}); strings.Join(got, ",") != "https://a" {
		t.Errorf("widgets news went to %v", got)
	}
	if got := deliver(Message{Project: "quotes"}); strings.Join(got, ",") != "https://a,https://b" {
		t.Errorf("quotes news went to %v", got)
	}
	if got := deliver(Message{Project: "quotes", forOthers: true}); strings.Join(got, ",") != "https://b" {
		t.Errorf("an item already in the maintainer's summary went to %v", got)
	}
	if got := deliver(Message{Title: "5 updates"}); strings.Join(got, ",") != "https://a" {
		t.Errorf("the summary went to %v", got)
	}
}
