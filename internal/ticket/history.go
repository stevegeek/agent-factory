package ticket

import (
	"reflect"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
)

// Entry is one history entry (§3.2). Event and Detail are on every entry;
// Type and the payload's fields only on typed entries, flattened into the
// same object. An entry keeps its own key order and the keys no view knows,
// like every object of the tree.
type Entry struct {
	At     string          // "at"
	Event  string          // the legacy name: "move", "start", "note", "pr", ...
	Detail string          // the sentence a person reads; new code never parses it
	Rule   string          // which rule decided, in a sentence (optional)
	Type   model.EventType // optional
	Data   Payload         // the typed fields, chosen by Type; nil on untyped entries

	// legacyStage is the "stage" key of an untyped entry, which starts()
	// prefers to the detail; the key itself stays in the tree.
	legacyStage Opt[string]
}

// entryBase is the part of an entry every one has, in Python's order.
type entryBase struct {
	At     string          `json:"at"`
	Event  string          `json:"event"`
	Detail string          `json:"detail"`
	Rule   string          `json:"rule,omitempty"`
	Type   model.EventType `json:"type,omitempty"`
}

// Payload is the typed part of an entry.
type Payload interface{ EventType() model.EventType }

// The payloads of the typed events (EVENT_TYPES), with their fields in the
// order Python writes them. A field marked "?" in the model is omitempty.
type (
	// Created: a ticket was filed.
	Created struct {
		Door   string `json:"door"`
		Column string `json:"column"`
		By     string `json:"by"`
	}
	// Moved: a ticket changed column. Text is not stored: Interpret reads
	// it from the detail (the words after the columns: a summary, a PR link,
	// a person's note).
	Moved struct {
		From   string       `json:"from"`
		To     string       `json:"to"`
		Reason model.Reason `json:"reason,omitempty"`
		By     string       `json:"by"`
		Text   string       `json:"-"`
	}
	// Started: an agent started a stage.
	Started struct {
		Stage  string `json:"stage"`
		Agent  string `json:"agent"`
		Host   string `json:"host"`
		Model  string `json:"model,omitempty"`
		Branch string `json:"branch,omitempty"`
	}
	// Finished: a stage's agent finished.
	Finished struct {
		Stage string `json:"stage"`
		Agent string `json:"agent"`
	}
	// Memory: a run's memory was settled, was over its limit, or was lost.
	Memory struct {
		Role        string   `json:"role"`
		Commit      string   `json:"commit,omitempty"`
		Files       []string `json:"files,omitempty"`
		OverLimitKB int      `json:"over_limit_kb,omitempty"`
		LostOn      string   `json:"lost_on,omitempty"`
	}
	// Noted: the maintainer's note number Note.
	Noted struct {
		By       string `json:"by"`
		Note     int    `json:"note"`
		Question string `json:"question,omitempty"`
	}
	// Asked: a question was filed as task Ticket.
	Asked struct {
		Ticket string `json:"ticket"`
		Text   string `json:"text"`
	}
	// Answered: task Ticket answered the question it was filed for.
	Answered struct {
		Ticket string `json:"ticket"`
		Text   string `json:"text"`
	}
	// Commented: comment N on a board ticket.
	Commented struct {
		By string `json:"by"`
		N  int    `json:"n"`
	}
	// Claimed: a board ticket was claimed.
	Claimed struct {
		By string `json:"by"`
	}
	// Edited: a board ticket's fields were changed.
	Edited struct {
		By     string   `json:"by"`
		Fields []string `json:"fields"`
	}
	// Split: a board ticket was split into Children.
	Split struct {
		By       string   `json:"by"`
		Children []string `json:"children"`
	}
	// Linked: a board ticket got a link.
	Linked struct {
		By     string `json:"by"`
		Rel    string `json:"rel"`
		Ticket string `json:"ticket,omitempty"`
		URL    string `json:"url,omitempty"`
	}
	// QuestionAsked: question N went to Recipient.
	QuestionAsked struct {
		By        string `json:"by"`
		Recipient string `json:"recipient"`
		N         int    `json:"n"`
	}
	// QuestionAnswered: question N was answered.
	QuestionAnswered struct {
		By string `json:"by"`
		N  int    `json:"n"`
	}
	// SentToFactory: a board ticket was sent to a factory project as Ticket.
	SentToFactory struct {
		By     string `json:"by"`
		Ticket string `json:"ticket"`
	}
)

// The payloads of the events Python wrote untyped (error, pr, gate, filed,
// vote, panel, host, review requested). They are written typed since it
// retired (D9); Interpret reads old history's from the detail.
type (
	// Errored: something went wrong (event "error"). Text is the error
	// itself; the detail says it in a sentence, with where it came from.
	Errored struct {
		Text string `json:"text"`
	}
	// PRNote: something happened to the PR (event "pr"): Action is one of
	// the PR* actions, Why what GitHub or the factory said.
	PRNote struct {
		Action string `json:"action"`
		Number int    `json:"number,omitempty"`
		Why    string `json:"why,omitempty"`
	}
	// Gated: Base moved to Commit and the checks pass again with it merged
	// in (event "gate").
	Gated struct {
		Base   string `json:"base"`
		Commit string `json:"commit"`
	}
	// Filed: the ticket filed Ticket (event "filed"). Text is not stored:
	// Interpret reads it from the detail (the words after the id).
	Filed struct {
		Ticket string `json:"ticket"`
		Text   string `json:"-"`
	}
	// Voted: K of N votes are in (event "vote").
	Voted struct {
		K int `json:"k"`
		N int `json:"n"`
	}
	// PanelScored: the panel scored the proposal this ticket was filed for,
	// and kept it or dropped it (event "panel"; the rule says why).
	PanelScored struct {
		Kept bool `json:"kept"`
	}
	// Pinned: the ticket was pinned to Host (event "host").
	Pinned struct {
		Host string `json:"host"`
	}
	// ReviewRequested: reviewers were asked (event "review requested").
	ReviewRequested struct {
		Reviewers []string `json:"reviewers"`
	}
)

// The actions of a pr event (PRNote.Action).
const (
	PRDraft          = "draft"           // turned back to draft while it is reworked
	PRMergeRequested = "merge_requested" // the maintainer asked the factory to merge it
	PRMergeWaiting   = "merge_waiting"   // approved, but GitHub has not merged it yet
	PRNotMerged      = "not_merged"      // a merge was asked for and did not happen
	PRNotOpened      = "not_opened"      // an open was asked for and did not happen
)

func (Created) EventType() model.EventType          { return model.EventCreated }
func (Moved) EventType() model.EventType            { return model.EventMoved }
func (Started) EventType() model.EventType          { return model.EventStarted }
func (Finished) EventType() model.EventType         { return model.EventFinished }
func (Memory) EventType() model.EventType           { return model.EventMemory }
func (Noted) EventType() model.EventType            { return model.EventNoted }
func (Asked) EventType() model.EventType            { return model.EventAsked }
func (Answered) EventType() model.EventType         { return model.EventAnswered }
func (Commented) EventType() model.EventType        { return model.EventCommented }
func (Claimed) EventType() model.EventType          { return model.EventClaimed }
func (Edited) EventType() model.EventType           { return model.EventEdited }
func (Split) EventType() model.EventType            { return model.EventSplit }
func (Linked) EventType() model.EventType           { return model.EventLinked }
func (QuestionAsked) EventType() model.EventType    { return model.EventQuestionAsked }
func (QuestionAnswered) EventType() model.EventType { return model.EventQuestionAnswered }
func (SentToFactory) EventType() model.EventType    { return model.EventSentToFactory }
func (Errored) EventType() model.EventType          { return model.EventErrored }
func (PRNote) EventType() model.EventType           { return model.EventPR }
func (Gated) EventType() model.EventType            { return model.EventGated }
func (Filed) EventType() model.EventType            { return model.EventFiled }
func (Voted) EventType() model.EventType            { return model.EventVoted }
func (PanelScored) EventType() model.EventType      { return model.EventPanel }
func (Pinned) EventType() model.EventType           { return model.EventPinned }
func (ReviewRequested) EventType() model.EventType  { return model.EventReviewRequested }

// payloadFor is a pointer to a new payload of type et, or nil for a type the
// factory does not know.
func payloadFor(et model.EventType) any {
	switch et {
	case model.EventCreated:
		return &Created{}
	case model.EventMoved:
		return &Moved{}
	case model.EventStarted:
		return &Started{}
	case model.EventFinished:
		return &Finished{}
	case model.EventMemory:
		return &Memory{}
	case model.EventNoted:
		return &Noted{}
	case model.EventAsked:
		return &Asked{}
	case model.EventAnswered:
		return &Answered{}
	case model.EventCommented:
		return &Commented{}
	case model.EventClaimed:
		return &Claimed{}
	case model.EventEdited:
		return &Edited{}
	case model.EventSplit:
		return &Split{}
	case model.EventLinked:
		return &Linked{}
	case model.EventQuestionAsked:
		return &QuestionAsked{}
	case model.EventQuestionAnswered:
		return &QuestionAnswered{}
	case model.EventSentToFactory:
		return &SentToFactory{}
	case model.EventErrored:
		return &Errored{}
	case model.EventPR:
		return &PRNote{}
	case model.EventGated:
		return &Gated{}
	case model.EventFiled:
		return &Filed{}
	case model.EventVoted:
		return &Voted{}
	case model.EventPanel:
		return &PanelScored{}
	case model.EventPinned:
		return &Pinned{}
	case model.EventReviewRequested:
		return &ReviewRequested{}
	}
	return nil
}

func (e *Entry) decodeJSON(d *decoder, path string, v jsonx.Value) bool {
	if v.IsNull() {
		return true
	}
	o, ok := v.Object()
	if !ok {
		d.bad(path, reflect.TypeFor[Entry](), v)
		return false
	}
	var b entryBase
	d.decodeFields(path, o, reflect.ValueOf(&b).Elem())
	*e = Entry{At: b.At, Event: b.Event, Detail: b.Detail, Rule: b.Rule, Type: b.Type}
	if p := payloadFor(b.Type); p != nil {
		pv := reflect.ValueOf(p).Elem()
		d.decodeFields(path, o, pv)
		e.Data = pv.Interface().(Payload)
	} else if sv, ok := o.Lookup("stage"); ok {
		st, _ := sv.Str()
		e.legacyStage = Some(st)
	}
	return true
}

func (e Entry) mergeJSON(old jsonx.Object, hasOld bool) jsonx.Value {
	b := entryBase{At: e.At, Event: e.Event, Detail: e.Detail, Rule: e.Rule, Type: e.Type}
	out := mergeFields(old, hasOld, reflect.ValueOf(b))
	if e.Data != nil {
		out = mergeFields(out, hasOld, reflect.ValueOf(e.Data))
	}
	return out.Value()
}
