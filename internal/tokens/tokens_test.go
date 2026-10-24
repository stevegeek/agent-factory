package tokens

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

var at = time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	env := testkit.Root(t)
	return &Store{Path: filepath.Join(env.Home, ".config", "factory", "cfg", "api-tokens.json"), Clock: clock.Fixed{T: at}}
}

func add(t *testing.T, s *Store, name, can string, projects ...string) string {
	t.Helper()
	out, err := s.Add(context.Background(), AddIn{Name: name, Projects: projects, Can: can,
		People: []string{"alice", "bob"}, Known: []string{"sandbox", "other", "fac"}})
	if err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
	return out.Token
}

// port: TT TokenTest.test_a_token_is_shown_once_and_only_its_hash_is_kept
func TestATokenIsShownOnceAndOnlyItsHashIsKept(t *testing.T) {
	s := newStore(t)
	out, err := s.Add(context.Background(), AddIn{Name: "example-agent", Projects: []string{"sandbox", "sandbox"},
		Can: "file, talk", People: []string{"alice"}, Known: []string{"sandbox"}})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^fct_[A-Za-z0-9_-]{43}$`).MatchString(out.Token) {
		t.Fatalf("token %q", out.Token)
	}
	if want := "example-agent: read,file,talk on sandbox. The token is shown this once; it is not kept."; out.Note != want {
		t.Errorf("note %q, want %q", out.Note, want)
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("store mode %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(s.Path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("store dir mode %v, want 0700", di.Mode().Perm())
	}
	raw, _ := os.ReadFile(s.Path)
	if bytes.Contains(raw, []byte(out.Token)) {
		t.Fatal("the plain token is in the store")
	}
	var f struct {
		Version int
		Tokens  []map[string]any
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	rec := f.Tokens[0]
	sum := sha256.Sum256([]byte(out.Token))
	if rec["sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 %v", rec["sha256"])
	}
	if got, _ := json.Marshal([]any{rec["can"], rec["projects"], rec["added"], rec["expires"], rec["tickets"]}); string(got) !=
		`[["read","file","talk"],["sandbox"],"2026-10-17T08:00:00+00:00",null,null]` {
		t.Errorf("record %s", got)
	}
	// Python's json.dump(indent=2) of the record, keys in its order
	want := "{\n  \"version\": 1,\n  \"tokens\": [\n    {\n      \"name\": \"example-agent\",\n      \"sha256\": \"" + hex.EncodeToString(sum[:]) +
		"\",\n      \"projects\": [\n        \"sandbox\"\n      ],\n      \"can\": [\n        \"read\",\n        \"file\",\n        \"talk\"\n      ],\n" +
		"      \"added\": \"2026-10-17T08:00:00+00:00\",\n      \"expires\": null,\n      \"tickets\": null\n    }\n  ]\n}"
	if string(raw) != want {
		t.Errorf("store file:\n%s", testkit.Diff(want, string(raw)))
	}
	rows, err := s.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if b := (jsonx.Format{Indent: -1}).Encode(rows); bytes.Contains(b, []byte("sha256")) || !bytes.Contains(b, []byte(`"name": "example-agent"`)) {
		t.Errorf("rows %s", b)
	}
	// the secret checks out, and only it
	if name, ok := s.Verify(out.Token); !ok || name != "example-agent" {
		t.Errorf("verify: %q %v", name, ok)
	}
	if _, ok := s.Verify(out.Token + "x"); ok {
		t.Error("a wrong token verified")
	}
	if _, ok := s.Verify(""); ok {
		t.Error("an empty token verified")
	}
}

// port: TT TokenTest.test_add_refuses_reserved_names_people_duplicates_and_unknown_projects
func TestAddRefusesReservedNamesPeopleDuplicatesAndUnknownProjects(t *testing.T) {
	s := newStore(t)
	add(t, s, "example-agent", "read,file,talk,work,plan", "sandbox", "fac")
	for _, c := range []struct {
		in   AddIn
		want string
	}{
		{AddIn{Name: "factory", Projects: []string{"sandbox"}, Can: "read"}, "factory is reserved; pick another name"},
		{AddIn{Name: "alice", Projects: []string{"sandbox"}, Can: "read"}, "alice is a person in [board] people; pick another name"},
		{AddIn{Name: "maintainer", Projects: []string{"sandbox"}, Can: "read"}, "maintainer is reserved; pick another name"},
		{AddIn{Name: "example-agent", Projects: []string{"sandbox"}, Can: "read"}, "there is already a token example-agent; revoke it first"},
		{AddIn{Name: "new-one", Projects: []string{"nowhere", "fac", "gone"}, Can: "read"}, "unknown project nowhere, gone; known: sandbox, other, fac"},
		{AddIn{Name: "new-one", Projects: []string{"sandbox"}, Can: "merge"}, "--can takes some of: read,file,talk,work,plan"},
		{AddIn{Name: "new-one", Projects: []string{"sandbox"}, Can: " , "}, "--can takes some of: read,file,talk,work,plan"},
		{AddIn{Name: "new-one", Projects: nil, Can: "read"}, "name at least one --project"},
		{AddIn{Name: "Bad_Name", Projects: []string{"sandbox"}, Can: "read"}, "a token name is lower-case letters, digits and dashes, starting with a letter (at most 32)"},
		{AddIn{Name: "a" + strings.Repeat("b", 32), Projects: []string{"sandbox"}, Can: "read"}, "a token name is lower-case letters, digits and dashes, starting with a letter (at most 32)"},
		{AddIn{Name: "new-one\n", Projects: []string{"sandbox"}, Can: "read"}, "a token name is lower-case letters, digits and dashes, starting with a letter (at most 32)"},
		{AddIn{Name: "", Projects: []string{"sandbox"}, Can: "read"}, "factory tokens add NAME"},
	} {
		c.in.People, c.in.Known = []string{"alice", "bob"}, []string{"sandbox", "other", "fac"}
		_, err := s.Add(context.Background(), c.in)
		var r *Refused
		if !errors.As(err, &r) || r.Error() != c.want {
			t.Errorf("add %q: %v, want %q", c.in.Name, err, c.want)
		}
	}
	f, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.Records()); n != 1 {
		t.Errorf("%d records, want 1", n)
	}
	// a 32-letter name is fine
	add(t, s, "a"+strings.Repeat("b", 31), "read", "sandbox")
}

// port: TT TokenTest.test_a_revoked_or_expired_token_is_unauthorized
func TestARevokedOrExpiredTokenIsGone(t *testing.T) {
	s := newStore(t)
	tok := add(t, s, "example-agent", "read", "sandbox")
	if _, ok, err := s.Live("example-agent"); !ok || err != nil {
		t.Fatalf("live: %v %v", ok, err)
	}
	if err := s.Revoke(context.Background(), "example-agent"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Live("example-agent"); ok {
		t.Error("a revoked token is live")
	}
	if _, ok := s.Verify(tok); ok {
		t.Error("a revoked token verified")
	}
	var r *Refused
	if err := s.Revoke(context.Background(), "example-agent"); !errors.As(err, &r) || r.Error() != "no token example-agent" {
		t.Errorf("second revoke: %v", err)
	}
	if err := s.Revoke(context.Background(), ""); !errors.As(err, &r) || r.Error() != "factory tokens revoke NAME" {
		t.Errorf("revoke without a name: %v", err)
	}
	short := add(t, s, "short-lived", "read", "sandbox")
	if err := s.Change(context.Background(), func(f *File) error {
		return f.Set("short-lived", "expires", jsonx.String("2000-01-01T00:00:00+00:00"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Live("short-lived"); ok {
		t.Error("an expired token is live")
	}
	if name, ok := s.Verify(short); ok || name != "" || !s.Stale(short) {
		t.Errorf("verify expired: %q %v, stale %v", name, ok, s.Stale(short))
	}
}

func TestExpiry(t *testing.T) {
	now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		expires string // JSON
		want    bool
	}{
		{`null`, false}, {`""`, false}, {`0`, false}, {`false`, false}, {`[]`, false},
		{`"2026-10-18T10:00:00+00:00"`, true}, // <= now
		{`"2026-10-18T10:00:01+00:00"`, false},
		{`"2026-10-18T11:00:00+01:00"`, true},
		{`"2026-10-18T10:00:00Z"`, true},
		{`"2027-01-01T00:00:00Z"`, false},
		{`"2030-01-01T00:00:00"`, true}, // naive: Python cannot compare it, so it has expired
		{`"2030-01-01"`, true},
		{`"soon"`, true}, {`5`, true}, {`true`, true}, {`["x"]`, true},
	} {
		v, err := jsonx.Parse([]byte(c.expires))
		if err != nil {
			t.Fatal(err)
		}
		rec := Record{v: jsonx.ObjectOf(jsonx.Member{Key: "expires", Value: v})}
		if got := rec.Expired(now); got != c.want {
			t.Errorf("expires %s: expired %v, want %v", c.expires, got, c.want)
		}
	}
	if (Record{v: jsonx.ObjectOf()}).Expired(now) {
		t.Error("no expires key has expired")
	}
}

func TestLoadShapes(t *testing.T) {
	s := newStore(t)
	f, err := s.Load()
	if err != nil || len(f.Records()) != 0 {
		t.Fatalf("missing store: %v %v", f, err)
	}
	os.MkdirAll(filepath.Dir(s.Path), 0o700)
	for _, bad := range []string{`[]`, `{"version": 2, "tokens": []}`, `{"version": 1}`, `{"version": 1, "tokens": {}}`} {
		os.WriteFile(s.Path, []byte(bad), 0o600)
		_, err := s.Load()
		var u *UnknownStoreError
		if !errors.As(err, &u) || err.Error() != s.Path+" is not a token store this factory knows" {
			t.Errorf("%s: %v", bad, err)
		}
		if _, _, err := s.Live("x"); err == nil {
			t.Errorf("%s: Live gave no error", bad)
		}
	}
	os.WriteFile(s.Path, []byte(`{not json`), 0o600)
	if _, err := s.Load(); err == nil {
		t.Error("bad JSON loaded")
	}
	// version 1.0 (and true) is 1 in Python
	os.WriteFile(s.Path, []byte(`{"version": 1.0, "tokens": ["junk", {"name": 5}, {"name": "ok", "projects": "sandbox"}], "extra": 1}`), 0o600)
	f, err = s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Records()) != 3 {
		t.Fatalf("records %d", len(f.Records()))
	}
	r, ok, err := s.Live("ok")
	if err != nil || !ok {
		t.Fatalf("live ok: %v %v", ok, err)
	}
	// a string for projects is iterated by character, as Python does
	if got := strings.Join(r.Projects(), "|"); got != "s|a|n|d|b|o|x" {
		t.Errorf("projects %q", got)
	}
	if got := f.Records()[1].NameString(); got != "5" {
		t.Errorf("name %q", got)
	}
	if got := f.Records()[0].NameString(); got != "None" {
		t.Errorf("name of junk %q", got)
	}
	// a change keeps the keys and records it does not know
	if err := s.Change(context.Background(), func(f *File) error { return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.Path)
	if !bytes.Contains(raw, []byte(`"extra": 1`)) || !bytes.Contains(raw, []byte(`"junk"`)) {
		t.Errorf("change dropped what it did not know:\n%s", raw)
	}
}

// The CLI's list forms, against the Python goldens of `factory tokens list`.
func TestListGoldens(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "cli", "testdata", "py", "root", "api-tokens.json"))
	if err != nil {
		t.Skip("no cli goldens:", err)
	}
	var runs struct {
		Commands []struct {
			Argv   []string
			Stdout string
		}
	}
	data, err := os.ReadFile(filepath.Join("..", "cli", "testdata", "py", "commands.json"))
	if err != nil {
		t.Skip("no cli goldens:", err)
	}
	if err := json.Unmarshal(data, &runs); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	os.MkdirAll(filepath.Dir(s.Path), 0o700)
	os.WriteFile(s.Path, src, 0o600)
	seen := 0
	for _, c := range runs.Commands {
		if len(c.Argv) < 2 || c.Argv[0] != "tokens" || c.Argv[1] != "list" {
			continue
		}
		var got string
		if len(c.Argv) == 3 && c.Argv[2] == "--json" {
			rows, err := s.Rows()
			if err != nil {
				t.Fatal(err)
			}
			got = string(jsonx.Format{Indent: -1}.Encode(rows)) + "\n"
		} else {
			lines, err := s.Lines()
			if err != nil {
				t.Fatal(err)
			}
			got = strings.Join(lines, "\n") + "\n"
		}
		if got != c.Stdout {
			t.Errorf("%v:\n%s", c.Argv, testkit.Diff(c.Stdout, got))
		}
		seen++
	}
	if seen != 2 {
		t.Errorf("%d tokens list goldens, want 2", seen)
	}
}

func TestChangesAreSerialised(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "agent-" + string(rune('a'+i))
			if _, err := s.Add(context.Background(), AddIn{Name: name, Projects: []string{"p"}, Can: "read", Known: []string{"p"}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	f, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.Records()); n != 8 {
		t.Errorf("%d records after 8 concurrent adds", n)
	}
}

func TestChangeHonoursItsContext(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Change(ctx, func(*File) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("change on a cancelled context: %v", err)
	}
}

// Foreman refuses a store others can read, or one in a directory others can
// write, and logs that once (apitokens.go storePerms).
func TestALooseStoreHoldsNoTokens(t *testing.T) {
	s := newStore(t)
	var logged []string
	s.Log = logFunc(func(format string, args ...any) { logged = append(logged, format) })
	tok := add(t, s, "example-agent", "read", "sandbox")
	if err := s.CheckPerms(); err != nil {
		t.Fatal(err)
	}
	os.Chmod(s.Path, 0o644)
	if err := s.CheckPerms(); err == nil || !strings.Contains(err.Error(), "is readable by others") {
		t.Errorf("loose file: %v", err)
	}
	if _, ok := s.Verify(tok); ok {
		t.Error("a token verified from a loose store")
	}
	s.Verify(tok)
	if len(logged) != 1 {
		t.Errorf("logged %d times, want once", len(logged))
	}
	os.Chmod(s.Path, 0o600)
	os.Chmod(filepath.Dir(s.Path), 0o777)
	if err := s.CheckPerms(); err == nil || !strings.Contains(err.Error(), "is writable by others") {
		t.Errorf("loose dir: %v", err)
	}
	os.Chmod(filepath.Dir(s.Path), 0o700)
	if name, ok := s.Verify(tok); !ok || name != "example-agent" {
		t.Errorf("verify after tightening: %q %v", name, ok)
	}
}

// A hand edit is seen by the next read: the store is re-read when its
// (inode, mtime, size) changes.
func TestAnEditIsSeenOnTheNextRead(t *testing.T) {
	s := newStore(t)
	add(t, s, "example-agent", "read", "sandbox")
	if _, ok, _ := s.Live("example-agent"); !ok {
		t.Fatal("not live")
	}
	raw, _ := os.ReadFile(s.Path)
	edited := bytes.Replace(raw, []byte(`"example-agent"`), []byte(`"renamed-agent"`), 1)
	os.WriteFile(s.Path, edited, 0o600)
	if _, ok, _ := s.Live("example-agent"); ok {
		t.Error("the old name is still live after a hand edit")
	}
	if _, ok, _ := s.Live("renamed-agent"); !ok {
		t.Error("the new name is not live after a hand edit")
	}
}

type logFunc func(format string, args ...any)

func (f logFunc) Printf(format string, args ...any) { f(format, args...) }
