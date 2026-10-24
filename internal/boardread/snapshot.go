package boardread

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Snapshot is Foreman's view of the board: the ticket files re-read only
// when their (ino, mtime_ns, size) change, so one directory scan serves a
// whole request. A file that stops parsing (half-written, or broken by
// hand) keeps its last good read and is not read again until it changes;
// a broken file never read whole is skipped. Both are logged once per
// state. A Snapshot is safe for concurrent use.
//
// The tickets it returns are shared between callers and must not be
// changed: ticket.Clone one first.
type Snapshot struct {
	r     *Reader
	mu    sync.Mutex
	cache map[string]snapEntry // file name -> its last read
}

type snapEntry struct {
	st Stat
	t  *ticket.Ticket // nil: never read whole
}

// NewSnapshot is an empty snapshot of r's board.
func NewSnapshot(r *Reader) *Snapshot {
	return &Snapshot{r: r, cache: map[string]snapEntry{}}
}

// View is the board as one Load saw it, oldest id first.
type View struct {
	Tickets []*ticket.Ticket
	byID    map[ticket.ID]*ticket.Ticket
}

// Get is the ticket with that id.
func (v *View) Get(id ticket.ID) (*ticket.Ticket, bool) {
	if v.byID == nil {
		v.index()
	}
	t, ok := v.byID[id]
	return t, ok
}

func (v *View) index() {
	v.byID = make(map[ticket.ID]*ticket.Ticket, len(v.Tickets))
	for _, t := range v.Tickets {
		v.byID[t.ID] = t
	}
}

// Version changes whenever any ticket does (Foreman's boardVersion): the
// dispatcher stamps `updated` on every save, and the counts catch a writer
// that does not. It drives the live poll's 204 and every page's data-v.
func (v *View) Version() string {
	h := sha256.New()
	for _, t := range v.Tickets {
		agent := ""
		if a, ok := t.Agent.Get(); ok {
			agent = a.Name
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%d\x00%s\n", t.ID, t.Updated, t.Column, len(t.History), len(t.Notes), agent)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Load is the board now. Only files whose name is a ticket id and whose
// ticket carries that id count. A directory that cannot be read is an
// error, with an empty view.
func (s *Snapshot) Load() (*View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.r.Paths.Tickets)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &View{}, fmt.Errorf("board: %w", err)
	}
	seen := make(map[string]bool, len(entries))
	v := &View{}
	for _, e := range entries {
		name := e.Name()
		id, ok := TicketFileName(name)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		seen[name] = true
		st := StatOf(fi)
		c, cached := s.cache[name]
		if !cached || c.st != st {
			path := filepath.Join(s.r.Paths.Tickets, name)
			t, p, err := s.r.read(path)
			if err == nil && t.ID != id {
				err = fmt.Errorf("%w: it holds ticket %q", ErrUnreadable, string(t.ID))
				p.Err = err
			}
			s.r.report(path, p)
			if err != nil {
				// the last good read beats dropping the ticket; not re-read
				// until the file changes
				c = snapEntry{st: st, t: c.t}
			} else {
				c = snapEntry{st: st, t: t}
			}
			s.cache[name] = c
		}
		if c.t != nil {
			v.Tickets = append(v.Tickets, c.t)
		}
	}
	for name := range s.cache {
		if !seen[name] {
			delete(s.cache, name)
		}
	}
	sort.Slice(v.Tickets, func(i, j int) bool { return v.Tickets[i].ID.Less(v.Tickets[j].ID) })
	v.index()
	return v, nil
}
