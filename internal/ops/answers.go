package ops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Answer is one answer the desk offers for an ask. An answer with
// Href is a plain link; the others are carried out by Service.Answer with
// their Value.
type Answer struct {
	Value, Label, Sub string
	Primary           bool
	Href              string // only ever an absolute http(s) link
	NeedsNote         bool
	NoteFor           string // "to send it back": names the answer the note is needed for
	Danger            bool   // closes the ticket or the PR: shown last, in red
}

// Answers that are not answers to the ask: they need no check that the ask
// is still the one the person saw.
const (
	AnswerNote = "note" // a note for the next agent
	AnswerAsk  = "ask"  // a question for an agent to answer from the code
	AnswerTalk = "talk" // a reply in the conversation
)

// MaxNote is the most an answer's note may hold, in bytes.
const MaxNote = 4000

// mergeable: the gate found the change auto-merge eligible and its PR is
// open and not a draft, so the factory may merge it on the maintainer's
// word.
func mergeable(t *ticket.Ticket) bool {
	g, okG := t.Gate.Get()
	pr, okPR := t.PR.Get()
	return t.Column == string(model.ColPROpen) && okG && g.AutoEligible && okPR && pr.Number > 0 && !pr.Draft.Or(false)
}

// Answers are the answers offered for ask a on t (Foreman's Answers); a may
// be nil, when only a ticket waiting to go out as a PR offers one.
func (s *Service) Answers(t *ticket.Ticket, a *ticket.Ask) []Answer { return Answers(t, a) }

// Answers is Service.Answers, which needs nothing of the service.
func Answers(t *ticket.Ticket, a *ticket.Ask) []Answer {
	pr, hasPR := t.PR.Get()
	if a == nil {
		if t.Column == string(model.ColReadyForPR) {
			if hasPR && pr.Number > 0 {
				return []Answer{{Value: "open_pr", Label: fmt.Sprintf("Update PR #%d", pr.Number), Sub: "push the reworked branch; the same PR updates", Primary: true}}
			}
			return []Answer{{Value: "open_pr", Label: "Open the PR", Sub: "push the branch and open it on GitHub", Primary: true}}
		}
		return nil
	}
	// the factory never closes an issue it was given; say so where dropping might suggest it
	issueStays := ""
	if src, ok := t.Source.Get(); ok && src.Issue != 0 {
		issueStays = "the issue stays open on GitHub"
	}
	switch model.AskKind(a.Kind) {
	case model.AskReview:
		var out []Answer
		u := ""
		if hasPR {
			u = safeURL(pr.URL)
		}
		if u != "" {
			switch {
			case mergeable(t) && ticket.NeedsApproval(t):
				// GitHub merges it only on an approval of its head, which is
				// the factory's go-ahead too: no "Merge it" to tap in vain
				out = append(out, Answer{Label: fmt.Sprintf("Approve PR #%d on GitHub", pr.Number),
					Sub: "GitHub merges it only once you approve it; the factory merges it by itself on the next tick after that", Primary: true, Href: u})
			case mergeable(t):
				out = append(out, Answer{Value: "merge", Label: "Merge it", Sub: fmt.Sprintf("the factory merges PR #%d and closes the ticket", pr.Number), Primary: true},
					Answer{Label: "Read it on GitHub", Sub: "the diff and the checks, before you decide", Href: u})
			case ticket.MergeQueued(t):
				who := "you approved it"
				if !ticket.ApprovedAtHead(t) {
					who = "you asked for the merge"
				}
				out = append(out, Answer{Label: fmt.Sprintf("Open PR #%d on GitHub", pr.Number),
					Sub: who + " and the factory merges it by itself; request changes there for a rework", Primary: true, Href: u})
			default:
				out = append(out, Answer{Label: fmt.Sprintf("Open PR #%d on GitHub", pr.Number),
					Sub: "approve it there and the factory merges it; or request changes for a rework", Primary: true, Href: u})
			}
		}
		out = append(out, Answer{Value: "send_back", Label: "Send back with a note", Sub: "a worker reworks it; the PR is a draft meanwhile", NeedsNote: true, NoteFor: "to send it back"})
		if u != "" {
			out = append(out, Answer{Label: "Close it on GitHub", Sub: "the factory sees it closed and drops the ticket", Href: u, Danger: true})
		}
		return out
	case model.AskOpen:
		open := Answer{Value: "open_pr", Label: "Open the PR", Sub: "push the branch and open it on GitHub", Primary: true}
		if hasPR && pr.Number > 0 {
			open.Label, open.Sub = fmt.Sprintf("Update PR #%d", pr.Number), "push the reworked branch; the same PR updates"
		}
		return []Answer{open, {Value: "send_back", Label: "Send back with a note", Sub: "a worker changes it first", NeedsNote: true, NoteFor: "to send it back"}}
	case model.AskStart:
		return []Answer{{Value: "start", Label: "Start", Sub: "a worker picks it up on the next tick", Primary: true},
			{Value: "not_now", Label: "Not now", Sub: orSub(issueStays, "the ticket closes"), Danger: true}}
	case model.AskDecide:
		return []Answer{{Value: "start", Label: "Start anyway", Sub: "a worker picks it up on the next tick", Primary: true},
			{Value: "reply", Label: "Ask triage again", Sub: "triage reruns with your note", NeedsNote: true, NoteFor: "to ask triage again"},
			{Value: "drop", Label: "Drop", Sub: orSub(issueStays, "the ticket closes"), Danger: true}}
	case model.AskDesign:
		return []Answer{{Value: "approve", Label: "Approve the ticked ones", Sub: "each goes to Ready on its own", Primary: true},
			{Value: "redirect", Label: "Redirect with a note", Sub: "the designer tries again", NeedsNote: true, NoteFor: "to redirect"}}
	case model.AskProposal:
		return []Answer{{Value: "file", Label: "File it", Sub: "becomes a change ticket in Ready", Primary: true},
			{Value: "drop", Label: "Drop", Sub: "the proposal closes; nothing is filed", Danger: true}}
	case model.AskUnstick:
		if ticket.PublicBlocked(t) {
			return publicAnswers(t)
		}
		var out []Answer
		if rc := a.RetryColumn.Or(""); rc != "" {
			out = append(out, Answer{Value: "retry", Label: "Retry", Sub: "back to " + model.ColumnLabel(rc), Primary: true},
				Answer{Value: "retry_note", Label: "Retry with a note", NeedsNote: true, NoteFor: "to retry with it"})
		}
		return append(out, Answer{Value: "drop", Label: "Drop", Sub: "the ticket closes", Danger: true})
	case model.AskNotice:
		out := []Answer{{Value: "done", Label: "Done", Sub: "close it; nothing else happens", Primary: true}}
		if verifiable(t) {
			// a result whose findings are prose (or never verified) can still
			// go through the findings flow
			out = append(out, Answer{Value: "verify", Label: "Verify these findings",
				Sub: "a verifier re-runs each finding on the base branch; then you choose which become tickets"})
		}
		return out
	case model.AskFindings:
		if ticket.OpenFindings(t) == 0 {
			return []Answer{{Value: "file_none", Label: "Close it", Sub: "nothing is left to file", Primary: true}}
		}
		return []Answer{{Value: "file", Label: "File selected", Sub: "each ticked finding becomes a ticket: a change goes to triage, an intent to its designer; the hunt closes", Primary: true},
			{Value: "file_none", Label: "File none", Sub: "the hunt closes; nothing is filed", Danger: true}}
	case model.AskQuestion:
		choices, _ := Choices(t)
		return []Answer{{Value: "answer", Label: "Send the answer", Primary: true, NeedsNote: len(choices) == 0}}
	case model.AskDeploy:
		return []Answer{{Value: "deploy", Label: "Deploy it", Sub: "the master builds main, installs it and checks Foreman; a failed check puts the running build back", Primary: true},
			{Value: "drop", Label: "Not now", Sub: "the ticket closes; the next merge asks again", Danger: true}}
	case model.AskDraft:
		return draftAnswers(t)
	}
	return nil
}

// publicAnswers are the answers to a public review that blocked a change:
// back to work with a note (the worker sees the findings too, and a second
// block comes back here), a person's pass of the head public review read,
// or drop.
func publicAnswers(t *ticket.Ticket) []Answer {
	out := []Answer{{Value: "send_back", Label: "Send back with a note", Primary: true, NeedsNote: true, NoteFor: "to send it back",
		Sub: "a worker rewrites its commits with the findings and your note; public review reads it again"}}
	if h := t.HeadBeforePublic.Or(""); h != "" {
		out = append(out, Answer{Value: "publish", Label: "Safe to publish at " + textx.Prefix(h, 12),
			Sub: "you read it all: your word stands for public review's pass of this commit only, and the gate checks it next"})
	}
	return append(out, Answer{Value: "drop", Label: "Drop", Sub: "the ticket closes; nothing is published", Danger: true})
}

// draftAnswers are the answers to a manager's drafts and actions:
// "I've applied it" while a draft is the maintainer's to apply by hand,
// and "Apply it", the maintainer's tap, while an item waits or failed.
// While the factory has an item to run (new, or tapped) only send back and
// drop are offered. A ticket with no items (an ask stored before the operator) keeps
// today's answers.
func draftAnswers(t *ticket.Ticket) []Answer {
	c := ticket.Items(t)
	apply := Answer{Value: "apply", Label: "Apply it", Primary: true,
		Sub: "each waiting change goes to config main as a PR the factory merges, then deploys (or asks Deploy? while held); each action runs now; a rebuild pauses ticks while it builds"}
	sendBack := Answer{Value: "send_back", Label: "Send back with a note", Sub: "the manager redrafts it", NeedsNote: true, NoteFor: "to send it back"}
	waits := c.Waiting+c.Failed > 0
	drop := Answer{Value: "drop", Label: "Drop", Sub: "anything already applied stays; the ticket closes", Danger: true}
	if c.New+c.Requested > 0 {
		return []Answer{sendBack, drop}
	}
	if c.ByHand > 0 || len(t.Drafts)+len(t.Actions) == 0 {
		applied := Answer{Value: "applied", Label: "I've applied it", Sub: "you deployed it from your machine; the ticket closes", Primary: true}
		out := []Answer{applied}
		if waits {
			out[0].Sub = "you deployed the by-hand drafts from your machine; the ticket closes and what waits for a tap is not applied"
			apply.Primary = false
			out = append(out, apply)
		}
		return append(out, sendBack, Answer{Value: "drop", Label: "Drop", Sub: "nothing changes; the ticket closes", Danger: true})
	}
	if waits {
		return []Answer{apply, sendBack, drop}
	}
	return []Answer{sendBack, drop}
}

// unapplied names the items "I've applied it" leaves waiting or failed,
// for the move's detail: "" when there are none.
func unapplied(t *ticket.Ticket) string {
	var names []string
	left := func(state string) bool { return state == model.ItemWaiting || state == model.ItemFailed }
	for _, d := range t.Drafts {
		if left(ticket.DraftState(d)) {
			names = append(names, d.Title)
		}
	}
	for _, a := range t.Actions {
		if left(ticket.ActionState(a)) {
			names = append(names, a.ItemLabel())
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "not applied: " + strings.Join(names, ", ")
}

func orSub(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// safeURL keeps only an absolute http(s) URL with a host (Foreman's
// safeURL): a link built from ticket data never runs script.
func safeURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// AnswerIn is one answer a person gave on a ticket.
type AnswerIn struct {
	ID    string
	Value string // an Answer's Value, or AnswerNote, AnswerAsk or AnswerTalk
	Note  string // as typed
	Picks []int  // the design's proposed tickets, or the findings, ticked, 1-based
	// Options are the keys of an agent's question's options, chosen.
	Options []string
	// As is how a ticked finding is filed: change or intent; one it does not
	// name is filed by its default (an intent when it changes behaviour).
	As map[int]string
	// Kind and Since are the ask the person answered, as they saw it; when
	// either is set, an answer to an ask that has moved on is ErrStale.
	Kind, Since string
	// By is the Foreman user answering, recorded as by on what the answer
	// writes; "" is the maintainer.
	By string
}

// ErrNoteNeeded is an answer that needs a note given without one.
var ErrNoteNeeded = errors.New("that answer needs a note")

// Answer carries out one answer to a ticket's ask (Foreman's Plan and the
// commands it ran): under the ticket's lock, on a fresh read,
// with one save. A note typed with any answer is added first, for the next
// agent. It returns what the commands would have printed, for the person.
//
// The note is trimmed and must be at most 4000 bytes with no control
// characters but tab and line breaks; it never reaches an argv, so a leading
// "-" is just text.
func (s *Service) Answer(ctx context.Context, in AnswerIn) (string, error) {
	note, err := cleanNote(in.Note)
	if err != nil {
		return "", err
	}
	in.Options = optionKeys(in.Options)
	switch in.Value {
	case AnswerAsk:
		if note == "" {
			return "", refuse(BadRequest, "write the question first")
		}
	case AnswerTalk:
		if note == "" && len(in.Options) == 0 {
			return "", refuse(BadRequest, "write your reply first")
		}
	}
	var out string
	err = s.change(ctx, in.ID, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		if err := c.actAs(in.By); err != nil {
			return err
		}
		a := s.askOf(t)
		free := in.Value == AnswerNote || in.Value == AnswerAsk || in.Value == AnswerTalk || in.Value == AnswerAfter
		if !free && (in.Kind != "" || in.Since != "") && (a == nil || a.Kind != in.Kind || a.Since != in.Since) {
			return fmt.Errorf("%s: %w", t.ID, ErrStale)
		}
		if err := c.answer(ctx, t, a, in); err != nil {
			return err
		}
		out = strings.Join(c.out, "\n")
		return nil
	})
	return out, err
}

// cleanNote is a typed note trimmed, refused when it is longer than MaxNote
// bytes or has control characters but tab and line breaks.
func cleanNote(s string) (string, error) {
	note := strings.TrimSpace(s)
	if len(note) > MaxNote {
		return "", refuse(BadRequest, "the note is longer than %d characters", MaxNote)
	}
	for _, r := range note {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return "", refuse(BadRequest, "the note has control characters")
		}
	}
	return note, nil
}

// askOf is the ask the desk shows for t.
func (s *Service) askOf(t *ticket.Ticket) *ticket.Ask {
	if s.AskOf != nil {
		return s.AskOf(t)
	}
	if t.Board() {
		return nil
	}
	for _, col := range model.AskColumns() {
		if t.Column == string(col) {
			if a, ok := t.Ask.Get(); ok && a.Kind != "" {
				return &a
			}
			return nil
		}
	}
	return nil
}

// answer is Plan, carried out on the ticket in hand.
func (c *call) answer(ctx context.Context, t *ticket.Ticket, a *ticket.Ask, in AnswerIn) error {
	value, note, picks := in.Value, strings.TrimSpace(in.Note), in.Picks
	switch value {
	case AnswerAsk:
		child, err := c.ask(ctx, t, note)
		c.print(string(child))
		return err
	case AnswerTalk:
		line, err := c.reply(ctx, t, note, in.Options)
		c.print(line)
		return err
	case AnswerAfter:
		return c.setAfter(t, note)
	}
	// then: a note first, if one was typed (or needed), and the answer's action
	var act func() error
	need := false
	move := func(col string) func() error { return func() error { return c.move(ctx, t, col, "") } }
	switch {
	case value == AnswerNote:
		if closedColumn(t.Column) || t.Column == string(model.BoardDropped) {
			return refuse(Conflict, "closed tickets take no notes")
		}
		need = true
	case a == nil:
		if value == "open_pr" && t.Column == string(model.ColReadyForPR) {
			url, err := c.openPR(ctx, t)
			c.printNonEmpty(url)
			return err
		}
		return refuse(Conflict, "this ticket is not waiting for an answer")
	default:
		// every Foreman user may give every answer; each is recorded as
		// theirs
		switch a.Kind + ":" + value {
		case "review:merge":
			if !mergeable(t) {
				return refuse(Conflict, "this PR is not one the factory may merge; review and merge it on GitHub")
			}
			line, err := c.requestMerge(t)
			c.print(line)
			return err
		case "open:open_pr":
			url, err := c.openPR(ctx, t)
			c.printNonEmpty(url)
			return err
		case "unstick:send_back", "unstick:publish":
			if !ticket.PublicBlocked(t) {
				return refuse(BadRequest, "%q is not an answer to this ask", value)
			}
			if value == "send_back" {
				need, act = true, func() error { return c.publicBack(t) }
			} else {
				act = func() error { return c.publicPass(t) }
			}
		case "review:send_back", "open:send_back", "draft:send_back":
			need, act = true, move(string(model.ColReady))
		case "start:start", "decide:start":
			act = move(string(model.ColReady))
		case "deploy:deploy":
			act = func() error { return c.requestDeploy(t) }
		case "start:not_now", "decide:drop", "proposal:drop", "unstick:drop", "draft:drop", "deploy:drop":
			act = move(string(model.ColRejected))
		case "decide:reply":
			need, act = true, move(string(model.ColTriage))
		case "question:answer":
			if note == "" && len(in.Options) == 0 {
				return ErrNoteNeeded
			}
			line, err := c.reply(ctx, t, note, in.Options)
			c.print(line)
			return err
		case "design:approve":
			only, err := designPicks(len(t.Proposed), picks)
			if err != nil {
				return err
			}
			act = func() error { return c.approveOut(ctx, t, only) }
		case "design:redirect":
			if note == "" {
				return ErrNoteNeeded
			}
			// the note travels with the redirect itself
			return c.redirect(t, note)
		case "proposal:file":
			act = func() error { return c.approveOut(ctx, t, nil) }
		case "unstick:retry", "unstick:retry_note":
			rc := a.RetryColumn.Or("")
			if rc == "" || !model.IsColumn(rc) {
				return refuse(Conflict, "there is nothing to retry from; drop it or add a note")
			}
			need, act = value == "retry_note", move(rc)
		case "draft:apply":
			act = func() error { return c.requestApply(t) }
		case "draft:applied":
			n := ticket.Items(t)
			if n.New+n.Requested > 0 {
				// closing now would drop what the factory has still to run
				return refuse(Conflict, "the factory is still applying the manager's changes; wait for the tick")
			}
			if n.ByHand == 0 && len(t.Drafts)+len(t.Actions) > 0 {
				return refuse(BadRequest, "no draft here is yours to apply by hand")
			}
			act = func() error { return c.move(ctx, t, string(model.ColDone), unapplied(t)) }
		case "notice:done":
			act = move(string(model.ColDone))
		case "notice:verify":
			act = func() error { return c.verify(t) }
		case "findings:file":
			act = func() error {
				_, err := c.fileFindings(ctx, t, picks, in.As)
				return err
			}
		case "findings:file_none":
			act = func() error {
				if err := waitingFindings(t); err != nil {
					return err
				}
				ticket.CloseFindings(t, c.by(), "", c.now)
				return nil
			}
		default:
			return refuse(BadRequest, "%q is not an answer to this ask", value)
		}
	}
	if need && note == "" {
		return ErrNoteNeeded
	}
	if note != "" {
		ticket.AddNote(t, note, c.by(), nil, c.now)
	}
	if act != nil {
		return act()
	}
	return nil
}

// designPicks is Foreman's check of the ticked proposals: each 1..n, at
// least one; nil when all of them are ticked (approve without --only).
func designPicks(n int, picks []int) ([]int, error) {
	seen := map[int]bool{}
	var only []int
	for _, p := range picks {
		if p < 1 || p > n {
			return nil, refuse(BadRequest, "pick numbers 1 to %d", n)
		}
		if !seen[p] {
			seen[p] = true
			only = append(only, p)
		}
	}
	switch len(only) {
	case 0:
		return nil, refuse(BadRequest, "tick at least one ticket to approve")
	case n:
		return nil, nil
	}
	return only, nil
}

func (c *call) approveOut(ctx context.Context, t *ticket.Ticket, only []int) error {
	ids, err := c.approve(ctx, t, only)
	if err != nil {
		return err
	}
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = string(id)
	}
	c.print(strings.Join(names, " "))
	return nil
}

func (c *call) printNonEmpty(s string) {
	if s != "" {
		c.print(s)
	}
}

// publicBack sends a change public review blocked back to work, as public
// review's own first block does: the worker is told the findings beside
// the note, and a block after it comes back to a person.
func (c *call) publicBack(t *ticket.Ticket) error {
	if a, ok := t.Agent.Get(); ok {
		return refuse(Conflict, "%s has a running agent (%s); wait for it first", t.ID, a.Name)
	}
	ticket.Move(t, string(model.ColReady), model.ReasonPublicChanges, "sent back to work after public review by "+c.by(), "", c.by(), c.now)
	return nil
}

// publicPass is a person's word that the head public review read is safe
// to publish: it is recorded as public review's pass of that head, with
// who said so, and the gate checks the change next. A branch that has
// moved since is read again before the gate (the pass names one commit).
func (c *call) publicPass(t *ticket.Ticket) error {
	if a, ok := t.Agent.Get(); ok {
		return refuse(Conflict, "%s has a running agent (%s); wait for it first", t.ID, a.Name)
	}
	h := t.HeadBeforePublic.Or("")
	if h == "" {
		return refuse(Conflict, "public review has not read a commit of this change; send it back instead")
	}
	t.PublicHead = ticket.Some(h)
	ticket.Move(t, string(model.ColGate), model.ReasonPublicPassed, "safe to publish at "+textx.Prefix(h, 12)+", said by "+c.by(), "", c.by(), c.now)
	return nil
}
