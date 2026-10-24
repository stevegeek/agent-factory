package present

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The Foreman tests ran with the desk's clock in UTC; the story and the floor
// write local times.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

// The legacy fixtures are shared with package ticket: hand-written old
// boards, and tickets the dispatcher's real code paths wrote.
const (
	legacyBoard    = "../ticket/testdata/legacy/board"
	legacyContract = "../ticket/testdata/legacy/contract"
)

// load reads every ticket file in dir, oldest id first, as the board does.
func load(t *testing.T, dir string) []*ticket.Ticket {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "T*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*ticket.Ticket
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		tk, _, err := ticket.Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out = append(out, tk)
	}
	sort.SliceStable(out, func(i, j int) bool { return IDLess(out[i].ID, out[j].ID) })
	return out
}

func fixtures(t *testing.T) []*ticket.Ticket {
	t.Helper()
	ts := load(t, legacyBoard)
	if len(ts) != 12 {
		t.Fatalf("loaded %d fixture tickets", len(ts))
	}
	return ts
}

func contractTickets(t *testing.T) map[ticket.ID]*ticket.Ticket {
	t.Helper()
	ts := load(t, legacyContract)
	if len(ts) < 4 {
		t.Fatalf("loaded %d contract tickets", len(ts))
	}
	return ByID(ts)
}

func find(ts []*ticket.Ticket, id string) *ticket.Ticket { return ByID(ts)[ticket.ID(id)] }

// tk decodes a ticket written in a test as JSON.
func tk(t *testing.T, src string) *ticket.Ticket {
	t.Helper()
	x, problems, err := ticket.Decode([]byte(src))
	if err != nil || len(problems) > 0 {
		t.Fatalf("test ticket: %v %v", err, problems)
	}
	return x
}

func whos(s Story) []string {
	var out []string
	for _, st := range s.Steps {
		out = append(out, st.Who)
	}
	return out
}

func fmtSteps(s Story) string {
	var out []string
	for _, st := range s.Steps {
		out = append(out, st.Who+": "+st.What)
	}
	return strings.Join(out, "\n")
}

func note(at, detail string) ticket.Entry { return ticket.Entry{At: at, Event: "note", Detail: detail} }

// you is the maintainer reading the desk, as the first of [board] people.
var you = Viewer{By: "maintainer", Maintainer: true}

// livePath is the ticket directory of the private live-board snapshot named
// by FACTORY_LIVE_SNAPSHOT; the test skips without one.
func livePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(testkit.LiveSnapshot(t), "board", "tickets")
}
