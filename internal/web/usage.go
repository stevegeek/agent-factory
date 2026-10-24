package web

import (
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	Groups  []TokenGroup
	Quiet   bool // nothing recorded in the longest window
	Partial int  // runs of the longest window read only in part
}

// TokenGroup is a block of the table: everything, by stage, by project.
type TokenGroup struct {
	Title string
	Rows  []TokenRow
}

// TokenRow is one line of the table, a cell per window.
type TokenRow struct {
	Label string
	Sub   bool
	Cells []string
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
	u := s.viewer(r)
	recs = slices.DeleteFunc(recs, func(rec usage.Record) bool { return !u.Sees(rec.Project) })
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
			out[i] = tokenCell(f(t))
		}
		return out
	}
	all := TokenGroup{Title: "All projects", Rows: []TokenRow{
		{Label: "Tokens", Cells: cells(usage.Tokens.Total)},
		{Label: "Input", Sub: true, Cells: cells(func(t usage.Tokens) int64 { return t.Input })},
		{Label: "Output", Sub: true, Cells: cells(func(t usage.Tokens) int64 { return t.Output })},
		{Label: "Cache write", Sub: true, Cells: cells(func(t usage.Tokens) int64 { return t.CacheWrite })},
		{Label: "Cache read", Sub: true, Cells: cells(func(t usage.Tokens) int64 { return t.CacheRead })},
		{Label: "Runs", Cells: []string{strconv.Itoa(runs[0]), strconv.Itoa(runs[1]), strconv.Itoa(runs[2])}},
	}}
	v.Groups = []TokenGroup{all, byWindow("By stage", recs, starts, usage.ByStage), byWindow("By project", recs, starts, usage.ByProject)}
	return v
}

// byWindow is a group with a row per key, in the order of the longest
// window (largest first), each cell that window's total for the key.
func byWindow(title string, recs []usage.Record, starts []time.Time, by string) TokenGroup {
	g := TokenGroup{Title: title}
	per := make([]map[string]int64, len(starts))
	for i, since := range starts {
		rows, _ := usage.Sum(recs, since, by)
		per[i] = map[string]int64{}
		for _, row := range rows {
			per[i][row.Key] = row.Tokens.Total()
		}
	}
	longest, _ := usage.Sum(recs, starts[len(starts)-1], by)
	for _, row := range longest {
		tr := TokenRow{Label: row.Key}
		for i := range starts {
			tr.Cells = append(tr.Cells, tokenCell(per[i][row.Key]))
		}
		g.Rows = append(g.Rows, tr)
	}
	return g
}

// tokenCell is a count in thousands' groups, "–" for none.
func tokenCell(n int64) string {
	if n == 0 {
		return "–"
	}
	return usage.Commas(n)
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
		tt.Runs = append(tt.Runs, RunTokens{Name: name, Total: usage.Commas(t.Total()), Split: tokenSplit(t), Partial: u.Partial})
		tt.Partial = tt.Partial || u.Partial
	}
	if len(tt.Runs) == 0 {
		return nil
	}
	tt.Total, tt.Split = usage.Commas(sum.Total()), tokenSplit(sum)
	return &tt
}

// tokenSplit is "input 1,010 · output 2,020 · cache write 3,000 · cache
// read 400,000".
func tokenSplit(t usage.Tokens) string {
	return strings.Join([]string{"input " + usage.Commas(t.Input), "output " + usage.Commas(t.Output),
		"cache write " + usage.Commas(t.CacheWrite), "cache read " + usage.Commas(t.CacheRead)}, " · ")
}
