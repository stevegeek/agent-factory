package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/usage"
)

// maxUsageDays bounds `factory usage --days`.
const maxUsageDays = 3660

// runUsage is `factory usage [--days N] [--by KEY] [--json]`: the tokens
// usage.jsonl records for the runs of the last N days, grouped by project,
// stage, ticket, host or model, largest first. Tokens, not money: the
// agents run on a subscription. A row with runs read only in part (a
// transcript over the reader's caps) is a lower bound, its total marked
// with a "+".
func runUsage(_ context.Context, e *Env, a Args) error {
	days, by := a.Int("days"), a.Str("by")
	if days < 1 || days > maxUsageDays {
		return Usage("--days must be from 1 to %d", maxUsageDays)
	}
	since := e.Clock.Now().Add(-time.Duration(days) * 24 * time.Hour)
	recs, err := usage.Load(e.Paths().UsageLog(), since)
	if err != nil {
		return err
	}
	rows, total := usage.Sum(recs, since, by)
	recs = usage.Since(recs, since)
	runs, partial := len(recs), usage.PartialRuns(recs)
	if a.Bool("json") {
		vs := make([]jsonx.Value, len(rows))
		for i, r := range rows {
			vs[i] = tokensJSON(jsonx.Member{Key: "key", Value: jsonx.String(r.Key)}, r.Runs, r.Partial, r.Tokens)
		}
		e.Stdout.Write(jsonx.EncodePython(jsonx.ObjectOf(
			jsonx.Member{Key: "days", Value: jsonx.Int(int64(days))},
			jsonx.Member{Key: "by", Value: jsonx.String(by)},
			jsonx.Member{Key: "since", Value: jsonx.String(clock.Stamp(since))},
			jsonx.Member{Key: "total", Value: tokensJSON(jsonx.Member{}, runs, partial, total)},
			jsonx.Member{Key: "rows", Value: jsonx.ArrayOf(vs...)}), -1))
		fmt.Fprintln(e.Stdout)
		return nil
	}
	fmt.Fprintf(e.Stdout, "Tokens, the last %d days, by %s: %d runs\n", days, by, runs)
	if runs == 0 {
		return nil
	}
	table := [][]string{{by, "runs", "total", "input", "output", "cache write", "cache read"}}
	line := func(key string, runs, partial int, t usage.Tokens) []string {
		sum := usage.Commas(t.Total())
		if partial > 0 {
			sum += "+"
		}
		return []string{key, strconv.Itoa(runs), sum, usage.Commas(t.Input), usage.Commas(t.Output),
			usage.Commas(t.CacheWrite), usage.Commas(t.CacheRead)}
	}
	for _, r := range rows {
		table = append(table, line(r.Key, r.Runs, r.Partial, r.Tokens))
	}
	table = append(table, line("total", runs, partial, total))
	widths := make([]int, len(table[0]))
	for _, row := range table {
		for i, cell := range row {
			widths[i] = max(widths[i], len(cell))
		}
	}
	for _, row := range table {
		cells := make([]string, len(row))
		for i, cell := range row {
			if i == 0 {
				cells[i] = ljust(cell, widths[i])
			} else {
				cells[i] = strings.Repeat(" ", widths[i]-len(cell)) + cell
			}
		}
		fmt.Fprintln(e.Stdout, strings.Join(cells, "  "))
	}
	if partial > 0 {
		fmt.Fprintf(e.Stdout, "+ a lower bound: %s read only in part (a transcript over the reader's caps)\n", present.Plural(partial, "run"))
	}
	return nil
}

// tokensJSON is a row of factory usage --json: key (when given), runs,
// the runs read only in part, total and the four kinds of token, and
// messages.
func tokensJSON(key jsonx.Member, runs, partial int, t usage.Tokens) jsonx.Value {
	var ms []jsonx.Member
	if key.Key != "" {
		ms = append(ms, key)
	}
	ms = append(ms,
		jsonx.Member{Key: "runs", Value: jsonx.Int(int64(runs))},
		jsonx.Member{Key: "partial", Value: jsonx.Int(int64(partial))},
		jsonx.Member{Key: "total", Value: jsonx.Int(t.Total())},
		jsonx.Member{Key: "input", Value: jsonx.Int(t.Input)},
		jsonx.Member{Key: "output", Value: jsonx.Int(t.Output)},
		jsonx.Member{Key: "cache_write", Value: jsonx.Int(t.CacheWrite)},
		jsonx.Member{Key: "cache_read", Value: jsonx.Int(t.CacheRead)},
		jsonx.Member{Key: "messages", Value: jsonx.Int(t.Messages)})
	return jsonx.ObjectOf(ms...)
}
