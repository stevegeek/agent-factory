package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// memLog records what the store logs.
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

type fixture struct {
	env *testkit.Env
	p   paths.Paths
	log *memLog
	b   *Board
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	l := &memLog{}
	return &fixture{env: env, p: p, log: l, b: &Board{
		Reader: &boardread.Reader{Paths: p, Log: l},
		Paths:  p,
		Clock:  testkit.Fixed{T: testkit.Stamp()},
		Log:    l,
	}}
}

// file files a factory ticket, as new_ticket("p", title, "b", "human", column).
func (f *fixture) file(t testing.TB, title, column string) *ticket.Ticket {
	t.Helper()
	tk, err := f.b.FileTicket(t.Context(), ticket.NewIn{Project: "p", Title: title, Body: "b", Door: "human", Column: column})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (f *fixture) read(t testing.TB, id ticket.ID) string {
	t.Helper()
	b, err := os.ReadFile(f.p.TicketFile(string(id)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) load(t testing.TB, id ticket.ID) *ticket.Ticket {
	t.Helper()
	tk, err := f.b.Reader.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func noTemps(t *testing.T, dir string) {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(names) != 0 {
		t.Errorf("temp files left: %v", names)
	}
}

// new_ticket: the id, the file, the created event's log line after the save.
func TestFileTicketSavesAndLogsAfterTheSave(t *testing.T) {
	f := newFixture(t)
	tk := f.file(t, "Fix", "ready")
	if tk.ID != "T0001" {
		t.Fatalf("id %s", tk.ID)
	}
	data := f.read(t, "T0001")
	for _, want := range []string{`"id": "T0001"`, `"created": "2026-01-02T03:04:05+00:00"`, `"updated": "2026-01-02T03:04:05+00:00"`, `"type": "created"`} {
		if !strings.Contains(data, want) {
			t.Errorf("file lacks %s:\n%s", want, data)
		}
	}
	if !strings.HasSuffix(data, "}") || strings.HasSuffix(data, "\n") {
		t.Error("json.dump writes no trailing newline")
	}
	if got := f.log.all(); len(got) != 1 || got[0] != "T0001 created door=human column=ready" {
		t.Errorf("log %q", got)
	}
	if len(tk.Logs()) != 0 {
		t.Error("the saved ticket still has queued lines")
	}
	noTemps(t, f.p.Tickets)
}

// save_ticket stamps updated; a ticket saved unchanged is written back byte
// for byte apart from that.
func TestSaveStampsUpdatedAndKeepsEverythingElse(t *testing.T) {
	f := newFixture(t)
	f.file(t, "Fix", "ready")
	before := f.read(t, "T0001")
	tk := f.load(t, "T0001")
	f.b.Clock = testkit.Fixed{T: testkit.Stamp().Add(3600e9)}
	if err := f.b.Save(tk); err != nil {
		t.Fatal(err)
	}
	after := f.read(t, "T0001")
	if want := strings.Replace(before, `"updated": "2026-01-02T03:04:05+00:00"`, `"updated": "2026-01-02T04:04:05+00:00"`, 1); after != want {
		t.Errorf("save changed more than updated:\n%s", testkit.Diff(want, after))
	}
}

// Save refuses an id it could not have read: no path is built from it.
func TestSaveRefusesABadID(t *testing.T) {
	f := newFixture(t)
	tk := f.file(t, "Fix", "ready")
	tk.ID = "../T0001"
	if err := f.b.Save(tk); err == nil {
		t.Error("saved a ticket with a bad id")
	}
}
