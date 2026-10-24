package present

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var flightNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func ago(m int) time.Time { return flightNow.Add(-time.Duration(m) * time.Minute) }

// flightTicket is a factory ticket filed into triage at filed minutes ago.
func flightTicket(t *testing.T, id, project, kind string, filed int) *ticket.Ticket {
	t.Helper()
	k := "null"
	if kind != "" {
		k = `"` + kind + `"`
	}
	at := clock.Stamp(ago(filed))
	return tk(t, fmt.Sprintf(`{"id": %q, "project": %q, "title": "Ticket %s", "body": "", "door": "cli", "by": "factory",
		"column": "triage", "kind": %s, "agent": null, "created": %q, "history": [{"at": %q, "event": "created",
		"detail": "door=cli column=triage", "type": "created", "door": "cli", "column": "triage", "by": "factory"}]}`, id, project, id, k, at, at))
}

func running(t *ticket.Ticket, stage, host string, started int) {
	t.Agent = ticket.Some(ticket.Agent{Name: strings.ToLower(string(t.ID)) + "-x1", Stage: stage, Host: host, Started: clock.Stamp(ago(started))})
}

func flightPlan() Plan {
	return Plan{Known: true, MaxAgents: 2,
		Projects: map[string]PlanProject{
			"widgets": {GateMemory: "900m", HeavyMemory: "1200m"},
			"big":     {HeavyGate: true, GateMemory: "2g", HeavyMemory: "1200m"},
			"quiet":   {Paused: "switched off in Foreman by robin"},
			"notes":   {Board: true},
		},
		Hosts: []PlanHost{
			{Name: "helpera", Slots: 1, Stages: []string{"work", "review"}, Down: "is paused until 12:30 (a usage limit)"},
			{Name: "desk", Slots: 1, Stages: []string{"gate"}, Pull: true},
		}}
}

func byID(qs []Queued) map[ticket.ID]Queued {
	m := map[ticket.ID]Queued{}
	for _, q := range qs {
		m[q.T.ID] = q
	}
	return m
}

// Waiting: every factory ticket the tick would start an agent or a gate
// on and no agent is on, oldest wait first, each with the reason the start
// loop gives; running: the agents; board tickets and board projects never.
func TestInFlightClassifiesAndExplains(t *testing.T) {
	review := flightTicket(t, "T0101", "widgets", "", 300)
	ticket.Move(review, "review", model.ReasonWorkDone, "", "", "", ago(40))
	pinned := flightTicket(t, "T0102", "widgets", "", 300)
	ticket.Move(pinned, "ready", model.ReasonByHuman, "", "", "maintainer", ago(20))
	pinned.Host = ticket.Some("helpera")
	heavy := flightTicket(t, "T0103", "big", "", 300)
	ticket.Move(heavy, "gate", model.ReasonSecurityPassed, "", "", "", ago(60))
	paused := flightTicket(t, "T0104", "quiet", "task", 300)
	ticket.Move(paused, "ready", model.ReasonByHuman, "", "", "maintainer", ago(90))
	work := flightTicket(t, "T0105", "widgets", "", 300)
	ticket.Move(work, "working", model.ReasonStarted, "", "", "", ago(15))
	running(work, "work", "local", 15)
	triage := flightTicket(t, "T0106", "widgets", "", 5) // waits since it was filed
	board := flightTicket(t, "T0107", "widgets", "", 5)
	board.Mode = ticket.Some("board")
	boardProject := flightTicket(t, "T0108", "notes", "", 5)
	proposal := flightTicket(t, "T0109", "widgets", "proposal", 5)
	ticket.Move(proposal, "ready", model.ReasonProposalsReady, "", "", "", ago(4))

	ts := []*ticket.Ticket{review, pinned, heavy, paused, work, triage, board, boardProject, proposal}
	f := BuildInFlight(ts, flightPlan(), nil, you, flightNow)

	var order []string
	for _, q := range f.Waiting {
		order = append(order, string(q.T.ID))
	}
	if got := strings.Join(order, " "); got != "T0104 T0103 T0101 T0102 T0106" {
		t.Fatalf("waiting, oldest first: %s", got)
	}
	w := byID(f.Waiting)
	for id, want := range map[ticket.ID]struct {
		for_, why string
		held      bool
		waited    string
	}{
		// the start loop in board order: T0101's reviewer takes this host's
		// last slot (one runs), so the heavy gate after it goes to the pull
		// host that takes gates that would wait here, and triage finds no slot
		"T0101": {"Reviewer", "starts at the next tick, here", false, "40 min"},
		"T0102": {"Worker", "pinned to helpera, which is paused until 12:30 (a usage limit)", true, "20 min"},
		"T0103": {"the gate", "starts at the next tick, as a gate job on desk", false, "1 h"},
		"T0104": {"Task agent", "the project is switched off in Foreman by robin", true, "1 h"},
		"T0106": {"Triage", "waiting for a free slot: here 2 of 2 busy", true, "5 min"},
	} {
		q := w[id]
		if q.For != want.for_ || q.Why != want.why || q.Held != want.held || q.Waited != want.waited {
			t.Errorf("%s: %+v, want %+v", id, q, want)
		}
	}
	if len(f.Running) != 1 || f.Running[0].T != work || f.Running[0].Role != "Worker" || f.Running[0].Host != "" || f.Running[0].Elapsed != "15 min" {
		t.Errorf("running %+v", f.Running)
	}
	for _, m := range f.Moved {
		if m.T == board || m.T == boardProject {
			t.Errorf("a board ticket moved in flight: %+v", m)
		}
	}
}

// A heavy gate with agents beside it waits for the host to be idle, and
// holds it: nothing after it starts here; a pull host that takes gates
// takes it instead when it is free; a pull host whose vitals are stale is
// down; a stage no host runs says so.
func TestInFlightHeavyGatesAndHosts(t *testing.T) {
	work := flightTicket(t, "T0201", "widgets", "", 100)
	running(work, "work", "", 10)
	heavy := flightTicket(t, "T0202", "big", "", 100)
	ticket.Move(heavy, "gate", model.ReasonSecurityPassed, "", "", "", ago(30))
	after := flightTicket(t, "T0203", "widgets", "", 100)
	ticket.Move(after, "security", model.ReasonReviewPassed, "", "", "", ago(20))
	task := flightTicket(t, "T0204", "nowhere", "task", 100) // no checkout here
	ticket.Move(task, "ready", model.ReasonByHuman, "", "", "maintainer", ago(10))

	p := flightPlan()
	p.Hosts[1].Down = "may be away: its last report is from 20 Oct 11:00"
	w := byID(BuildInFlight([]*ticket.Ticket{work, heavy, after, task}, p, nil, you, flightNow).Waiting)
	if q := w["T0202"]; !q.Held || q.Why != "a heavy gate (gate memory 2g, above heavy_memory 1200m): it waits for this host to be idle, and 1 agent run here" {
		t.Errorf("heavy gate: %+v", q)
	}
	if q := w["T0203"]; q.Why != "waiting for a free slot: here held for T0202's heavy gate" {
		t.Errorf("after the heavy gate: %+v", q)
	}
	if q := w["T0204"]; q.Why != "no host runs it: this project has no checkout here, and no [hosts] host lists task" {
		t.Errorf("task with no host: %+v", q)
	}

	p.Hosts[1].Down = ""
	w = byID(BuildInFlight([]*ticket.Ticket{work, heavy, after}, p, nil, you, flightNow).Waiting)
	if q := w["T0202"]; q.Held || q.Why != "starts at the next tick, as a gate job on desk" {
		t.Errorf("gate job: %+v", q)
	}
	if q := w["T0203"]; q.Why != "starts at the next tick, here" {
		t.Errorf("after a gate job: %+v", q)
	}

	// config unread: no reason is made up
	if q := byID(BuildInFlight([]*ticket.Ticket{heavy}, Plan{}, nil, you, flightNow).Waiting)["T0202"]; q.Why != "" || q.Held {
		t.Errorf("without config: %+v", q)
	}
}

// Just moved: the moves of the last day across the viewer's tickets,
// newest first, capped; who moved it (the viewer's own marked, older
// untyped history read too) and why.
func TestInFlightJustMoved(t *testing.T) {
	a := flightTicket(t, "T0301", "widgets", "", 3000)
	ticket.Move(a, "needs_human", model.ReasonTriageAccepted, "triage accepted: looks fine", "", "", ago(2000)) // too old
	ticket.Move(a, "ready", model.ReasonByHuman, "go ahead", "", "maintainer", ago(30))
	ticket.Move(a, "working", model.ReasonStarted, "", "", "", ago(25))
	b := flightTicket(t, "T0302", "widgets", "", 3000)
	ticket.Move(b, "ready", model.ReasonByHuman, "", "", "robin", ago(10))
	c := flightTicket(t, "T0303", "widgets", "", 3000)
	c.History = append(c.History, ticket.Entry{At: clock.Stamp(ago(5)), Event: "move", Detail: "triage -> ready: by human"})
	c.Column = "ready"
	hidden := flightTicket(t, "T0304", "secret", "", 3000)
	ticket.Move(hidden, "ready", model.ReasonByHuman, "", "", "maintainer", ago(1))
	running(hidden, "work", "local", 1)

	ts := []*ticket.Ticket{a, b, c, hidden}
	sees := func(p string) bool { return p == "widgets" }
	f := BuildInFlight(ts, flightPlan(), sees, you, flightNow)
	var got []string
	for _, m := range f.Moved {
		got = append(got, fmt.Sprintf("%s %s %s→%s %s: %s mine=%v", m.Time, m.T.ID, m.From, m.To, m.Who, m.Why, m.Mine))
	}
	want := []string{
		"11:55 T0303 Triage→Ready you:  mine=true",
		"11:50 T0302 Triage→Ready robin:  mine=false",
		"11:35 T0301 Ready→Working the factory: started mine=false",
		"11:30 T0301 Needs you→Ready you: go ahead mine=true",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("just moved\n got %s\nwant %s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(f.Running) != 0 {
		t.Errorf("another project's agent shown: %+v", f.Running)
	}
	// robin's own move is hers on her page
	for _, m := range BuildInFlight(ts, flightPlan(), sees, Viewer{By: "robin"}, flightNow).Moved {
		if m.Mine != (m.T == b) {
			t.Errorf("robin's: %+v", m)
		}
	}
	// the slots the hidden project's agents take still count
	q := flightTicket(t, "T0305", "widgets", "", 5)
	if w := byID(BuildInFlight([]*ticket.Ticket{hidden, q}, flightPlan(), sees, you, flightNow).Waiting)["T0305"]; w.Why != "starts at the next tick, here" {
		t.Errorf("T0305: %+v", w)
	}
	full := flightTicket(t, "T0306", "secret", "", 5)
	running(full, "review", "local", 1)
	if w := byID(BuildInFlight([]*ticket.Ticket{hidden, full, q}, flightPlan(), sees, you, flightNow).Waiting)["T0305"]; w.Why != "waiting for a free slot: here 2 of 2 busy" {
		t.Errorf("T0305 behind hidden agents: %+v", w)
	}

	var many []*ticket.Ticket
	for i := range MovedMax + 5 {
		x := flightTicket(t, fmt.Sprintf("T04%02d", i), "widgets", "", 3000)
		ticket.Move(x, "ready", model.ReasonByHuman, "", "", "maintainer", ago(100-i))
		many = append(many, x)
	}
	m := BuildInFlight(many, flightPlan(), nil, you, flightNow).Moved
	if len(m) != MovedMax || m[0].T.ID != "T0434" {
		t.Errorf("capped newest first: %d, first %s", len(m), m[0].T.ID)
	}
}
