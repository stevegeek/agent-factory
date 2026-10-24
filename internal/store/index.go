package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/fsx"
)

// RefreshIndex is ticket_index(): under board/.index.lock (a leaf), the
// index read from index.json and refreshed from the ticket files, written
// back (json.dump, no indent, through "<index>.<pid>.tmp") only when an
// entry was re-read or ids appeared or vanished. Ticket writers never touch
// the index.
func (b *Board) RefreshIndex(ctx context.Context) (boardread.Index, error) {
	if err := os.MkdirAll(b.Paths.Tickets, 0o777); err != nil {
		return boardread.Index{}, fmt.Errorf("index: %w", err)
	}
	l, err := fsx.Lock(ctx, b.Paths.IndexLock, fsx.Forever)
	if err != nil {
		return boardread.Index{}, fmt.Errorf("index: %w", err)
	}
	defer l.Unlock()
	ix, changed, err := b.Reader.ScanIndex(b.Reader.ReadIndex())
	if err != nil {
		return boardread.Index{}, err
	}
	if changed {
		if err := fsx.WriteAtomic(b.Paths.Index, ix.Encode(), 0o666); err != nil {
			return boardread.Index{}, fmt.Errorf("index: %w", err)
		}
	}
	return ix, nil
}

// Reindex is reindex(): index.json deleted and rebuilt; the count of
// tickets in it.
func (b *Board) Reindex(ctx context.Context) (int, error) {
	if err := os.Remove(b.Paths.Index); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("reindex: %w", err)
	}
	ix, err := b.RefreshIndex(ctx)
	if err != nil {
		return 0, err
	}
	return ix.Len(), nil
}
