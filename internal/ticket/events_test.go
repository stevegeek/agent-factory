package ticket

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/testkit/pyticket"
)

var testNow = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

func newIn(project, title, body, door, column, by string) NewIn {
	return NewIn{Project: project, Title: title, Body: body, Door: door, Column: column, By: by}
}

func fields(t *testing.T, s string) jsonx.Object {
	o, ok := parse(t, s).Object()
	if !ok {
		t.Fatalf("%s is not an object", s)
	}
	return o
}

// save is store.Save's part that this package can do: stamp updated, encode.
func save(tk *Ticket, now time.Time) []byte {
	tk.Updated = clock.Stamp(now)
	return Encode(tk)
}

// New and the mutators make the same files as new_ticket, move, add_note
// and event (testdata/new/gen.py), but for the events typed since.
func TestNewTicketsAreByteIdenticalToPython(t *testing.T) {
	cases := map[string]func() *Ticket{
		"T0001": func() *Ticket {
			tk := New("T0001", newIn("gem", "Fix", "b", "human", "ready", "alice"), testNow)
			Move(tk, "needs_human", model.ReasonTriageAccepted, "triage accepted; move to ready to start work", "", "", testNow)
			save(tk, testNow)
			AddNote(tk, "handle the empty case", "alice", nil, testNow)
			AddNote(tk, strings.Repeat("which one? ", 30), "maintainer", &NoteAnswer{Question: "which one?"}, testNow)
			Move(tk, "ready", model.ReasonByHuman, "by human", "", "alice", testNow)
			RecordStart(tk, StartIn{Stage: model.StageWork, Agent: "t0001-w1", Host: "helper", Model: "claude-opus-5-5", Branch: "bot/fix"}, testNow)
			RecordStart(tk, StartIn{Stage: model.StageTriage, Agent: "t0001-t1", Host: model.LocalHost}, testNow)
			RecordFinish(tk, model.StageWork, "t0001-w1", testNow)
			Event(tk, "error", "dispatcher error: boom", "a rule", Errored{Text: "boom"}, testNow)
			return tk
		},
		"T0002": func() *Ticket {
			in := newIn("gem", "Idea", "idea", "human", "ready", "")
			in.Fields = fields(t, `{"kind": "intent"}`)
			return New("T0002", in, testNow)
		},
		"T0003": func() *Ticket {
			return New("T0003", newIn(model.NoticeProject, "Update something", "A newer release is out.", "clock", "needs_human", ""), testNow)
		},
		"T0004": func() *Ticket {
			in := newIn("b", "Board item", "body", "agent", "inbox", "agent-a")
			in.Mode = model.ModeBoard
			in.Fields = fields(t, `{"labels": ["x"], "links": [], "idempotency": {"by": "agent-a", "key": "k1", "digest": "`+strings.Repeat("d", 64)+`"}, "parent": "T0001"}`)
			return New("T0004", in, testNow)
		},
		"T0005": func() *Ticket {
			in := newIn("gem", "Task", "t", "human", "needs_human", "")
			in.Fields = fields(t, `{"kind": "task"}`)
			return New("T0005", in, testNow)
		},
	}
	for id, make := range cases {
		want := mustRead(t, "testdata/new/"+id+".json")
		if got := pyticket.History(string(save(make(), testNow))); got != string(want) {
			t.Errorf("%s:\n%s", id, testkit.Diff(string(want), got))
		}
	}
}

// port: TT TypedEventTest.test_writers_carry_types_and_the_same_details
func TestWritersCarryTypesAndTheSameDetails(t *testing.T) {
	tk := New("T0001", newIn("p", "Fix", "b", "human", "ready", "alice"), testNow)
	h := tk.History[0]
	if h.Event != "created" || h.Detail != "door=human column=ready" || h.Type != model.EventCreated ||
		h.Data != (Created{Door: "human", Column: "ready", By: "alice"}) {
		t.Errorf("created: %+v", h)
	}
	Move(tk, "needs_human", model.ReasonTriageAccepted, "triage accepted; move to ready to start work", "", "", testNow)
	h = tk.History[len(tk.History)-1]
	if h.Detail != "ready -> needs_human: triage accepted; move to ready to start work" || h.Type != model.EventMoved ||
		h.Data != (Moved{From: "ready", To: "needs_human", Reason: model.ReasonTriageAccepted, By: "factory"}) {
		t.Errorf("moved: %+v", h)
	}
	AddNote(tk, "handle the empty case", "alice", nil, testNow)
	h = tk.History[len(tk.History)-1]
	if h.Event != "note" || h.Detail != "handle the empty case" || h.Type != model.EventNoted || h.Data != (Noted{By: "alice", Note: 0}) {
		t.Errorf("noted: %+v", h)
	}
	if got := tk.Logs(); !reflect.DeepEqual(got, []string{"T0001 created door=human column=ready",
		"T0001 move ready -> needs_human: triage accepted; move to ready to start work", "T0001 note handle the empty case"}) {
		t.Errorf("logs %q", got)
	}
	if len(tk.TakeLogs()) != 3 || len(tk.Logs()) != 0 {
		t.Error("TakeLogs does not empty the queue")
	}
}

// port: TT TypedEventTest.test_a_start_and_finish_say_stage_agent_host_model_and_branch
func TestStartAndFinishSayStageAgentHostModelAndBranch(t *testing.T) {
	tk := New("T0001", newIn("p", "Fix it", "b", "human", "ready", ""), testNow)
	Move(tk, "working", model.ReasonStarted, "", "", "", testNow)
	RecordStart(tk, StartIn{Stage: model.StageWork, Agent: "t0001-w1", Host: model.LocalHost, Model: "claude-opus-5-5", Branch: "bot/fix-it"}, testNow)
	h := tk.History[len(tk.History)-1]
	if h.Detail != "work t0001-w1 on bot/fix-it (claude-opus-5-5)" ||
		h.Data != (Started{Stage: "work", Agent: "t0001-w1", Host: "local", Model: "claude-opus-5-5", Branch: "bot/fix-it"}) {
		t.Errorf("start: %+v", h)
	}
	if mv := tk.History[len(tk.History)-2].Data.(Moved); mv.Reason != model.ReasonStarted {
		t.Errorf("the move before the start: %+v", mv)
	}
	if Starts(tk, model.StageWork) != 1 {
		t.Errorf("Starts = %d", Starts(tk, model.StageWork))
	}
	RecordFinish(tk, model.StageWork, "t0001-w1", testNow)
	Move(tk, "review", model.ReasonWorkDone, "", "", "", testNow)
	fin, mv := tk.History[len(tk.History)-2], tk.History[len(tk.History)-1]
	if fin.Detail != "work t0001-w1" || fin.Type != model.EventFinished || fin.Data != (Finished{Stage: "work", Agent: "t0001-w1"}) {
		t.Errorf("finished: %+v", fin)
	}
	if mv.Detail != "working -> review" || mv.Data.(Moved).Reason != model.ReasonWorkDone {
		t.Errorf("move: %+v", mv)
	}
	// a helper host and no branch or model
	RecordStart(tk, StartIn{Stage: model.StageReview, Agent: "t0001-r1", Host: "helper"}, testNow)
	if h := tk.History[len(tk.History)-1]; h.Detail != "review t0001-r1 @helper" {
		t.Errorf("remote start detail %q", h.Detail)
	}
	enc := string(Encode(tk))
	if strings.Contains(enc, `"model": ""`) || strings.Contains(enc, `"branch": ""`) {
		t.Errorf("an empty model or branch was written:\n%s", enc)
	}
}

// port: TT TypedEventTest.test_old_history_without_fields_still_counts_starts_and_finds_asks
func TestOldHistoryStillCountsStartsAndFindsAsks(t *testing.T) {
	tk := mustDecode(t, []byte(`{"id": "T0001", "history": [
		{"at": "x", "event": "start", "detail": "work t0001-w1 on b"},
		{"at": "x", "event": "start", "detail": "review t0001-r1"},
		{"at": "x", "event": "start", "detail": "tasks t0001-k1", "stage": "task"}]}`))
	if w, r, k := Starts(tk, model.StageWork), Starts(tk, model.StageReview), Starts(tk, model.StageTask); w != 1 || r != 1 || k != 1 {
		t.Errorf("starts work %d, review %d, task %d", w, r, k)
	}
	// asked_on reads the typed ticket, or the "<id>: " wording of old history
	p, legacy := Interpret(Entry{At: "x", Event: "asked", Detail: "T0002: why?"})
	if !legacy || p != (Asked{Ticket: "T0002", Text: "why?"}) {
		t.Errorf("legacy asked: %+v %v", p, legacy)
	}
	tk = mustDecode(t, []byte(`{"id": "T0001", "history": [{"at": "x", "event": "asked", "detail": "T0009: no", "type": "asked", "ticket": "T0002"}]}`))
	p, legacy = Interpret(tk.History[0])
	if legacy || p.(Asked).Ticket != "T0002" {
		t.Errorf("typed asked: %+v %v", p, legacy)
	}
}

// work rounds restart at a passed review, as the board stores it;
// Starts still counts the ticket's life.
func TestWorkRoundsRestartAtAPassedReview(t *testing.T) {
	tk := mustDecode(t, []byte(`{"id": "T0001", "history": [
		{"at": "x", "event": "start", "detail": "work t0001-w1 on b"},
		{"at": "x", "event": "move", "detail": "review -> ready: review asked for changes", "type": "moved", "from": "review", "to": "ready", "reason": "review_changes", "by": "factory"},
		{"at": "x", "event": "start", "detail": "work t0001-w2 on b"},
		{"at": "x", "event": "move", "detail": "review -> security", "type": "moved", "from": "review", "to": "security", "reason": "review_passed", "by": "factory"},
		{"at": "x", "event": "start", "detail": "work t0001-w3 on b"}]}`))
	if w, r := Starts(tk, model.StageWork), WorkRounds(tk); w != 3 || r != 1 {
		t.Errorf("starts %d, rounds %d", w, r)
	}
	// an old untyped move names no reason: the rounds are the life's
	tk = mustDecode(t, []byte(`{"id": "T0001", "history": [
		{"at": "x", "event": "start", "detail": "work t0001-w1 on b"},
		{"at": "x", "event": "move", "detail": "review -> security"},
		{"at": "x", "event": "start", "detail": "work t0001-w2 on b"}]}`))
	if r := WorkRounds(tk); r != 2 {
		t.Errorf("legacy rounds %d", r)
	}
	// a start put straight back in ready (a pull host gave it back, a usage
	// limit) never did the work: it is not a round; one sent to a person is
	tk = mustDecode(t, []byte(`{"id": "T0001", "history": [
		{"at": "x", "event": "start", "detail": "work t0001-w1 on b"},
		{"at": "x", "event": "move", "detail": "working -> ready: desktop gave t0001-w1 back", "type": "moved", "from": "working", "to": "ready", "reason": "agent_lost", "by": "factory"},
		{"at": "x", "event": "start", "detail": "work t0001-w2 on b"},
		{"at": "x", "event": "move", "detail": "working -> ready: pi hit a usage limit", "type": "moved", "from": "working", "to": "ready", "reason": "usage_limit", "by": "factory"},
		{"at": "x", "event": "start", "detail": "work t0001-w3 on b"},
		{"at": "x", "event": "move", "detail": "working -> needs_human: agent t0001-w3 disappeared", "type": "moved", "from": "working", "to": "needs_human", "reason": "agent_lost", "by": "factory"},
		{"at": "x", "event": "start", "detail": "work t0001-w4 on b"}]}`))
	if w, r := Starts(tk, model.StageWork), WorkRounds(tk); w != 4 || r != 2 {
		t.Errorf("starts %d, rounds %d", w, r)
	}
}

// port: TT TypedEventTest.test_every_move_in_the_dispatcher_names_its_reason
// Move takes a model.Reason, so the compiler checks what the Python source
// grep did; a move's typed event always carries the reason it was given.
func TestEveryMoveNamesItsReason(t *testing.T) {
	tk := New("T0001", newIn("p", "x", "", "human", "ready", ""), testNow)
	for _, r := range model.Reasons() {
		Move(tk, "ready", r.Reason, "", "", "", testNow)
		if got := tk.History[len(tk.History)-1].Data.(Moved).Reason; got != r.Reason {
			t.Errorf("Move(%q) recorded %q", r.Reason, got)
		}
	}
}

// the contract tickets need no legacy reader; the old boards still go
// through it, and it reads their starts and moves.
func TestInterpretOnTheLegacyFixtures(t *testing.T) {
	for _, f := range readFixtures(t, "testdata/legacy/contract/T*.json", false) {
		tk := mustDecode(t, f.data)
		for i, e := range tk.History {
			if p, legacy := Interpret(e); legacy && e.Type == "" && p != nil {
				t.Errorf("%s history[%d] (%s) is untyped", f.name, i, e.Event)
			} else if !legacy && p == nil {
				t.Errorf("%s history[%d]: typed with no payload", f.name, i)
			}
		}
	}
	starts, moves := 0, 0
	for _, f := range readFixtures(t, "testdata/legacy/board/T*.json", true) {
		tk := mustDecode(t, f.data)
		for _, e := range tk.History {
			p, legacy := Interpret(e)
			switch v := p.(type) {
			case Started:
				if legacy {
					starts++
				}
				if v.Stage == "" || v.Agent == "" || v.Host == "" {
					t.Errorf("%s: start %q read as %+v", f.name, e.Detail, v)
				}
			case Moved:
				if legacy {
					moves++
				}
				if v.From == "" || v.To == "" {
					t.Errorf("%s: move %q read as %+v", f.name, e.Detail, v)
				}
			}
			if e.Event == "start" && p == nil || e.Event == "move" && p == nil {
				t.Errorf("%s: %s %q not read", f.name, e.Event, e.Detail)
			}
		}
	}
	if starts == 0 || moves == 0 {
		t.Errorf("the old boards gave %d legacy starts and %d legacy moves", starts, moves)
	}
}

func TestInterpretReadsEveryLegacyForm(t *testing.T) {
	cases := []struct {
		event, detail string
		want          Payload
	}{
		{"start", "work t0001-w1 on bot/x @helper (claude-opus-5-5)", Started{Stage: "work", Agent: "t0001-w1", Branch: "bot/x", Host: "helper", Model: "claude-opus-5-5"}},
		{"start", "triage t0001-t1", Started{Stage: "triage", Agent: "t0001-t1", Host: "local"}},
		{"start", "nonsense", nil},
		{"finished", "work t0001-w1", Finished{Stage: "work", Agent: "t0001-w1"}},
		{"move", "needs_human -> ready: by human", Moved{From: "needs_human", To: "ready", Reason: model.ReasonByHuman, Text: "by human"}},
		{"move", "working -> needs_human: work asks: which one?", Moved{From: "working", To: "needs_human", Reason: model.ReasonAgentQuestion, Text: "work asks: which one?"}},
		{"move", "voting -> done: panel kept T0006; dropped 1", Moved{From: "voting", To: "done", Reason: model.ReasonPanelDone, Text: "panel kept T0006; dropped 1"}},
		{"move", "é -> ü", Moved{From: "é", To: "ü"}},
		{"move", "ready->working", nil},
		{"answered", "T0002: yes: really", Answered{Ticket: "T0002", Text: "yes: really"}},
		{"memory", "worker memory abc1234: tickets/T0001.md, notes.md …", Memory{Role: "worker", Commit: "abc1234", Files: []string{"tickets/T0001.md", "notes.md"}}},
		{"memory", "no memory came back from helper; this run's notes are lost", Memory{LostOn: "helper"}},
		{"memory", "worker memory is 600 KB, over its limit: tidy it up", Memory{Role: "worker", OverLimitKB: 600}},
		{"created", "door=github column=triage", Created{Door: "github", Column: "triage"}},
		{"error", "dispatcher error: boom", Errored{Text: "dispatcher error: boom"}},
		{"vote", "2/3 in", Voted{K: 2, N: 3}},
		{"host", "helper", Pinned{Host: "helper"}},
		{"review requested", "a, b", ReviewRequested{Reviewers: []string{"a", "b"}}},
		{"filed", "T0009: pre-existing, found in review: x", Filed{Ticket: "T0009", Text: "pre-existing, found in review: x"}},
		{"gate", "main moved; the checks pass again with it merged in (abc1234)", Gated{Base: "main", Commit: "abc1234"}},
		{"gate", "something else", nil},
		{"panel", "mean 4.0, spread 1, scores [4, 4]", PanelScored{}},
		{"pr", "approved; not merged yet: checks", PRNote{Why: "approved; not merged yet: checks"}},
	}
	for _, c := range cases {
		got, legacy := Interpret(Entry{Event: c.event, Detail: c.detail})
		if !legacy || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Interpret(%s %q) = %#v, %v; want %#v", c.event, c.detail, got, legacy, c.want)
		}
	}
	// a legacy panel event is kept when its rule says so
	if got, _ := Interpret(Entry{Event: "panel", Detail: "mean 4.0", Rule: "kept: mean 4.0 (3.5 needed)"}); got != (PanelScored{Kept: true}) {
		t.Errorf("kept panel: %#v", got)
	}
	// a typed move gets its text from the detail
	got, legacy := Interpret(Entry{Event: "move", Detail: "ready -> working: go", Type: model.EventMoved, Data: Moved{From: "ready", To: "working", Reason: model.ReasonStarted}})
	if legacy || got.(Moved).Text != "go" {
		t.Errorf("typed move: %+v %v", got, legacy)
	}
}

// the events Python wrote untyped are written typed, the detail as it
// was, and read back to the same payload with no legacy reader.
func TestOnceUntypedEventsAreWrittenTyped(t *testing.T) {
	tk := mustDecode(t, []byte(`{"id": "T0001", "history": []}`))
	Event(tk, "pr", "PR #3 back to draft while it is reworked", "", PRNote{Action: PRDraft, Number: 3}, testNow)
	want := "{\n  \"id\": \"T0001\",\n  \"history\": [\n    {\n      \"at\": \"2026-10-18T10:00:00+00:00\",\n      \"event\": \"pr\",\n      \"detail\": \"PR #3 back to draft while it is reworked\",\n      \"type\": \"pr\",\n      \"action\": \"draft\",\n      \"number\": 3\n    }\n  ]\n}"
	if got := string(Encode(tk)); got != want {
		t.Errorf("got %s", got)
	}
	cases := []struct {
		event, detail string
		p             Payload
	}{
		{"error", "github: boom", Errored{Text: "boom"}},
		{"pr", "approved; not merged yet: checks", PRNote{Action: PRMergeWaiting, Number: 3, Why: "checks"}},
		{"gate", "main moved; the checks pass again with it merged in (abc1234)", Gated{Base: "main", Commit: "abc1234def"}},
		{"filed", "T0009: finding 1, a change: x", Filed{Ticket: "T0009", Text: "finding 1, a change: x"}},
		{"vote", "1/3 in", Voted{K: 1, N: 3}},
		{"panel", "mean 4.0, spread 1, scores [4, 4]", PanelScored{Kept: false}},
		{"host", "helper", Pinned{Host: "helper"}},
		{"review requested", "a, b", ReviewRequested{Reviewers: []string{"a", "b"}}},
	}
	for _, c := range cases {
		tk := mustDecode(t, []byte(`{"id": "T0001", "history": []}`))
		Event(tk, c.event, c.detail, "", c.p, testNow)
		back := mustDecode(t, Encode(tk))
		e := back.History[0]
		got, legacy := Interpret(e)
		if e.Event != c.event || e.Detail != c.detail || e.Type != c.p.EventType() || legacy || !reflect.DeepEqual(got, c.p) {
			t.Errorf("%s: %+v read back as %#v, %v", c.event, e, got, legacy)
		}
	}
}

// MOVE_DETAIL against Python's own answers (internal/textx's regex golden).
func TestMoveDetailMatchesPython(t *testing.T) {
	b, err := os.ReadFile("../textx/testdata/py/regex.json")
	if err != nil {
		t.Skipf("regex golden: %v", err)
	}
	var g struct {
		Patterns []struct {
			Name  string
			Cases []struct {
				In  string
				Out json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range g.Patterns {
		if p.Name != "move_detail" {
			continue
		}
		for _, c := range p.Cases {
			n++
			var out []*string
			if err := json.Unmarshal(c.Out, &out); err != nil {
				t.Fatal(err)
			}
			m := moveDetail.FindStringSubmatchIndex(c.In)
			if (m == nil) != (out == nil) {
				t.Errorf("%q: match %v, Python %v", c.In, m != nil, out != nil)
				continue
			}
			for i := 0; m != nil && i < len(out); i++ {
				got := (*string)(nil)
				if m[2*i] >= 0 {
					s := c.In[m[2*i]:m[2*i+1]]
					got = &s
				}
				if (got == nil) != (out[i] == nil) || got != nil && *got != *out[i] {
					t.Errorf("%q group %d: %v, Python %v", c.In, i, got, out[i])
				}
			}
		}
	}
	if n == 0 {
		t.Error("no move_detail cases in the golden")
	}
}
