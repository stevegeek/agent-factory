// Package panel is the blind voting panel (inventory-features §8): the
// tally of one proposal's scores, the rule sentence that says why it was
// kept or dropped, and the reveal that files every proposal of a prospect
// as its own ticket once the last voter is in.
//
// Everything here is pure: the caller passes the time and the ids of the
// tickets to file, and saves what comes back.
package panel

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// ErrTooFewIDs is a reveal given fewer ids than it files tickets.
var ErrTooFewIDs = errors.New("panel: too few ticket ids for the proposals")

// Tally is panel_tally (F:2025): the scores voters gave proposal index
// (1-based), their mean and population spread, how many were keen (4 or
// more), the priority (mean - spread/2) and whether it survives (two keen
// voters, or a mean of 3.5). Means, spreads and priorities are rounded to
// two places as Python's round() does. Votes is left empty.
func Tally(index int, votes []ticket.Vote) ticket.Panel {
	scores := []int{}
	for _, v := range votes {
		if s, _, ok := v.Score(index); ok {
			scores = append(scores, s)
		}
	}
	if len(scores) == 0 {
		return ticket.Panel{Scores: scores}
	}
	n := float64(len(scores))
	sum, keen := 0, 0
	for _, s := range scores {
		sum += s
		if s >= model.PanelKeenScore {
			keen++
		}
	}
	mean := float64(sum) / n
	sq := 0.0
	for _, s := range scores {
		d := float64(s) - mean
		sq += d * d
	}
	spread := math.Pow(sq/n, 0.5)
	return ticket.Panel{
		Scores: scores, Mean: pyRound2(mean), Spread: pyRound2(spread), Keen: keen,
		Priority: pyRound2(mean - 0.5*spread),
		Survives: keen >= model.PanelKeenNeeded || mean >= model.PanelMeanNeeded,
	}
}

// pyRound2 is Python's round(x, 2): the double nearest to x correctly
// rounded to two decimal places (half to even on the exact value).
func pyRound2(x float64) float64 {
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 2, 64), 64)
	if err != nil {
		return x
	}
	return f
}

// TallyValue is the tally as Python's dict holds it: scores, mean, spread,
// keen, priority, survives, and votes when it has them (a proposal's panel).
func TallyValue(p ticket.Panel) jsonx.Value {
	scores := make([]jsonx.Value, len(p.Scores))
	for i, s := range p.Scores {
		scores[i] = jsonx.Int(int64(s))
	}
	o := jsonx.Object{
		{Key: "scores", Value: jsonx.ArrayOf(scores...)}, {Key: "mean", Value: jsonx.Float(p.Mean)},
		{Key: "spread", Value: jsonx.Float(p.Spread)}, {Key: "keen", Value: jsonx.Int(int64(p.Keen))},
		{Key: "priority", Value: jsonx.Float(p.Priority)}, {Key: "survives", Value: jsonx.Bool(p.Survives)},
	}
	if p.Votes != nil {
		vs := make([]jsonx.Value, len(p.Votes))
		for i, v := range p.Votes {
			vs[i] = jsonx.ObjectOf(jsonx.Member{Key: "voter", Value: jsonx.String(v.Voter)},
				jsonx.Member{Key: "host", Value: jsonx.String(v.Host)},
				jsonx.Member{Key: "score", Value: jsonx.Int(int64(v.Score))},
				jsonx.Member{Key: "reason", Value: jsonx.String(v.Reason)})
		}
		o = append(o, jsonx.Member{Key: "votes", Value: jsonx.ArrayOf(vs...)})
	}
	return o.Value()
}

// Rule is panel_rule (F:2038): the sentence that says why a tally was kept
// or dropped, keen voters first unless the mean alone kept it.
func Rule(p ticket.Panel) string {
	n := len(p.Scores)
	keen := fmt.Sprintf("no voter scored %d or more", model.PanelKeenScore)
	if p.Keen > 0 {
		keen = fmt.Sprintf("%d of %d voters scored %d or more (%d needed)", p.Keen, n, model.PanelKeenScore, model.PanelKeenNeeded)
	}
	mean := "mean " + pyFloat(p.Mean) + " (" + pyFloat(model.PanelMeanNeeded) + " needed)"
	switch {
	case !p.Survives:
		return "dropped: " + keen + "; " + mean
	case p.Keen >= model.PanelKeenNeeded:
		return "kept: " + keen + "; " + mean
	}
	return "kept: " + mean + "; " + keen
}

// Votes is panel_votes (F:2048): each voter's score and reason for
// proposal index, in voting order; a voter with no host ran here.
func Votes(index int, votes []ticket.Vote) []ticket.PanelVote {
	out := []ticket.PanelVote{}
	for _, v := range votes {
		s, reason, ok := v.Score(index)
		if !ok {
			continue
		}
		host := v.Host
		if host == "" {
			host = model.LocalHost
		}
		out = append(out, ticket.PanelVote{Voter: v.Voter, Host: host, Score: s, Reason: reason})
	}
	return out
}

// ProposalText is proposal_text (F:1487): how a proposal reads to a voter
// and, without its heading, on its own ticket.
func ProposalText(i int, p ticket.Proposal) string {
	return fmt.Sprintf("### Proposal %d: %s\n\n- Noticed: %s\n- Change: %s\n- Why it is worth it: %s\n- Cost: %s",
		i, p.Title, p.Noticed, p.Change, p.Why, p.Cost)
}

// File is file_proposal (F:2054): proposal i of prospect t as a ticket
// with id, kind proposal, carrying its tally and votes; a kept one waits for
// a person (asking "proposal"), a dropped one is filed rejected. at, when
// set, back-dates a filing made after the fact (migrate). The caller saves
// it.
func File(t *ticket.Ticket, i int, p ticket.Proposal, votes []ticket.Vote, id ticket.ID, now time.Time, at string) *ticket.Ticket {
	tally := Tally(i, votes)
	tally.Votes = Votes(i, votes)
	reasons := make([]string, len(tally.Votes))
	for k, v := range tally.Votes {
		reasons[k] = strconv.Itoa(v.Score) + ": " + v.Reason
	}
	body := ""
	if parts := strings.SplitN(ProposalText(i, p), "\n", 3); len(parts) == 3 {
		body = parts[2]
	}
	body += "\n\nPanel: " + strings.Join(reasons, "; ")
	column := string(model.ColRejected)
	if tally.Survives {
		column = string(model.ColNeedsHuman)
	}
	c := ticket.New(id, ticket.NewIn{
		Project: t.Project, Title: p.Title, Body: body, Door: string(model.DoorClock), Column: column,
		Fields: jsonx.Object{
			{Key: "kind", Value: jsonx.String(string(model.KindProposal))},
			{Key: "parent", Value: jsonx.String(string(t.ID))},
			{Key: "proposal", Value: proposalValue(p)},
			{Key: "panel", Value: TallyValue(tally)},
		},
	}, now)
	ticket.Event(c, "panel", "mean "+pyFloat(tally.Mean)+", spread "+pyFloat(tally.Spread)+", scores "+intList(tally.Scores),
		Rule(tally), ticket.PanelScored{Kept: tally.Survives}, now)
	if at != "" {
		c.Created = at
		for k := range c.History {
			c.History[k].At = at
		}
		if a, ok := c.Ask.Get(); ok {
			a.Since = at
			c.Ask = ticket.Some(a)
		}
	}
	return c
}

func proposalValue(p ticket.Proposal) jsonx.Value {
	return jsonx.ObjectOf(jsonx.Member{Key: "title", Value: jsonx.String(p.Title)},
		jsonx.Member{Key: "noticed", Value: jsonx.String(p.Noticed)},
		jsonx.Member{Key: "change", Value: jsonx.String(p.Change)},
		jsonx.Member{Key: "why", Value: jsonx.String(p.Why)},
		jsonx.Member{Key: "cost", Value: jsonx.String(p.Cost)})
}

// Needs is how many tickets Reveal files for t: one per proposal.
func Needs(t *ticket.Ticket) int { return len(t.Proposals) }

// Reveal is reveal (F:2073): every proposal of prospect t filed as its own
// ticket (ids[k] for proposal k+1), what the panel kept and dropped
// recorded on t, and t moved to done. The filed tickets come back for the
// caller to save, before it saves t.
func Reveal(t *ticket.Ticket, ids []ticket.ID, now time.Time) ([]*ticket.Ticket, error) {
	if len(ids) < len(t.Proposals) {
		return nil, ErrTooFewIDs
	}
	kept, dropped, droppedIDs := []string{}, []string{}, []string{}
	filed := make([]*ticket.Ticket, 0, len(t.Proposals))
	for k, p := range t.Proposals {
		c := File(t, k+1, p, t.Votes, ids[k], now, "")
		filed = append(filed, c)
		tally, _ := c.Panel.Get()
		if tally.Survives {
			kept = append(kept, string(c.ID))
			continue
		}
		dropped = append(dropped, p.Title+" ("+intList(tally.Scores)+")")
		droppedIDs = append(droppedIDs, string(c.ID))
	}
	t.PanelResult = ticket.Some(ticket.PanelResult{Kept: kept, Dropped: dropped, DroppedTickets: droppedIDs})
	keptText := strings.Join(kept, ", ")
	if keptText == "" {
		keptText = "none"
	}
	ticket.Move(t, string(model.ColDone), model.ReasonPanelDone,
		fmt.Sprintf("panel kept %s; dropped %d", keptText, len(dropped)),
		fmt.Sprintf("panel: kept %d of %d; %d voters scoring %d or more, or a mean of %s, keeps a proposal",
			len(kept), len(t.Proposals), model.PanelKeenNeeded, model.PanelKeenScore, pyFloat(model.PanelMeanNeeded)),
		"", now)
	return filed, nil
}

// intList is Python's str() of a list of ints: "[5, 4, 5]".
func intList(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// pyFloat is str() of a Python float: 4.67, 3.0.
func pyFloat(f float64) string { return string(jsonx.EncodePython(jsonx.Float(f), -1)) }
