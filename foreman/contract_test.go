package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Event details exactly as bin/factory writes them (copied from the live log, 2026-10-17). Foreman reads facts
// back out of these strings, so when the dispatcher's wording changes these tests are what notice. The
// hand-written fixtures elsewhere missed three such drifts.

func TestStoryReadsTheDispatchersRealEventText(t *testing.T) {
	tk := &Ticket{ID: "T0029", Project: "encoded-id-cr", Title: "Port", Column: "review", History: []Hist{
		{At: "2026-10-17T05:38:44Z", Event: "start", Detail: "triage t0029-t1 (claude-sonnet-5-5)"},
		{At: "2026-10-17T05:58:46Z", Event: "finished", Detail: "triage t0029-t1"},
		{At: "2026-10-17T08:31:00Z", Event: "start", Detail: "work t0029-w1 on bot/port-sqids (claude-opus-5-5)"},
		{At: "2026-10-17T08:53:14Z", Event: "memory", Detail: "worker memory 91a7fac: knowledge/INDEX.md, knowledge/gem-reference.md, tickets/T0029.md"},
		{At: "2026-10-17T08:53:14Z", Event: "finished", Detail: "work t0029-w1"},
		{At: "2026-10-17T08:55:00Z", Event: "start", Detail: "task t0030-k1 @steve-linux (claude-sonnet-5-5)"},
		{At: "2026-10-17T08:56:00Z", Event: "memory", Detail: "worker memory is 600 KB, over its limit: tidy it up"},
		{At: "2026-10-17T08:57:00Z", Event: "memory", Detail: "no memory came back from steve-linux; this run's notes are lost"},
	}}
	got := fmtSteps(BuildStory(tk, map[string]*Ticket{tk.ID: tk}))
	for _, want := range []string{
		"Triage: ", "Worker: ", // agent runs with a model in their start line
		"Task agent on steve-linux: ", // and on a helper host
		"Worker remembered: knowledge/INDEX.md, knowledge/gem-reference.md, tickets/T0029.md.",
		"Worker memory is 600 KB, over its limit: tidy it up.", // not "remembered"
		"No memory came back from steve-linux; this run's notes are lost.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("story lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "remembered: tidy it up") {
		t.Errorf("a memory warning read as a memory change:\n%s", got)
	}
}

func TestFloorReadsTheDispatchersRealMoveText(t *testing.T) {
	tk := &Ticket{ID: "T0023", Project: "typed_operation", Kind: "task", Column: "done", History: []Hist{
		{At: "2026-10-16T19:57:49Z", Event: "move", Detail: "needs_human -> done: answered on T0018"},
	}}
	if cls, label, detail := floorOutcome(tk, 0); label != "Answered" || !strings.Contains(detail, "T0018") || strings.Contains(detail, "by you") {
		t.Errorf("answered question: %q %q %q", cls, label, detail)
	}
}

// testdata/contract: tickets the dispatcher's real code paths wrote (tests/test_tickets.py ContractFixture
// regenerates them). Foreman reads them through the typed fields alone: no fallback to parsing the wording.

func contractTickets(t *testing.T) map[string]*Ticket {
	t.Helper()
	ts := (&Board{dir: "testdata/contract/tickets", cache: map[string]cached{}}).Load()
	if len(ts) < 4 {
		t.Fatalf("loaded %d contract tickets", len(ts))
	}
	return byID(ts)
}

func TestContractTicketsReadFromTypedHistory(t *testing.T) {
	all := contractTickets(t)
	fallbacks.Store(0)
	work := BuildStory(all["T0001"], all)
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
	if task := fmtSteps(BuildStory(all["T0002"], all)); !strings.Contains(task, "Task agent on helper: Answered. Yes, it does.") {
		t.Errorf("a run on a helper host:\n%s", task)
	}
	tk := all["T0002"]
	_, label, detail := floorOutcome(tk, len(tk.History)-1)
	if label != "Answered" || detail != "the answer went back to T0001" {
		t.Errorf("floor: %q %q", label, detail)
	}
	v := Conversation(all["T0003"], all, "", time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	var mine []string
	for _, m := range v.Msgs {
		if m.Side == "me" {
			mine = append(mine, m.Text)
		}
	}
	if len(mine) != 2 || mine[1] != "UTF-8" {
		t.Errorf("the answer to the agent's question: %q", mine)
	}
	talk := Conversation(all["T0001"], all, "", time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(fmt.Sprint(talk.Msgs), "Does it keep CRLF?") {
		t.Errorf("the question asked on T0001 is not in its conversation")
	}
	for _, tk := range all {
		Desk([]*Ticket{tk})
		BuildFloor([]*Ticket{tk}, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
		News(map[string]snap{}, []*Ticket{tk})
	}
	if n := fallbacks.Load(); n != 0 {
		t.Errorf("%d reads of typed history fell back to parsing its wording", n)
	}
	// the old wording still reads, through the fallback
	fallbacks.Store(0)
	TestStoryReadsTheDispatchersRealEventText(t)
	if fallbacks.Load() == 0 {
		t.Errorf("untyped history did not go through the fallback")
	}
}

func TestABoardTicketStaysOffTheDeskFloorBoardsAndPush(t *testing.T) {
	all := contractTickets(t)
	b := *all["T0004"]
	if !b.board() || b.By != "example-agent" || len(b.Questions) != 1 || len(b.Comments) != 2 {
		t.Fatalf("board ticket fields: %+v", b)
	}
	// waiting on a question to a person, with the ask the dispatcher computed: still nowhere until step 4
	b.Column = "waiting"
	b.Ask = &Ask{Kind: "question", Question: "Which locale?", Since: "2026-10-17T09:30:00+00:00"}
	ts := []*Ticket{&b}
	if AskFor(&b) != nil || len(Desk(ts)) != 0 {
		t.Errorf("a board ticket reached the desk")
	}
	f := BuildFloor(ts, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	if len(f.Days) != 0 {
		t.Errorf("a board ticket is on the floor: %+v", f.Days)
	}
	if s := summaries(ts); len(s) != 0 {
		t.Errorf("a board ticket is on the boards: %+v", s)
	}
	if n := News(map[string]snap{}, ts); len(n) != 0 {
		t.Errorf("a board ticket was pushed: %+v", n)
	}
	if st := fmtSteps(BuildStory(&b, all)); !strings.HasPrefix(st, "Agent: Filed by example-agent.") {
		t.Errorf("story: %s", st)
	}
}

func TestABoardTicketPageHasNoAnswerForms(t *testing.T) {
	dir := t.TempDir()
	tickets := filepath.Join(dir, "board", "tickets")
	os.MkdirAll(tickets, 0o755)
	data, err := os.ReadFile("testdata/contract/tickets/T0004.json")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tickets, "T0004.json"), data, 0o644)
	h := newHarness(t, false)
	h.srv.board = NewBoard(dir)
	c := h.login()
	w := h.do("GET", "/t/T0004", nil, c)
	page := w.Body.String()
	if w.Code != 200 || !strings.Contains(page, "Date picker") || !strings.Contains(page, "Done") {
		t.Fatalf("board ticket page: %d", w.Code)
	}
	if strings.Contains(page, `name="note"`) || strings.Contains(page, `name="answer"`) {
		t.Errorf("a board ticket page offers answer forms")
	}
	if talk := h.do("GET", "/t/T0004/talk", nil, c).Body.String(); strings.Contains(talk, `id="reply"`) {
		t.Errorf("a board ticket's conversation offers a reply")
	}
	if w := h.do("GET", "/b/sandbox", nil, c); w.Code != http.StatusNotFound {
		t.Errorf("/b/sandbox: %d", w.Code)
	}
}
