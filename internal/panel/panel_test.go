package panel

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var at = time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)

// vote is one ballot as finish_vote writes it: {"i": {"score", "reason"}}.
func vote(voter, host string, scores map[int]int, reason string) ticket.Vote {
	var o jsonx.Object
	for i := 1; i <= 5; i++ {
		s, ok := scores[i]
		if !ok {
			continue
		}
		o = append(o, jsonx.Member{Key: string(rune('0' + i)), Value: jsonx.ObjectOf(
			jsonx.Member{Key: "score", Value: jsonx.Int(int64(s))},
			jsonx.Member{Key: "reason", Value: jsonx.String(reason)})})
	}
	return ticket.Vote{Voter: voter, Host: host, Scores: o}
}

// The exhaustive table: panel_tally and panel_rule for every combination of
// one to four voters' scores, and the bytes json.dumps writes for the tally.
func TestTallyMatchesPythonForEveryScoreCombination(t *testing.T) {
	// exhaustive, with the golden:
	// port: TF PanelTest.test_tally_ranks_agreement_above_a_split
	data, err := os.ReadFile("testdata/py/tally.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		KeenScore  int     `json:"keen_score"`
		KeenNeeded int     `json:"keen_needed"`
		MeanNeeded float64 `json:"mean_needed"`
		Rows       []struct {
			Scores    []int  `json:"scores"`
			TallyJSON string `json:"tally_json"`
			Rule      string `json:"rule"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if g.KeenScore != model.PanelKeenScore || g.KeenNeeded != model.PanelKeenNeeded || g.MeanNeeded != model.PanelMeanNeeded {
		t.Fatalf("the golden's rule numbers %d/%d/%v differ from model's", g.KeenScore, g.KeenNeeded, g.MeanNeeded)
	}
	if len(g.Rows) != 1+5+25+125+625 {
		t.Fatalf("%d rows", len(g.Rows))
	}
	for _, row := range g.Rows {
		var votes []ticket.Vote
		for i, s := range row.Scores {
			votes = append(votes, vote("v"+string(rune('0'+i)), "", map[int]int{1: s}, ""))
		}
		tally := Tally(1, votes)
		if got := string(jsonx.EncodePython(TallyValue(tally), -1)); got != row.TallyJSON {
			t.Errorf("%v: tally %s, want %s", row.Scores, got, row.TallyJSON)
		}
		if got := Rule(tally); got != row.Rule {
			t.Errorf("%v: rule %q, want %q", row.Scores, got, row.Rule)
		}
	}
}

func TestTallyRanksAgreementAboveASplit(t *testing.T) {
	// port: TF PanelTest.test_tally_ranks_agreement_above_a_split
	var votes []ticket.Vote
	for _, p := range [][2]int{{4, 5}, {4, 1}, {4, 5}} {
		votes = append(votes, vote("v", "", map[int]int{1: p[0], 2: p[1]}, ""))
	}
	agree, split := Tally(1, votes), Tally(2, votes)
	if agree.Priority <= split.Priority {
		t.Errorf("agreement %v should rank above the split %v", agree.Priority, split.Priority)
	}
	if !split.Survives {
		t.Error("two keen voters keep the split proposal")
	}
}

func TestTallySkipsVotersWithoutAScore(t *testing.T) {
	votes := []ticket.Vote{vote("a", "", map[int]int{2: 5}, ""), vote("b", "", map[int]int{1: 3}, "")}
	if got := Tally(1, votes).Scores; !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("scores %v", got)
	}
	if got := Tally(3, votes); got.Survives || got.Scores == nil || len(got.Scores) != 0 {
		t.Errorf("no scores: %+v", got)
	}
}

func TestVotesNameEachVoterAndHost(t *testing.T) {
	votes := []ticket.Vote{vote("t0001-v1", "pi", map[int]int{1: 5}, "good"), vote("t0001-v2", "", map[int]int{1: 2}, "meh"),
		vote("t0001-v3", "local", map[int]int{2: 4}, "x")}
	want := []ticket.PanelVote{{Voter: "t0001-v1", Host: "pi", Score: 5, Reason: "good"},
		{Voter: "t0001-v2", Host: "local", Score: 2, Reason: "meh"}}
	if got := Votes(1, votes); !reflect.DeepEqual(got, want) {
		t.Errorf("votes %+v", got)
	}
}

func prospect() *ticket.Ticket {
	tk := ticket.New("T0001", ticket.NewIn{Project: "gem", Title: "Scan", Body: "b", Door: "clock", Column: "voting",
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("prospect")}}}, at)
	for i := 1; i <= 3; i++ {
		n := string(rune('0' + i))
		tk.Proposals = append(tk.Proposals, ticket.Proposal{Title: "P" + n, Noticed: "n", Change: "c", Why: "w", Cost: "small"})
	}
	return tk
}

func TestFileProposalCarriesItsVotesAndWaitsOnlyWhenKept(t *testing.T) {
	// the filing:
	// port: TF PanelTest.test_voters_are_blind_and_the_panel_drops_one_voter_wonders
	tk := prospect()
	tk.Votes = []ticket.Vote{vote("v0", "pi", map[int]int{1: 5, 2: 5, 3: 2}, "r"), vote("v1", "pi", map[int]int{1: 4, 2: 2, 3: 1}, "r"),
		vote("v2", "pi", map[int]int{1: 5, 2: 3, 3: 2}, "r")}
	kept := File(tk, 1, tk.Proposals[0], tk.Votes, "T0002", at, "")
	if kept.Column != "needs_human" || kept.KindName() != "proposal" || kept.Parent.Or("") != "T0001" || kept.Door != "clock" {
		t.Fatalf("kept: %s %s %v %s", kept.Column, kept.KindName(), kept.Parent, kept.Door)
	}
	if a, ok := kept.Ask.Get(); !ok || a.Kind != "proposal" {
		t.Errorf("kept ask %+v", kept.Ask)
	}
	if got := kept.History[len(kept.History)-1]; got.Rule != "kept: 3 of 3 voters scored 4 or more (2 needed); mean 4.67 (3.5 needed)" ||
		got.Event != "panel" || got.Detail != "mean 4.67, spread 0.47, scores [5, 4, 5]" {
		t.Errorf("panel event %+v", got)
	}
	wantBody := "- Noticed: n\n- Change: c\n- Why it is worth it: w\n- Cost: small\n\nPanel: 5: r; 4: r; 5: r"
	if kept.Body != wantBody {
		t.Errorf("body %q", kept.Body)
	}
	p, _ := kept.Panel.Get()
	if !reflect.DeepEqual(p.Scores, []int{5, 4, 5}) || len(p.Votes) != 3 || p.Votes[0] != (ticket.PanelVote{Voter: "v0", Host: "pi", Score: 5, Reason: "r"}) {
		t.Errorf("panel %+v", p)
	}
	dropped := File(tk, 2, tk.Proposals[1], tk.Votes, "T0003", at, "")
	if dropped.Column != "rejected" || dropped.Ask.Present() {
		t.Errorf("dropped: %s %+v", dropped.Column, dropped.Ask)
	}
	if got := dropped.History[len(dropped.History)-1].Rule; got != "dropped: 1 of 3 voters scored 4 or more (2 needed); mean 3.33 (3.5 needed)" {
		t.Errorf("dropped rule %q", got)
	}
	// the key order new_ticket writes, kind, parent, proposal and panel after the history
	enc := string(ticket.Encode(kept))
	if !inOrder(enc, `"history"`, `"kind"`, `"parent"`, `"proposal"`, `"panel"`, `"ask"`) {
		t.Errorf("key order:\n%s", enc)
	}
	if !inOrder(enc, `"scores"`, `"mean": 4.67`, `"spread": 0.47`, `"keen": 3`, `"priority": 4.43`, `"survives": true`, `"votes"`) {
		t.Errorf("panel keys:\n%s", enc)
	}
}

func TestFileProposalAtBackDatesALateFiling(t *testing.T) {
	tk := prospect()
	tk.Votes = []ticket.Vote{vote("v0", "", map[int]int{1: 5}, ""), vote("v1", "", map[int]int{1: 5}, "")}
	c := File(tk, 1, tk.Proposals[0], tk.Votes, "T0002", at, "2026-01-01T00:00:00+00:00")
	if c.Created != "2026-01-01T00:00:00+00:00" {
		t.Errorf("created %s", c.Created)
	}
	for _, h := range c.History {
		if h.At != "2026-01-01T00:00:00+00:00" {
			t.Errorf("entry at %s", h.At)
		}
	}
	if a, _ := c.Ask.Get(); a.Since != "2026-01-01T00:00:00+00:00" {
		t.Errorf("since %s", a.Since)
	}
}

func TestRevealFilesEveryProposalAndClosesTheProspect(t *testing.T) {
	// the reveal:
	// port: TF PanelTest.test_voters_are_blind_and_the_panel_drops_one_voter_wonders
	tk := prospect()
	tk.Votes = []ticket.Vote{vote("v0", "pi", map[int]int{1: 5, 2: 5, 3: 2}, "r"), vote("v1", "pi", map[int]int{1: 4, 2: 2, 3: 1}, "r"),
		vote("v2", "pi", map[int]int{1: 5, 2: 3, 3: 2}, "r")}
	if n := Needs(tk); n != 3 {
		t.Fatalf("needs %d ids", n)
	}
	if _, err := Reveal(tk, []ticket.ID{"T0002"}, at); err == nil {
		t.Fatal("too few ids must fail")
	}
	filed, err := Reveal(tk, []ticket.ID{"T0002", "T0003", "T0004"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(filed) != 3 || tk.Column != "done" {
		t.Fatalf("filed %d, column %s", len(filed), tk.Column)
	}
	r, _ := tk.PanelResult.Get()
	if !reflect.DeepEqual(r, ticket.PanelResult{Kept: []string{"T0002"}, Dropped: []string{"P2 ([5, 2, 3])", "P3 ([2, 1, 2])"},
		DroppedTickets: []string{"T0003", "T0004"}}) {
		t.Errorf("panel result %+v", r)
	}
	last := tk.History[len(tk.History)-1]
	if last.Detail != "voting -> done: panel kept T0002; dropped 2" ||
		last.Rule != "panel: kept 1 of 3; 2 voters scoring 4 or more, or a mean of 3.5, keeps a proposal" {
		t.Errorf("move %+v", last)
	}
	if m, ok := last.Data.(ticket.Moved); !ok || m.Reason != model.ReasonPanelDone {
		t.Errorf("reason %+v", last.Data)
	}
	// nothing kept, nothing dropped
	empty := prospect()
	empty.Proposals = []ticket.Proposal{}
	if filed, err := Reveal(empty, nil, at); err != nil || len(filed) != 0 || !strings.Contains(empty.History[len(empty.History)-1].Detail, "panel kept none; dropped 0") {
		t.Errorf("empty reveal: %v %v", filed, err)
	}
}

func TestProposalText(t *testing.T) {
	got := ProposalText(2, ticket.Proposal{Title: "T", Noticed: "N", Change: "C", Why: "W", Cost: "X"})
	want := "### Proposal 2: T\n\n- Noticed: N\n- Change: C\n- Why it is worth it: W\n- Cost: X"
	if got != want {
		t.Errorf("%q", got)
	}
}

func inOrder(s string, parts ...string) bool {
	at := 0
	for _, p := range parts {
		i := strings.Index(s[at:], p)
		if i < 0 {
			return false
		}
		at += i + len(p)
	}
	return true
}
