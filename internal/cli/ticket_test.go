package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

// call is TicketCase.call: `ticket VERB --json` with the request on stdin.
func (f *fixture) call(verb string, req any, as string, wait string) (int, map[string]any, string) {
	f.t.Helper()
	argv := []string{"ticket", verb, "--json"}
	if as != "" {
		argv = append(argv, "--as", as)
	}
	if wait != "" {
		argv = append(argv, "--wait", wait)
	}
	b, _ := json.Marshal(req)
	code, out, errs := f.run(string(b), argv...)
	if code != 0 && code != 1 {
		f.t.Fatalf("%s: exit %d, stderr %q", verb, code, errs)
	}
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		f.t.Fatalf("%s: not one JSON line: %q", verb, out)
	}
	var reply map[string]any
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		f.t.Fatal(err)
	}
	if reply["ok"] != (code == 0) {
		f.t.Fatalf("%s: ok %v with exit %d", verb, reply["ok"], code)
	}
	return code, reply, errs
}

func (f *fixture) ok(verb string, req any, as string) map[string]any {
	f.t.Helper()
	code, reply, _ := f.call(verb, req, as, "")
	if code != 0 {
		f.t.Fatalf("%s %v: %v", verb, req, reply)
	}
	return reply
}

func errCode(reply map[string]any) string {
	e, _ := reply["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestTicketJSONAnswersOneLine(t *testing.T) {
	f := ticketFixture(t)
	r := f.ok("create", map[string]any{"project": "sandbox", "title": "Fix the date picker"}, "")
	tk, _ := r["ticket"].(map[string]any)
	if tk["id"] != "T0001" || tk["column"] != "inbox" {
		t.Fatalf("create: %v", r)
	}
	// An empty request is {}.
	code, out, _ := f.run("", "ticket", "projects", "--json")
	if code != 0 || !strings.HasPrefix(out, `{"ok": true, "projects": [`) {
		t.Errorf("projects: %d %q", code, out)
	}
	// format md on a write answers Markdown.
	r = f.ok("note", map[string]any{"id": "T0001", "text": "looked", "format": "md"}, "")
	if md, _ := r["markdown"].(string); !strings.Contains(md, "looked") {
		t.Errorf("note md: %v", r)
	}
}

// port: TT VerbTest.test_bad_input_is_bad_request_and_a_crash_is_not_a_refusal
// Ported here: in-process part.
func TestBadInputIsBadRequest(t *testing.T) {
	f := ticketFixture(t)
	for _, stdin := range []string{"not json", "[1]", `{"id": "` + strings.Repeat("x", 10) + `"}` + strings.Repeat(" ", 1024*1024)} {
		code, out, _ := f.run(stdin, "ticket", "get", "--json")
		var reply map[string]any
		json.Unmarshal([]byte(out), &reply)
		if code != 1 || errCode(reply) != "bad_request" {
			t.Errorf("stdin %.20q: %d %q", stdin, code, out)
		}
	}
	code, reply, _ := f.call("whoami", map[string]any{}, "nobody", "")
	if code != 1 || errCode(reply) != "unauthorized" {
		t.Errorf("unknown token: %v", reply)
	}
	code, reply, _ = f.call("create", map[string]any{"project": "sandbox", "title": "t", "format": "html"}, "", "")
	if code != 1 || errCode(reply) != "bad_request" {
		t.Errorf("bad format: %v", reply)
	}
}

// panicky is a ticket service that crashes, as the Python test's KeyError does.
type panicky struct{ err error }

func (p panicky) Do(context.Context, ticketapi.Caller, string, jsonx.Value, time.Duration) (jsonx.Object, error) {
	if p.err != nil {
		return nil, p.err
	}
	var m map[string]int
	m["boom"]++ // a nil map: a real crash
	return nil, nil
}

func (p panicky) Call(ctx context.Context, c ticketapi.Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error) {
	return p.Do(ctx, c, verb, req, wait)
}

// port: TT VerbTest.test_bad_input_is_bad_request_and_a_crash_is_not_a_refusal
// Ported here: the crash.
func TestACrashIsExitThreeNotARefusal(t *testing.T) {
	f := ticketFixture(t)
	for _, svc := range []panicky{{}, {err: errors.New("disk on fire")}} {
		var out, errb bytes.Buffer
		e := Env{Stdin: strings.NewReader(`{"id": "T0001"}`), Stdout: &out, Stderr: &errb, Environ: f.env.Vars}
		e.ticketService = func(*Env) (ticketService, error) { return svc, nil }
		code := Main([]string{"factory", "ticket", "get", "--json"}, e)
		if code != 3 || out.Len() != 0 || errb.Len() == 0 {
			t.Errorf("%v: %d %q %q", svc.err, code, out.String(), errb.String())
		}
		if svc.err == nil && !strings.Contains(errb.String(), "nil map") {
			t.Errorf("the stack should be on stderr: %q", errb.String())
		}
	}
	// A config that is missing is the factory's own failure too.
	os.Remove(f.env.Path("config.toml"))
	code, out, errs := f.run("{}", "ticket", "whoami", "--json")
	if code != 3 || out != "" || !strings.Contains(errs, "config.toml") {
		t.Errorf("no config: %d %q %q", code, out, errs)
	}
}

// port: TT VerbTest.test_busy_when_the_tick_holds_the_ticket
// Ported here: the CLI part.
// port: TF TickRaceTest.test_a_cli_change_gives_up_while_the_tick_holds_the_ticket
// Ported here: the ticket verbs' part.
func TestBusyWhenTheTickHoldsTheTicket(t *testing.T) {
	f := ticketFixture(t)
	f.ok("create", map[string]any{"project": "sandbox", "title": "t"}, "")
	f.token("example-agent", []string{"sandbox", "fac"}, "read,file,talk,work,plan")
	lockDir := f.env.Path("board", "tickets", ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lf, err := os.OpenFile(filepath.Join(lockDir, "T0001"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, reply, _ := f.call("note", map[string]any{"id": "T0001", "text": "x"}, "example-agent", "0.3")
	if code != 1 || errCode(reply) != "busy" {
		t.Errorf("note while held: %v", reply)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("--wait 0.3 waited %v", d)
	}
	syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	f.ok("note", map[string]any{"id": "T0001", "text": "x"}, "example-agent")
}

// port: TT ModeTest.test_ls_shows_board_tickets_with_their_assignee
func TestLsShowsBoardTicketsWithTheirAssignee(t *testing.T) {
	f := ticketFixture(t)
	f.ok("create", map[string]any{"project": "sandbox", "title": "Fix the date picker"}, "")
	f.token("example-agent", []string{"sandbox", "fac"}, "read,file,talk,work,plan")
	f.ok("claim", map[string]any{"id": "T0001"}, "example-agent")
	code, out, _ := f.run("", "ls")
	if code != 0 || !regexp.MustCompile(`T0001\s+doing\s+sandbox\s+item\s+example-agent`).MatchString(out) {
		t.Errorf("ls: %d %q", code, out)
	}
}

func TestTicketHumanForms(t *testing.T) {
	f := ticketFixture(t)
	f.token("example-agent", []string{"sandbox", "fac"}, "read,file,talk,work,plan")
	f.ok("create", map[string]any{"project": "sandbox", "title": "Fix the date picker"}, "")
	f.ok("create", map[string]any{"project": "other", "title": "Another"}, "")
	f.ok("claim", map[string]any{"id": "T0001"}, "example-agent")
	f.ok("ask", map[string]any{"id": "T0001", "question": "Which?"}, "example-agent")

	code, out, _ := f.run("", "ticket", "list")
	want := "T0002  inbox     other          item                    Another\n" +
		"T0001  waiting   sandbox        item   example-agent    Fix the date picker  (1 open question)\n"
	if code != 0 || out != want {
		t.Errorf("list: %d\n%q\nwant\n%q", code, out, want)
	}
	code, out, _ = f.run("", "ticket", "list", "--project", "other")
	if code != 0 || !strings.HasPrefix(out, "T0002 ") || strings.Contains(out, "T0001") {
		t.Errorf("list --project: %d %q", code, out)
	}
	code, out, _ = f.run("", "ticket", "get", "t0001")
	if code != 0 || !strings.Contains(out, "Fix the date picker") || !strings.Contains(out, "Which?") {
		t.Errorf("get: %d %q", code, out)
	}
	code, out, _ = f.run("", "ticket", "answer", "t0001", "1", "this", "one")
	if code != 0 || out != "T0001: answered; it is in Doing\n" {
		t.Errorf("answer: %d %q", code, out)
	}
	code, out, errs := f.run("", "ticket", "answer", "T0001", "1", "again")
	if code != 1 || out != "" || !strings.Contains(errs, "answered already") {
		t.Errorf("answer twice: %d %q %q", code, out, errs)
	}
	code, out, _ = f.run("", "ticket", "reindex")
	if code != 0 || out != "2\n" {
		t.Errorf("reindex: %d %q", code, out)
	}
	usage := "factory ticket get T0042 | list [--project P] | answer T0042 N TEXT | reindex; or any verb with --json and the request on stdin\n"
	for _, argv := range [][]string{{"ticket", "get"}, {"ticket", "list", "x"}, {"ticket", "answer", "T1", "one", "x"}, {"ticket", "create"}} {
		code, out, errs := f.run("", argv...)
		if code != 1 || out != "" || errs != usage {
			t.Errorf("%q: %d %q %q", argv, code, out, errs)
		}
	}
	// The human forms act as a token too.
	code, out, _ = f.run("", "ticket", "list", "--as", "example-agent")
	if code != 0 || strings.Count(out, "\n") != 1 {
		t.Errorf("list --as: %d %q", code, out)
	}
	code, _, errs = f.run("", "ticket", "reindex", "--as", "example-agent")
	if code != 1 || !strings.Contains(errs, "only a person rebuilds the index") {
		t.Errorf("reindex --as: %d %q", code, errs)
	}
}
