package present

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestStoryCollapsesRunsAndNamesPeople(t *testing.T) {
	ts := fixtures(t)
	s := BuildStory(find(ts, "T0001"), ByID(ts))
	want := []string{"GitHub sweep", "Triage", "Dispatcher", "You", "Worker", "Reviewer", "Security", "Gate", "Dispatcher", "Waiting for you"}
	if !reflect.DeepEqual(whos(s), want) {
		t.Fatalf("steps\n got %v\nwant %v", whos(s), want)
	}
	st := s.Steps
	if st[0].When != "5 Oct 09:00" || st[0].What != "Filed issue #17 for triage." {
		t.Errorf("first step %+v", st[0])
	}
	if st[3].What != "Moved it to Ready." || st[3].When != "6 Oct 10:00" {
		t.Errorf("a by-human move is the person's: %+v", st[3])
	}
	if st[4].When != "10:01–10:11" || st[4].What != "Changed the widget finder to use the primary key." {
		t.Errorf("start and finished should be one worker step: %+v", st[4])
	}
	if st[6].What != "Tier 2 (input reaches a sink): 2 questions, none above low." {
		t.Errorf("security step %q", st[6].What)
	}
	if st[7].What != "Gate: needs a maintainer: 40 changed lines (limit 10)." {
		t.Errorf("a rule replaces the dispatcher's own words: %q", st[7].What)
	}
	if st[8].What != "Opened draft PR #23 and asked for review." || st[9].When != "now" || st[9].Tone != "now" {
		t.Errorf("tail %+v %+v", st[8], st[9])
	}
}

func TestStoryPanelVotesFromProspect(t *testing.T) {
	ts := fixtures(t)
	s := BuildStory(find(ts, "T0006"), ByID(ts))
	want := []string{"Clock", "Prospector", "Panel", "Dispatcher", "Waiting for you"}
	if !reflect.DeepEqual(whos(s), want) {
		t.Fatalf("steps %v", whos(s))
	}
	panel := s.Steps[2]
	if panel.What != "Scored 4 · 5 · 4." || len(panel.Quotes) != 3 || panel.Quotes[1].Text != "clear win" || panel.When != "20:10–20:24" {
		t.Errorf("panel step %+v", panel)
	}
	if s.Steps[3].What != "Kept: 3 of 3 voters scored 4 or more (2 needed); mean 4.3 (3.5 needed)." {
		t.Errorf("keep rule %q", s.Steps[3].What)
	}
	if !strings.Contains(s.Caveat, "All 3 voters ran on helper") {
		t.Errorf("caveat %q", s.Caveat)
	}
	p := BuildStory(find(ts, "T0005"), ByID(ts))
	if !reflect.DeepEqual(whos(p), []string{"Clock", "Prospector", "Panel", "Dispatcher"}) {
		t.Fatalf("prospect steps %v", whos(p))
	}
	if p.Steps[2].What != "3 voters scored each proposal 1–5, blind to each other." {
		t.Errorf("votes should collapse into one panel step: %q", p.Steps[2].What)
	}
}

func TestDroppedProposalsFromOlderBoards(t *testing.T) {
	ts := fixtures(t)
	d := DroppedProposals(ts)
	if len(d) != 1 || d[0].Title != "Proposal 2" || d[0].Column != "rejected" || AskFor(d[0]) != nil {
		t.Fatalf("dropped %+v", d)
	}
	if got := d[0].History[0].Rule; got != "dropped: no voter scored 4 or more; mean 1.7 (3.5 needed)" {
		t.Errorf("drop rule %q", got)
	}
	s := BuildStory(d[0], ByID(ts))
	last := s.Steps[len(s.Steps)-1]
	if last.Tone != "flag" || !strings.Contains(last.What, "never reached your desk") {
		t.Errorf("dropped story ends %+v", last)
	}
	// once the dispatcher files it as a ticket, it is not made up again
	filed := tk(t, `{"id": "T0099", "kind": "proposal", "column": "rejected", "parent": "T0005", "proposal": {"title": "Proposal 2"}}`)
	if len(DroppedProposals(append(ts, filed))) != 0 {
		t.Errorf("a filed dropped proposal should not be invented again")
	}
}

func TestPanelRule(t *testing.T) {
	for _, c := range []struct {
		scores []int
		want   string
	}{
		{nil, "dropped: no scores"},
		{[]int{4, 5, 4}, "kept: 3 of 3 voters scored 4 or more (2 needed); mean 4.3 (3.5 needed)"},
		{[]int{4, 1, 1}, "dropped: 1 of 3 voters scored 4 or more (2 needed); mean 2.0 (3.5 needed)"},
		{[]int{2, 2, 1}, "dropped: no voter scored 4 or more; mean 1.7 (3.5 needed)"},
		{[]int{3, 4}, "kept: 1 of 2 voters scored 4 or more (2 needed); mean 3.5 (3.5 needed)"},
	} {
		if got := PanelRule(c.scores); got != c.want {
			t.Errorf("%v: %q", c.scores, got)
		}
	}
}

func TestAskJoinsTheStoryWithItsAnswer(t *testing.T) {
	parent := ticket.Clone(find(fixtures(t), "T0001"))
	parent.History = append(parent.History, ticket.Entry{At: "2026-10-14T21:00:00+00:00", Event: "asked", Detail: "T0099: Did steep pass?"})
	child := tk(t, `{"id": "T0099", "kind": "task", "parent": "T0001", "column": "needs_human", "updated": "2026-10-14T21:05:00+00:00",
		"result": "Yes: both Steep runs passed."}`)
	all := map[ticket.ID]*ticket.Ticket{"T0001": parent, "T0099": child}
	joined := fmtSteps(BuildStory(parent, all))
	if !strings.Contains(joined, "You: Asked: Did steep pass?\nAnswer: Yes: both Steep runs passed.") {
		t.Errorf("question and answer should sit together in the story:\n%s", joined)
	}
	child.Result, child.Agent = ticket.Opt[string]{}, ticket.Some(ticket.Agent{Name: "t0099-k1"})
	if !strings.Contains(fmtSteps(BuildStory(parent, all)), "Answer: An agent is working on it (T0099).") {
		t.Errorf("a running answer should say so")
	}
	child.Agent, child.Column = ticket.Nil[ticket.Agent](), "rejected"
	if !strings.Contains(fmtSteps(BuildStory(parent, all)), "Answer: None came back (T0099 closed).") {
		t.Errorf("a closed task with no answer should say so")
	}
	child.Column = "ready"
	if !strings.Contains(fmtSteps(BuildStory(parent, all)), "Answer: Queued (T0099); an agent picks it up on the next tick.") {
		t.Errorf("a queued task should say so")
	}
}

func TestMemoryChangesShowInTheStory(t *testing.T) {
	x := tk(t, `{"id": "T0042", "project": "p", "column": "review", "history": [
		{"at": "2026-10-16T10:00:00Z", "event": "memory", "detail": "worker memory 1a2b3c4: knowledge/rules.md, tickets/T0042.md",
		 "rule": "knowledge changed on a ticket from a public issue: check it"}]}`)
	got := fmtSteps(BuildStory(x, map[ticket.ID]*ticket.Ticket{"T0042": x}))
	if !strings.Contains(got, "Memory") || !strings.Contains(got, "Worker remembered: knowledge/rules.md, tickets/T0042.md.") ||
		!strings.Contains(got, "public issue") {
		t.Errorf("story: %s", got)
	}
}

func TestRepeatedErrorsCollapse(t *testing.T) {
	x := tk(t, `{"id": "T0044", "project": "p", "column": "ready", "history": [
		{"at": "2026-10-16T10:00:00Z", "event": "error", "detail": "ssh: no route"},
		{"at": "2026-10-16T10:05:00Z", "event": "error", "detail": "ssh: no route"},
		{"at": "2026-10-16T10:10:00Z", "event": "error", "detail": "ssh: no route"},
		{"at": "2026-10-16T10:15:00Z", "event": "error", "detail": "other"}]}`)
	s := BuildStory(x, nil)
	if len(s.Steps) != 2 || s.Steps[0].What != "Error: ssh: no route. (3 times)" || s.Steps[0].Tone != "flag" {
		t.Errorf("errors: %+v", s.Steps)
	}
}

func TestEarlierAndRecentFoldTheStory(t *testing.T) {
	s := Story{Steps: make([]Step, 6)}
	if s.Earlier() != nil || len(s.Recent()) != 6 {
		t.Errorf("six steps all show")
	}
	s.Steps = make([]Step, 9)
	if len(s.Earlier()) != 4 || len(s.Recent()) != 5 {
		t.Errorf("nine steps: %d earlier, %d recent", len(s.Earlier()), len(s.Recent()))
	}
}
