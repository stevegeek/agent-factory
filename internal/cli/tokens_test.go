package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

const ticketConfig = `
[board]
people = ["alice", "bob"]

[projects.sandbox]
mode = "board"

[projects.other]
mode = "board"

[projects.fac]
path = "/nonexistent"
github = "o/fac"
`

func ticketFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.env.Vars["FACTORY_API_TOKENS"] = f.env.Path("cfg", "api-tokens.json")
	f.env.WriteConfig(t, ticketConfig)
	return f
}

// token is TicketCase.token: `tokens add` through main, the token on stdout.
func (f *fixture) token(name string, projects []string, can string) string {
	f.t.Helper()
	argv := []string{"tokens", "add", name, "--can", can}
	for _, p := range projects {
		argv = append(argv, "--project", p)
	}
	code, out, errs := f.run("", argv...)
	if code != 0 {
		f.t.Fatalf("tokens add %s: %d %q", name, code, errs)
	}
	return strings.TrimSpace(out)
}

// port: TT TokenTest.test_a_token_is_shown_once_and_only_its_hash_is_kept
func TestATokenIsShownOnceAndOnlyItsHashIsKept(t *testing.T) {
	f := ticketFixture(t)
	argv := []string{"tokens", "add", "example-agent", "--project", "sandbox", "--can", "file,talk"}
	code, out, errs := f.run("", argv...)
	token := strings.TrimSpace(out)
	if code != 0 || !regexp.MustCompile(`^fct_[A-Za-z0-9_-]{43}$`).MatchString(token) {
		t.Fatalf("tokens add: %d %q %q", code, out, errs)
	}
	if errs != "example-agent: read,file,talk on sandbox. The token is shown this once; it is not kept.\n" {
		t.Errorf("stderr %q", errs)
	}
	path := f.env.Vars["FACTORY_API_TOKENS"]
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("store mode: %v %v", st, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), token) {
		t.Error("the plain token is in the store")
	}
	var store struct {
		Tokens []struct {
			SHA      string   `json:"sha256"`
			Can      []string `json:"can"`
			Projects []string `json:"projects"`
		}
	}
	if err := json.Unmarshal(raw, &store); err != nil || len(store.Tokens) != 1 {
		t.Fatal(err, string(raw))
	}
	sum := sha256.Sum256([]byte(token))
	rec := store.Tokens[0]
	if rec.SHA != hex.EncodeToString(sum[:]) || strings.Join(rec.Can, ",") != "read,file,talk" || strings.Join(rec.Projects, ",") != "sandbox" {
		t.Errorf("record %+v", rec)
	}
	code, out, _ = f.run("", "tokens", "list", "--json")
	if code != 0 || strings.Contains(out, "sha256") || !strings.HasPrefix(out, `[{"name": "example-agent", `) {
		t.Errorf("list --json: %d %q", code, out)
	}
}

// port: TT TokenTest.test_add_refuses_reserved_names_people_duplicates_and_unknown_projects
func TestTokensAddRefuses(t *testing.T) {
	f := ticketFixture(t)
	f.token("example-agent", []string{"sandbox", "fac"}, "read,file,talk,work,plan")
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"tokens", "add", "factory", "--project", "sandbox", "--can", "read"}, "factory is reserved; pick another name"},
		{[]string{"tokens", "add", "alice", "--project", "sandbox", "--can", "read"}, "alice is a person in [board] people; pick another name"},
		{[]string{"tokens", "add", "example-agent", "--project", "sandbox", "--can", "read"}, "there is already a token example-agent; revoke it first"},
		{[]string{"tokens", "add", "new-one", "--project", "nowhere", "--can", "read"}, "unknown project nowhere; known: sandbox, other, fac"},
		{[]string{"tokens", "add", "new-one", "--project", "sandbox", "--can", "merge"}, "--can takes some of: read,file,talk,work,plan"},
		{[]string{"tokens", "add", "Bad_Name", "--project", "sandbox", "--can", "read"}, "a token name is lower-case letters, digits and dashes, starting with a letter (at most 32)"},
		{[]string{"tokens", "add", "new-one", "--can", "read"}, "name at least one --project"},
		{[]string{"tokens", "add"}, "factory tokens add NAME"},
		{[]string{"tokens", "revoke"}, "factory tokens revoke NAME"},
		{[]string{"tokens", "revoke", "nobody"}, "no token nobody"},
	} {
		code, out, errs := f.run("", c.argv...)
		if code != 1 || out != "" || errs != c.want+"\n" {
			t.Errorf("%q: %d %q %q, want %q", c.argv, code, out, errs, c.want)
		}
	}
	code, out, _ := f.run("", "tokens", "list")
	if code != 0 || strings.Count(out, "\n") != 1 {
		t.Errorf("one token is left: %q", out)
	}
	if code, _, errs := f.run("", "tokens", "bogus"); code != 2 || !strings.Contains(errs, "invalid choice: 'bogus'") {
		t.Errorf("tokens bogus: %d %q", code, errs)
	}
}

// port: TT TokenTest.test_a_revoked_or_expired_token_is_unauthorized
// Ported here: the CLI part.
func TestARevokedTokenIsUnauthorized(t *testing.T) {
	f := ticketFixture(t)
	f.token("example-agent", []string{"sandbox", "fac"}, "read,file,talk,work,plan")
	code, out, _ := f.run("{}", "ticket", "whoami", "--json", "--as", "example-agent")
	if code != 0 || !strings.HasPrefix(out, `{"ok": true, "name": "example-agent"`) {
		t.Fatalf("whoami: %d %q", code, out)
	}
	if code, out, _ := f.run("", "tokens", "revoke", "example-agent"); code != 0 || out != "revoked example-agent\n" {
		t.Fatalf("revoke: %d %q", code, out)
	}
	code, out, _ = f.run("{}", "ticket", "whoami", "--json", "--as", "example-agent")
	if code != 1 || !strings.Contains(out, `"code": "unauthorized"`) {
		t.Errorf("whoami after revoke: %d %q", code, out)
	}
}
