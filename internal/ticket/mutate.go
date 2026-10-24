package ticket

import (
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// The mutators that record history. Each changes the typed views only and
// queues the log line Python's event() writes; the store saves the ticket
// and then writes the lines.

// Event appends a history entry {at, event, detail, rule?, type?, fields...}
// (event). p is the typed part, or nil for an untyped entry.
func Event(t *Ticket, kind, detail, rule string, p Payload, now time.Time) {
	e := Entry{At: clock.Stamp(now), Event: kind, Detail: detail, Rule: rule}
	if p != nil {
		e.Type, e.Data = p.EventType(), p
	}
	t.History = append(t.History, e)
	t.logs = append(t.logs, textx.RStrip(string(t.ID)+" "+kind+" "+detail))
}

// Move puts the ticket in column to, records a typed moved event whose
// detail is "<old> -> <to>[: detail]", and recomputes its ask (move).
// by is who moved it ("" is the factory).
func Move(t *Ticket, to string, reason model.Reason, detail, rule, by string, now time.Time) {
	if by == "" {
		by = "factory"
	}
	old := t.Column
	t.Column = to
	d := old + " -> " + to
	if detail != "" {
		d += ": " + detail
	}
	Event(t, "move", d, rule, Moved{From: old, To: to, Reason: reason, By: by}, now)
	t.SetAsk(AskFor(t, old, detail, clock.Stamp(now), reason))
}

// NoteAnswer marks a note that answers an agent's question.
type NoteAnswer struct {
	Question string
	Answer   Opt[string] // null when the question was answered by the note's text alone
}

// AddNote adds a maintainer's note, which every later agent on the ticket
// gets, and a typed noted event (add_note). by is the person or the
// factory ("" is the factory); qa is set when the note answers an agent's
// question.
func AddNote(t *Ticket, text, by string, qa *NoteAnswer, now time.Time) {
	if by == "" {
		by = "factory"
	}
	n := Note{At: clock.Stamp(now), Text: text}
	p := Noted{By: by, Note: len(t.Notes)}
	if qa != nil {
		n.Question, n.Answer = Some(qa.Question), qa.Answer
		if !n.Answer.Set {
			n.Answer = Nil[string]()
		}
		p.Question = qa.Question
	}
	if t.Notes == nil {
		t.Notes = []Note{}
	}
	t.Notes = append(t.Notes, n)
	Event(t, "note", textx.Prefix(text, 200), "", p, now)
}

// StartIn is an agent's start, for its history entry.
type StartIn struct {
	Stage  model.Stage
	Agent  string
	Host   string // model.LocalHost for this host
	Model  string // "" for the harness's default
	Branch string // "" for a stage without a branch
}

// RecordStart appends the typed start event, with Python's detail
// "<stage> <agent>[ on <branch>][ @<host>][ (<model>)]" (start_stage).
func RecordStart(t *Ticket, in StartIn, now time.Time) {
	host := in.Host
	if host == "" {
		host = model.LocalHost
	}
	d := string(in.Stage) + " " + in.Agent
	if in.Branch != "" {
		d += " on " + in.Branch
	}
	if host != model.LocalHost {
		d += " @" + host
	}
	if in.Model != "" {
		d += " (" + in.Model + ")"
	}
	Event(t, "start", d, "", Started{Stage: string(in.Stage), Agent: in.Agent, Host: host, Model: in.Model, Branch: in.Branch}, now)
}

// RecordFinish appends the typed finished event "<stage> <agent>".
func RecordFinish(t *Ticket, stage model.Stage, agent string, now time.Time) {
	Event(t, "finished", string(stage)+" "+agent, "", Finished{Stage: string(stage), Agent: agent}, now)
}

// Starts counts a stage's starts in the ticket's whole life, from the typed
// stage or, on old history, a "<stage> " prefix of the detail (starts). It numbers agents and gate jobs and turns the voters' models, so
// it never resets; the work attempts a review allows are WorkRounds.
func Starts(t *Ticket, stage model.Stage) int {
	n := 0
	for _, e := range t.History {
		if startOf(e, stage) {
			n++
		}
	}
	return n
}

// WorkRounds counts the work starts since the last passed review (a typed
// move with reason review_passed), which bound the reworks a review may ask
// for: a rework a GitHub review asks for after the factory's review
// passed starts a new round. Python counted the ticket's whole life.
// Old untyped moves carry no reason, so on such history it
// counts as Starts does.
func WorkRounds(t *Ticket) int {
	n := 0
	for _, e := range t.History {
		if m, ok := e.Data.(Moved); ok && m.Reason == model.ReasonReviewPassed {
			n = 0
		}
		if startOf(e, model.StageWork) {
			n++
		}
	}
	return n
}

// startOf reports an entry that is a start of stage.
func startOf(e Entry, stage model.Stage) bool {
	if e.Event != "start" {
		return false
	}
	if s, ok := e.Data.(Started); ok {
		return s.Stage == string(stage)
	}
	if st, ok := e.legacyStage.Get(); ok {
		return st == string(stage)
	}
	return len(e.Detail) > len(stage) && e.Detail[:len(stage)+1] == string(stage)+" "
}

// New is a ticket as new_ticket makes it, before its first save:
// the keys in Python's order, the typed created event, and its ask. fields
// are new_ticket's **fields (kind, parent, labels, idempotency, ...), in the
// caller's order; a key they share with the standard ones replaces its value
// in place, as dict.update does. The store stamps "updated" on the save.
func New(id ID, in NewIn, now time.Time) *Ticket {
	by := in.By
	if by == "" {
		by = "factory"
	}
	o := jsonx.Object{{Key: "id", Value: jsonx.String(string(id))}}
	board := in.Mode == model.ModeBoard
	if board {
		o.Set("schema", jsonx.Int(1))
		o.Set("mode", jsonx.String("board"))
	}
	for _, m := range []jsonx.Member{
		{Key: "project", Value: jsonx.String(in.Project)}, {Key: "title", Value: jsonx.String(in.Title)},
		{Key: "body", Value: jsonx.String(in.Body)}, {Key: "door", Value: jsonx.String(in.Door)},
		{Key: "by", Value: jsonx.String(by)}, {Key: "column", Value: jsonx.String(in.Column)},
	} {
		o.Set(m.Key, m.Value)
	}
	if board {
		empty := func() jsonx.Value { return jsonx.ArrayOf() }
		for _, m := range []jsonx.Member{
			{Key: "kind", Value: jsonx.String("item")}, {Key: "assignee", Value: jsonx.Null()}, {Key: "labels", Value: empty()},
			{Key: "parent", Value: jsonx.Null()}, {Key: "children", Value: empty()}, {Key: "links", Value: empty()},
			{Key: "waiting", Value: jsonx.Null()}, {Key: "questions", Value: empty()}, {Key: "comments", Value: empty()},
			{Key: "result", Value: jsonx.Null()}, {Key: "notes", Value: empty()},
		} {
			o.Set(m.Key, m.Value)
		}
	}
	for _, m := range []jsonx.Member{
		{Key: "branch", Value: jsonx.Null()}, {Key: "agent", Value: jsonx.Null()}, {Key: "reports", Value: jsonx.ObjectOf()},
		{Key: "created", Value: jsonx.String(clock.Stamp(now))}, {Key: "history", Value: jsonx.ArrayOf()},
	} {
		o.Set(m.Key, m.Value)
	}
	for _, m := range in.Fields {
		o.Set(m.Key, m.Value.Clone())
	}
	t, _, err := fromTree(o.Value())
	if err != nil {
		panic(err) // o is an object
	}
	Event(t, "created", "door="+in.Door+" column="+in.Column, "", Created{Door: in.Door, Column: in.Column, By: by}, now)
	t.SetAsk(AskFor(t, "", "", clock.Stamp(now), ""))
	return t
}

// NewIn is what new_ticket takes.
type NewIn struct {
	Project, Title, Body, Door, Column string
	By                                 string     // "" is the factory
	Mode                               model.Mode // "board" for a board project's ticket
	Fields                             jsonx.Object
}
