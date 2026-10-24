package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/testkit"
)

func newUsers(t *testing.T) *Users {
	t.Helper()
	return NewUsers(filepath.Join(t.TempDir(), "foreman-users.json"), testkit.Fixed{T: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)})
}

// Foreman's TestAnotherPersonSeesOnlyTheirProjects, the store half (the
// pages are in package web).
func TestUsersAddMatchRemove(t *testing.T) {
	u := newUsers(t)
	secret, err := u.Add("robin", []string{"quotes"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		name     string
		projects []string
	}{{"maintainer", []string{"x"}}, {"Robin", []string{"x"}}, {"-x", []string{"x"}}, {"ok", nil},
		{strings.Repeat("a", 33), []string{"x"}}, {"robin", []string{"x"}}} {
		if _, err := u.Add(bad.name, bad.projects); err == nil {
			t.Errorf("Add(%q, %v) should be refused", bad.name, bad.projects)
		}
	}
	if u.Match(secret) != "robin" || u.Match("  "+secret+"\n") != "robin" || u.Match("wrong") != "" || u.Match("") != "" {
		t.Errorf("match")
	}
	got := u.Get("robin")
	if got == nil || !got.Sees("quotes") || got.Sees("widgets") || got.Added != "2026-10-09T12:00:00Z" {
		t.Fatalf("get %+v", got)
	}
	var nobody *User
	if !nobody.Sees("anything") {
		t.Errorf("a nil user is the maintainer, who sees everything")
	}
	if !u.Sees("robin", "quotes") || u.Sees("robin", "widgets") || u.Sees("gone", "quotes") {
		t.Errorf("Sees by name")
	}
	info, _ := os.Stat(u.path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("users file mode %v", info.Mode())
	}
	data, _ := os.ReadFile(u.path)
	if strings.Contains(string(data), secret) {
		t.Errorf("the secret itself is stored")
	}
	if err := u.Remove("robin"); err != nil {
		t.Fatal(err)
	}
	if u.Get("robin") != nil || u.Remove("robin") == nil {
		t.Errorf("removed user still there")
	}
}

// A removed user's sessions end (the session check asks the store).
func TestARemovedUserIsLoggedOut(t *testing.T) {
	u := newUsers(t)
	if _, err := u.Add("robin", []string{"quotes"}); err != nil {
		t.Fatal(err)
	}
	a := newAuth(t, Options{Users: u})
	s := a.NewSession("robin")
	id := a.Session(s)
	if id == "" || a.UserOf(id) != "robin" {
		t.Fatalf("robin's session %q", id)
	}
	if err := u.Remove("robin"); err != nil {
		t.Fatal(err)
	}
	if a.Session(s) != "" {
		t.Errorf("a removed user is still logged in")
	}
	// with no users store, only the maintainer's sessions count
	b := newAuth(t, Options{})
	if b.Session(b.NewSession("robin")) != "" {
		t.Errorf("a user session without a users store passed")
	}
}

func TestUsersFileReReadWhenItChanges(t *testing.T) {
	u := newUsers(t)
	if u.Get("x") != nil {
		t.Errorf("no file, no users")
	}
	os.WriteFile(u.path, []byte(`{"users": [{"name": "ann", "secret_sha256": "00", "projects": ["p"]}]}`), 0o600)
	future := time.Now().Add(time.Hour)
	os.Chtimes(u.path, future, future)
	if u.Get("ann") == nil {
		t.Errorf("a changed file should be read again")
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
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := UsersCommand(secretFile, args, &out, clk)
		return out.String(), err
	}
	if _, err := run(); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no args: %v", err)
	}
	out, err := run("add", "robin", "--project", "quotes", "--project", "widgets")
	if err != nil || !strings.Contains(out, "robin can log in with this secret (shown once):") {
		t.Fatalf("add: %q %v", out, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if NewUsers(filepath.Join(dir, "foreman-users.json"), clk).Match(lines[len(lines)-1]) != "robin" {
		t.Errorf("the printed secret does not log in")
	}
	if _, err := run("add", "x", "--bogus"); err == nil {
		t.Errorf("an unknown flag passed")
	}
	if _, err := run("add"); err == nil {
		t.Errorf("add without a name passed")
	}
	if out, _ := run("list"); !strings.Contains(out, "robin") || !strings.Contains(out, "quotes, widgets") {
		t.Errorf("list: %q", out)
	}
	if out, err := run("remove", "robin"); err != nil || !strings.Contains(out, "their sessions end now") {
		t.Errorf("remove: %q %v", out, err)
	}
	if _, err := run("remove"); err == nil {
		t.Errorf("remove without a name passed")
	}
	if _, err := run("frob"); err == nil {
		t.Errorf("an unknown command passed")
	}
}
