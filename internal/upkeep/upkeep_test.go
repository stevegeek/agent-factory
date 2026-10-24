package upkeep

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/doors"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// memLog records what upkeep logs.
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
	env   *testkit.Env
	p     paths.Paths
	log   *memLog
	board *store.Board
	doors doors.Service
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	l := &memLog{}
	b := &store.Board{Paths: p, Clock: testkit.Fixed{T: testkit.Stamp()}, Log: l}
	setReader(b, p, l)
	return &fixture{env: env, p: p, log: l, board: b, doors: doors.Service{Board: b, Log: l}}
}

// setReader gives the board its reader without naming boardread: upkeep's
// tests may import only upkeep's row of the table, which reaches the
// board through store.
func setReader(b *store.Board, p paths.Paths, l *memLog) {
	rv := reflect.ValueOf(&b.Reader).Elem()
	rv.Set(reflect.New(rv.Type().Elem()))
	b.Reader.Paths = p
	b.Reader.Log = l
}

func parse(t testing.TB, doc string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (f *fixture) tickets(t testing.TB) []*ticket.Ticket {
	t.Helper()
	ts, problems := f.board.Reader.List()
	if len(problems) > 0 {
		t.Fatalf("board problems: %v", problems)
	}
	return ts
}
