// Package pyticket compares tickets with the ones Python wrote. It is apart
// from testkit because it reads JSON, and jsonx's own tests use testkit.
package pyticket

import (
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
)

// History is a ticket file as Python would have written it: the
// history entries of the events Python left untyped, and the factory types
// since it retired, lose their type and its fields. Their detail, the
// human text, is left as it is, so a test can still compare the rest with
// Python's goldens byte for byte. Anything else comes back unchanged.
func History(file string) string {
	v, err := jsonx.Parse([]byte(file))
	if err != nil {
		return file
	}
	top, ok := v.Object()
	if !ok {
		return file
	}
	history, ok := top.Get("history").Array()
	if !ok {
		return file
	}
	typed := onceUntyped()
	out := make([]jsonx.Value, len(history))
	changed := false
	for i, e := range history {
		out[i] = e
		o, ok := e.Object()
		et, _ := o.Get("type").Str()
		fields, d9 := typed[model.EventType(et)]
		if !ok || !d9 {
			continue
		}
		o = o.Clone()
		o.Delete("type")
		for _, f := range fields {
			o.Delete(strings.TrimSuffix(f, "?"))
		}
		out[i], changed = o.Value(), true
	}
	if !changed {
		return file
	}
	top = top.Clone()
	top.Set("history", jsonx.ArrayOf(out...))
	b := string(jsonx.EncodePython(top.Value(), 2))
	if strings.HasSuffix(file, "\n") {
		b += "\n"
	}
	return b
}

// onceUntyped are the event types typed since, with their fields.
func onceUntyped() map[model.EventType][]string {
	out := map[model.EventType][]string{}
	for _, e := range model.EventTypes() {
		switch e.Type {
		case model.EventErrored, model.EventPR, model.EventGated, model.EventFiled,
			model.EventVoted, model.EventPanel, model.EventPinned, model.EventReviewRequested:
			out[e.Type] = e.Fields
		}
	}
	return out
}
