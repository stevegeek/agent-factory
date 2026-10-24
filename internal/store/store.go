// Package store writes the board and the factory's small state files:
// ticket saves, ids, the per-ticket and other locks, the index, state.json,
// watches.json and pauses.json.
//
// Every lock is flock(2) on the path Python uses, so Python and Go
// processes exclude each other, and each acquisition has its own open file
// description, so goroutines exclude each other too. Locks are taken in this
// order: tick, create, ticket (several: lowest id first), then the
// leaves (id, index), which are never held while another lock is taken.
//
// A change is made on a fresh copy and saved only at a commit point: when
// the step's function returns nil and its context is still live.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Logger is the factory log (*logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Board writes the board under Paths. Reader reads it (the same Paths);
// Log may be nil.
type Board struct {
	Reader *boardread.Reader
	Paths  paths.Paths
	Clock  clock.Clock
	Log    Logger
}

// Waits a writer gives the tick before it says busy.
const (
	// CLIWait is CLI_WAIT: a CLI change waits 60 s for the tick to let go
	// of a ticket (a gate can hold it far longer).
	CLIWait = fsx.Wait(60 * time.Second)
	// APIWait is how long an API or MCP caller waits for a ticket.
	APIWait = fsx.Wait(10 * time.Second)
	// NoWait tries once (--wait 0).
	NoWait = fsx.NoWait
	// Forever is the tick's wait, bounded only by its context.
	Forever = fsx.Forever
)

// BusyError is a ticket another writer holds past the wait: Python's
// Refused("busy", ...). It matches fsx.ErrBusy.
type BusyError struct{ ID ticket.ID }

func (e *BusyError) Error() string {
	return string(e.ID) + " is busy: the dispatcher is working on it; try again in a few minutes"
}

// Unwrap makes errors.Is(err, fsx.ErrBusy) hold.
func (e *BusyError) Unwrap() error { return fsx.ErrBusy }

// NotFoundError is a ticket that is not there, or a name that is not an
// id: Python's Refused("not_found", "no ticket <ID>"). It matches
// fs.ErrNotExist.
type NotFoundError struct{ ID string }

func (e *NotFoundError) Error() string { return "no ticket " + e.ID }

// Unwrap makes errors.Is(err, fs.ErrNotExist) hold.
func (e *NotFoundError) Unwrap() error { return fs.ErrNotExist }

func (b *Board) now() time.Time {
	if b.Clock == nil {
		return clock.System{}.Now()
	}
	return b.Clock.Now()
}

func (b *Board) logf(format string, args ...any) {
	if b.Log != nil {
		b.Log.Printf(format, args...)
	}
}

// Save writes the ticket (save_ticket): updated is stamped now, the file is
// written through "<file>.<pid>.tmp", fsync'd and renamed into place, and
// then the log lines its mutators queued are written. A save that fails
// leaves the old file and keeps the lines queued.
func (b *Board) Save(t *ticket.Ticket) error {
	id, ok := ticket.ParseID(string(t.ID))
	if !ok || id != t.ID {
		return fmt.Errorf("save %q: %w", string(t.ID), boardread.ErrBadID)
	}
	if err := os.MkdirAll(b.Paths.Tickets, 0o777); err != nil {
		return fmt.Errorf("save %s: %w", t.ID, err)
	}
	t.Updated = clock.Stamp(b.now())
	if err := fsx.WriteAtomicSync(b.Paths.TicketFile(string(t.ID)), ticket.Encode(t), 0o666); err != nil {
		return fmt.Errorf("save %s: %w", t.ID, err)
	}
	for _, line := range t.TakeLogs() {
		b.logf("%s", line)
	}
	return nil
}

// FileTicket makes and saves a new ticket (new_ticket): an id, the keys in
// Python's order, the created event and its ask.
func (b *Board) FileTicket(ctx context.Context, in ticket.NewIn) (*ticket.Ticket, error) {
	id, err := b.NextID(ctx)
	if err != nil {
		return nil, err
	}
	t := ticket.New(id, in, b.now())
	if err := b.Save(t); err != nil {
		return nil, err
	}
	return t, nil
}

// Lock takes a ticket's lock, board/tickets/.locks/<ID> (ticket_lock),
// making the directory if needed. Past the wait it is a *BusyError; when
// ctx ends, ctx's error.
func (b *Board) Lock(ctx context.Context, id ticket.ID, wait fsx.Wait) (*fsx.Locked, error) {
	clean, ok := ticket.ParseID(string(id))
	if !ok || clean != id {
		return nil, fmt.Errorf("lock %q: %w", string(id), boardread.ErrBadID)
	}
	if err := os.MkdirAll(b.Paths.Locks, 0o777); err != nil {
		return nil, fmt.Errorf("lock %s: %w", id, err)
	}
	l, err := fsx.Lock(ctx, b.Paths.TicketLock(string(id)), wait)
	if errors.Is(err, fsx.ErrBusy) {
		return nil, &BusyError{ID: id}
	}
	return l, err
}

// Changing is changing(): the CLI's way to change one ticket. The name is
// upper-cased and must be an id whose file exists (else a *NotFoundError,
// which also stops "../T0001"); the ticket is locked (waiting up to wait),
// read afresh and passed to fn, and saved only if fn returns nil and ctx is
// still live. Nothing is saved after a cancel.
func (b *Board) Changing(ctx context.Context, name string, wait fsx.Wait, fn func(*ticket.Ticket) error) error {
	id, ok := ticket.ParseID(name)
	if !ok {
		return &NotFoundError{ID: upper(name)}
	}
	if _, err := os.Stat(b.Paths.TicketFile(string(id))); err != nil {
		return &NotFoundError{ID: string(id)}
	}
	l, err := b.Lock(ctx, id, wait)
	if err != nil {
		return err
	}
	defer l.Unlock()
	t, err := b.Reader.Load(id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &NotFoundError{ID: string(id)}
		}
		return err
	}
	if err := fn(t); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("change %s not saved: %w", id, err)
	}
	return b.Save(t)
}

// upper is Python's str.upper for the not_found message.
func upper(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'a' && r <= 'z' {
			out[i] = r - 'a' + 'A'
		}
	}
	return string(out)
}

// EachLocked is each_locked(): for every ticket in ts that pred wants, it
// takes the ticket's lock (waiting as long as ctx allows), re-reads it (the
// copy in hand if the file has vanished), puts the fresh copy in ts, and if
// pred still wants it, calls fn with a clone while the lock is held. A
// person's change made since ts was read is therefore what fn sees, and a
// ticket that no longer qualifies is left alone.
//
// fn saves the ticket itself (b.Save) at its commit points. When fn returns
// nil, its clone replaces the copy in ts; when it returns an error, the
// iteration stops with that error and ts keeps the fresh, unchanged copy.
// A ticket whose file no longer reads is skipped (it is logged by the
// Reader).
func (b *Board) EachLocked(ctx context.Context, ts []*ticket.Ticket, pred func(*ticket.Ticket) bool, fn func(*ticket.Ticket) error) error {
	for i, t := range ts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !pred(t) {
			continue
		}
		if err := b.lockedStep(ctx, ts, i, pred, fn); err != nil {
			return err
		}
	}
	return nil
}

func (b *Board) lockedStep(ctx context.Context, ts []*ticket.Ticket, i int, pred func(*ticket.Ticket) bool, fn func(*ticket.Ticket) error) error {
	t := ts[i]
	l, err := b.Lock(ctx, t.ID, fsx.Forever)
	if err != nil {
		return err
	}
	defer l.Unlock()
	fresh, err := b.Reader.Load(t.ID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fresh = t
	case err != nil:
		return nil // unreadable now: skipped, as every reader skips it
	}
	ts[i] = fresh
	if !pred(fresh) {
		return nil
	}
	work := ticket.Clone(fresh)
	if err := fn(work); err != nil {
		return err
	}
	ts[i] = work
	return nil
}

// LockMany takes several tickets' locks lowest id first (held()), so two
// writers never deadlock, each waiting up to wait. If one cannot be taken,
// those already taken are released. unlock releases them all.
func (b *Board) LockMany(ctx context.Context, ids []ticket.ID, wait fsx.Wait) (unlock func(), err error) {
	var held []*fsx.Locked
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].Unlock()
		}
	}
	for _, id := range sortedIDs(ids) {
		l, err := b.Lock(ctx, id, wait)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, l)
	}
	return release, nil
}

// sortedIDs is sorted(set(ids), key=lambda x: int(x[1:])).
func sortedIDs(ids []ticket.ID) []ticket.ID {
	seen := map[ticket.ID]bool{}
	var out []ticket.ID
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Less(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// CreateLock is create_lock(): idempotency lookups and the creates they
// guard, one at a time (board/.create.lock).
func (b *Board) CreateLock(ctx context.Context, wait fsx.Wait) (*fsx.Locked, error) {
	if err := os.MkdirAll(b.Paths.Tickets, 0o777); err != nil {
		return nil, fmt.Errorf("create lock: %w", err)
	}
	return fsx.Lock(ctx, b.Paths.CreateLock, wait)
}

// TickLock is the tick's lock, ROOT/.tick.lock, held for the whole run: the
// tick tries once (NoWait) and on fsx.ErrBusy leaves .tick.again; rebuild
// waits for it.
func (b *Board) TickLock(ctx context.Context, wait fsx.Wait) (*fsx.Locked, error) {
	if err := os.MkdirAll(b.Paths.Root, 0o777); err != nil {
		return nil, fmt.Errorf("tick lock: %w", err)
	}
	return fsx.Lock(ctx, b.Paths.TickLock, wait)
}

// Again leaves ROOT/.tick.again: a wake arrived during a tick, which then
// goes round once more.
func (b *Board) Again() error { return fsx.Touch(b.Paths.TickAgain) }

// AgainSet reports a .tick.again left since it was last cleared.
func (b *Board) AgainSet() bool {
	_, err := os.Lstat(b.Paths.TickAgain)
	return err == nil
}

// ClearAgain removes .tick.again, if it is there.
func (b *Board) ClearAgain() error {
	if err := os.Remove(b.Paths.TickAgain); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear %s: %w", b.Paths.TickAgain, err)
	}
	return nil
}
