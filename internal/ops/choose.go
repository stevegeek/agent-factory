package ops

import (
	"context"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Choice is one option of an agent's open question, as the desk offers it:
// Effect says in words what choosing it does.
type Choice struct {
	Key, Label, Detail, Effect string
	Recommended                bool
}

// Choices are the options of t's open question, and whether several may
// be chosen; none when it has no open question with options.
func Choices(t *ticket.Ticket) ([]Choice, bool) {
	q, ok := t.Question.Get()
	if !ok || q.Answered != "" {
		return nil, false
	}
	var out []Choice
	for _, o := range q.Options {
		out = append(out, Choice{Key: o.Key, Label: o.Label, Detail: o.Detail, Effect: EffectWords(o), Recommended: o.Recommended})
	}
	return out, q.Many()
}

// EffectWords is what choosing o does, in words: "closes the ticket",
// "files a change, then carries on".
func EffectWords(o ticket.QuestionOption) string {
	switch o.EffectOf() {
	case ticket.EffectClose:
		return "closes the ticket"
	case ticket.EffectDrop:
		return "drops the ticket"
	case ticket.EffectFile:
		as := "a change"
		if o.KindOf() == string(model.KindIntent) {
			as = "an intent"
		}
		if o.Then == ticket.EffectClose {
			return "files " + as + ", then closes the ticket"
		}
		return "files " + as + ", then carries on"
	}
	return "a new run carries on"
}

// optionKeys are the keys a person sent, trimmed, with the empty ones (the
// form's "None") dropped.
func optionKeys(in []string) []string {
	var out []string
	for _, k := range in {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// choose answers t's open question q with the options keys, and the note
// text if any: it records the choice and carries out its effect. Only a
// continue sends the ticket to a column where a stage runs.
func (c *call) choose(ctx context.Context, t *ticket.Ticket, q ticket.Question, text string, keys []string) (string, error) {
	if len(q.Options) == 0 {
		return "", refuse(BadRequest, "%s's question has no options; answer it in words", t.ID)
	}
	var picked []ticket.QuestionOption
	var names []string
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		o, ok := q.Option(k)
		if !ok {
			return "", refuse(BadRequest, "%q is not an option of %s's question", k, t.ID)
		}
		picked = append(picked, o)
		names = append(names, k)
	}
	if len(picked) > 1 && !q.Many() {
		return "", refuse(BadRequest, "%s's question takes one option", t.ID)
	}
	o := picked[0]
	effect, then := o.EffectOf(), o.EffectOf()
	if effect == ticket.EffectFile {
		then = o.Then
	}
	if pr, ok := t.PR.Get(); ok && pr.Number > 0 && pr.MergeCommit == "" && (then == ticket.EffectClose || then == ticket.EffectDrop) {
		return "", refuse(Conflict, "%s has PR #%d open; close the PR on GitHub first", t.ID, pr.Number)
	}
	if effect == ticket.EffectFile && o.KindOf() == string(model.KindIntent) {
		if d, ok := model.DoorOf(t.Door); !ok || d.Untrusted {
			return "", refuse(Conflict, "%s came through an untrusted door; file it as a change, which triage reads first", t.ID)
		}
	}
	filed := ""
	if effect == ticket.EffectFile {
		id, err := c.fileOption(ctx, t, q, o, text)
		if err != nil {
			return "", err
		}
		filed = string(id)
	}
	q.Answered, q.Chosen, q.Effect, q.Filed = clockStamp(c), names, effect, filed
	t.Question = ticket.Some(q)
	labels := make([]string, len(picked))
	for i, p := range picked {
		labels[i] = "“" + p.Label + "”"
	}
	detail := "chose " + strings.Join(labels, ", ") + ": " + EffectWords(o)
	if filed != "" {
		detail += "; filed " + filed
	}
	ticket.Event(t, "chose", detail, "",
		ticket.OptionChosen{By: c.by(), Keys: names, Effect: effect, Filed: filed}, c.now)
	answer := chosenText(picked, filed, text)
	// agents read the sentence; Foreman reads the question from its own field
	ticket.AddNote(t, "Your question: “"+textx.Clip(q.Text, 300)+"” The maintainer's answer: "+answer, c.by(),
		&ticket.NoteAnswer{Question: q.Text, Answer: ticket.Some(answer)}, c.now)
	switch then {
	case ticket.EffectClose:
		result := o.Label
		if filed != "" {
			result = "Filed as " + filed + ": " + o.Label
		}
		if text != "" {
			result += "\n\n" + text
		}
		t.Result = ticket.Some(result)
		ticket.Move(t, string(model.ColDone), model.ReasonQuestionClosed, "closed by the answer: "+textx.Clip(o.Label, 120), "", c.by(), c.now)
		return string(t.ID) + ": closed: " + o.Label, nil
	case ticket.EffectDrop:
		why := text
		if why == "" {
			why = o.Label
		}
		ticket.Move(t, string(model.ColRejected), model.ReasonQuestionDropped, "dropped by the answer: "+textx.Clip(why, 200), "", c.by(), c.now)
		return string(t.ID) + ": dropped", nil
	}
	ticket.Move(t, resumeColumn(q.Stage), model.ReasonQuestionAnswered, "answered the agent's question", "", c.by(), c.now)
	line := string(t.ID) + ": answered; a new " + q.Stage + " run carries on"
	if filed != "" {
		line = string(t.ID) + ": filed " + filed + "; a new " + q.Stage + " run carries on"
	}
	return line, nil
}

// chosenText is the answer the next run reads: the options chosen, with
// their details, what was filed, and the maintainer's note.
func chosenText(picked []ticket.QuestionOption, filed, text string) string {
	parts := make([]string, len(picked))
	for i, o := range picked {
		parts[i] = "“" + o.Label + "”"
		if o.Detail != "" {
			parts[i] += " (" + o.Detail + ")"
		}
	}
	out := "chose " + strings.Join(parts, "; ") + "."
	if filed != "" {
		out += " Filed as " + filed + "."
	}
	if text != "" {
		out += " " + text
	}
	return out
}

// fileOption files option o of t's question q as a ticket in t's project,
// through t's door: a change to triage, an intent to ready. It links the
// two and records a filed event on t.
func (c *call) fileOption(ctx context.Context, t *ticket.Ticket, q ticket.Question, o ticket.QuestionOption, note string) (ticket.ID, error) {
	body := []string{}
	if o.Detail != "" {
		body = append(body, o.Detail, "")
	}
	body = append(body, "From "+string(t.ID)+" ("+t.Title+"), where the "+q.Stage+" agent asked:", "", q.Text)
	if note != "" {
		body = append(body, "", "The maintainer's note: "+note)
	}
	column, as := model.ColTriage, "a change"
	fs := fields(jsonx.Member{Key: "parent", Value: jsonx.String(string(t.ID))})
	if o.KindOf() == string(model.KindIntent) {
		column, as = model.ColReady, "an idea to design first"
		fs = fields(jsonx.Member{Key: "kind", Value: jsonx.String(string(model.KindIntent))},
			jsonx.Member{Key: "parent", Value: jsonx.String(string(t.ID))})
	}
	child, err := c.s.Board.FileTicket(ctx, ticket.NewIn{Project: t.Project, Title: textx.Prefix(o.Label, 120),
		Body: strings.Join(body, "\n") + "\n", Door: t.Door, Column: string(column), By: c.by(), Fields: fs})
	if err != nil {
		return "", err
	}
	t.Children = append(t.Children, string(child.ID))
	ticket.Event(t, "filed", string(child.ID)+": "+as+": "+textx.Clip(o.Label, 120), "", ticket.Filed{Ticket: string(child.ID)}, c.now)
	c.wake = true
	return child.ID, nil
}
