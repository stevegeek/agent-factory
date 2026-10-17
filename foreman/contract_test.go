package main

import (
	"strings"
	"testing"
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
