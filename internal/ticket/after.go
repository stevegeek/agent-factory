package ticket

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
)

// Blockers sorts the tickets t waits after: open ones still hold it, and
// gone ones were closed without being done or no longer exist, so they
// cannot release it. A done ticket releases it.
func Blockers(t *Ticket, byID map[ID]*Ticket) (open, gone []ID) {
	for _, id := range t.After {
		d, ok := byID[id]
		switch {
		case !ok || (model.Closed(d.Column) && d.Column != string(model.ColDone)):
			gone = append(gone, id)
		case d.Column != string(model.ColDone):
			open = append(open, id)
		}
	}
	return open, gone
}

// CheckAfter refuses an after list for the ticket id that names an unknown
// ticket, the ticket itself, or a ticket that already waits on id (a cycle).
// It returns the list without repeats, in the order given.
func CheckAfter(id ID, after []ID, byID map[ID]*Ticket) ([]ID, error) {
	var out []ID
	for _, a := range after {
		if a == id {
			return nil, fmt.Errorf("%s cannot wait for itself", id)
		}
		if _, ok := byID[a]; !ok {
			return nil, fmt.Errorf("no ticket %s", a)
		}
		if waitsOn(a, id, byID, map[ID]bool{}) {
			return nil, fmt.Errorf("%s already waits for %s: that would be a cycle", a, id)
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

// waitsOn reports whether from reaches to through after links.
func waitsOn(from, to ID, byID map[ID]*Ticket, seen map[ID]bool) bool {
	if seen[from] {
		return false
	}
	seen[from] = true
	t, ok := byID[from]
	if !ok {
		return false
	}
	for _, a := range t.After {
		if a == to || waitsOn(a, to, byID, seen) {
			return true
		}
	}
	return false
}

// AfterLabel is a proposed ticket's after list as read: "#1, T0391".
func (p Proposed) AfterLabel() string {
	out := make([]string, len(p.After))
	for i, a := range p.After {
		if _, err := strconv.Atoi(a); err == nil {
			a = "#" + a
		}
		out[i] = a
	}
	return strings.Join(out, ", ")
}

// ByID indexes ts by id.
func ByID(ts []*Ticket) map[ID]*Ticket {
	m := make(map[ID]*Ticket, len(ts))
	for _, t := range ts {
		m[t.ID] = t
	}
	return m
}
