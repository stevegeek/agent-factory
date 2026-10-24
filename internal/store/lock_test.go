package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func note(text string) func(*ticket.Ticket) error {
	return func(t *ticket.Ticket) error {
		ticket.AddNote(t, text, "", nil, testkit.Stamp())
		return nil
	}
}

// port: TF TickRaceTest.test_a_cli_change_gives_up_while_the_tick_holds_the_ticket
func TestACLIChangeGivesUpWhileTheTickHoldsTheTicket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.file(t, "Fix", "gate")
		held, err := f.b.Lock(t.Context(), "T0001", fsx.Forever) // the tick
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		err = f.b.Changing(t.Context(), "T0001", CLIWait, note("x"))
		var busy *BusyError
		if !errors.As(err, &busy) || !errors.Is(err, fsx.ErrBusy) || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("want busy, got %v", err)
		}
		if err.Error() != "T0001 is busy: the dispatcher is working on it; try again in a few minutes" {
			t.Errorf("message %q", err)
		}
		if waited := time.Since(start); waited < time.Duration(CLIWait) {
			t.Errorf("gave up after %v", waited)
		}
		if strings.Contains(f.read(t, "T0001"), `"notes"`) {
			t.Error("the change was saved")
		}
		held.Unlock()
		if err := f.b.Changing(t.Context(), "T0001", CLIWait, note("x")); err != nil {
			t.Errorf("once the tick let go: %v", err)
		}
	})
	f := newFixture(t)
	f.file(t, "Fix", "gate")
	for _, bad := range []string{"../T0001", "T001", "T0002", "", "T0001.json"} {
		err := f.b.Changing(t.Context(), bad, CLIWait, note("x"))
		var nf *NotFoundError
		if !errors.As(err, &nf) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := f.b.Changing(t.Context(), "../T0001", CLIWait, note("x")); err.Error() != "no ticket ../T0001" {
		t.Errorf("message %q", err)
	}
	// load_ticket upper-cases
	if err := f.b.Changing(t.Context(), "t0001", NoWait, note("lower")); err != nil {
		t.Errorf("t0001: %v", err)
	}
}

// changing(): saved only if the block ends normally, and (§5.2) never after
// its context is cancelled.
func TestChangingSavesOnlyWhatFinished(t *testing.T) {
	f := newFixture(t)
	f.file(t, "Fix", "ready")
	before := f.read(t, "T0001")

	boom := errors.New("refused")
	err := f.b.Changing(t.Context(), "T0001", NoWait, func(tk *ticket.Ticket) error {
		ticket.AddNote(tk, "half done", "", nil, testkit.Stamp())
		return boom
	})
	if !errors.Is(err, boom) || f.read(t, "T0001") != before {
		t.Errorf("an error saved: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	err = f.b.Changing(ctx, "T0001", NoWait, func(tk *ticket.Ticket) error {
		ticket.AddNote(tk, "cut short", "", nil, testkit.Stamp())
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || f.read(t, "T0001") != before {
		t.Errorf("a cancelled change saved: %v", err)
	}
	if err := f.b.Changing(ctx, "T0001", NoWait, note("late")); !errors.Is(err, context.Canceled) {
		t.Errorf("a dead context: %v", err)
	}
	if n := len(f.log.all()); n != 1 {
		t.Errorf("unsaved changes logged: %q", f.log.all())
	}

	if err := f.b.Changing(t.Context(), "T0001", NoWait, note("done")); err != nil {
		t.Fatal(err)
	}
	tk := f.load(t, "T0001")
	if len(tk.Notes) != 1 || tk.Notes[0].Text != "done" {
		t.Errorf("notes %+v", tk.Notes)
	}
	if got := f.log.all(); got[len(got)-1] != "T0001 note done" {
		t.Errorf("log %q", got)
	}
	// the lock is free again after each of them
	l, err := fsx.TryLock(f.p.TicketLock("T0001"))
	if err != nil {
		t.Fatal(err)
	}
	l.Unlock()
}

// port: TF TickRaceTest.test_a_send_back_made_while_the_tick_runs_is_kept
//
// The store's part: each_locked re-reads every ticket under its lock and
// re-checks it, so a person's change made after the tick read the board is
// what the tick sees, and a ticket that no longer qualifies is left alone.
func TestEachLockedSeesAChangeMadeAfterTheBoardWasRead(t *testing.T) {
	f := newFixture(t)
	f.file(t, "Fix", "gate")
	f.file(t, "Other", "gate")
	ts, _ := f.b.Reader.List() // the tick reads the board
	if err := f.b.Changing(t.Context(), "T0001", NoWait, func(tk *ticket.Ticket) error {
		ticket.AddNote(tk, "handle the empty case", "", nil, testkit.Stamp())
		ticket.Move(tk, "ready", model.ReasonByHuman, "", "", "maintainer", testkit.Stamp())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var visited []ticket.ID
	err := f.b.EachLocked(t.Context(), ts, func(tk *ticket.Ticket) bool { return tk.Column == "gate" },
		func(tk *ticket.Ticket) error {
			visited = append(visited, tk.ID)
			if _, err := fsx.TryLock(f.p.TicketLock(string(tk.ID))); !errors.Is(err, fsx.ErrBusy) {
				t.Errorf("%s is not locked while the body runs: %v", tk.ID, err)
			}
			ticket.Move(tk, "ready_for_pr", model.ReasonGatePassed, "", "", "", testkit.Stamp())
			return f.b.Save(tk)
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(visited) != 1 || visited[0] != "T0002" {
		t.Errorf("visited %v", visited)
	}
	if ts[0].Column != "ready" || ts[1].Column != "ready_for_pr" {
		t.Errorf("the list holds %s and %s", ts[0].Column, ts[1].Column)
	}
	tk := f.load(t, "T0001")
	if tk.Column != "ready" || len(tk.Notes) != 1 || tk.Notes[0].Text != "handle the empty case" {
		t.Errorf("the send-back was lost: %s %+v", tk.Column, tk.Notes)
	}
}

func TestEachLockedEdges(t *testing.T) {
	f := newFixture(t)
	f.file(t, "a", "gate")
	f.file(t, "b", "gate")
	f.file(t, "c", "gate")
	ts, _ := f.b.Reader.List()
	os.Remove(f.p.TicketFile("T0002")) // reread falls back to the copy in hand
	boom := errors.New("boom")
	var visited []ticket.ID
	err := f.b.EachLocked(t.Context(), ts, func(*ticket.Ticket) bool { return true }, func(tk *ticket.Ticket) error {
		visited = append(visited, tk.ID)
		tk.Column = "changed"
		if tk.ID == "T0002" {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || len(visited) != 2 {
		t.Fatalf("%v %v", err, visited)
	}
	if ts[0].Column != "changed" || ts[1].Column != "gate" || ts[2].Column != "gate" {
		t.Errorf("an error's clone replaced the list's copy: %s %s %s", ts[0].Column, ts[1].Column, ts[2].Column)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	if err := f.b.EachLocked(ctx, ts, func(*ticket.Ticket) bool { return true }, func(*ticket.Ticket) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Errorf("a cancelled context: %v %v", err, called)
	}
	// a ticket whose file broke since the read is skipped, not fatal
	f.env.WriteFile(t, "board/tickets/T0003.json", `{"id": `)
	visited = nil
	if err := f.b.EachLocked(t.Context(), ts[2:], func(*ticket.Ticket) bool { return true }, func(tk *ticket.Ticket) error {
		visited = append(visited, tk.ID)
		return nil
	}); err != nil || len(visited) != 0 {
		t.Errorf("a broken file: %v %v", err, visited)
	}
}

// held(): several tickets, lowest id first (by number), so two writers
// never deadlock; one busy ticket releases the rest.
func TestLockMany(t *testing.T) {
	f := newFixture(t)
	unlock, err := f.b.LockMany(t.Context(), []ticket.ID{"T0010", "T0002", "T0002", "T100000", "T0001"}, NoWait)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"T0001", "T0002", "T0010", "T100000"} {
		if _, err := fsx.TryLock(f.p.TicketLock(id)); !errors.Is(err, fsx.ErrBusy) {
			t.Errorf("%s not held: %v", id, err)
		}
	}
	unlock()
	other, err := f.b.Lock(t.Context(), "T0010", NoWait)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Unlock()
	if _, err := f.b.LockMany(t.Context(), []ticket.ID{"T0001", "T0010", "T0002"}, NoWait); !errors.Is(err, fsx.ErrBusy) {
		t.Fatalf("with T0010 held: %v", err)
	}
	for _, id := range []string{"T0001", "T0002"} {
		l, err := fsx.TryLock(f.p.TicketLock(id))
		if err != nil {
			t.Errorf("%s was not released: %v", id, err)
			continue
		}
		l.Unlock()
	}
	if _, err := f.b.LockMany(t.Context(), []ticket.ID{"../x"}, NoWait); err == nil {
		t.Error("a bad id")
	}
	if got := sortedIDs([]ticket.ID{"T0010", "T0002", "T0002", "T100000", "T0001"}); strings.Join(asStrings(got), ",") != "T0001,T0002,T0010,T100000" {
		t.Errorf("order %v", got)
	}
}

func asStrings(ids []ticket.ID) []string {
	var out []string
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

// ticket_lock makes board/tickets/.locks; lock files are never deleted.
func TestLockMakesItsDirectoryAndRefusesABadID(t *testing.T) {
	f := newFixture(t)
	os.RemoveAll(f.p.Locks)
	l, err := f.b.Lock(t.Context(), "T0001", NoWait)
	if err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	if _, err := os.Stat(f.p.TicketLock("T0001")); err != nil {
		t.Errorf("the lock file went: %v", err)
	}
	if _, err := f.b.Lock(t.Context(), "../T0001", NoWait); err == nil {
		t.Error("a lock outside .locks")
	}
}

func TestCreateLockAndTickLock(t *testing.T) {
	f := newFixture(t)
	c, err := f.b.CreateLock(t.Context(), NoWait)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.CreateLock(t.Context(), NoWait); !errors.Is(err, fsx.ErrBusy) {
		t.Errorf("a second create lock: %v", err)
	}
	c.Unlock()

	tick, err := f.b.TickLock(t.Context(), NoWait)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.TickLock(t.Context(), NoWait); !errors.Is(err, fsx.ErrBusy) {
		t.Errorf("a second tick: %v", err)
	}
	tick.Unlock()

	if f.b.AgainSet() {
		t.Error("no wake yet")
	}
	if err := f.b.Again(); err != nil || !f.b.AgainSet() {
		t.Errorf("Again: %v", err)
	}
	if err := f.b.ClearAgain(); err != nil || f.b.AgainSet() {
		t.Errorf("ClearAgain: %v", err)
	}
	if err := f.b.ClearAgain(); err != nil {
		t.Errorf("ClearAgain with nothing to clear: %v", err)
	}
}
