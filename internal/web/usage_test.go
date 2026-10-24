package web

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/usage"
)

// usageLog writes usage.jsonl around the harness's clock (2026-10-09
// 12:00 UTC): two runs in the last hour (one on a project robin does not
// see), one five days ago and one twenty days ago.
func usageLog(t *testing.T, h *harness) {
	t.Helper()
	for _, r := range []usage.Record{
		{At: "2026-10-09T11:30:00+00:00", Ticket: "T0001", Project: "widgets", Stage: "work", Run: "t0001-w1", Host: "local",
			Models: map[string]usage.Tokens{"claude-opus": {Input: 1000, Output: 2000, CacheWrite: 3000, CacheRead: 400000, Messages: 10}}},
		{At: "2026-10-04T09:00:00+00:00", Ticket: "T0002", Project: "widgets", Stage: "review", Run: "t0002-r1", Host: "local",
			Models: map[string]usage.Tokens{"claude-opus": {Input: 50, Messages: 1}}},
		{At: "2026-09-19T09:00:00+00:00", Ticket: "T0003", Project: "widgets", Stage: "work", Run: "t0003-w1", Host: "local",
			Models: map[string]usage.Tokens{"claude-opus": {Input: 7000, Messages: 1}}},
		{At: "2026-10-09T11:45:00+00:00", Ticket: "T0008", Project: "factory", Stage: "task", Run: "t0008-k1", Host: "local",
			Models: map[string]usage.Tokens{"claude-opus": {Output: 900_000, Messages: 1}}},
	} {
		if err := usage.Append(h.paths.UsageLog(), r); err != nil {
			t.Fatal(err)
		}
	}
}

// Floor metrics end with Tokens: today, the last 7 and 30 days, in all and
// by stage and by project, over the projects the viewer sees.
func TestFloorMetricsShowTokens(t *testing.T) {
	h := newHarness(t, false)
	usageLog(t, h)
	page := h.do("GET", "/floor/metrics", nil, h.login()).Body.String()
	for _, want := range []string{
		`aria-label="Tokens"`,
		`<th scope="col">Today</th><th scope="col">7 days</th><th scope="col">30 days</th>`,
		`<th scope="row">Tokens</th><td>1,306,000</td><td>1,306,050</td><td>1,313,050</td>`,
		`<tr class="msub"><th scope="row">Input</th><td>1,000</td><td>1,050</td><td>8,050</td>`,
		`<tr class="msub"><th scope="row">Cache read</th><td>400,000</td>`,
		`<th scope="row">Runs</th><td>2</td><td>3</td><td>4</td>`,
		`<th scope="row">work</th><td>406,000</td><td>406,000</td><td>413,000</td>`,
		`<th scope="row">review</th><td>–</td><td>50</td><td>50</td>`,
		`<th scope="row">factory</th><td>900,000</td>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the tokens section lacks %q", want)
		}
	}
	if strings.Contains(page, "cost") || strings.Contains(page, "$") {
		t.Error("tokens are not money")
	}
	secret, _ := h.users.Add("robin", []string{"widgets"})
	robin := h.do("GET", "/floor/metrics", nil, h.loginWith(secret, "/")).Body.String()
	if strings.Contains(robin, "900,000") || !strings.Contains(robin, `<th scope="row">Tokens</th><td>406,000</td>`) {
		t.Errorf("robin sees another project's tokens")
	}
	// no log: the section says nothing was recorded
	h2 := newHarness(t, false)
	if page := h2.do("GET", "/floor/metrics", nil, h2.login()).Body.String(); !strings.Contains(page, "No tokens recorded in the last 30 days.") {
		t.Errorf("no log: the tokens section is not quiet")
	}
}

// A ticket's page shows each run's tokens from its usage.json and the
// ticket's total; a run with none (Pi) shows nothing.
func TestTicketPageShowsEachRunsTokens(t *testing.T) {
	h := newHarness(t, false)
	w1 := usage.Usage{Models: map[string]usage.Tokens{"claude-opus": {Input: 1000, Output: 2000, CacheWrite: 3000, CacheRead: 400000, Messages: 10}}}
	r1 := usage.Usage{Models: map[string]usage.Tokens{"claude-haiku": {Input: 10, Output: 20, Messages: 1}}}
	h.env.WriteFile(t, "logs/T0001/t0001-w1/usage.json", string(w1.JSON()))
	h.env.WriteFile(t, "logs/T0001/t0001-r1/usage.json", string(r1.JSON()))
	h.env.WriteFile(t, "logs/T0001/t0001-s1/report.json", `{}`)
	h.env.WriteFile(t, "logs/T0001/t0001-x1/usage.json", `not json`)
	page := h.do("GET", "/t/T0001", nil, h.login()).Body.String()
	for _, want := range []string{
		`Tokens: <span class="mono">t0001-r1</span> <span title="input 10 · output 20 · cache write 0 · cache read 0">30</span>`,
		`<span class="mono">t0001-w1</span> <span title="input 1,000 · output 2,000 · cache write 3,000 · cache read 400,000">406,000</span>`,
		`total 406,030 (input 1,010 · output 2,020 · cache write 3,000 · cache read 400,000)`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the ticket page lacks %q", want)
		}
	}
	if strings.Contains(page, `t0001-s1</span>`) || strings.Contains(page, `t0001-x1</span>`) {
		t.Error("a run without a usable usage.json shows tokens")
	}
	if page := h.do("GET", "/t/T0002", nil, h.login()).Body.String(); strings.Contains(page, "Tokens:") {
		t.Error("a ticket with no recorded runs shows tokens")
	}
}

// A run read only in part is a lower bound: Floor metrics say how many
// there were, and the ticket page marks the run and the total with "+".
func TestTokensMarkPartialRuns(t *testing.T) {
	h := newHarness(t, false)
	usageLog(t, h)
	if err := usage.Append(h.paths.UsageLog(), usage.Record{At: "2026-10-09T11:50:00+00:00", Ticket: "T0001", Project: "widgets", Stage: "work",
		Run: "t0001-w2", Host: "local", Partial: true, Models: map[string]usage.Tokens{"claude-opus": {Input: 5, Messages: 1}}}); err != nil {
		t.Fatal(err)
	}
	page := h.do("GET", "/floor/metrics", nil, h.login()).Body.String()
	if !strings.Contains(page, "1 of the last 30 days' runs read only in part") {
		t.Errorf("no partial note on the metrics page")
	}
	w2 := usage.Usage{Models: map[string]usage.Tokens{"claude-opus": {Input: 5, Messages: 1}}, Partial: true}
	h.env.WriteFile(t, "logs/T0001/t0001-w2/usage.json", string(w2.JSON()))
	page = h.do("GET", "/t/T0001", nil, h.login()).Body.String()
	for _, want := range []string{
		`<span title="input 5 · output 0 · cache write 0 · cache read 0 · read only in part: a lower bound">5+</span>`,
		`total 5+ (`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the ticket page lacks %q", want)
		}
	}
}
