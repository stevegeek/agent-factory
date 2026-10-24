package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// NextID allocates a ticket id no one else has or will get (next_id, §4.5):
// under board/tickets/.id.lock, max(.last-id, the number of every T*.json)
// + 1, written back to the counter (through ".last-id.tmp", as Python
// does) before the lock is let go. Numbers only go up, so a deleted
// ticket's id is never handed out again, and two creators (the tick and
// Foreman) never collide. A missing, garbage or negative counter counts
// as 0.
func (b *Board) NextID(ctx context.Context) (ticket.ID, error) {
	if err := os.MkdirAll(b.Paths.Tickets, 0o777); err != nil {
		return "", fmt.Errorf("next id: %w", err)
	}
	l, err := fsx.Lock(ctx, b.Paths.IDLock, fsx.Forever)
	if err != nil {
		return "", fmt.Errorf("next id: %w", err)
	}
	defer l.Unlock()
	names, err := filepath.Glob(filepath.Join(b.Paths.Tickets, "T*.json"))
	if err != nil {
		return "", fmt.Errorf("next id: %w", err)
	}
	n := readCounter(b.Paths.LastID)
	for _, name := range names {
		// int(re.sub(r"\D", "", basename)): a name with no digit counts as none
		if k := ticket.ID(filepath.Base(name)).Num(); k > n {
			n = k
		}
	}
	n++
	if err := writeCounter(b.Paths.LastID, n); err != nil {
		return "", fmt.Errorf("next id: %w", err)
	}
	return ticket.FormatID(n), nil
}

// readCounter is int(f.read().strip() or 0), 0 when missing or garbage.
func readCounter(path string) int {
	data, err := fsx.ReadNoFollow(path, 64)
	if err != nil {
		return 0
	}
	s := strings.TrimPrefix(strings.TrimSpace(string(data)), "+")
	n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
	if err != nil || n < 0 || strings.HasPrefix(s, "-") {
		return 0
	}
	return n
}

// writeCounter writes the counter through Python's fixed temp name. The id
// lock is held, so no one else writes it; the temp is opened without
// following a link and fsync'd before the rename.
func writeCounter(path string, n int) (err error) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.WriteString(strconv.Itoa(n)); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if d, derr := os.Open(filepath.Dir(path)); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
