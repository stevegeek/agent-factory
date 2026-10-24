package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// boardTicket files a board ticket as `ticket create` does (file_ticket).
func (f *fixture) boardTicket(t *testing.T, labels ...string) *ticket.Ticket {
	t.Helper()
	ls := make([]jsonx.Value, len(labels))
	for i, l := range labels {
		ls[i] = jsonx.String(l)
	}
	tk, err := f.b.FileTicket(t.Context(), ticket.NewIn{
		Project: "sandbox", Title: "Fix the date picker", Door: "human", Column: "inbox", By: "maintainer", Mode: model.ModeBoard,
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("item")}, {Key: "labels", Value: jsonx.ArrayOf(ls...)}, {Key: "links", Value: jsonx.ArrayOf()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func keys(t *testing.T, ix interface{ IDs() []string }) string {
	ids := ix.IDs()
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// port: TT IndexTest.test_built_from_nothing_beside_the_tickets_never_among_them
func TestIndexBuiltFromNothingBesideTheTicketsNeverAmongThem(t *testing.T) {
	f := newFixture(t)
	a := f.boardTicket(t, "ui")
	ix, err := f.b.RefreshIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if keys(t, ix) != string(a.ID) {
		t.Fatalf("index %v", ix.IDs())
	}
	e, _ := ix.Get(string(a.ID))
	if got := string(jsonx.EncodePython(e.Get("labels"), -1)) + " " + e.Str("mode") + " " + e.Str("kind"); got != `["ui"] board item` {
		t.Errorf("entry %s", got)
	}
	if filepath.Dir(f.p.Index) != filepath.Dir(f.p.Tickets) {
		t.Errorf("index.json at %s", f.p.Index)
	}
	names, _ := os.ReadDir(f.p.Tickets)
	for _, n := range names {
		if strings.HasPrefix(n.Name(), "index") {
			t.Errorf("%s among the tickets", n.Name())
		}
	}
	data, err := os.ReadFile(f.p.Index)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), `{"version": 2, "tickets": {"T0001": {"project": "sandbox", "mode": "board", `) {
		t.Errorf("index.json is not json.dump's: %.120s", data)
	}
}

// port: TT IndexTest.test_it_picks_up_a_hand_edit_and_a_deleted_file
func TestIndexPicksUpAHandEditAndADeletedFile(t *testing.T) {
	f := newFixture(t)
	a, b := f.boardTicket(t), f.boardTicket(t)
	if _, err := f.b.RefreshIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := f.p.TicketFile(string(a.ID))
	var raw map[string]any
	json.Unmarshal([]byte(f.read(t, a.ID)), &raw)
	raw["title"] = "Edited by hand, and longer"
	data, _ := json.Marshal(raw)
	os.WriteFile(path, data, 0o644)
	os.Chtimes(path, time.Unix(0, 1), time.Unix(0, 1))
	os.Remove(f.p.TicketFile(string(b.ID)))
	ix, err := f.b.RefreshIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if keys(t, ix) != string(a.ID) {
		t.Errorf("index %v", ix.IDs())
	}
	if e, _ := ix.Get(string(a.ID)); e.Str("title") != "Edited by hand, and longer" {
		t.Errorf("title %q", e.Str("title"))
	}
}

// port: TT IndexTest.test_a_corrupt_index_or_an_unknown_version_is_rebuilt
func TestACorruptIndexOrAnUnknownVersionIsRebuilt(t *testing.T) {
	f := newFixture(t)
	a := f.boardTicket(t)
	for _, junk := range []string{"{not json", `{"version": 99, "tickets": {"T9999": {}}}`} {
		os.WriteFile(f.p.Index, []byte(junk), 0o644)
		ix, err := f.b.RefreshIndex(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if keys(t, ix) != string(a.ID) {
			t.Errorf("after %q: %v", junk, ix.IDs())
		}
		if b, _ := os.ReadFile(f.p.Index); !strings.Contains(string(b), `"T0001"`) || strings.Contains(string(b), "T9999") {
			t.Errorf("not rewritten: %s", b)
		}
	}
}

// port: TT IndexTest.test_reindex_gives_the_same_index
//
// The store's part (the verb's refusal for a token is ticketapi's).
func TestReindexGivesTheSameIndex(t *testing.T) {
	f := newFixture(t)
	f.boardTicket(t)
	f.boardTicket(t)
	before, err := f.b.RefreshIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.b.Reindex(t.Context())
	if err != nil || n != 2 {
		t.Fatalf("reindex: %d %v", n, err)
	}
	after, _ := f.b.RefreshIndex(t.Context())
	for _, id := range before.IDs() {
		x, _ := before.Get(id)
		y, _ := after.Get(id)
		xo, yo := x.Object().Clone(), y.Object().Clone()
		xo.Delete("ino")
		yo.Delete("ino")
		if a, b := string(jsonx.EncodePython(xo.Value(), -1)), string(jsonx.EncodePython(yo.Value(), -1)); a != b {
			t.Errorf("%s: %s\n%s", id, a, b)
		}
	}
	if keys(t, before) != keys(t, after) {
		t.Errorf("%v %v", before.IDs(), after.IDs())
	}
}

// The index is written only when something changed, and never by a ticket
// save: a cache of the files, not a second source of truth.
func TestIndexIsWrittenOnlyWhenSomethingChanged(t *testing.T) {
	f := newFixture(t)
	f.boardTicket(t)
	if _, err := os.Stat(f.p.Index); !os.IsNotExist(err) {
		t.Fatal("a ticket write touched the index")
	}
	f.b.RefreshIndex(t.Context())
	old := time.Unix(1000, 0)
	os.Chtimes(f.p.Index, old, old)
	f.b.RefreshIndex(t.Context())
	if st, _ := os.Stat(f.p.Index); !st.ModTime().Equal(old) {
		t.Error("an unchanged board rewrote the index")
	}
	f.boardTicket(t)
	f.b.RefreshIndex(t.Context())
	if st, _ := os.Stat(f.p.Index); st.ModTime().Equal(old) {
		t.Error("a new ticket did not rewrite the index")
	}
	noTemps(t, f.p.Board)
	// the index lock is a leaf: free once the refresh is done
	if _, err := os.Stat(f.p.IndexLock); err != nil {
		t.Errorf("no index lock file: %v", err)
	}
}

// ticket_index makes the board if it is missing.
func TestRefreshIndexOfNoBoard(t *testing.T) {
	f := newFixture(t)
	os.RemoveAll(f.p.Board)
	ix, err := f.b.RefreshIndex(t.Context())
	if err != nil || ix.Len() != 0 {
		t.Fatalf("%v %d", err, ix.Len())
	}
	if b, _ := os.ReadFile(f.p.Index); string(b) != "" {
		t.Errorf("an empty board wrote %q", b)
	}
}
