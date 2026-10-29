package ops

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Move moves a ticket by hand (cmd_move): to a factory column, never while
// an agent runs, recorded as the maintainer's with the note or "by human".
// Leaving pr_open for anything but done or rejected holds the PR (back to
// draft, so the old version cannot be merged meanwhile). It returns the
// warning to print when the PR could not be held, else "".
func (s *Service) Move(ctx context.Context, id, column, note string) (string, error) {
	if !model.IsColumn(column) {
		names := make([]string, 0, 12)
		for _, c := range model.Columns() {
			names = append(names, string(c.Name))
		}
		return "", refuse(BadRequest, "column must be one of: %s", strings.Join(names, ", "))
	}
	var out string
	err := s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		if err := c.move(ctx, t, column, note); err != nil {
			return err
		}
		out = strings.Join(c.out, "\n")
		return nil
	})
	return out, err
}

// move is cmd_move's change on the ticket in hand.
func (c *call) move(ctx context.Context, t *ticket.Ticket, column, note string) error {
	if a, ok := t.Agent.Get(); ok {
		return refuse(Conflict, "%s has a running agent (%s); wait for it first", t.ID, a.Name)
	}
	old := t.Column
	if note == "" {
		note = "by human"
	}
	ticket.Move(t, column, model.ReasonByHuman, note, "", c.by(), c.now)
	if old == string(model.ColPROpen) && column != string(model.ColPROpen) && column != string(model.ColDone) &&
		column != string(model.ColRejected) {
		c.hold(ctx, t)
	}
	return nil
}

// hold is hold_pr through PRActions, with Python's warning when it fails.
func (c *call) hold(ctx context.Context, t *ticket.Ticket) {
	if c.s.PR == nil {
		return
	}
	if err := c.s.PR.Hold(ctx, c.cfg, t); err != nil {
		n := 0
		if pr, ok := t.PR.Get(); ok {
			n = pr.Number
		}
		c.print(fmt.Sprintf("warning: PR #%d is still open for merging; it could not be turned back to draft", n))
	}
}

// Note adds the maintainer's note (cmd_note): on a factory ticket every
// later agent gets it; on a board ticket it joins the conversation as a
// comment.
func (s *Service) Note(ctx context.Context, id, text string) error {
	t, err := s.load(id)
	if err == nil && t.Board() {
		if s.Notes == nil {
			return refuse(FactoryTicket, "%s is on a board project; use `factory ticket note`", t.ID)
		}
		return s.Notes.BoardNote(ctx, t.ID, text)
	}
	return s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if t.Board() { // it was not readable before the lock
			return refuse(FactoryTicket, "%s is on a board project; use `factory ticket note`", t.ID)
		}
		ticket.AddNote(t, text, c.by(), nil, c.now)
		return nil
	})
}

// Host pins a ticket's stages to a host, or unpins it with "auto"
// (cmd_host).
func (s *Service) Host(ctx context.Context, id, host string) error {
	return s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		if err := knownHost(c.cfg, host); err != nil {
			return err
		}
		if err := kindRefused(c.cfg, host, t.Project); err != nil {
			return err
		}
		c.pinHost(t, host)
		return nil
	})
}

// waiting is intent_waiting: a ticket of one of kinds that waits for the
// maintainer's review, with no agent on it.
func waiting(t *ticket.Ticket, kinds ...string) error {
	if !contains(kinds, t.KindName()) || t.Column != string(model.ColNeedsHuman) || t.Agent.Present() {
		return refuse(Conflict, "%s is not an %s waiting for review", t.ID, strings.Join(kinds, " or "))
	}
	return nil
}

// Approve files an intent's chosen proposed tickets (all of them when only
// is empty), or a proposal's change ticket, into ready, and closes the
// intent or proposal (cmd_approve). It returns the new tickets' ids.
func (s *Service) Approve(ctx context.Context, id string, only []int) ([]ticket.ID, error) {
	var ids []ticket.ID
	err := s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		var err error
		ids, err = c.approve(ctx, t, only)
		return err
	})
	return ids, err
}

// approve is approve() on the ticket in hand.
func (c *call) approve(ctx context.Context, t *ticket.Ticket, only []int) ([]ticket.ID, error) {
	if err := waiting(t, string(model.KindIntent), string(model.KindProposal)); err != nil {
		return nil, err
	}
	board := c.s.Board
	if t.KindName() == string(model.KindProposal) {
		p, ok := t.Proposal.Get()
		if !ok {
			return nil, refuse(Conflict, "%s has no proposal to file", t.ID)
		}
		child, err := board.FileTicket(ctx, ticket.NewIn{Project: t.Project, Title: p.Title,
			Body: p.Change + "\n\nWhy: " + p.Why + "\n\nNoticed: " + p.Noticed, Door: string(model.DoorHuman),
			Column: string(model.ColReady), By: c.by()})
		if err != nil {
			return nil, err
		}
		child.Parent = ticket.Some(string(t.ID))
		if err := board.Save(child); err != nil {
			return nil, err
		}
		t.Children = []string{string(child.ID)}
		ticket.Move(t, string(model.ColDone), model.ReasonProposalApproved, "proposal approved; filed "+string(child.ID),
			"", c.by(), c.now)
		return []ticket.ID{child.ID}, nil
	}
	proposed := t.Proposed
	if len(proposed) == 0 {
		return nil, refuse(Conflict, "%s has no proposed tickets; redirect it instead", t.ID)
	}
	picks := dedupe(only)
	if len(picks) == 0 {
		for i := range proposed {
			picks = append(picks, i+1)
		}
	}
	for _, i := range picks {
		if i < 1 || i > len(proposed) {
			return nil, refuse(BadRequest, "pick numbers 1..%d", len(proposed))
		}
	}
	// a ticket that waits for a sibling is filed after it, in the numbers'
	// order; the after list goes in with the ticket's first save, so no
	// tick can start it before it waits
	if slices.ContainsFunc(picks, func(i int) bool { return slices.ContainsFunc(proposed[i-1].After, isNumber) }) {
		slices.Sort(picks)
	}
	all, _ := board.Reader.List()
	byID := ticket.ByID(all)
	filed := map[int]ticket.ID{}
	var ids []ticket.ID
	var names []string
	for _, i := range picks {
		p := proposed[i-1]
		after, unmet := proposedAfter(p, filed, byID)
		body := p.Body + "\n\nPart of: " + t.Title
		if len(unmet) > 0 {
			body += "\n\nThe design had this ticket wait for " + strings.Join(unmet, ", ") +
				", which was not filed with it: check whether it still needs that change."
		}
		var fields jsonx.Object
		if len(after) > 0 {
			list := make([]jsonx.Value, len(after))
			for k, id := range after {
				list[k] = jsonx.String(string(id))
			}
			fields = jsonx.Object{{Key: "after", Value: jsonx.ArrayOf(list...)}}
		}
		child, err := board.FileTicket(ctx, ticket.NewIn{Project: t.Project, Title: p.Title,
			Body: body, Door: string(model.DoorHuman), Column: string(model.ColReady),
			By: c.by(), Fields: fields})
		if err != nil {
			return nil, err
		}
		filed[i] = child.ID
		child.Parent = ticket.Some(string(t.ID))
		if h, ok := t.Host.Get(); ok && h != "" {
			child.Host = ticket.Some(h)
		}
		if err := board.Save(child); err != nil {
			return nil, err
		}
		ids = append(ids, child.ID)
		names = append(names, string(child.ID))
	}
	t.Children = names
	ticket.Move(t, string(model.ColDone), model.ReasonDesignApproved, "design approved; filed "+strings.Join(names, ", "),
		"", c.by(), c.now)
	return ids, nil
}

// isNumber is a proposed ticket's after entry that names a sibling.
func isNumber(a string) bool {
	_, err := strconv.Atoi(a)
	return err == nil
}

// proposedAfter turns a proposed ticket's after list into the ids its filed
// ticket waits for: an earlier sibling by the id it was filed as, an open
// ticket by its id. A sibling not filed with it, or a ticket closed without
// being done or gone, is returned as unmet; a done ticket needs no wait.
func proposedAfter(p ticket.Proposed, filed map[int]ticket.ID, byID map[ticket.ID]*ticket.Ticket) (after []ticket.ID, unmet []string) {
	for _, a := range p.After {
		if n, err := strconv.Atoi(a); err == nil {
			if id, ok := filed[n]; ok {
				after = append(after, id)
			} else {
				unmet = append(unmet, "proposed ticket "+a)
			}
			continue
		}
		id, ok := ticket.ParseID(a)
		if !ok {
			continue
		}
		d, known := byID[id]
		switch {
		case known && d.Column == string(model.ColDone):
		case known && !model.Closed(d.Column):
			after = append(after, id)
		default:
			unmet = append(unmet, string(id))
		}
	}
	return after, unmet
}

// dedupe is list(dict.fromkeys(xs)).
func dedupe(xs []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Redirect sends an intent's design back to the designer with the
// maintainer's note (cmd_redirect): the design and its proposed tickets are
// kept as feedback, and the intent goes back to ready.
func (s *Service) Redirect(ctx context.Context, id, note string) error {
	return s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		return c.redirect(t, note)
	})
}

func (c *call) redirect(t *ticket.Ticket, note string) error {
	if err := waiting(t, string(model.KindIntent)); err != nil {
		return err
	}
	if t.Feedback == nil {
		t.Feedback = []ticket.Feedback{}
	}
	t.Feedback = append(t.Feedback, ticket.Feedback{At: clockStamp(c), Design: t.Design.Or(""), Note: note})
	t.Design = ticket.Some("")
	t.Proposed = []ticket.Proposed{}
	ticket.Move(t, string(model.ColReady), model.ReasonRedirected, "redirected: "+textx.Prefix(note, 150), "", c.by(), c.now)
	return nil
}

// RequestMerge records the maintainer's go-ahead for an eligible PR
// (cmd_merge) and wakes the tick, which merges it: the merge may first
// re-run the checks with main merged in, which takes minutes. It returns the
// line to print.
func (s *Service) RequestMerge(ctx context.Context, id string) (string, error) {
	var out string
	err := s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		var err error
		out, err = c.requestMerge(t)
		return err
	})
	return out, err
}

func (c *call) requestMerge(t *ticket.Ticket) (string, error) {
	if p, _ := projectOf(c.cfg, t.Project); p.GitHub == "" {
		return "", refuse(Conflict, "%s has no GitHub repository", t.Project)
	}
	pr, ok := t.PR.Get()
	if t.Column != string(model.ColPROpen) || !ok {
		return "", refuse(Conflict, "%s has no open PR", t.ID)
	}
	if g, ok := t.Gate.Get(); !ok || !g.AutoEligible {
		return "", refuse(Conflict, "%s is not auto-merge eligible; review and merge it on GitHub", t.ID)
	}
	if ticket.NeedsApproval(t) {
		// GitHub would refuse it: the approval is the go-ahead, and the tick
		// merges on it by itself
		return "", refuse(Conflict, "GitHub merges it only once you approve it: approve PR #%d on GitHub, and the factory merges it by itself on the next tick", pr.Number)
	}
	pr.MergeRequested = ticket.Some(clockStamp(c))
	t.PR = ticket.Some(pr)
	n := strconv.Itoa(pr.Number)
	ticket.Event(t, "pr", "you asked the factory to merge PR #"+n+"; it merges on the next tick", "",
		ticket.PRNote{Action: ticket.PRMergeRequested, Number: pr.Number}, c.now)
	c.wake = true
	return "PR #" + n + " merges on the next tick (a minute or two; longer if main moved)", nil
}

// OpenPR opens or updates the ticket's PR through PRActions (cmd_pr) and
// returns its URL ("" when only the request was recorded).
func (s *Service) OpenPR(ctx context.Context, id string) (string, error) {
	var url string
	err := s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		var err error
		url, err = c.openPR(ctx, t)
		return err
	})
	return url, err
}

func (c *call) openPR(ctx context.Context, t *ticket.Ticket) (string, error) {
	if c.s.PR == nil {
		return "", refuse(Conflict, "opening a PR is not available here")
	}
	if _, ok := c.cfg.Project(t.Project); !ok {
		return "", refuse(Conflict, "unknown project %s", t.Project)
	}
	return c.s.PR.Open(ctx, c.cfg, t)
}

// Result is what `factory result` prints, without its last newline: an
// intent's design and proposed tickets with the approve/redirect hint, or a
// task's result (or summary).
func (s *Service) Result(ctx context.Context, id string) (string, error) {
	t, err := s.load(id)
	if err != nil {
		return "", err
	}
	if t.KindName() == string(model.KindIntent) {
		d := t.Design.Or("")
		if d == "" {
			return "", refuse(Conflict, "%s has no design yet", t.ID)
		}
		var b strings.Builder
		b.WriteString(textx.RStrip(d) + "\n\n## Proposed tickets\n\n")
		for i, p := range t.Proposed {
			title := p.Title
			if len(p.After) > 0 {
				title += " (after " + p.AfterLabel() + ")"
			}
			fmt.Fprintf(&b, "%d. %s\n\n   %s\n\n", i+1, title, strings.ReplaceAll(p.Body, "\n", "\n   "))
		}
		fmt.Fprintf(&b, "factory approve %s [--only 1,2]  |  factory redirect %s --note \"...\"", t.ID, t.ID)
		return b.String(), nil
	}
	if r := t.Result.Or(""); r != "" {
		return r, nil
	}
	if task, ok := t.Reports.Lookup("task"); ok {
		if v := task.Get("summary"); jsonx.Truthy(v) {
			return jsonx.PyStr(v), nil
		}
	}
	return string(t.ID) + " has no task result", nil
}

// load reads a ticket outside its lock (load_ticket): a name that is not an
// id, or a ticket that is not there, is "no ticket <ID>".
func (s *Service) load(name string) (*ticket.Ticket, error) {
	id, ok := ticket.ParseID(name)
	if !ok {
		return nil, &store.NotFoundError{ID: strings.ToUpper(name)}
	}
	t, err := s.Board.Reader.Load(id)
	if err != nil {
		if isNotExist(err) {
			return nil, &store.NotFoundError{ID: string(id)}
		}
		return nil, err
	}
	return t, nil
}

func clockStamp(c *call) string { return stampOf(c.now) }
