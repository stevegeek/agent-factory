package ticket

import (
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
)

func afterBoard() map[ID]*Ticket {
	return ByID([]*Ticket{
		{ID: "T0001", Column: string(model.ColDone)},
		{ID: "T0002", Column: string(model.ColRejected)},
		{ID: "T0003", Column: string(model.ColReady), After: []ID{"T0004"}},
		{ID: "T0004", Column: string(model.ColReview)},
		{ID: "T0005", Column: string(model.BoardDropped)},
	})
}

func TestBlockersSortsOpenAndGoneTickets(t *testing.T) {
	tk := &Ticket{ID: "T0009", After: []ID{"T0001", "T0002", "T0004", "T0005", "T0099"}}
	open, gone := Blockers(tk, afterBoard())
	if !slices.Equal(open, []ID{"T0004"}) || !slices.Equal(gone, []ID{"T0002", "T0005", "T0099"}) {
		t.Fatalf("open %v, gone %v", open, gone)
	}
}

func TestCheckAfterRefusesSelfUnknownAndCycles(t *testing.T) {
	for _, c := range []struct {
		id    ID
		after []ID
		want  string
	}{
		{"T0004", []ID{"T0004"}, "itself"},
		{"T0004", []ID{"T0099"}, "no ticket T0099"},
		{"T0004", []ID{"T0003"}, "cycle"},
	} {
		if _, err := CheckAfter(c.id, c.after, afterBoard()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s after %v: %v, want %q", c.id, c.after, err, c.want)
		}
	}
}

func TestCheckAfterDropsRepeats(t *testing.T) {
	got, err := CheckAfter("T0003", []ID{"T0004", "T0001", "T0004"}, afterBoard())
	if err != nil || !slices.Equal(got, []ID{"T0004", "T0001"}) {
		t.Fatalf("%v, %v", got, err)
	}
}
