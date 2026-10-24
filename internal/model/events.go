package model

// EventType is the type of a typed history entry.
type EventType string

// The typed events the factory writes (EVENT_TYPES), in order.
const (
	EventCreated          EventType = "created"
	EventMoved            EventType = "moved"
	EventStarted          EventType = "started"
	EventFinished         EventType = "finished"
	EventMemory           EventType = "memory"
	EventNoted            EventType = "noted"
	EventAsked            EventType = "asked"
	EventAnswered         EventType = "answered"
	EventCommented        EventType = "commented"
	EventClaimed          EventType = "claimed"
	EventEdited           EventType = "edited"
	EventSplit            EventType = "split"
	EventLinked           EventType = "linked"
	EventQuestionAsked    EventType = "question_asked"
	EventQuestionAnswered EventType = "question_answered"
	EventSentToFactory    EventType = "sent_to_factory"
	// The events Python wrote untyped (error, pr, gate, filed, vote, panel,
	// host, review requested), typed since it retired (§3.2, D9). Old
	// history still has them untyped.
	EventErrored         EventType = "errored"
	EventPR              EventType = "pr"
	EventGated           EventType = "gated"
	EventFiled           EventType = "filed"
	EventVoted           EventType = "voted"
	EventPanel           EventType = "panel"
	EventPinned          EventType = "pinned"
	EventReviewRequested EventType = "review_requested"
)

// EventTypeInfo is one typed event and the fields beside its type; a
// trailing "?" marks a field that may be absent.
type EventTypeInfo struct {
	Type   EventType
	Fields []string
}

// EventTypes are the typed events, in order.
func EventTypes() []EventTypeInfo {
	f := func(fields ...string) []string { return fields }
	return []EventTypeInfo{
		{EventCreated, f("door", "column", "by")},
		{EventMoved, f("from", "to", "reason", "by")},
		{EventStarted, f("stage", "agent", "host", "model?", "branch?")},
		{EventFinished, f("stage", "agent")},
		{EventMemory, f("role", "commit?", "files?", "over_limit_kb?", "lost_on?")},
		{EventNoted, f("by", "note", "question?")},
		{EventAsked, f("ticket", "text")},
		{EventAnswered, f("ticket", "text")},
		{EventCommented, f("by", "n")},
		{EventClaimed, f("by")},
		{EventEdited, f("by", "fields")},
		{EventSplit, f("by", "children")},
		{EventLinked, f("by", "rel", "ticket?", "url?")},
		{EventQuestionAsked, f("by", "recipient", "n")},
		{EventQuestionAnswered, f("by", "n")},
		{EventSentToFactory, f("by", "ticket")},
		{EventErrored, f("text")},
		{EventPR, f("action", "number?", "why?")},
		{EventGated, f("base", "commit")},
		{EventFiled, f("ticket")},
		{EventVoted, f("k", "n")},
		{EventPanel, f("kept")},
		{EventPinned, f("host")},
		{EventReviewRequested, f("reviewers")},
	}
}
