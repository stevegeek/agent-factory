package web

import (
	"context"
	"errors"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// OpsVerbs is what web's answer, filing and look services use of package
// ops (*ops.Service). Foreman builds that service with ops.Intents as its
// PR actions, so nothing reached from here talks to GitHub: an answer that
// needs GitHub records an intent and wakes the tick.
type OpsVerbs interface {
	Answer(ctx context.Context, in ops.AnswerIn) (string, error)
	File(ctx context.Context, project, kind, title, body, by, after string) (ticket.ID, error)
	Look(ctx context.Context, id string, lines int) (ops.Screen, error)
	Pass(ctx context.Context, id, to, by, note string) (string, error)
}

// Ops is web's Answerer, Filer and Looker over package ops, in-process:
// what an answer means, what a new ticket gets and how a screen is
// read are all decided in ops; this only translates.
type Ops struct {
	Verbs OpsVerbs
}

// OpsServices is the Services for a Foreman whose answers, ＋ page and
// Live screens go to v, with cfg as the config the pages read. The token
// APIs (API, MCP) are the caller's to add.
func OpsServices(v OpsVerbs, cfg ConfigSource) Services {
	o := Ops{Verbs: v}
	return Services{Answers: o, Filer: o, Looker: o, Passer: o, Config: cfg}
}

// Pass is ops.Service.Pass: the ticket given to a person.
func (o Ops) Pass(ctx context.Context, id ticket.ID, to, by, note string) (string, error) {
	return o.Verbs.Pass(ctx, string(id), to, by, note)
}

// Answers are ops.Answers: web keeps no answer table of its own.
func (Ops) Answers(t *ticket.Ticket, a *ticket.Ask) []Answer {
	in := ops.Answers(t, a)
	if in == nil {
		return nil
	}
	out := make([]Answer, len(in))
	for i, x := range in {
		out[i] = Answer(x)
	}
	return out
}

// Answer is ops.Service.Answer: one lock, one save, as the person
// answering. An answer to an ask that has moved on comes back as
// ErrStale.
func (o Ops) Answer(ctx context.Context, req AnswerRequest) (string, error) {
	out, err := o.Verbs.Answer(ctx, ops.AnswerIn{ID: string(req.ID), Value: req.Value, Note: req.Note, Picks: req.Picks,
		As: req.As, Kind: req.Kind, Since: req.Since, By: req.By, Options: req.Options})
	if err != nil && errors.Is(err, ops.ErrStale) && !errors.Is(err, ErrStale) {
		err = fmt.Errorf("%w: %w", ErrStale, err)
	}
	return out, err
}

// File is ops.Service.File: the person's ticket through the human door.
func (o Ops) File(ctx context.Context, n NewTicket) (ticket.ID, error) {
	return o.Verbs.File(ctx, n.Project, n.Kind, n.Title, n.Body, n.By, n.After)
}

// Look is ops.Service.Look, read-only, as `factory look T --lines N`
// printed it: the header line, then the screen.
func (o Ops) Look(ctx context.Context, id ticket.ID, lines int) (string, error) {
	s, err := o.Verbs.Look(ctx, string(id), lines)
	if err != nil {
		return "", err
	}
	return s.String(), nil
}
