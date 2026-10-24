package present

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Event details exactly as bin/factory writes them. The story reads facts back out of these strings, so when the
// dispatcher's wording changes these tests are what notice.
func realEventText(t *testing.T) *ticket.Ticket {
	return tk(t, `{"id": "T0029", "project": "example", "title": "Port", "column": "review", "history": [
		{"at": "2026-10-17T05:38:44Z", "event": "start", "detail": "triage t0029-t1 (claude-sonnet-5-5)"},
		{"at": "2026-10-17T05:58:46Z", "event": "finished", "detail": "triage t0029-t1"},
		{"at": "2026-10-17T08:31:00Z", "event": "start", "detail": "work t0029-w1 on bot/port-tinyids (claude-opus-5-5)"},
		{"at": "2026-10-17T08:53:14Z", "event": "memory", "detail": "worker memory 91a7fac: knowledge/INDEX.md, knowledge/gem-reference.md, tickets/T0029.md"},
		{"at": "2026-10-17T08:53:14Z", "event": "finished", "detail": "work t0029-w1"},
		{"at": "2026-10-17T08:55:00Z", "event": "start", "detail": "task t0030-k1 @helper-host (claude-sonnet-5-5)"},
		{"at": "2026-10-17T08:56:00Z", "event": "memory", "detail": "worker memory is 600 KB, over its limit: tidy it up"},
		{"at": "2026-10-17T08:57:00Z", "event": "memory", "detail": "no memory came back from helper-host; this run's notes are lost"}]}`)
}

func TestStoryReadsTheDispatchersRealEventText(t *testing.T) {
	r := &reader{}
	x := realEventText(t)
	got := fmtSteps(r.story(x, map[ticket.ID]*ticket.Ticket{x.ID: x}))
	for _, want := range []string{
		"Triage: ", "Worker: ", // agent runs with a model in their start line
		"Task agent on helper-host: ", // and on a helper host
		"Worker remembered: knowledge/INDEX.md, knowledge/gem-reference.md, tickets/T0029.md.",
		"Worker memory is 600 KB, over its limit: tidy it up.", // not "remembered"
		"No memory came back from helper-host; this run's notes are lost.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("story lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "remembered: tidy it up") {
		t.Errorf("a memory warning read as a memory change:\n%s", got)
	}
	// the old wording still reads, through the legacy readers
	if r.legacy == 0 {
		t.Errorf("untyped history did not go through the legacy readers")
	}
}

func TestFloorReadsTheDispatchersRealMoveText(t *testing.T) {
	x := tk(t, `{"id": "T0023", "project": "example", "kind": "task", "column": "done", "history": [
		{"at": "2026-10-16T19:57:49Z", "event": "move", "detail": "needs_human -> done: answered on T0018"}]}`)
	if cls, label, detail := FloorOutcome(x, 0); label != "Answered" || !strings.Contains(detail, "T0018") || strings.Contains(detail, "by you") {
		t.Errorf("answered question: %q %q %q", cls, label, detail)
	}
}

// The contract tickets were written by the dispatcher's real code paths
// (the Python tests ContractFixture). They read through the typed fields
// alone: no legacy reader is needed.
func TestContractTicketsReadFromTypedHistory(t *testing.T) {
	all := contractTickets(t)
	r := &reader{}
	work := r.story(all["T0001"], all)
	got := fmtSteps(work)
	for _, want := range []string{
		"You: Filed it.",
		"Worker: Kept the last line.", // a run with a model in its start line
		"Reviewer: Pass.", "Security: Tier 1",
		"Memory: Worker remembered: tickets/T0001.md.",
		"Memory: Worker memory is 2 KB, over its limit: tidy it up.",
		"You: Asked: Does it keep CRLF?", "Answer: Yes: see the test.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("story lacks %q:\n%s", want, got)
		}
	}
	for _, st := range work.Steps {
		if strings.Contains(st.What, "over its limit") && st.Tone != "flag" {
			t.Errorf("a memory over its limit is not marked as a warning")
		}
	}
	if task := fmtSteps(r.story(all["T0002"], all)); !strings.Contains(task, "Task agent on helper: Answered. Yes, it does.") {
		t.Errorf("a run on a helper host:\n%s", task)
	}
	x := all["T0002"]
	_, label, detail := r.floorOutcome(x, len(x.History)-1)
	if label != "Answered" || detail != "the answer went back to T0001" {
		t.Errorf("floor: %q %q", label, detail)
	}
	day, noonOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	v := r.conversation(all["T0003"], all, nil, noonOf)
	var mine []string
	for _, m := range v.Msgs {
		if m.Side == "me" {
			mine = append(mine, m.Text)
		}
	}
	if len(mine) != 2 || mine[1] != "UTF-8" {
		t.Errorf("the answer to the agent's question: %q", mine)
	}
	talk := r.conversation(all["T0001"], all, nil, noonOf)
	if !strings.Contains(fmt.Sprint(talk.Msgs), "Does it keep CRLF?") {
		t.Errorf("the question asked on T0001 is not in its conversation")
	}
	for _, x := range all {
		r.desk([]*ticket.Ticket{x}, you)
		r.floor([]*ticket.Ticket{x}, day, noonOf, you)
		r.facts(x, AskFor(x), all)
	}
	if r.legacy != 0 {
		t.Errorf("%d reads of typed history went through a legacy reader", r.legacy)
	}
}

// A board ticket is on the desk only while an agent's question to a person
// is open, worked out from its questions (not a stored ask that lags them),
// and never on the floor.
func TestABoardQuestionReachesTheDeskButNotTheFloor(t *testing.T) {
	all := contractTickets(t)
	b := ticket.Clone(all["T0004"])
	if !b.Board() || b.By != "example-agent" || len(b.Questions) != 1 || len(b.Comments) != 2 {
		t.Fatalf("board ticket fields: %+v", b)
	}
	// a stored ask whose question was answered since asks nothing
	b.Column = "waiting"
	b.Ask = ticket.Some(ticket.Ask{Kind: "question", Question: "Which locale?", Since: "2026-10-17T09:30:00+00:00"})
	ts := []*ticket.Ticket{b}
	if AskFor(b) != nil || len(Desk(ts, you)) != 0 {
		t.Errorf("an answered board question is on the desk")
	}
	// waiting on the question, still open
	b.Questions[0].Answer, b.Questions[0].AnsweredBy, b.Questions[0].AnsweredAt = ticket.Nil[string](), ticket.Nil[string](), ticket.Nil[string]()
	a := AskFor(b)
	if a == nil || a.Kind != "question" || a.Question != "Which locale?" || a.Since != "2026-10-17T09:22:00+00:00" || a.N.Or(0) != 1 {
		t.Fatalf("the board ask: %+v", a)
	}
	d := Desk(ts, you)
	if len(d) != 1 || Waiting(d) != 1 || Action(b, d[0].Ask) != "Answer example-agent" {
		t.Errorf("desk: %+v", d)
	}
	if w := WaitingWhat(b, a); w != "example-agent asked alice and waits for the answer." {
		t.Errorf("waiting: %q", w)
	}
	f := BuildFloor(ts, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), you)
	if len(f.Days) != 0 {
		t.Errorf("a board ticket is on the floor: %+v", f.Days)
	}
	st := fmtSteps(BuildStory(b, all))
	if !strings.HasPrefix(st, "Agent: Filed by example-agent.") || !strings.Contains(st, "example-agent: Asked alice: Which locale?") {
		t.Errorf("story: %s", st)
	}
	talk := Conversation(b, all, nil, time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	if !talk.Question || talk.Ask == nil || !strings.Contains(fmt.Sprint(talk.Msgs), "Question for alice:** Which locale?") {
		t.Errorf("conversation: %+v", talk)
	}
	facts := fmt.Sprint(Facts(b, a, all))
	for _, want := range []string{"Column Waiting", "Assignee example-agent", "Labels ui", "Filed by example-agent"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts %s lack %q", facts, want)
		}
	}
}

// The conversation of an answered board question shows it and its answer.
func TestABoardConversationShowsQuestionsAndAnswers(t *testing.T) {
	all := contractTickets(t)
	v := Conversation(all["T0004"], all, nil, time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	var got []string
	for _, m := range v.Msgs {
		got = append(got, m.Who+"/"+m.Side+": "+m.Text)
	}
	s := strings.Join(got, "\n")
	for _, want := range []string{"example-agent/agent: **Question for alice:** Which locale?\n\nOptions: en-GB · en-US", "alice/me: en-GB", "example-agent/agent: Uses en-GB."} {
		if !strings.Contains(s, want) {
			t.Errorf("conversation lacks %q:\n%s", want, s)
		}
	}
	if v.Question || v.Ask != nil {
		t.Errorf("a closed board ticket asks: %+v", v.Ask)
	}
}

// Every ticket of a private snapshot of the live board presents without a
// panic, and the typed history the live dispatcher wrote needs no legacy
// reader beyond what the old tickets on it need.
func TestLiveBoardPresents(t *testing.T) {
	dir := livePath(t)
	ts := load(t, dir)
	all := ByID(ts)
	r := &reader{}
	now := time.Now()
	r.desk(ts, you)
	r.floor(ts, now, now, you)
	for _, x := range ts {
		r.story(x, all)
		r.conversation(x, all, nil, now)
		r.facts(x, AskFor(x), all)
		PRCallout(x)
	}
	t.Logf("%d live tickets, %d legacy reads", len(ts), r.legacy)
}
