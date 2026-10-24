package stage

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const proposals3 = `{"title": "P1", "noticed": "n", "change": "c", "why": "w", "cost": "small"},
	{"title": "P2", "noticed": "n", "change": "c", "why": "w", "cost": "small"},
	{"title": "P3", "noticed": "n", "change": "c", "why": "w", "cost": "small"}`

func prospecting(t *testing.T) *ticket.Ticket {
	tk := decode(t, `{"id": "T0001", "project": "g", "title": "Scan", "body": "b", "door": "clock", "by": "factory", "column": "working",
		"branch": null, "agent": {"name": "t0001-p1", "stage": "prospect", "started": "x", "host": "local"}, "reports": {},
		"created": "x", "history": [], "kind": "prospect", "ask": null}`)
	return tk
}

func vote(t *testing.T, tk *ticket.Ticket, c *config.Config, scores ...int) Outcome {
	t.Helper()
	tk.Agent = ticket.Some(ticket.Agent{Name: fmt.Sprintf("v%d", len(tk.Votes)), Stage: "vote", Started: "x", Host: "pi"})
	var parts []string
	for i, s := range scores {
		parts = append(parts, fmt.Sprintf(`{"index": %d, "score": %d, "reason": "r"}`, i+1, s))
	}
	return finish(t, FinishIn{Ticket: tk, Report: report(`{"scores": [` + strings.Join(parts, ", ") + `]}`), Config: c})
}

func TestVotersAreBlindAndThePanelDropsOneVoterWonders(t *testing.T) {
	// port: TF PanelTest.test_voters_are_blind_and_the_panel_drops_one_voter_wonders
	c := cfg(t, "")
	tk := prospecting(t)
	tk.Column = "ready"
	if s, _ := ForTicket(tk); s != model.StageProspect {
		t.Errorf("stage %s", s)
	}
	tk.Column = "working"
	out := finish(t, FinishIn{Ticket: tk, Report: report(`{"proposals": [` + proposals3 + `, {"title": "no evidence"}]}`), Config: c})
	tk = out.Ticket
	if s, _ := ForTicket(tk); tk.Column != "voting" || len(tk.Proposals) != 3 || s != model.StageVote || tk.Votes == nil {
		t.Fatalf("%s %d %s", tk.Column, len(tk.Proposals), s)
	}
	tk = vote(t, tk, c, 5, 5, 2).Ticket
	prompt := For(model.StageVote).Prompt(PromptIn{Ticket: tk})
	if !strings.Contains(prompt, "Proposal 3: P3") || strings.Contains(strings.Split(prompt, "Proposal 1")[0], "5") {
		t.Errorf("vote prompt:\n%s", prompt)
	}
	if s, _ := ForTicket(tk); tk.Column != "voting" || s != model.StageVote || last(tk).Detail != "1/3 in" {
		t.Errorf("%s %s %q", tk.Column, s, last(tk).Detail)
	}
	tk = vote(t, tk, c, 4, 2, 1).Ticket
	out = vote(t, tk, c, 5, 3, 2)
	tk = out.Ticket
	if tk.Column != "done" || len(out.File) != 3 {
		t.Fatalf("%s, filed %d", tk.Column, len(out.File))
	}
	var kept, dropped []*ticket.Ticket
	for _, f := range out.File {
		switch f.Column {
		case "needs_human":
			kept = append(kept, f)
		case "rejected":
			dropped = append(dropped, f)
		}
	}
	if len(kept) != 1 || kept[0].Title != "P1" {
		t.Fatalf("kept %v", kept)
	}
	if p, _ := kept[0].Panel.Get(); !reflect.DeepEqual(p.Scores, []int{5, 4, 5}) {
		t.Errorf("scores %v", p.Scores)
	}
	if _, ok := ForTicket(kept[0]); ok {
		t.Error("a proposal runs no stage")
	}
	r, _ := tk.PanelResult.Get()
	if len(r.Dropped) != 2 || !reflect.DeepEqual(r.DroppedTickets, []string{string(dropped[0].ID), string(dropped[1].ID)}) {
		t.Errorf("panel result %+v", r)
	}
	if a, _ := kept[0].Ask.Get(); a.Kind != "proposal" {
		t.Errorf("ask %+v", a)
	}
	if got := last(kept[0]).Rule; got != "kept: 3 of 3 voters scored 4 or more (2 needed); mean 4.67 (3.5 needed)" {
		t.Errorf("rule %q", got)
	}
	for _, d := range dropped {
		if d.Ask.Present() {
			t.Errorf("%s asks %+v", d.Title, d.Ask)
		}
	}
	p, _ := dropped[0].Panel.Get()
	want := []ticket.PanelVote{{Voter: "v0", Host: "pi", Score: 5, Reason: "r"}, {Voter: "v1", Host: "pi", Score: 2, Reason: "r"},
		{Voter: "v2", Host: "pi", Score: 3, Reason: "r"}}
	if !reflect.DeepEqual(p.Votes, want) {
		t.Errorf("votes %+v", p.Votes)
	}
	if got := last(dropped[0]).Rule; got != "dropped: 1 of 3 voters scored 4 or more (2 needed); mean 3.33 (3.5 needed)" {
		t.Errorf("rule %q", got)
	}
	if got := last(dropped[1]).Rule; got != "dropped: no voter scored 4 or more; mean 1.67 (3.5 needed)" {
		t.Errorf("rule %q", got)
	}
	if last(tk).Rule == "" || reason(tk) != model.ReasonPanelDone {
		t.Error("the reveal names its rule")
	}
}

func TestInvalidVotesAreIgnored(t *testing.T) {
	c := cfg(t, "[panel]\nvoters = 1\n")
	tk := prospecting(t)
	tk = finish(t, FinishIn{Ticket: tk, Report: report(`{"proposals": [` + proposals3 + `]}`), Config: c}).Ticket
	tk.Agent = ticket.Some(ticket.Agent{Name: "t0001-v1", Stage: "vote", Started: "x"})
	tk.AgentLastHost = ticket.Opt[string]{}
	r := report(`{"scores": [
		{"index": 1, "score": 0}, {"index": 4, "score": 3}, {"index": 0, "score": 3}, {"index": 2, "score": 6},
		{"index": "x", "score": 3}, {"score": 3}, "junk", [1, 2], {"index": 1.5, "score": 2.9, "reason": "floats are cut"},
		{"index": " 2 ", "score": true, "reason": 7}, {"index": 3, "score": "3.0"}, {"index": 3, "score": null},
		{"index": 3, "score": 4, "reason": "` + strings.Repeat("r", 400) + `"}]}`)
	out := finish(t, FinishIn{Ticket: tk, Report: r, Config: c})
	votes := out.Ticket.Votes
	if len(votes) != 1 || votes[0].Voter != "t0001-v1" || votes[0].Host != "local" {
		t.Fatalf("votes %+v", votes)
	}
	if got := strings.Join(votes[0].Scores.Keys(), ","); got != "1,2,3" {
		t.Errorf("scored %s", got)
	}
	if s, reason, _ := votes[0].Score(1); s != 2 || reason != "floats are cut" {
		t.Errorf("1: %d %q", s, reason)
	}
	if s, reason, _ := votes[0].Score(2); s != 1 || reason != "7" {
		t.Errorf("2: %d %q", s, reason)
	}
	if s, reason, _ := votes[0].Score(3); s != 4 || len(reason) != 300 {
		t.Errorf("3: %d %d", s, len(reason))
	}
	if out.Ticket.Column != "done" || len(out.File) != 3 {
		t.Errorf("one voter is the whole panel: %s %d", out.Ticket.Column, len(out.File))
	}
}

func TestProspectEmptyAndCaps(t *testing.T) {
	out := finish(t, FinishIn{Ticket: prospecting(t), Report: report(`{"summary": "nothing", "proposals": [{"title": "t"}, "x"]}`)})
	if out.Ticket.Column != "done" || reason(out.Ticket) != model.ReasonProspectEmpty ||
		last(out.Ticket).Detail != "working -> done: prospecting found nothing worth proposing: nothing" {
		t.Errorf("%s %q", out.Ticket.Column, last(out.Ticket).Detail)
	}
	var ps []string
	for i := range 7 {
		ps = append(ps, fmt.Sprintf(`{"title": "T%d", "noticed": "%s", "change": "c", "why": "w", "cost": 1}`, i, strings.Repeat("n", 2100)))
	}
	out = finish(t, FinishIn{Ticket: prospecting(t), Report: report(`{"proposals": [` + strings.Join(ps, ",") + `]}`)})
	if len(out.Ticket.Proposals) != 5 || len(out.Ticket.Proposals[0].Noticed) != 2000 || out.Ticket.Proposals[0].Cost != "1" {
		t.Errorf("%d %d %q", len(out.Ticket.Proposals), len(out.Ticket.Proposals[0].Noticed), out.Ticket.Proposals[0].Cost)
	}
	if last(out.Ticket).Detail != "working -> voting: 5 proposals; blind panel next" || reason(out.Ticket) != model.ReasonProposalsReady {
		t.Errorf("%q", last(out.Ticket).Detail)
	}
}
