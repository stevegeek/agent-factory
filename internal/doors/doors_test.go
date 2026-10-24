package doors

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// memLog records what the doors log.
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

func (l *memLog) has(sub string) bool {
	for _, line := range l.all() {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// fakeIssues answers OpenIssues from a table and records the repos asked.
type fakeIssues struct {
	mu     sync.Mutex
	issues map[string][]github.Issue
	errs   map[string]error
	asked  []string
}

func (f *fakeIssues) OpenIssues(_ context.Context, repo string) ([]github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, repo)
	if err := f.errs[repo]; err != nil {
		return nil, err
	}
	return f.issues[repo], nil
}

// fakeReport is a memory reporter that counts its calls.
type fakeReport struct {
	text   string
	calls  int
	closed map[string]bool
}

func (f *fakeReport) Report(_ context.Context, _ *config.Config, closed map[string]bool) (string, bool) {
	f.calls++
	f.closed = closed
	return f.text, f.text != ""
}

// fakeVersions counts version checks.
type fakeVersions struct{ calls int }

func (f *fakeVersions) Versions(context.Context, *config.Config) error {
	f.calls++
	return nil
}

type fixture struct {
	env      *testkit.Env
	p        paths.Paths
	log      *memLog
	clock    *testkit.Step
	issues   *fakeIssues
	report   *fakeReport
	versions *fakeVersions
	s        Service
}

// newFixture is a scratch root whose clock stands still unless a test
// moves it on (Advance).
func newFixture(t testing.TB) *fixture {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	l := &memLog{}
	c := testkit.NewStep(testkit.Stamp(), 0)
	f := &fixture{env: env, p: p, log: l, clock: c, issues: &fakeIssues{}, report: &fakeReport{}, versions: &fakeVersions{}}
	f.s = Service{
		Board: &store.Board{
			Reader: &boardread.Reader{Paths: p, Log: l},
			Paths:  p,
			Clock:  c,
			Log:    l,
		},
		Issues:   f.issues,
		Memory:   f.report,
		Versions: f.versions,
		Log:      l,
	}
	return f
}

func parse(t testing.TB, doc string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// tickets lists the board, failing on any file that does not read.
func (f *fixture) tickets(t testing.TB) []*ticket.Ticket {
	t.Helper()
	ts, problems := f.s.Board.Reader.List()
	if len(problems) > 0 {
		t.Fatalf("board problems: %v", problems)
	}
	return ts
}

func (f *fixture) raw(t testing.TB, id ticket.ID) string {
	t.Helper()
	b, err := os.ReadFile(f.p.TicketFile(string(id)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) advance(d time.Duration) { f.clock.Advance(d) }

// keyOrder is the order of a ticket file's top-level keys.
func keyOrder(t testing.TB, raw string) []string {
	t.Helper()
	var keys []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "  \"") && !strings.HasPrefix(line, "   ") {
			k, _, _ := strings.Cut(strings.TrimPrefix(line, "  \""), "\"")
			keys = append(keys, k)
		}
	}
	return keys
}

func tail(keys []string, n int) []string {
	if len(keys) < n {
		return keys
	}
	return keys[len(keys)-n:]
}
