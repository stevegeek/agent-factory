package present

import (
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var huntAt = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

// huntTicket is T0030's whole life: filed by the clock, its task's
// findings verified, waiting on the desk.
func huntTicket(t *testing.T) *ticket.Ticket {
	t.Helper()
	at := func(m int) time.Time { return huntAt.Add(time.Duration(m) * time.Minute) }
	h := tk(t, `{"id": "T0030", "project": "idcodec", "title": "Bug hunt", "body": "Look.", "door": "clock", "by": "factory",
		"column": "ready", "kind": "task", "branch": null, "agent": null,
		"reports": {"task": {"status": "done", "summary": "Six bugs"}, "verify": {"status": "done", "summary": "2 of 3 confirmed"}},
		"created": "2026-10-18T09:00:00+00:00", "history": [{"at": "2026-10-18T09:00:00+00:00", "event": "created",
		"detail": "door=clock column=ready", "type": "created", "door": "clock", "column": "ready", "by": "factory"}]}`)
	ticket.Move(h, "working", model.ReasonStarted, "", "", "", at(1))
	ticket.RecordStart(h, ticket.StartIn{Stage: model.StageTask, Agent: "t0030-k1", Host: "local"}, at(1))
	ticket.RecordFinish(h, model.StageTask, "t0030-k1", at(20))
	ticket.Move(h, "verifying", model.ReasonFindingsReported, "task done with 3 findings: Six bugs; a verifier checks them next", "", "", at(20))
	ticket.RecordStart(h, ticket.StartIn{Stage: model.StageVerify, Agent: "t0030-f1", Host: "local"}, at(21))
	ticket.RecordFinish(h, model.StageVerify, "t0030-f1", at(40))
	h.Findings = []ticket.Finding{
		{Title: "Raise a format error for nil", Severity: "low", Kind: "bug", Verdict: ticket.VerdictConfirmed, Body: "b", Evidence: "e"},
		{Title: "Keep hex leading zeros", Severity: "medium", Kind: "bug", BehaviourChange: true, Verdict: ticket.VerdictConfirmed, Body: "b"},
		{Title: "Make decode_hex errors consistent", Verdict: ticket.VerdictDuplicate, Duplicate: "PR #27", Body: "b"},
	}
	ticket.Move(h, "needs_human", model.ReasonFindingsVerified, "findings verified: 2 of 3 confirmed: 2 of 3 confirmed", "", "", at(40))
	return h
}

func TestTheFindingsAskHasItsWords(t *testing.T) {
	h := huntTicket(t)
	a := AskFor(h)
	if a == nil || a.Kind != string(model.AskFindings) {
		t.Fatalf("ask %+v", a)
	}
	if got := Headline(h, a); got != "File 2 confirmed findings as tickets?" {
		t.Errorf("headline %q", got)
	}
	if got := Action(h, a); got != "File findings · 2 of 3 confirmed" {
		t.Errorf("action %q", got)
	}
	if got := WaitingWhat(h, a); !strings.Contains(got, "Tick the findings to file") {
		t.Errorf("waiting %q", got)
	}
}

func TestTheHuntsStoryTellsTheFindingsFlow(t *testing.T) {
	h := huntTicket(t)
	s := BuildStory(h, map[ticket.ID]*ticket.Ticket{h.ID: h})
	got := fmtSteps(s)
	for _, want := range []string{"Clock: Filed a scheduled check: Bug hunt", "Task agent: Answered. Six bugs",
		"Dispatcher: The task reported 3 findings; a verifier checks them before any becomes a ticket.",
		"Verifier: Confirmed 2 of 3 findings (1 duplicate).", "Waiting for you"} {
		if !strings.Contains(got, want) {
			t.Errorf("story lacks %q:\n%s", want, got)
		}
	}
	// filed and closed: the story names the tickets
	c1, _ := ticket.FileFinding("T0040", h, 1, "change", "maintainer", huntAt.Add(time.Hour))
	ticket.FileFinding("T0041", h, 2, "intent", "maintainer", huntAt.Add(time.Hour))
	ticket.CloseFindings(h, "maintainer", "", huntAt.Add(time.Hour))
	got = fmtSteps(BuildStory(h, map[ticket.ID]*ticket.Ticket{h.ID: h}))
	if !strings.Contains(got, "You: Filed the findings as T0040 (change), T0041 (intent).") {
		t.Errorf("closed story:\n%s", got)
	}
	// the filed ticket's own story starts from the hunt
	cs := fmtSteps(BuildStory(c1, map[ticket.ID]*ticket.Ticket{h.ID: h, c1.ID: c1}))
	if !strings.Contains(cs, "You: Filed finding 1 of T0030 (verified).") {
		t.Errorf("child story:\n%s", cs)
	}
}

func TestOtherFindingsMoves(t *testing.T) {
	h := huntTicket(t)
	ticket.CloseFindings(h, "maintainer", "", huntAt.Add(time.Hour))
	if got := fmtSteps(BuildStory(h, nil)); !strings.Contains(got, "You: Closed it; no findings filed.") {
		t.Errorf("dismissed:\n%s", got)
	}
	b := huntTicket(t)
	ticket.Move(b, "verifying", model.ReasonVerifyRequested, "verify the findings in its result; a verifier checks each one", "", "maintainer", huntAt.Add(time.Hour))
	ticket.Move(b, "needs_human", model.ReasonVerifierBlocked, "verifier blocked: no checkout", "", "", huntAt.Add(2*time.Hour))
	got := fmtSteps(BuildStory(b, nil))
	if !strings.Contains(got, "You: Sent its findings to the verifier.") || !strings.Contains(got, "Verifier blocked: no checkout.") {
		t.Errorf("verify asked, then blocked:\n%s", got)
	}
}
