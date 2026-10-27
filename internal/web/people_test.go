package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// onDesk reports whether the desk page lists ticket id in its queue.
func onDesk(page, id string) bool {
	return regexp.MustCompile(`class="qitem[^"]*" href="/t/` + id + `[?"]`).MatchString(page)
}

// The secret logs in only while nobody has a login; then each person logs
// in with their email and password, and the secret's sessions end.
func TestPeopleLogInWithEmailAndPassword(t *testing.T) {
	h := newHarness(t, false)
	if page := h.do("GET", "/login", nil).Body.String(); !strings.Contains(page, "Nobody has a login yet") || strings.Contains(page, `name="email"`) {
		t.Errorf("the first login page asks for an email")
	}
	m := h.login()
	if err := h.users.Add("ann", "Ann@Example.com", userPassword("ann"), nil); err != nil {
		t.Fatal(err)
	}
	if w := h.do("GET", "/", nil, m); w.Code != http.StatusSeeOther {
		t.Errorf("the secret's session outlived the first person: %d", w.Code)
	}
	if page := h.do("GET", "/login", nil).Body.String(); !strings.Contains(page, `name="email"`) || strings.Contains(page, "Nobody has a login yet") {
		t.Errorf("the login page does not ask for an email")
	}
	if w := h.do("POST", "/login", url.Values{"email": {"ann@example.com"}, "password": {testSecret}}); w.Code != http.StatusUnauthorized {
		t.Errorf("the secret as a password: %d", w.Code)
	}
	if w := h.do("POST", "/login", url.Values{"password": {testSecret}}); w.Code != http.StatusUnauthorized ||
		!strings.Contains(w.Body.String(), "That email and password do not match.") {
		t.Errorf("the secret alone: %d", w.Code)
	}
	if w := h.do("POST", "/login", url.Values{"email": {"robin@example.com"}, "password": {userPassword("ann")}}); w.Code != http.StatusUnauthorized {
		t.Errorf("an unknown email: %d", w.Code)
	}
	c := h.loginAs("ann", "/")
	if page := h.do("GET", "/", nil, c).Body.String(); !strings.Contains(page, `<span class="menu-who">ann</span>`) {
		t.Errorf("ann's page does not name her")
	}
}

// earlier is a session cookie for a user, issued an hour ago, as if they
// were added two hours ago: the harness's clock stands still, and a logout
// ends the sessions issued before its second.
func (h *harness) earlier(name string) *http.Cookie {
	h.t.Helper()
	if err := h.users.Revoke(name, testNow.Add(-2*time.Hour)); err != nil {
		h.t.Fatal(err)
	}
	a, err := auth.New([]byte(testSecret), auth.Options{Users: h.users, Clock: testkit.Fixed{T: testNow.Add(-time.Hour)}})
	if err != nil {
		h.t.Fatal(err)
	}
	return &http.Cookie{Name: auth.SessionCookie, Value: a.NewSession(name)}
}

// Logging out ends one's own sessions, not anyone else's.
func TestLogoutIsOnesOwn(t *testing.T) {
	h := newHarness(t, false)
	h.person("ann")
	h.person("robin")
	ann, robin := h.earlier("ann"), h.earlier("robin")
	if w := h.do("POST", "/logout", url.Values{"csrf": {h.csrf(ann)}}, ann); w.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := h.do("GET", "/", nil, ann); w.Code != http.StatusSeeOther {
		t.Errorf("ann's session outlived her logout: %d", w.Code)
	}
	if w := h.do("GET", "/", nil, robin); w.Code != http.StatusOK {
		t.Errorf("robin was logged out by ann: %d", w.Code)
	}
	if w := h.do("GET", "/", nil, h.loginAs("ann", "/")); w.Code != http.StatusOK {
		t.Errorf("ann logs in again: %d", w.Code)
	}
}

// A desk holds the asks of its desk projects, none named: every project.
func TestTheDeskIsTheirProjects(t *testing.T) {
	h := newHarness(t, false)
	byProject := map[string][]string{}
	for _, tk := range h.fixtures() {
		if present.AskFor(tk) != nil {
			byProject[tk.Project] = append(byProject[tk.Project], string(tk.ID))
		}
	}
	if len(byProject["widgets"]) == 0 || len(byProject["factory"]) == 0 {
		t.Fatalf("the fixtures need asks on widgets and factory: %v", byProject)
	}
	every := h.do("GET", "/", nil, h.person("ann")).Body.String()
	mine := h.do("GET", "/", nil, h.person("robin", "factory")).Body.String()
	for _, id := range byProject["widgets"] {
		if !onDesk(every, id) || onDesk(mine, id) {
			t.Errorf("%s (widgets): on ann's desk %v, on robin's factory desk %v", id, onDesk(every, id), onDesk(mine, id))
		}
	}
	for _, id := range byProject["factory"] {
		if !onDesk(every, id) || !onDesk(mine, id) {
			t.Errorf("%s (factory) is missing from a desk", id)
		}
	}
	// passed to robin, a widgets ask is on robin's desk alone
	id := byProject["widgets"][0]
	h.change(id, func(tk *ticket.Ticket) { tk.Assignee = ticket.Some("robin") })
	every = h.do("GET", "/", nil, h.loginAs("ann", "/")).Body.String()
	mine = h.do("GET", "/", nil, h.loginAs("robin", "/")).Body.String()
	if onDesk(every, id) || !onDesk(mine, id) {
		t.Errorf("%s assigned to robin: on ann's desk %v, on robin's %v", id, onDesk(every, id), onDesk(mine, id))
	}
}

// Passing a ticket goes to the service as the person passing it; once
// passed, it is in the other person's For you until they put it away.
func TestAPassedTicketIsForItsPerson(t *testing.T) {
	h := newHarness(t, false)
	ann := h.person("ann")
	robin := h.person("robin")
	page := h.do("GET", "/t/T0001", nil, ann).Body.String()
	if !strings.Contains(page, `action="/t/T0001/assign"`) || !strings.Contains(page, `<option value="robin">robin</option>`) {
		t.Errorf("the ticket page offers no pass to robin")
	}
	w := h.do("POST", "/t/T0001/assign", url.Values{"csrf": {h.csrf(ann)}, "to": {"robin"}, "note": {"yours, @robin"}}, ann)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/t/T0001" {
		t.Fatalf("pass: %d %s", w.Code, w.Header().Get("Location"))
	}
	// ann is the first of [board] people: the services name her as the maintainer
	if got := h.ops.passes; len(got) != 1 || got[0] != [4]string{"T0001", "robin", "", "yours, @robin"} {
		t.Errorf("passes %q", got)
	}
	// as the service records it
	at := testNow.Add(-time.Minute)
	h.change("T0001", func(tk *ticket.Ticket) {
		tk.Assignee = ticket.Some("robin")
		ticket.Event(tk, "edit", "ann passed it to robin", "", ticket.Edited{By: "ann", Fields: []string{"assignee"}}, at)
	})
	desk := h.do("GET", "/", nil, robin).Body.String()
	if !strings.Contains(desk, "For you") || !strings.Contains(desk, "ann passed it to you") {
		t.Fatalf("robin's desk lacks the pass")
	}
	if strings.Contains(h.do("GET", "/", nil, ann).Body.String(), "ann passed it to you") {
		t.Errorf("ann's own pass is on her desk")
	}
	page = h.do("GET", "/t/T0001", nil, robin).Body.String()
	if !strings.Contains(page, `action="/t/T0001/seen"`) || !strings.Contains(page, "ann passed this to you") {
		t.Fatalf("robin's ticket page has no Put away")
	}
	stamp := regexp.MustCompile(`name="at" value="([^"]+)"`).FindStringSubmatch(page)
	if stamp == nil {
		t.Fatal("no time on Put away")
	}
	if w := h.do("POST", "/t/T0001/seen", url.Values{"csrf": {h.csrf(robin)}, "at": {html.UnescapeString(stamp[1])}}, robin); w.Code != http.StatusSeeOther {
		t.Fatalf("seen: %d", w.Code)
	}
	if strings.Contains(h.do("GET", "/", nil, robin).Body.String(), "ann passed it to you") {
		t.Errorf("a put-away pass is still on robin's desk")
	}
	if w := h.do("POST", "/t/T0001/seen", url.Values{"csrf": {h.csrf(robin)}, "at": {"yesterday"}}, robin); w.Code != http.StatusNotFound {
		t.Errorf("seen with a bad time: %d", w.Code)
	}
	if w := h.do("POST", "/t/T0001/assign", url.Values{"to": {"robin"}}, ann); w.Code != http.StatusForbidden {
		t.Errorf("a pass without the CSRF token: %d", w.Code)
	}
}

// Settings: anyone adds a person, sets a desk, gives a new password and
// removes one; a user missing from [board] people is pointed out.
func TestSettingsManagePeople(t *testing.T) {
	h := newHarness(t, false)
	cfg, err := config.Parse([]byte(strings.Replace(testConfig, `people = ["ann", "robin"]`, `people = ["ann"]`, 1) + "\n[projects.widgets]\npath = \"/x/widgets\"\n\n[projects.gadgets]\npath = \"/x/gadgets\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.svc.Config = fakeConfig{cfg}
	m := h.login()
	page := h.do("GET", "/settings", nil, m).Body.String()
	if !strings.Contains(page, "Add yourself first") || strings.Contains(page, `action="/settings/desk"`) {
		t.Errorf("the first settings: no hint to add yourself, or a desk for the secret")
	}
	add := url.Values{"csrf": {h.csrf(m)}, "name": {"ann"}, "email": {"ann@example.com"}, "password": {userPassword("ann")}, "desk": {"widgets", "nonesuch"}}
	if w := h.do("POST", "/settings/users", add, m); w.Code != http.StatusSeeOther {
		t.Fatalf("add: %d", w.Code)
	}
	if u := h.users.Get("ann"); u == nil || strings.Join(u.Desk, ",") != "widgets" {
		t.Fatalf("ann: %+v", u)
	}
	ann := h.loginAs("ann", "/")
	add = url.Values{"csrf": {h.csrf(ann)}, "name": {"robin"}, "email": {"robin@example.com"}, "password": {"short"}}
	h.do("POST", "/settings/users", add, ann)
	if h.users.Get("robin") != nil {
		t.Errorf("a short password was taken")
	}
	add.Set("password", userPassword("robin"))
	h.do("POST", "/settings/users", add, ann)
	page = h.do("GET", "/settings", nil, ann).Body.String()
	if !strings.Contains(page, `action="/settings/desk"`) || !strings.Contains(page, `value="widgets" checked`) ||
		!strings.Contains(page, "<b>robin</b>") || !strings.Contains(page, "Not in <code>[board] people</code> in config.toml: robin.") {
		t.Errorf("ann's settings:\n%s", page)
	}
	if w := h.do("POST", "/settings/desk", url.Values{"csrf": {h.csrf(ann)}, "desk": {"gadgets", "widgets"}}, ann); w.Code != http.StatusSeeOther {
		t.Fatalf("desk: %d", w.Code)
	}
	if u := h.users.Get("ann"); strings.Join(u.Desk, ",") != "gadgets,widgets" {
		t.Errorf("ann's desk %v", u.Desk)
	}
	// anyone changes anyone: a desk, a password (which ends their sessions), a removal
	robin := h.earlier("robin")
	h.do("POST", "/settings/users/robin", url.Values{"csrf": {h.csrf(ann)}, "do": {"desk"}, "desk": {"gadgets"}}, ann)
	if u := h.users.Get("robin"); strings.Join(u.Desk, ",") != "gadgets" {
		t.Errorf("robin's desk %v", u.Desk)
	}
	h.do("POST", "/settings/users/robin", url.Values{"csrf": {h.csrf(ann)}, "do": {"password"}, "password": {"a brand new password"}}, ann)
	if w := h.do("GET", "/", nil, robin); w.Code != http.StatusSeeOther {
		t.Errorf("robin's session outlived a new password: %d", w.Code)
	}
	if w := h.do("POST", "/login", url.Values{"email": {"robin@example.com"}, "password": {"a brand new password"}}); w.Code != http.StatusSeeOther {
		t.Errorf("robin's new password: %d", w.Code)
	}
	h.do("POST", "/settings/users/robin", url.Values{"csrf": {h.csrf(ann)}, "do": {"remove"}}, ann)
	if h.users.Get("robin") != nil {
		t.Errorf("robin was not removed")
	}
	if w := h.do("POST", "/settings/users/nobody", url.Values{"csrf": {h.csrf(ann)}, "do": {"remove"}}, ann); w.Code != http.StatusNotFound {
		t.Errorf("an unknown user: %d", w.Code)
	}
}
