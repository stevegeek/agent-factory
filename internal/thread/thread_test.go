package thread

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func tk(t *testing.T, src string) *ticket.Ticket {
	t.Helper()
	x, problems, err := ticket.Decode([]byte(src))
	if err != nil || len(problems) > 0 {
		t.Fatalf("test ticket: %v %v", err, problems)
	}
	return x
}

// relTicket is a ticket on project widgets with extra JSON members.
func relTicket(t *testing.T, id, title, column, extra string) *ticket.Ticket {
	t.Helper()
	if extra != "" {
		extra = ", " + extra
	}
	return tk(t, fmt.Sprintf(`{"id": %q, "project": "widgets", "title": %q, "body": "", "door": "human", "by": "factory",
		"column": %q, "created": "2026-10-18T09:00:00+00:00", "history": []%s}`, id, title, column, extra))
}

// hygieneBoard is a check with findings filed as tickets, a question asked
// on it, and a follow-up to one of the findings.
func hygieneBoard(t *testing.T) map[ticket.ID]*ticket.Ticket {
	root := relTicket(t, "T0016", "Repo hygiene check", "done", `"kind": "task", "children": ["T0020", "T0021"]`)
	root.Findings = []ticket.Finding{{Title: "a", Verdict: ticket.VerdictConfirmed, Filed: "T0020"},
		{Title: "b", Verdict: ticket.VerdictConfirmed, Filed: "T0021"}}
	ticket.Event(root, "asked", "T0030: why?", "", ticket.Asked{Ticket: "T0030", Text: "why?"}, time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC))
	return present.ByID([]*ticket.Ticket{root,
		relTicket(t, "T0020", "Drop the stray file", "done", `"parent": "T0016", "found_in": {"ticket": "T0016", "n": 1}`),
		relTicket(t, "T0021", "Pin the tool", "needs_human", `"parent": "T0016", "found_in": {"ticket": "T0016", "n": 2},
			"ask": {"kind": "question", "question": "Which version?", "why": "", "since": "2026-10-20T09:00:00+00:00", "retry_column": null}`),
		relTicket(t, "T0030", "Question on T0016: why?", "working", `"kind": "task", "parent": "T0016"`),
		relTicket(t, "T0040", "Pin it in CI too", "ready", `"parent": "T0021"`),
		relTicket(t, "T0050", "Unrelated", "ready", ""),
	})
}

func threadOf(r Relations) string {
	var out []string
	for _, x := range r.Thread {
		s := x.ID
		if x.How != "" {
			s = x.How + " " + s
		}
		if x.Current {
			s += "*"
		}
		out = append(out, s)
	}
	return strings.Join(out, " > ")
}

func TestAFindingTicketShowsItsRootAndFindingNumber(t *testing.T) {
	all := hygieneBoard(t)
	r := Relate(all["T0021"], all)
	if got := threadOf(r); got != "T0016 > Finding 2 T0021*" {
		t.Errorf("thread %q", got)
	}
	if r.Lead != "Finding 2 of" || r.ParentID != "T0016" || r.ParentTitle != "Repo hygiene check" {
		t.Errorf("header %q %q %q", r.Lead, r.ParentID, r.ParentTitle)
	}
	if root := r.Thread[0]; root.Kind != "task" || root.Column != "Done" || root.Open {
		t.Errorf("root %+v", root)
	}
	if !r.Thread[1].Waits {
		t.Errorf("the finding ticket waits for the maintainer")
	}
}

func TestTheRootListsItsDescendantsByHowTheyCame(t *testing.T) {
	all := hygieneBoard(t)
	r := Relate(all["T0016"], all)
	if len(r.Thread) != 0 || r.ParentID != "" {
		t.Errorf("a root has no thread: %+v", r.Thread)
	}
	var got []string
	for _, g := range r.Groups {
		var ids []string
		for _, x := range g.Tickets {
			ids = append(ids, strings.TrimSpace(x.How+" "+x.ID+" "+x.Column+fmt.Sprint(x.Waits)))
		}
		got = append(got, fmt.Sprintf("%s (%d open, %d done): %s", g.Label, g.Open, g.Done, strings.Join(ids, ", ")))
	}
	want := []string{
		"Filed from its findings (1 open, 1 done): Finding 1 T0020 Donefalse, Finding 2 T0021 Needs youtrue",
		"Questions asked on it (1 open, 0 done): T0030 Workingfalse",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("groups\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r := Relate(all["T0030"], all); r.Lead != "Question on" || r.ParentID != "T0016" {
		t.Errorf("question header %q %q", r.Lead, r.ParentID)
	}
}

func TestAThreeLevelChainRendersInOrder(t *testing.T) {
	all := hygieneBoard(t)
	r := Relate(all["T0040"], all)
	if got := threadOf(r); got != "T0016 > Finding 2 T0021 > Follow-up T0040*" {
		t.Errorf("thread %q", got)
	}
	if r.Lead != "Filed from" || r.ParentID != "T0021" {
		t.Errorf("header %q %q", r.Lead, r.ParentID)
	}
	if g := Relate(all["T0021"], all).Groups; len(g) != 1 || g[0].Label != "Follow-ups" || g[0].Tickets[0].ID != "T0040" {
		t.Errorf("the finding ticket's follow-ups %+v", g)
	}
}

func TestTicketsOutsideTheSetAreLeftOut(t *testing.T) {
	all := hygieneBoard(t)
	delete(all, "T0016")
	delete(all, "T0020")
	r := Relate(all["T0040"], all)
	if got := threadOf(r); got != "T0021 > Follow-up T0040*" {
		t.Errorf("thread %q", got)
	}
	if r := Relate(all["T0021"], all); r.ParentID != "" || len(r.Thread) != 0 {
		t.Errorf("a hidden parent shows: %+v", r)
	}
	if present.ParentOf(all["T0030"], all) != "" {
		t.Errorf("a hidden parent tags its child")
	}
	all = hygieneBoard(t)
	delete(all, "T0020")
	for _, g := range Relate(all["T0016"], all).Groups {
		for _, x := range g.Tickets {
			if x.ID == "T0020" {
				t.Errorf("a hidden child is listed")
			}
		}
	}
}

func TestATicketWithoutRelationsHasNone(t *testing.T) {
	all := hygieneBoard(t)
	if r := Relate(all["T0050"], all); len(r.Thread) != 0 || len(r.Groups) != 0 || r.Lead != "" || r.ParentID != "" {
		t.Errorf("relations %+v", r)
	}
}

func TestLinksAreListedBothWays(t *testing.T) {
	a := relTicket(t, "T0001", "A", "ready", `"links": [{"rel": "blocks", "ticket": "T0002"}]`)
	b := relTicket(t, "T0002", "B", "ready", "")
	all := present.ByID([]*ticket.Ticket{a, b})
	if g := Relate(a, all).Groups; len(g) != 1 || g[0].Label != "Links" || g[0].Tickets[0].How != "blocks" {
		t.Errorf("a's links %+v", g)
	}
	if g := Relate(b, all).Groups; len(g) != 1 || g[0].Tickets[0].ID != "T0001" || g[0].Tickets[0].How != "blocks (from it)" {
		t.Errorf("b's links %+v", g)
	}
}
