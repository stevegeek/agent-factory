package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/usage"
)

// usageLog writes usage.jsonl: runs today, three days ago and forty days
// ago (the fixture's clock is 2026-10-18T10:00).
func usageLog(t *testing.T, f *fixture) {
	t.Helper()
	for _, r := range []usage.Record{
		{At: "2026-10-18T09:00:00+00:00", Ticket: "T0001", Project: "gem", Stage: "work", Run: "t0001-w1", Host: "local",
			Models: map[string]usage.Tokens{"claude-opus": {Input: 1000, Output: 2000, CacheWrite: 3000, CacheRead: 400000, Messages: 10}}},
		{At: "2026-10-15T09:00:00+00:00", Ticket: "T0002", Project: "app", Stage: "review", Run: "t0002-r1", Host: "hostx",
			Models: map[string]usage.Tokens{"claude-haiku": {Input: 10, Output: 20, Messages: 1}}},
		{At: "2026-09-08T09:00:00+00:00", Ticket: "T0003", Project: "app", Stage: "work", Run: "t0003-w1", Host: "local",
			Models: map[string]usage.Tokens{"claude-opus": {Input: 9_000_000, Messages: 5}}},
	} {
		if err := usage.Append(f.env.Path("usage.jsonl"), r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsageTotalsTheWindowLargestFirst(t *testing.T) {
	f := newFixture(t)
	usageLog(t, f)
	code, out, errs := f.run("", "usage")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	for _, want := range []string{
		"Tokens, the last 7 days, by project: 2 runs",
		"project  runs    total  input  output  cache write  cache read",
		"gem         1  406,000  1,000   2,000        3,000     400,000",
		"app         1       30     10      20            0           0",
		"total       2  406,030  1,010   2,020        3,000     400,000",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "T0003") || strings.Contains(out, "9,000,000") {
		t.Errorf("a run outside the window:\n%s", out)
	}
	code, out, _ = f.run("", "usage", "--days", "60", "--by", "model")
	if code != 0 || !strings.Contains(out, "the last 60 days, by model: 3 runs") ||
		strings.Index(out, "claude-opus") > strings.Index(out, "claude-haiku") || !strings.Contains(out, "9,406,000") {
		t.Errorf("by model:\n%s", out)
	}
}

func TestUsageJSONAndBadArguments(t *testing.T) {
	f := newFixture(t)
	usageLog(t, f)
	code, out, errs := f.run("", "usage", "--by", "stage", "--json")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	var got struct {
		Days  int    `json:"days"`
		By    string `json:"by"`
		Since string `json:"since"`
		Total struct {
			Total, Input int64
			Runs         int
		} `json:"total"`
		Rows []struct {
			Key       string `json:"key"`
			Runs      int    `json:"runs"`
			Total     int64  `json:"total"`
			CacheRead int64  `json:"cache_read"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got.Days != 7 || got.By != "stage" || got.Since != "2026-10-11T10:00:00+00:00" || got.Total.Total != 406_030 || got.Total.Runs != 2 ||
		len(got.Rows) != 2 || got.Rows[0].Key != "work" || got.Rows[0].Total != 406_000 || got.Rows[0].CacheRead != 400_000 {
		t.Errorf("json %s", out)
	}
	for _, argv := range [][]string{{"usage", "--by", "cost"}, {"usage", "--days", "0"}} {
		if code, _, _ := f.run("", argv...); code != 2 {
			t.Errorf("%q: exit %d", argv, code)
		}
	}
	// no log yet: nothing spent, no error
	f2 := newFixture(t)
	if code, out, errs := f2.run("", "usage"); code != 0 || !strings.Contains(out, "0 runs") {
		t.Errorf("no log: %d %q %q", code, out, errs)
	}
}

// A run read only in part makes its row's and the window's totals a lower
// bound: marked "+", counted under partial in --json.
func TestUsageMarksPartialRuns(t *testing.T) {
	f := newFixture(t)
	usageLog(t, f)
	if err := usage.Append(f.env.Path("usage.jsonl"), usage.Record{At: "2026-10-18T09:30:00+00:00", Ticket: "T0004", Project: "app",
		Stage: "work", Run: "t0004-w1", Host: "local", Partial: true, Models: map[string]usage.Tokens{"claude-opus": {Input: 5, Messages: 1}}}); err != nil {
		t.Fatal(err)
	}
	code, out, errs := f.run("", "usage")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	for _, want := range []string{
		"gem         1   406,000  1,000   2,000        3,000     400,000\n",
		"app         2       35+     15      20            0           0\n",
		"total       3  406,035+  1,015   2,020        3,000     400,000\n",
		"+ a lower bound: 1 run read only in part (a transcript over the reader's caps)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	code, out, _ = f.run("", "usage", "--json")
	var got struct {
		Total struct{ Partial int } `json:"total"`
		Rows  []struct {
			Key     string `json:"key"`
			Partial int    `json:"partial"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || got.Total.Partial != 1 || got.Rows[1].Key != "app" || got.Rows[1].Partial != 1 || got.Rows[0].Partial != 0 {
		t.Errorf("json %s", out)
	}
}
