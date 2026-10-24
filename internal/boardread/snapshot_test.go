package boardread

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Foreman's TestBoardKeepsLastGoodRead: a half-written file keeps the
// last good read, and is not read again until it changes.
func TestSnapshotKeepsTheLastGoodRead(t *testing.T) {
	b := newBoard(t)
	b.put(t, "T0001", "one")
	s := NewSnapshot(b.r)
	v, err := s.Load()
	if err != nil || len(v.Tickets) != 1 {
		t.Fatalf("first read: %v %d", err, len(v.Tickets))
	}
	b.raw(t, "T0001.json", []byte(`{"id": "T0001", "trunc`))
	v, _ = s.Load()
	if len(v.Tickets) != 1 || v.Tickets[0].Title != "one" {
		t.Fatalf("a half-written file should keep the last good read: %v", ids(v.Tickets))
	}
	logged := len(b.log.all())
	if logged != 1 {
		t.Errorf("logged %q", b.log.all())
	}
	s.Load()
	if len(b.log.all()) != logged {
		t.Errorf("the same broken file was read and logged again: %q", b.log.all())
	}
	// a broken file never seen whole is skipped
	b.raw(t, "T0002.json", []byte(`{`))
	if v, _ = s.Load(); len(v.Tickets) != 1 {
		t.Errorf("%v", ids(v.Tickets))
	}
	// once it reads, it is the new ticket
	b.put(t, "T0001", "one, mended")
	if v, _ = s.Load(); v.Tickets[0].Title != "one, mended" {
		t.Errorf("%q", v.Tickets[0].Title)
	}
}

// The cache is keyed by (ino, mtime_ns, size): a rename over the file with
// the same size and mtime is still seen (its inode changed).
func TestSnapshotSeesAReplacedFileOfTheSameSizeAndTime(t *testing.T) {
	b := newBoard(t)
	b.put(t, "T0001", "aaa")
	path := filepath.Join(b.p.Tickets, "T0001.json")
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(path, when, when)
	s := NewSnapshot(b.r)
	if v, _ := s.Load(); v.Tickets[0].Title != "aaa" {
		t.Fatal(v.Tickets[0].Title)
	}
	data, _ := os.ReadFile(path)
	tmp := path + ".tmp"
	os.WriteFile(tmp, []byte(strings.Replace(string(data), `"aaa"`, `"bbb"`, 1)), 0o644)
	os.Chtimes(tmp, when, when)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Load(); v.Tickets[0].Title != "bbb" {
		t.Errorf("a replaced file was not re-read: %q", v.Tickets[0].Title)
	}
}

func TestSnapshotOrderLookupDeletionAndVersion(t *testing.T) {
	b := newBoard(t)
	for _, id := range []ticket.ID{"T10000", "T0002", "T0001"} {
		b.put(t, id, string(id))
	}
	b.raw(t, "T12.json", []byte(`{"id": "T12"}`))
	b.raw(t, "T0005.json", []byte(`{"id": "T0006"}`)) // the id inside is not the file's
	s := NewSnapshot(b.r)
	v, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(v.Tickets), ","); got != "T0001,T0002,T10000" {
		t.Errorf("order %s", got)
	}
	if tk, ok := v.Get("T0002"); !ok || tk.Title != "T0002" {
		t.Error("Get")
	}
	if _, ok := v.Get("T0005"); ok {
		t.Error("a ticket whose id is not its file's")
	}
	ver := v.Version()
	if len(ver) != 16 {
		t.Errorf("version %q", ver)
	}
	if v2, _ := s.Load(); v2.Version() != ver {
		t.Error("an unchanged board changed version")
	}
	os.Remove(filepath.Join(b.p.Tickets, "T0002.json"))
	v3, _ := s.Load()
	if _, ok := v3.Get("T0002"); ok || len(v3.Tickets) != 2 || v3.Version() == ver {
		t.Errorf("a deleted ticket stayed: %v", ids(v3.Tickets))
	}
	if len(s.cache) != 3 { // T0001, T10000 and the broken T0005, remembered as never read whole
		t.Errorf("the cache kept %d entries", len(s.cache))
	}
}

// boardVersion is Foreman's: sha256 over id, updated, column, the history
// and note counts and the agent's name, first 16 hex.
func TestVersionIsForemansFormula(t *testing.T) {
	tk := ticket.New("T0001", ticket.NewIn{Project: "p", Title: "x", Door: "human", Column: "ready"}, time.Unix(0, 0).UTC())
	tk.Updated = "2026-01-02T03:04:05+00:00"
	v := &View{Tickets: []*ticket.Ticket{tk}}
	// hashlib.sha256(b"T0001\x002026-01-02T03:04:05+00:00\x00ready\x001\x000\x00\n")
	if got := v.Version(); got != "3a8b7aad7fcef9ba" {
		t.Errorf("version %s", got)
	}
}

func TestSnapshotIsSafeForConcurrentLoads(t *testing.T) {
	b := newBoard(t)
	b.put(t, "T0001", "one")
	s := NewSnapshot(b.r)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if v, err := s.Load(); err != nil || len(v.Tickets) != 1 {
					t.Errorf("%v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
