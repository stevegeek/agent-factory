package desk

import (
	"reflect"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var people = []string{"ann", "robin"}

func stampAt(m int) string {
	return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC).Add(time.Duration(m) * time.Minute).Format(time.RFC3339)
}

func TestMentionsAreOnlyPeoplesNames(t *testing.T) {
	for text, want := range map[string][]string{
		"@robin can you look":            {"robin"},
		"ask @ann and @robin, then @ann": {"ann", "robin"},
		"(@robin)":                       {"robin"},
		"mail robin@example.com":         nil,
		"see a@robin":                    nil,
		"@robinson":                      nil,
		"@nobody here":                   nil,
	} {
		if got := Mentions(text, people); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

// A mention, or a pass, puts the ticket in front of its person until they
// put it away or write on the ticket themselves; their own words never
// nudge them.
func TestNudges(t *testing.T) {
	noted := func(tk *ticket.Ticket, by, text string, m int) {
		tk.Notes = append(tk.Notes, ticket.Note{At: stampAt(m), Text: text})
		tk.History = append(tk.History, ticket.Entry{At: stampAt(m), Event: "note", Type: model.EventNoted,
			Data: ticket.Noted{By: by, Note: len(tk.Notes) - 1}})
	}
	robin := present.Viewer{By: "robin", People: people}
	mention := &ticket.Ticket{ID: "T0001", Title: "One", Project: "widgets", Column: "ready"}
	noted(mention, "ann", "@robin what do you think?", 1)
	board := &ticket.Ticket{ID: "T0002", Title: "Two", Project: "notes", Column: "doing", Mode: ticket.Some("board"),
		Comments: []ticket.Comment{{N: 1, At: stampAt(2), By: "ann", Type: "comment", Text: "over to @robin"}}}
	passed := &ticket.Ticket{ID: "T0003", Title: "Three", Project: "gadgets", Column: "ready", Assignee: ticket.Some("robin"),
		History: []ticket.Entry{{At: stampAt(3), Event: "edit", Detail: "ann passed it to robin", Type: model.EventEdited,
			Data: ticket.Edited{By: "ann", Fields: []string{"assignee"}}}}}
	self := &ticket.Ticket{ID: "T0004", Project: "widgets", Column: "ready"}
	noted(self, "robin", "a note to @robin", 4)
	ts := []*ticket.Ticket{mention, board, passed, self}

	got := Nudges(ts, robin)
	if len(got) != 3 || got[0].T != passed || !got[0].Passed || got[0].By != "ann" || got[1].T != board || got[2].T != mention || got[2].Passed {
		t.Fatalf("nudges %+v", got)
	}
	if Nudges(ts, present.Viewer{By: "ann", People: people}) != nil {
		t.Errorf("ann's own words nudge her")
	}
	// put away up to the time shown; a newer mention comes back
	robin.Dismissed = map[string]string{"T0001": stampAt(1)}
	if n := Nudges(ts, robin); len(n) != 2 {
		t.Errorf("a dismissed mention stays: %+v", n)
	}
	noted(mention, "ann", "@robin again", 5)
	if n := Nudges(ts, robin); len(n) != 3 || n[0].T != mention {
		t.Errorf("a newer mention is back: %+v", n)
	}
	// robin writing on the ticket answers it
	noted(mention, "robin", "done", 6)
	if n := Nudges(ts, robin); len(n) != 2 {
		t.Errorf("robin's own note after the mention leaves it: %+v", n)
	}
	// a pass that is no longer theirs is no nudge
	passed.Assignee = ticket.Some("ann")
	if n := Nudges(ts, robin); len(n) != 1 || n[0].T != board {
		t.Errorf("after the ticket went to ann: %+v", n)
	}
	if items := ForYou(Nudges(ts, robin), time.Date(2026, 10, 9, 12, 10, 0, 0, time.UTC)); len(items) != 1 || items[0].Href != "/t/T0002" || items[0].Age == "" {
		t.Errorf("for you %+v", items)
	}
}
