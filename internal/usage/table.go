package usage

import (
	"strings"
	"time"
)

// The tables Foreman shows of what the agents spent: tokens, never money.

// TableGroup is a block of the table: everything, by stage, by project.
type TableGroup struct {
	Title string
	Rows  []TableRow
}

// TableRow is one line of the table, a cell per window.
type TableRow struct {
	Label string
	Sub   bool
	Cells []string
}

// ByWindow is a group with a row per key, in the order of the longest
// window (largest first), each cell that window's total for the key.
func ByWindow(title string, recs []Record, starts []time.Time, by string) TableGroup {
	g := TableGroup{Title: title}
	per := make([]map[string]int64, len(starts))
	for i, since := range starts {
		rows, _ := Sum(recs, since, by)
		per[i] = map[string]int64{}
		for _, row := range rows {
			per[i][row.Key] = row.Tokens.Total()
		}
	}
	longest, _ := Sum(recs, starts[len(starts)-1], by)
	for _, row := range longest {
		tr := TableRow{Label: row.Key}
		for i := range starts {
			tr.Cells = append(tr.Cells, Cell(per[i][row.Key]))
		}
		g.Rows = append(g.Rows, tr)
	}
	return g
}

// Cell is a count in thousands' groups, "–" for none.
func Cell(n int64) string {
	if n == 0 {
		return "–"
	}
	return Commas(n)
}

// Split is "input 1,010 · output 2,020 · cache write 3,000 · cache
// read 400,000".
func Split(t Tokens) string {
	return strings.Join([]string{"input " + Commas(t.Input), "output " + Commas(t.Output),
		"cache write " + Commas(t.CacheWrite), "cache read " + Commas(t.CacheRead)}, " · ")
}
