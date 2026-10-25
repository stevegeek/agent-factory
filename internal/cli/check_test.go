package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
)

const checkConfig = `
[board]
people = ["alice", "bob"]

[projects.sandbox]
mode = "board"

[projects.fac]
path = "/nonexistent"
github = "o/fac"
install = "true"
test = "true"
lint = "true"
`

// port: TT ModeTest.test_check_finds_bad_modes_people_and_tokens_for_gone_projects
// Ported here: the CLI part.
func TestCheckPrintsWarningsThenProblemsAndExitsOneOnProblems(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, checkConfig)
	if code, out, errs := f.run("", "check"); code != 0 || out != "" || errs != "" {
		t.Fatalf("clean check: %d %q %q", code, out, errs)
	}
	f.env.WriteConfig(t, `
[board]
people = ["alice", "factory"]

[projects.x]
mode = "kanban"

[projects.y]
mode = "board"
github = "o/y"
`)
	f.env.WriteFile(t, "api-tokens.json", `{"version": 1, "tokens": [{"name": "example-agent", "projects": ["other"]}]}`)
	code, out, _ := f.run("", "check")
	want := "warning: [projects.y] is a board project; github are not used\n" +
		"[projects.x] mode = 'kanban': it is one of factory, board\n" +
		"[board] people: 'factory' is not a name a person can have here\n" +
		"token example-agent names other, not in config\n"
	if code != 1 || out != want {
		t.Errorf("check: %d\n%q\nwant\n%q", code, out, want)
	}
}

func TestCheckReportsATokenStoreItCannotRead(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, checkConfig)
	f.env.WriteFile(t, "api-tokens.json", `{"version": 2, "tokens": []}`)
	code, out, _ := f.run("", "check")
	if code != 1 || !strings.Contains(out, "is not a token store this factory knows") {
		t.Errorf("check: %d %q", code, out)
	}
}

// `factory check` runs the host checks: [build] host = "self" on a
// host whose factory-remote does not build is a problem here.
func TestCheckRunsThisHostsChecks(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, checkConfig+"\n[build]\nhost = \"self\"\n")
	code, out, _ := f.run("", "check")
	if code != 1 || !strings.Contains(out, `[build] host = "self" needs build = true in [remote.`+config.ThisHost()+`]`) {
		t.Errorf("check: %d %q", code, out)
	}
}

func TestCheckBoardListsTicketsThatDoNotReadCleanly(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, checkConfig)
	f.env.WriteFile(t, "board/tickets/T0001.json", `{"id": "T0001", "project": "fac", "title": "ok", "column": "ready"}`)
	f.env.WriteFile(t, "board/tickets/T0002.json", "{broken")
	f.env.WriteFile(t, "board/tickets/T0003.json", `{"id": "T0003", "project": "fac", "title": 7, "column": "ready"}`)
	// Without --board the board's files are not listed.
	if code, out, _ := f.run("", "check"); code != 0 || out != "" {
		t.Errorf("check: %d %q", code, out)
	}
	code, out, _ := f.run("", "check", "--board")
	if code != 1 || strings.Contains(out, "T0001") || !strings.Contains(out, "board: T0002.json: ") || !strings.Contains(out, "board: T0003.json: ") {
		t.Errorf("check --board: %d %q", code, out)
	}
}

func TestCheckAsksListsAskColumnsWithNoStoredAsk(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, checkConfig)
	f.env.WriteFile(t, "board/tickets/T0001.json", `{"id": "T0001", "project": "fac", "title": "a", "column": "needs_human", "ask": {"kind": "question", "why": "w", "since": "2026-10-17T08:00:00+00:00"}}`)
	f.env.WriteFile(t, "board/tickets/T0002.json", `{"id": "T0002", "project": "fac", "title": "b", "column": "pr_open"}`)
	f.env.WriteFile(t, "board/tickets/T0003.json", `{"id": "T0003", "project": "fac", "title": "c", "column": "ready"}`)
	code, out, _ := f.run("", "check", "--asks")
	if code != 1 || out != "ask: T0002 is in pr_open with no stored ask\n" {
		t.Errorf("check --asks: %d %q", code, out)
	}
	os.Remove(f.env.Path("board/tickets/T0002.json"))
	if code, out, _ := f.run("", "check", "--asks", "--board"); code != 0 || out != "" {
		t.Errorf("clean check --asks --board: %d %q", code, out)
	}
}

// `factory check` warns about any intent older than one hour.
func TestCheckWarnsAboutOldIntents(t *testing.T) {
	f := newFixture(t) // FACTORY_TEST_NOW is 2026-10-18T10:00:00+00:00
	f.env.WriteConfig(t, checkConfig)
	f.env.WriteFile(t, "board/tickets/T0001.json", `{"id": "T0001", "project": "fac", "title": "a", "column": "ready",
		"pr": {"number": 3, "hold_requested": "2026-10-18T08:30:00+00:00", "merge_requested": "2026-10-18T09:30:00+00:00"}}`)
	f.env.WriteFile(t, "board/tickets/T0002.json", `{"id": "T0002", "project": "fac", "title": "b", "column": "pr_open",
		"pr": {"number": 4, "open_requested": "yesterday"}}`)
	code, out, _ := f.run("", "check")
	want := "warning: T0001 asked to hold its PR at 2026-10-18T08:30:00+00:00, over an hour ago, and the tick has not done it\n" +
		"warning: T0002 asked to open its PR at yesterday, over an hour ago, and the tick has not done it\n"
	if code != 0 || out != want {
		t.Errorf("check: %d\n%q\nwant\n%q", code, out, want)
	}
}

// A .git that is not a directory is refused; check says so first.
func TestCheckRefusesAGitFile(t *testing.T) {
	f := newFixture(t)
	repo := t.TempDir()
	if err := os.WriteFile(repo+"/.git", []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.env.WriteConfig(t, strings.Replace(checkConfig, `path = "/nonexistent"`, `path = "`+repo+`"`, 1))
	code, out, _ := f.run("", "check")
	if code != 1 || out != "[projects.fac] "+repo+"/.git is not a directory: the factory refuses a repository whose .git is a file or a link\n" {
		t.Errorf("check: %d %q", code, out)
	}
}

// A mount written as an absolute path that holds or reaches the trusted
// build caches (the root, a directory above it, the caches themselves) is
// what the gate and a helper refuse for every job, whatever its kind:
// check says so before an upgrade does, and of a [gate] mount that reaches
// the kept early checks, which every gate refuses too. [gate] is this host's, a problem;
// a helper's root is not known here, so [remote] is a warning, checked
// against this root and {factory_home}/factory.
func TestCheckFindsAbsoluteMountsOfTheTrustedCaches(t *testing.T) {
	f := newFixture(t)
	root := f.env.Root
	f.env.WriteConfig(t, checkConfig+`
[host]
factory_home = "/home/example-factory"

[gate]
mounts = ["`+filepath.Dir(root)+`:/x:ro", "/srv/elsewhere:/y", "`+root+`/gate-early/T0001.json:/e"]

[remote]
mounts = ["/home/example-factory/shared/..:/x", "/srv/data:/d"]

[remote.helper1]
mounts = ["`+root+`/cache:/c"]
`)
	code, out, _ := f.run("", "check")
	for _, want := range []string{
		`[gate] mounts: "` + filepath.Dir(root) + `:/x:ro" reaches ` + root + `/cache/go`,
		`[gate] mounts: "` + root + `/gate-early/T0001.json:/e" reaches ` + root + `/gate-early, the gate's kept early checks`,
		`warning: [remote] mounts: "/home/example-factory/shared/..:/x" reaches /home/example-factory/factory/cache/go`,
		`warning: [remote.helper1] mounts: "` + root + `/cache:/c" reaches ` + root + `/cache/go`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check lacks %q:\n%s", want, out)
		}
	}
	if code != 1 || strings.Contains(out, "/srv/") {
		t.Errorf("check: %d\n%s", code, out)
	}
}
