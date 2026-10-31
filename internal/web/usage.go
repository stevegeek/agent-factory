package web

import (
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/usage"
)

// TokensView is the Tokens section of Floor metrics: what the agents spent
// today and over the last 7 and 30 days, from usage.jsonl, over the
// projects the viewer sees. Tokens, never money: the agents run on a
// subscription.
type TokensView struct {
	Windows []string
	Groups  []usage.TableGroup
	Quiet   bool // nothing recorded in the longest window
	Partial int  // runs of the longest window read only in part
}

// tokensView reads usage.jsonl once, keeping only the longest window's
// records, and totals it for the windows.
func (s *Server) tokensView(r *http.Request) *TokensView {
	now := s.now().Local()
	starts := []time.Time{
		time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local),
		now.Add(-7 * 24 * time.Hour),
		now.Add(-30 * 24 * time.Hour),
	}
	oldest := slices.MinFunc(starts, time.Time.Compare)
	recs, err := usage.Load(s.paths.UsageLog(), oldest)
	if err != nil {
		s.logf("usage: %v", err)
	}
	v := &TokensView{Windows: []string{"Today", "7 days", "30 days"}}
	var totals []usage.Tokens
	var runs []int
	for _, since := range starts {
		_, t := usage.Sum(recs, since, usage.ByProject)
		totals = append(totals, t)
		runs = append(runs, len(usage.Since(recs, since)))
	}
	if runs[len(runs)-1] == 0 {
		v.Quiet = true
		return v
	}
	v.Partial = usage.PartialRuns(usage.Since(recs, starts[len(starts)-1]))
	cells := func(f func(usage.Tokens) int64) []string {
		out := make([]string, len(totals))
		for i, t := range totals {
			out[i] = usage.Cell(f(t))
		}
		return out
	}
	all := usage.TableGroup{Title: "All projects", Rows: []usage.TableRow{
		{Label: "Tokens", Cells: cells(usage.Tokens.Total)},
		{Label: "Input", Sub: true, Cells: cells(func(t usage.Tokens) int64 { return t.Input })},
		{Label: "Output", Sub: true, Cells: cells(func(t usage.Tokens) int64 { return t.Output })},
		{Label: "Cache write", Sub: true, Cells: cells(func(t usage.Tokens) int64 { return t.CacheWrite })},
		{Label: "Cache read", Sub: true, Cells: cells(func(t usage.Tokens) int64 { return t.CacheRead })},
		{Label: "Runs", Cells: []string{strconv.Itoa(runs[0]), strconv.Itoa(runs[1]), strconv.Itoa(runs[2])}},
	}}
	v.Groups = []usage.TableGroup{all, usage.ByWindow("By stage", recs, starts, usage.ByStage), usage.ByWindow("By project", recs, starts, usage.ByProject)}
	return v
}

// RunTokens is one archived run's tokens on the ticket page.
type RunTokens struct {
	Name, Total, Split string
	Partial            bool // its transcript was read only in part: a lower bound
}

// TicketTokens is what a ticket's runs spent, from each run's usage.json.
type TicketTokens struct {
	Runs         []RunTokens
	Total, Split string
	Partial      bool // a run was read only in part
}

// maxRunUsage caps the read of a run's usage.json.
const maxRunUsage = 1 << 20

// ticketTokens reads the usage.json of each of the ticket's archived runs
// (runs, as agentLogs lists them), never through a link; nil when none of
// them recorded any (Pi runs record nothing).
func (s *Server) ticketTokens(id string, runs []string) *TicketTokens {
	var tt TicketTokens
	var sum usage.Tokens
	for _, name := range runs {
		b, err := fsx.ReadNoFollow(filepath.Join(s.paths.TicketLogs(id), name, jobproto.MemberUsage), maxRunUsage)
		if err != nil {
			continue
		}
		u, err := usage.Parse(b)
		if err != nil || u.Empty() {
			continue
		}
		t := u.Sum()
		sum = sum.Add(t)
		tt.Runs = append(tt.Runs, RunTokens{Name: name, Total: usage.Commas(t.Total()), Split: usage.Split(t), Partial: u.Partial})
		tt.Partial = tt.Partial || u.Partial
	}
	if len(tt.Runs) == 0 {
		return nil
	}
	tt.Total, tt.Split = usage.Commas(sum.Total()), usage.Split(sum)
	return &tt
}
