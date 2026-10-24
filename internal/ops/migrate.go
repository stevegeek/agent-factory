package ops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// moveDetail is MOVE_DETAIL, read with re.S and fullmatch.
//
// py: (\w+) -> (\w+)(?:: (.*))?
var moveDetail = regexp.MustCompile(`\A(?s:(` + textx.PyWord + `+) -> (` + textx.PyWord + `+)(?:: (.*))?)\z`)

// Migrate backfills a board made before asks and filed votes (migrate, D7):
// answers already on the desk go back to the ticket they were asked on,
// waiting tickets get their ask, and a decided prospect's proposals get
// their votes, with the dropped ones filed as rejected proposal tickets
// back-dated to the decision. It is safe to run again, and returns the
// lines it prints, one per change.
func (s *Service) Migrate(ctx context.Context) ([]string, error) {
	var out []string
	// 1. a task on the desk whose answer belongs to the ticket it was asked on
	answers := func(t *ticket.Ticket) bool {
		return t.KindName() == string(model.KindTask) && t.Column == string(model.ColNeedsHuman) &&
			t.Result.Or("") != "" && s.askedOn(t) != nil
	}
	if err := s.Board.EachLocked(ctx, s.list(), answers, func(t *ticket.Ticket) error {
		summary := ""
		for i := len(t.History) - 1; i >= 0; i-- {
			if h := t.History[i]; h.Event == "move" && strings.Contains(h.Detail, "task done") {
				parts := strings.SplitN(h.Detail, "task done: ", 2)
				summary = parts[len(parts)-1]
				break
			}
		}
		delivered, err := s.deliverAnswer(ctx, t, summary)
		if err != nil || !delivered {
			return err
		}
		ticket.Move(t, string(model.ColDone), model.ReasonAnsweredOn, "answered on "+t.Parent.Or(""), "", "", s.now())
		return s.Board.Save(t)
	}); err != nil {
		return out, err
	}
	// 2. a ticket waiting for a person with no stored ask
	noAsk := func(t *ticket.Ticket) bool { return askColumn(t.Column) && !t.Ask.Present() }
	if err := s.Board.EachLocked(ctx, s.list(), noAsk, func(t *ticket.Ticket) error {
		old, detail, since := "", "", t.Created
		for i := len(t.History) - 1; i >= 0; i-- {
			h := t.History[i]
			if h.Event != "move" {
				continue
			}
			if m := moveDetail.FindStringSubmatch(h.Detail); m != nil && m[2] == t.Column {
				old, detail, since = m[1], m[3], h.At
				break
			}
		}
		a := ticket.AskFor(t, old, detail, since, "")
		t.SetAsk(a)
		if err := s.Board.Save(t); err != nil {
			return err
		}
		line := string(t.ID) + "  ask " + a.Kind
		if rc := a.RetryColumn.Or(""); rc != "" {
			line += " (retry from " + rc + ")"
		}
		out = append(out, line)
		return nil
	}); err != nil {
		return out, err
	}
	// 3. a decided prospect whose panel's votes are not on its proposals
	unmigrated := func(t *ticket.Ticket) bool {
		return t.KindName() == string(model.KindProspect) && t.Column == string(model.ColDone) && !t.Migrated.Or(false) &&
			len(t.Votes) > 0 && t.PanelResult.Present()
	}
	prospects := s.list()
	for _, t := range prospects {
		if unmigrated(t) && s.Panel == nil {
			return out, errors.New("migrate: no panel rules to file the prospects' proposals with")
		}
	}
	err := s.Board.EachLocked(ctx, prospects, unmigrated, func(t *ticket.Ticket) error {
		lines, err := s.migrateProspect(ctx, t)
		out = append(out, lines...)
		return err
	})
	return out, err
}

func (s *Service) migrateProspect(ctx context.Context, t *ticket.Ticket) ([]string, error) {
	var out []string
	decided := ""
	for i := len(t.History) - 1; i >= 0; i-- {
		h := t.History[i]
		head, _, _ := strings.Cut(h.Detail, ":")
		if h.Event == "move" && strings.HasSuffix(head, "-> done") {
			decided = h.At
			break
		}
	}
	var children []*ticket.Ticket
	for _, c := range s.list() {
		if c.KindName() == string(model.KindProposal) && c.Parent.Or("") == string(t.ID) {
			children = append(children, c)
		}
	}
	var filed []string
	for n, p := range t.Proposals {
		i := n + 1
		var mine *ticket.Ticket
		for _, c := range children {
			if cp, ok := c.Proposal.Get(); ok && cp.Title == p.Title {
				mine = c
				break
			}
		}
		switch {
		case mine == nil && !s.Panel.Survives(i, t.Votes):
			id, err := s.Board.NextID(ctx)
			if err != nil {
				return out, err
			}
			c := s.Panel.FileProposal(id, t, i, decided, s.now())
			if err := s.Board.Save(c); err != nil {
				return out, err
			}
			filed = append(filed, string(c.ID))
			out = append(out, fmt.Sprintf("%s  dropped proposal %d of %s filed as rejected", c.ID, i, t.ID))
		case mine != nil && !hasVotes(mine):
			n, err := s.addVotes(ctx, mine, i, t.Votes)
			if err != nil {
				return out, err
			}
			out = append(out, fmt.Sprintf("%s  votes added (%d)", mine.ID, n))
		}
	}
	pr := t.PanelResult.V
	pr.DroppedTickets = append(append([]string{}, pr.DroppedTickets...), filed...)
	t.PanelResult = ticket.Some(pr)
	t.Migrated = ticket.Some(true)
	if err := s.Board.Save(t); err != nil {
		return out, err
	}
	return append(out, string(t.ID)+"  prospect migrated"), nil
}

// hasVotes is "votes" in proposal.get("panel", {}).
func hasVotes(t *ticket.Ticket) bool {
	_, ok := lookup(t.Tree().Get("panel"), "votes")
	return ok
}

// addVotes puts the panel's votes for proposal i on its ticket, under the
// ticket's lock on a fresh read, and returns how many there are.
func (s *Service) addVotes(ctx context.Context, mine *ticket.Ticket, i int, votes []ticket.Vote) (int, error) {
	l, err := s.Board.Lock(ctx, mine.ID, store.Forever)
	if err != nil {
		return 0, err
	}
	defer l.Unlock()
	fresh, err := s.Board.Reader.Load(mine.ID)
	if err != nil {
		if !isNotExist(err) {
			return 0, err
		}
		fresh = mine
	}
	panel := fresh.Panel.V
	panel.Votes = s.Panel.Votes(i, votes)
	if panel.Votes == nil {
		panel.Votes = []ticket.PanelVote{}
	}
	fresh.Panel = ticket.Some(panel)
	if err := s.Board.Save(fresh); err != nil {
		return 0, err
	}
	return len(panel.Votes), nil
}

// askedOn is asked_on: the open ticket whose Ask filed this task, or nil.
func (s *Service) askedOn(t *ticket.Ticket) *ticket.Ticket {
	pid, ok := ticket.ParseID(t.Parent.Or(""))
	if !ok || string(pid) != t.Parent.Or("") {
		return nil
	}
	p, err := s.Board.Reader.Load(pid)
	if err != nil || closedColumn(p.Column) {
		return nil
	}
	for _, h := range p.History {
		if h.Event != "asked" {
			continue
		}
		if a, typed := h.Data.(ticket.Asked); typed {
			if a.Ticket == string(t.ID) {
				return p
			}
		} else if strings.HasPrefix(h.Detail, string(t.ID)+":") {
			return p
		}
	}
	return nil
}

// deliverAnswer is deliver_answer: the answer goes back to the ticket it
// was asked on, under that ticket's lock; false when there is none open.
func (s *Service) deliverAnswer(ctx context.Context, t *ticket.Ticket, summary string) (bool, error) {
	if s.askedOn(t) == nil {
		return false, nil
	}
	pid := ticket.ID(t.Parent.Or(""))
	l, err := s.Board.Lock(ctx, pid, store.Forever)
	if err != nil {
		return false, err
	}
	defer l.Unlock()
	p, err := s.Board.Reader.Load(pid)
	if err != nil {
		return false, err
	}
	if closedColumn(p.Column) {
		return false, nil
	}
	text := textx.Clip(summary, 300)
	ticket.Event(p, "answered", string(t.ID)+": "+text, "", ticket.Answered{Ticket: string(t.ID), Text: text}, s.now())
	return true, s.Board.Save(p)
}

func (s *Service) list() []*ticket.Ticket {
	ts, _ := s.Board.Reader.List()
	return ts
}

func askColumn(col string) bool {
	for _, c := range model.AskColumns() {
		if string(c) == col {
			return true
		}
	}
	return false
}
