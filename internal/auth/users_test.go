package auth

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

const robinPassword = "robins-password"

func newUsers(t *testing.T) *Users {
	t.Helper()
	return NewUsers(filepath.Join(t.TempDir(), "foreman-users.json"), testkit.Fixed{T: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}).WithCost(2)
}

func TestUsersAddLoginRemove(t *testing.T) {
	u := newUsers(t)
	if err := u.Add("robin", "Robin@Example.com", robinPassword, []string{"gizmos", " ", "gizmos"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct{ name, email, password string }{
		{"maintainer", "m@example.com", robinPassword}, {"factory", "f@example.com", robinPassword},
		{"Robin", "r2@example.com", robinPassword}, {"-x", "x@example.com", robinPassword},
		{strings.Repeat("a", 33), "a@example.com", robinPassword},
		{"robin", "other@example.com", robinPassword}, // the name is taken
		{"ann", "robin@example.com", robinPassword},   // the email is taken
		{"ann", "not-an-email", robinPassword},        // no @
		{"ann", "ann@example.com", "short"},           // too short
		{"ann", "ann@example.com", strings.Repeat("x", MaxPassword+1)},
	} {
		if err := u.Add(bad.name, bad.email, bad.password, nil); err == nil {
			t.Errorf("Add(%q, %q, %d chars) should be refused", bad.name, bad.email, len(bad.password))
		}
	}
	if u.Login("robin@example.com", robinPassword) != "robin" || u.Login(" ROBIN@example.com ", robinPassword) != "robin" {
		t.Errorf("the email is matched in any case")
	}
	if u.Login("robin@example.com", "wrong-password") != "" || u.Login("nobody@example.com", robinPassword) != "" || u.Login("", "") != "" {
		t.Errorf("a wrong password or email logged in")
	}
	got := u.Get("robin")
	if got == nil || got.Email != "robin@example.com" || strings.Join(got.Desk, ",") != "gizmos" || got.Added != "2026-10-09T12:00:00Z" {
		t.Fatalf("get %+v", got)
	}
	if !got.Sees("gizmos") || got.Sees("widgets") || !u.Sees("robin", "gizmos") || u.Sees("gone", "gizmos") {
		t.Errorf("a desk project")
	}
	var nobody *User
	if !nobody.Sees("anything") || !(&User{Name: "ann"}).Sees("anything") {
		t.Errorf("the main secret, and a user with no desk projects, see every project")
	}
	info, _ := os.Stat(u.path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("users file mode %v", info.Mode())
	}
	data, _ := os.ReadFile(u.path)
	if strings.Contains(string(data), robinPassword) || !strings.Contains(string(data), "pbkdf2-sha256$2$") {
		t.Errorf("the password must be stored hashed only: %s", data)
	}
	if err := u.Remove("robin"); err != nil {
		t.Fatal(err)
	}
	if u.Get("robin") != nil || u.Remove("robin") == nil || u.Any() {
		t.Errorf("removed user still there")
	}
}

func TestPasswordsDeskAndDismissals(t *testing.T) {
	u := newUsers(t)
	if err := u.Add("robin", "robin@example.com", robinPassword, nil); err != nil {
		t.Fatal(err)
	}
	if err := u.SetPassword("robin", "a-new-password"); err != nil {
		t.Fatal(err)
	}
	if u.Login("robin@example.com", robinPassword) != "" || u.Login("robin@example.com", "a-new-password") != "robin" {
		t.Errorf("the new password replaces the old")
	}
	if u.Get("robin").Since == 0 {
		t.Errorf("a new password ends the sessions before it")
	}
	if u.SetPassword("robin", "short") == nil || u.SetPassword("ghost", "long-enough-pw") == nil {
		t.Errorf("a short password, or no such user, passed")
	}
	if err := u.SetDesk("robin", []string{"widgets", "gizmos"}); err != nil || strings.Join(u.Get("robin").Desk, ",") != "gizmos,widgets" {
		t.Errorf("desk: %v %v", err, u.Get("robin").Desk)
	}
	if err := u.SetDesk("robin", nil); err != nil || !u.Get("robin").Sees("anything") {
		t.Errorf("an empty desk is every project")
	}
	u.Dismiss("robin", "T0001", "2026-10-09T12:00:00Z")
	u.Dismiss("robin", "T0001", "2026-10-09T11:00:00Z") // older: kept the newer
	if got := u.Get("robin").Dismissed["T0001"]; got != "2026-10-09T12:00:00Z" {
		t.Errorf("dismissed %q", got)
	}
	for i := 0; i < maxDismissed+5; i++ {
		u.Dismiss("robin", fmt.Sprintf("T9%03d", i), "2026-10-10T00:00:00Z")
	}
	if n := len(u.Get("robin").Dismissed); n != maxDismissed {
		t.Errorf("dismissals are capped: %d", n)
	}
}

// A removed user's sessions end, and so do those from before they logged
// out; the main secret logs in only while there are no users.
// A person removed and added again starts with no sessions; the main
// secret's old sessions stay over even when the last user goes; a name the
// board reserves is refused; an older file's secret-only users are nobody.
func TestUsersComeBackWithNothingOld(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clk := testkit.NewStep(at, 0)
	u := NewUsers(filepath.Join(t.TempDir(), "foreman-users.json"), clk).WithCost(2)
	a := newAuth(t, Options{Users: u, Clock: clk})
	boot := a.NewSession(Maintainer)
	clk.Advance(2 * time.Second)
	if err := u.Add("robin", "robin@example.com", robinPassword, nil); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Second)
	old := a.NewSession("robin")
	if err := u.Remove("robin"); err != nil {
		t.Fatal(err)
	}
	if a.Session(boot) != "" {
		t.Errorf("with the last user gone, the main secret's old session came back")
	}
	if a.Session(a.NewSession(Maintainer)) == "" {
		t.Errorf("with no users, a new main-secret session passes")
	}
	clk.Advance(2 * time.Second)
	if err := u.Add("robin", "robin@example.com", "another-password", nil); err != nil {
		t.Fatal(err)
	}
	if a.Session(old) != "" {
		t.Errorf("robin's cookie from before the removal came back")
	}
	for _, name := range []string{"human", "system", "me"} {
		if u.Add(name, name+"@example.com", robinPassword, nil) == nil {
			t.Errorf("the reserved name %s was taken", name)
		}
	}
	legacy := NewUsers(filepath.Join(t.TempDir(), "foreman-users.json"), clk)
	os.WriteFile(legacy.path, []byte(`{"users": [{"name": "ann", "secret_sha256": "00", "projects": ["p"]}]}`), 0o600)
	if legacy.Any() || legacy.Get("ann") != nil {
		t.Errorf("an older file's secret-only user shuts the main secret out")
	}
}

func TestUserSessions(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clk := testkit.NewStep(at, 0)
	u := NewUsers(filepath.Join(t.TempDir(), "foreman-users.json"), clk).WithCost(2)
	a := newAuth(t, Options{Users: u, Clock: clk})
	boot := a.NewSession(Maintainer)
	if a.Session(boot) == "" {
		t.Fatal("with no users the main secret's session passes")
	}
	if name, ok := a.LoginAs("192.0.2.1", "", testSecret); !ok || name != Maintainer {
		t.Fatalf("with no users the secret logs in: %q %v", name, ok)
	}
	if err := u.Add("robin", "robin@example.com", robinPassword, nil); err != nil {
		t.Fatal(err)
	}
	if err := u.Add("ann", "ann@example.com", "anns-password", nil); err != nil {
		t.Fatal(err)
	}
	if a.Session(boot) != "" {
		t.Errorf("once there are users the main secret's sessions end")
	}
	if _, ok := a.LoginAs("192.0.2.1", "", testSecret); ok {
		t.Errorf("once there are users the secret no longer logs in")
	}
	if name, ok := a.LoginAs("192.0.2.1", "robin@example.com", robinPassword); !ok || name != "robin" {
		t.Errorf("robin logs in: %q %v", name, ok)
	}
	robin, ann := a.NewSession("robin"), a.NewSession("ann")
	if id := a.Session(robin); id == "" || a.UserOf(id) != "robin" {
		t.Fatalf("robin's session %q", id)
	}
	clk.Advance(2 * time.Second)
	if err := a.Revoke("robin"); err != nil {
		t.Fatal(err)
	}
	if a.Session(robin) != "" {
		t.Errorf("robin logged out, but the session passes")
	}
	if a.Session(ann) == "" {
		t.Errorf("robin's logout ended ann's session")
	}
	if a.Session(a.NewSession("robin")) == "" {
		t.Errorf("robin's next login passes")
	}
	if err := u.Remove("ann"); err != nil {
		t.Fatal(err)
	}
	if a.Session(ann) != "" {
		t.Errorf("a removed user is still logged in")
	}
	// with no users store, only the main secret's sessions count
	b := newAuth(t, Options{})
	if b.Session(b.NewSession("robin")) != "" || b.Session(b.NewSession(Maintainer)) == "" {
		t.Errorf("without a users store")
	}
}

// A trusted Tailscale login is the user with that email once there are
// users; another login no longer counts.
func TestTailscaleLoginIsAUser(t *testing.T) {
	u := newUsers(t)
	a := newAuth(t, Options{Users: u, TailscaleUsers: []string{"robin@example.com", "other@example.com"}})
	ask := func(login string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("Tailscale-User-Login", login)
		return a.Identity(r)
	}
	if id := ask("other@example.com"); id == "" || a.UserOf(id) != Maintainer {
		t.Errorf("with no users a trusted login is the main secret's: %q", id)
	}
	if err := u.Add("robin", "robin@example.com", robinPassword, nil); err != nil {
		t.Fatal(err)
	}
	if id := ask("robin@example.com"); id == "" || a.UserOf(id) != "robin" {
		t.Errorf("robin's login: %q", id)
	}
	if ask("other@example.com") != "" {
		t.Errorf("a trusted login that is no user's counts once there are users")
	}
}

func TestUsersFileReReadWhenItChanges(t *testing.T) {
	u := newUsers(t)
	if u.Get("x") != nil {
		t.Errorf("no file, no users")
	}
	os.WriteFile(u.path, []byte(`{"users": [{"name": "ann", "email": "ann@example.com", "password": "x", "desk": ["p"]}]}`), 0o600)
	future := time.Now().Add(time.Hour)
	os.Chtimes(u.path, future, future)
	if u.Get("ann") == nil || u.ByEmail("ANN@example.com") == nil {
		t.Errorf("a changed file should be read again")
	}
	if u.Login("ann@example.com", "x") != "" {
		t.Errorf("a stored password that is no hash never matches")
	}
	os.WriteFile(u.path, []byte(`not json`), 0o600)
	os.Chtimes(u.path, future.Add(time.Hour), future.Add(time.Hour))
	if u.Get("ann") == nil {
		t.Errorf("a broken file keeps the last good read")
	}
}

func TestUsersCommand(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "foreman-secret")
	clk := testkit.Fixed{T: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := UsersCommand(secretFile, args, strings.NewReader(stdin), &out, clk)
		return out.String(), err
	}
	if _, err := run(""); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no args: %v", err)
	}
	out, err := run(robinPassword+"\n", "add", "robin", "robin@example.com", "--desk", "gizmos", "--desk", "widgets")
	if err != nil || !strings.Contains(out, "robin can log in as robin@example.com; desk: gizmos, widgets") {
		t.Fatalf("add: %q %v", out, err)
	}
	users := NewUsers(filepath.Join(dir, "foreman-users.json"), clk)
	if users.Login("robin@example.com", robinPassword) != "robin" {
		t.Errorf("the password from stdin does not log in")
	}
	if _, err := run("", "add", "ann", "ann@example.com"); err == nil {
		t.Errorf("add with no password on stdin passed")
	}
	if _, err := run(robinPassword+"\n", "add", "x", "x@example.com", "--bogus"); err == nil {
		t.Errorf("an unknown flag passed")
	}
	if out, _ := run("", "list"); !strings.Contains(out, "robin@example.com") || !strings.Contains(out, "gizmos, widgets") {
		t.Errorf("list: %q", out)
	}
	if out, err := run("", "desk", "robin"); err != nil || !strings.Contains(out, "every project") || len(users.Get("robin").Desk) != 0 {
		t.Errorf("desk: %q %v", out, err)
	}
	if out, err := run("another-password\n", "passwd", "robin"); err != nil || !strings.Contains(out, "new password") ||
		users.Login("robin@example.com", "another-password") != "robin" {
		t.Errorf("passwd: %q %v", out, err)
	}
	if out, err := run("", "remove", "robin"); err != nil || !strings.Contains(out, "their sessions end now") {
		t.Errorf("remove: %q %v", out, err)
	}
	if _, err := run("", "remove"); err == nil {
		t.Errorf("remove without a name passed")
	}
	if _, err := run("", "frob"); err == nil {
		t.Errorf("an unknown command passed")
	}
}
