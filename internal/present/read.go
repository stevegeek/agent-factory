package present

import (
	"encoding/json"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// reader reads history for one presentation call. Every read goes through
// ticket.Interpret; legacy counts the entries that had no typed facts and
// were read from their wording (Foreman's fallbacks), so a test can prove
// that typed history never needs the legacy readers. A reader belongs to one
// call: the exported functions make a fresh one.
type reader struct {
	legacy int
}

func (r *reader) interpret(e ticket.Entry) ticket.Payload {
	p, legacy := ticket.Interpret(e)
	if legacy {
		r.legacy++
	}
	return p
}

// runOf is a start or finished event's stage, agent and host ("" for the
// dispatcher's own host).
func (r *reader) runOf(e ticket.Entry) (stage, agent, host string, ok bool) {
	switch d := e.Data.(type) {
	case ticket.Started:
		return d.Stage, d.Agent, localAsEmpty(d.Host), d.Stage != "" && d.Agent != ""
	case ticket.Finished:
		return d.Stage, d.Agent, "", d.Stage != "" && d.Agent != ""
	}
	if e.Event != "start" && e.Event != "finished" {
		return "", "", "", false
	}
	switch d := r.interpret(e).(type) {
	case ticket.Started:
		return d.Stage, d.Agent, localAsEmpty(d.Host), true
	case ticket.Finished:
		return d.Stage, d.Agent, "", true
	}
	return "", "", "", false
}

func localAsEmpty(host string) string {
	if host == model.LocalHost {
		return ""
	}
	return host
}

// stageOf is the stage of a start or finished event, else "".
func (r *reader) stageOf(e ticket.Entry) string {
	if e.Event != "start" && e.Event != "finished" {
		return ""
	}
	stage, _, _, _ := r.runOf(e)
	return stage
}

// MoveFacts are what a move says: its columns, its typed reason ("" on older
// history), the words after the columns and whether a person made it.
type MoveFacts struct {
	From, To, Reason string
	Text             string // the words after the columns: a summary, a PR link, a person's note
	Human            bool   // moved by hand (factory move)
}

// Is reports the typed reason when there is one, else what the old wording
// said.
func (m MoveFacts) Is(reason model.Reason, worded bool) bool {
	if m.Reason != "" {
		return m.Reason == string(reason)
	}
	return worded
}

// MoveOf reads a move event.
func MoveOf(e ticket.Entry) MoveFacts { return new(reader).moveOf(e) }

func (r *reader) moveOf(e ticket.Entry) MoveFacts {
	_, text, _ := strings.Cut(e.Detail, ": ")
	if m, ok := e.Data.(ticket.Moved); ok {
		reason := string(m.Reason)
		return MoveFacts{From: m.From, To: m.To, Reason: reason, Text: text, Human: m.Reason == model.ReasonByHuman}
	}
	// older history: the wording only. The reason Interpret reads from the
	// wording stays unused, so every reason test reads the wording as Foreman
	// did (MoveFacts.Is with no reason).
	f := MoveFacts{Text: text, Human: strings.HasPrefix(text, "by human")}
	if m, ok := r.interpret(e).(ticket.Moved); ok {
		f.From, f.To = m.From, m.To
	}
	return f
}

// AskOf is the task an asked or answered event names, and its question or
// answer.
func AskOf(e ticket.Entry) (id, text string) { return new(reader).askOf(e) }

func (r *reader) askOf(e ticket.Entry) (id, text string) {
	switch d := e.Data.(type) {
	case ticket.Asked:
		return d.Ticket, d.Text
	case ticket.Answered:
		return d.Ticket, d.Text
	}
	switch d := r.interpret(e).(type) {
	case ticket.Asked:
		return d.Ticket, d.Text
	case ticket.Answered:
		return d.Ticket, d.Text
	}
	// no ": " in the detail: the whole detail names the task
	return e.Detail, ""
}

// createdIn is the column a ticket was filed into.
func (r *reader) createdIn(e ticket.Entry) string {
	if c, ok := e.Data.(ticket.Created); ok {
		return c.Column
	}
	if c, ok := r.interpret(e).(ticket.Created); ok {
		return c.Column
	}
	return ""
}

// panelDecided is the prospect's move that revealed the panel's scores.
func (r *reader) panelDecided(e ticket.Entry) bool {
	return e.Event == "move" && r.moveOf(e).Is(model.ReasonPanelDone, strings.Contains(e.Detail, "panel kept"))
}

// byOf is who made a typed entry ("" on older history, which never says).
func byOf(e ticket.Entry) string {
	switch d := e.Data.(type) {
	case ticket.Moved:
		return d.By
	case ticket.Noted:
		return d.By
	case ticket.Commented:
		return d.By
	case ticket.Created:
		return d.By
	case ticket.Claimed:
		return d.By
	case ticket.Edited:
		return d.By
	case ticket.Split:
		return d.By
	case ticket.Linked:
		return d.By
	case ticket.QuestionAsked:
		return d.By
	case ticket.QuestionAnswered:
		return d.By
	case ticket.SentToFactory:
		return d.By
	}
	return ""
}

// commentN is the comment number of a typed comment event, else 0.
func commentN(e ticket.Entry) int {
	if c, ok := e.Data.(ticket.Commented); ok {
		return c.N
	}
	return 0
}

// report is one stage's report as an agent wrote it, read loosely as
// Foreman's Report did: a string field trimmed, a number or a bool in its
// JSON form, anything else empty; a list field as its items, each object's
// string fields kept. A nil *report is a report that is not there.
type report struct {
	strs  map[string]string
	lists map[string][]map[string]string
}

func (r *report) str(k string) string {
	if r == nil {
		return ""
	}
	return r.strs[k]
}

func (r *report) list(k string) []map[string]string {
	if r == nil {
		return nil
	}
	return r.lists[k]
}

// reportOf is t's report for stage, or nil when there is none or it is not
// a JSON object.
func reportOf(t *ticket.Ticket, stage string) *report {
	v, ok := t.Reports.Lookup(stage)
	if !ok {
		return nil
	}
	o, ok := v.Object()
	if !ok {
		return nil
	}
	r := &report{strs: map[string]string{}, lists: map[string][]map[string]string{}}
	for _, f := range o {
		switch f.Value.Kind().String() {
		case "string":
			s, _ := f.Value.Str()
			r.strs[f.Key] = strings.TrimSpace(s)
		case "bool":
			b, _ := f.Value.Bool()
			r.strs[f.Key] = jsonText(b)
		case "number":
			if n, ok := f.Value.Float64(); ok {
				r.strs[f.Key] = jsonText(n)
			}
		case "array":
			items, _ := f.Value.Array()
			list := make([]map[string]string, len(items))
			for i, it := range items {
				if io, ok := it.Object(); ok {
					fields := map[string]string{}
					for _, g := range io {
						if s, ok := g.Value.Str(); ok {
							fields[g.Key] = s
						}
					}
					list[i] = fields
				}
			}
			r.lists[f.Key] = list
		}
	}
	return r
}

// parseReport reads a report file an agent wrote (an archived run's
// report.json), or nil when it is not a JSON object.
func parseReport(data []byte) *report {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil || m == nil {
		return nil
	}
	r := &report{strs: map[string]string{}, lists: map[string][]map[string]string{}}
	for k, v := range m {
		switch x := v.(type) {
		case string:
			r.strs[k] = strings.TrimSpace(x)
		case float64, bool:
			r.strs[k] = jsonText(x)
		case []any:
			list := make([]map[string]string, len(x))
			for i, it := range x {
				if io, ok := it.(map[string]any); ok {
					fields := map[string]string{}
					for gk, gv := range io {
						if s, ok := gv.(string); ok {
							fields[gk] = s
						}
					}
					list[i] = fields
				}
			}
			r.lists[k] = list
		}
	}
	return r
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
