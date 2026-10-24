package boardread

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// memLog records what a Reader logs.
type memLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *memLog) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *memLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

type board struct {
	env *testkit.Env
	p   paths.Paths
	log *memLog
	r   *Reader
}

func newBoard(t *testing.T) *board {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	l := &memLog{}
	return &board{env: env, p: p, log: l, r: &Reader{Paths: p, Log: l}}
}

// put writes a ticket file as Python would: a new ticket, encoded.
func (b *board) put(t *testing.T, id ticket.ID, title string) []byte {
	t.Helper()
	tk := ticket.New(id, ticket.NewIn{Project: "p", Title: title, Body: "b", Door: "human", Column: "ready"}, testkit.Stamp())
	tk.Updated = "2026-01-02T03:04:05+00:00"
	data := ticket.Encode(tk)
	b.raw(t, string(id)+".json", data)
	return data
}

func (b *board) raw(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(b.p.Tickets, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func ids(ts []*ticket.Ticket) []string {
	var out []string
	for _, t := range ts {
		out = append(out, string(t.ID))
	}
	return out
}

// A corrupt ticket file is skipped and logged, never fatal.
// Everything else is read in Python's sorted(glob("T*.json")) order.
func TestListSkipsWhatCannotBeReadAndLogsItOnce(t *testing.T) {
	b := newBoard(t)
	b.put(t, "T0002", "two")
	b.put(t, "T0001", "one")
	b.raw(t, "T0003.json", []byte(`{"id": "T0003", "trunc`))
	b.raw(t, "T0004.json", []byte(`[1, 2]`))
	b.raw(t, "T0006.json", []byte("{\"id\": \"T0006\", \"title\": \"\xff\"}"))
	if err := os.Symlink(filepath.Join(b.p.Tickets, "T0001.json"), filepath.Join(b.p.Tickets, "T0005.json")); err != nil {
		t.Fatal(err)
	}
	b.raw(t, "T0001.json.123.tmp", []byte("{"))
	b.raw(t, ".last-id", []byte("9"))
	b.raw(t, "notes.txt", []byte("x"))

	ts, problems := b.r.List()
	if got := strings.Join(ids(ts), ","); got != "T0001,T0002" {
		t.Errorf("tickets %s", got)
	}
	var skipped []string
	for _, p := range problems {
		if p.Err == nil {
			t.Errorf("unexpected non-fatal problem %v", p)
			continue
		}
		skipped = append(skipped, filepath.Base(p.File))
	}
	if got := strings.Join(skipped, ","); got != "T0003.json,T0004.json,T0005.json,T0006.json" {
		t.Errorf("skipped %s", got)
	}
	if n := len(b.log.all()); n != 4 {
		t.Errorf("logged %d lines, want 4: %q", n, b.log.all())
	}
	for _, line := range b.log.all() {
		if !strings.Contains(line, "cannot be read") {
			t.Errorf("log line %q", line)
		}
	}
	// the same files in the same state are not logged again
	b.r.List()
	if n := len(b.log.all()); n != 4 {
		t.Errorf("a second List logged again: %q", b.log.all())
	}
	// a changed file is
	b.raw(t, "T0003.json", []byte(`{"id": "T0003", "still trunc`))
	b.r.List()
	if n := len(b.log.all()); n != 5 {
		t.Errorf("a changed broken file should be logged once more: %q", b.log.all())
	}
}

func TestListOfAMissingBoardIsEmpty(t *testing.T) {
	b := newBoard(t)
	if err := os.RemoveAll(b.p.Board); err != nil {
		t.Fatal(err)
	}
	ts, problems := b.r.List()
	if len(ts) != 0 || len(problems) != 0 {
		t.Errorf("%v %v", ts, problems)
	}
}

// load_ticket upper-cases the id; anything that is not an id is refused
// before a path is built from it.
func TestLoad(t *testing.T) {
	b := newBoard(t)
	data := b.put(t, "T0001", "one")
	tk, err := b.r.Load("t0001")
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID != "T0001" || tk.Title != "one" || string(ticket.Encode(tk)) != string(data) {
		t.Errorf("loaded %s %q", tk.ID, tk.Title)
	}
	if _, err := b.r.Load("T0002"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	for _, bad := range []ticket.ID{"../T0001", "T001", "T1234567", "T0001.json", "", "X0001"} {
		if _, err := b.r.Load(bad); !errors.Is(err, ErrBadID) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	b.raw(t, "T0003.json", []byte(`{"id": `))
	if _, err := b.r.Load("T0003"); !errors.Is(err, ErrUnreadable) {
		t.Errorf("corrupt: %v", err)
	}
	if err := os.Symlink(filepath.Join(b.p.Tickets, "T0001.json"), filepath.Join(b.p.Tickets, "T0004.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.r.Load("T0004"); !errors.Is(err, ErrUnreadable) {
		t.Errorf("a link is not followed: %v", err)
	}
}

// Foreman's TestAgentTypedFieldKeepsTicket: an agent-written field of an
// unexpected type never hides the ticket; it reads as empty and is reported.
func TestAWrongTypedFieldKeepsTheTicket(t *testing.T) {
	b := newBoard(t)
	data, err := os.ReadFile(filepath.Join("..", "ticket", "testdata", "legacy", "board", "T0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(data), `"runtime_behaviour": "yes"`, `"runtime_behaviour": false`, -1)
	s = strings.Replace(s, `"title": "Fix the widget finder"`, `"title": 7`, 1)
	b.raw(t, "T0001.json", []byte(s))
	ts, problems := b.r.List()
	if len(ts) != 1 || ts[0].ID != "T0001" || !ts[0].Gate.Present() || !ts[0].PR.Present() {
		t.Fatalf("the ticket should still load: %v", ids(ts))
	}
	if ts[0].Title != "" {
		t.Errorf("title %q", ts[0].Title)
	}
	if len(problems) != 1 || problems[0].Err != nil || len(problems[0].Fields) == 0 || problems[0].Fields[0].Path != "title" {
		t.Fatalf("problems %v", problems)
	}
	if !strings.Contains(problems[0].String(), "title") {
		t.Errorf("problem %q", problems[0])
	}
	if got := string(ticket.Encode(ts[0])); got != s {
		t.Errorf("the wrong-typed field must survive a save:\n%s", testkit.Diff(s, got))
	}
	if n := len(b.log.all()); n != 1 {
		t.Errorf("logged %q", b.log.all())
	}
}

// The index's file name rule, re.fullmatch(r"(T\d{4,6})\.json"), against
// Python's own answers over the regex corpus.
func TestTicketFileNameAgreesWithPython(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "textx", "testdata", "py", "regex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Patterns []struct {
			Name  string
			Cases []struct {
				In  string
				Out json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range g.Patterns {
		if p.Name != "index_name" {
			continue
		}
		for _, c := range p.Cases {
			var out []string
			if err := json.Unmarshal(c.Out, &out); err != nil {
				t.Fatal(err)
			}
			id, ok := TicketFileName(c.In)
			if ok != (out != nil) || (ok && string(id) != out[1]) {
				t.Errorf("%q: got %q %v, Python %v", c.In, id, ok, out)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("no index_name cases in the golden")
	}
}

// Python reads a file only if json.load can: not UTF-8 is unreadable, an
// escaped lone surrogate is fine.
func TestUTF8(t *testing.T) {
	b := newBoard(t)
	b.raw(t, "T0001.json", []byte(`{"id": "T0001", "title": "\udc80 café"}`))
	tk, err := b.r.Load("T0001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(tk.Title, " café") {
		t.Errorf("%q", tk.Title)
	}
}

func TestProblemString(t *testing.T) {
	p := Problem{File: "/x/T0001.json", ID: "T0001", Err: ErrUnreadable}
	if !strings.Contains(p.String(), "T0001.json") {
		t.Errorf("%q", p.String())
	}
}
