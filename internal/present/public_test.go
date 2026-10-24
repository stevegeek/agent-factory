package present

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// withPublic gives x a public review report with a finding at each where.
func withPublic(t *testing.T, x *ticket.Ticket, verdict string, where ...string) {
	var fs []string
	for _, w := range where {
		fs = append(fs, `{"where": "`+w+`", "why": "names a host"}`)
	}
	x.Reports = tk(t, `{"id": "T0999", "reports": {"public": {"verdict": "`+verdict+`", "findings": [`+strings.Join(fs, ", ")+
		`], "summary": "read it"}}}`).Reports
}

func TestPublicReviewColumnAndRole(t *testing.T) {
	if got := ColumnLabel("public"); got != "Public review" {
		t.Errorf("column label %q", got)
	}
	if got := RoleLabel("public"); got != "Public review" {
		t.Errorf("role label %q", got)
	}
	if got, ok := model.StepName("public"); !ok || got != "public review" {
		t.Errorf("step name %q", got)
	}
}

// A public review running shows on Live with what it does; a public
// project's ticket in the gate column with no pass waits for public review.
func TestLiveShowsAPublicReview(t *testing.T) {
	run := flightTicket(t, "T0201", "widgets", "", 60)
	ticket.Move(run, "public", model.ReasonSecurityPassed, "", "", "", ago(10))
	running(run, "public", "local", 10)
	unread := flightTicket(t, "T0202", "widgets", "", 60)
	ticket.Move(unread, "gate", model.ReasonByHuman, "", "", "maintainer", ago(5))
	read := flightTicket(t, "T0203", "widgets", "", 60)
	ticket.Move(read, "gate", model.ReasonPublicPassed, "", "", "", ago(5))
	read.PublicHead = ticket.Some("abc")
	p := flightPlan()
	p.Projects["widgets"] = PlanProject{GateMemory: "900m", HeavyMemory: "1200m", Hygiene: true}
	f := BuildInFlight([]*ticket.Ticket{run, unread, read}, p, nil, Viewer{By: "maintainer", Maintainer: true}, flightNow)
	if len(f.Running) != 1 || f.Running[0].Role != "Public review" || f.Running[0].Doing != PublicDoing {
		t.Fatalf("running %+v", f.Running)
	}
	q := byID(f.Waiting)
	if q["T0202"].For != "Public review" || strings.Contains(q["T0202"].Why, "gate") {
		t.Errorf("unread: %+v", q["T0202"])
	}
	if q["T0203"].For != "the gate" {
		t.Errorf("read: %+v", q["T0203"])
	}
	// on a project that is not public the gate is the gate
	p.Projects["widgets"] = PlanProject{GateMemory: "900m", HeavyMemory: "1200m"}
	if f := BuildInFlight([]*ticket.Ticket{unread}, p, nil, Viewer{}, flightNow); f.Waiting[0].For != "the gate" {
		t.Errorf("not public: %+v", f.Waiting[0])
	}
	// the thread says what it is doing
	if v := Conversation(run, nil, nil, flightNow); !strings.Contains(v.Waiting, "reading what it would publish") {
		t.Errorf("thread: %q", v.Waiting)
	}
}

// A block waits on the desk with its findings, where and why; the story
// tells the read and the person's word.
func TestDeskShowsAPublicBlock(t *testing.T) {
	x := flightTicket(t, "T0204", "widgets", "", 60)
	x.HeadBeforePublic = ticket.Some("0123456789abcdef")
	withPublic(t, x, "block", "branch", "commit abc lib/x.rb:3")
	ticket.Move(x, "needs_human", model.ReasonPublicBlocked, "public review blocked it: 2 finding(s), 1 in text the worker cannot change", "", "", ago(5))
	a, _ := x.Ask.Get()
	if got := Action(x, &a); got != "Public review blocked it" {
		t.Errorf("action %q", got)
	}
	if got := WaitingWhat(x, &a); !strings.Contains(got, "safe to publish") {
		t.Errorf("waiting %q", got)
	}
	facts := map[string]string{}
	for _, f := range Facts(x, &a, nil) {
		facts[f.Label] = f.Value
	}
	if facts["Public review"] != "Blocked: 2 findings in what it would publish" || facts["branch"] != "names a host" ||
		facts["commit abc lib/x.rb:3"] != "names a host" {
		t.Errorf("facts %v", facts)
	}
	ticket.Move(x, "gate", model.ReasonPublicPassed, "safe to publish at 0123456789ab, said by alex", "", "alex", ago(1))
	x.PublicHead = ticket.Some("0123456789abcdef")
	s := BuildStory(x, nil)
	last := s.Steps[len(s.Steps)-1]
	if last.Who != "You" || !strings.Contains(last.What, "said by alex") {
		t.Errorf("story %+v", last)
	}
	// once passed, the findings leave the facts, and the line says whose
	// pass it is rather than the block it overrode
	for _, f := range Facts(x, nil, nil) {
		if f.Label == "branch" {
			t.Errorf("a finding is still a fact after the pass: %+v", f)
		}
		if f.Label == "Public review" && (f.Value != "Passed by alex at 0123456789ab (public review had blocked it)" || f.Tone != "") {
			t.Errorf("the line after a person's pass: %+v", f)
		}
	}
}

// Each public review run in the thread reads from its own report: an
// earlier block stays a block after a rework passed, and only the latest
// run names the head it read.
func TestThreadTellsEachPublicRunFromItsOwnReport(t *testing.T) {
	x := tk(t, `{"id": "T0047", "project": "p", "title": "T", "door": "human", "column": "gate",
		"head_before_public": "0123456789abcdef0123", "public_head": "0123456789abcdef0123",
		"reports": {"public": {"verdict": "pass", "findings": [], "summary": "second read"}},
		"history": [
			{"at": "2026-10-16T10:00:00Z", "event": "finished", "detail": "public t0047-o1", "type": "finished", "stage": "public", "agent": "t0047-o1"},
			{"at": "2026-10-16T10:20:00Z", "event": "finished", "detail": "public t0047-o2", "type": "finished", "stage": "public", "agent": "t0047-o2"}]}`)
	archive := func(id ticket.ID, agent string) []byte {
		if agent == "t0047-o1" {
			return []byte(`{"verdict": "block", "findings": [{"where": "summary", "why": "names a host"}], "summary": "first read"}`)
		}
		return nil
	}
	got := msgs(Conversation(x, nil, archive, noon))
	want := "You|me|**T**|\nPublic review|agent|Blocked: 1 finding in what it would publish. first read|\n" +
		"Public review|agent|Pass: nothing private in what it would publish at 0123456789ab. second read|"
	if got != want {
		t.Errorf("thread:\n%s", got)
	}
}

// The story tells the factory's own first block as public review's, and a
// person's send-back as the person's.
func TestStoryTellsWhoSentAPublicBlockBack(t *testing.T) {
	x := flightTicket(t, "T0206", "widgets", "", 60)
	ticket.Move(x, "ready", model.ReasonPublicChanges, "public review found 1 finding(s); back to the worker", "", "", ago(5))
	ticket.Move(x, "public", model.ReasonByHuman, "", "", "alex", ago(4))
	ticket.Move(x, "ready", model.ReasonPublicChanges, "sent back to work after public review by alex", "", "alex", ago(3))
	var whos []string
	for _, st := range BuildStory(x, nil).Steps {
		if strings.Contains(st.What, "public") || strings.Contains(st.What, "Public") {
			whos = append(whos, st.Who+": "+st.What)
		}
	}
	want := []string{"Public review: Found text this public project must not publish; back to the worker to rewrite its commits.",
		"You: Moved it to Public review.", "You: Sent it back to the worker with public review's findings."}
	if strings.Join(whos, "\n") != strings.Join(want, "\n") {
		t.Errorf("story:\n%s", strings.Join(whos, "\n"))
	}
}

func TestPublicLine(t *testing.T) {
	x := flightTicket(t, "T0205", "widgets", "", 60)
	if PublicLine(x) != "" {
		t.Error("no report, no line")
	}
	withPublic(t, x, "pass")
	x.PublicHead = ticket.Some("0123456789abcdef0123")
	if got := PublicLine(x); got != "Pass: nothing private in what it would publish at 0123456789ab." {
		t.Errorf("pass: %q", got)
	}
	x.Reports = tk(t, `{"id": "T0999", "reports": {"public": {"verdict": "fail"}}}`).Reports
	if got := PublicLine(x); got != "Its report could not be read." {
		t.Errorf("unread: %q", got)
	}
}
