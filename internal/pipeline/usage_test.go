package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/usage"
)

// writeUsage plays the archive: what a removed local agent's home, or a
// collected job, left in logs/<id>/<run>/usage.json.
func writeUsage(t *testing.T, h *harness, id, run string, u usage.Usage) {
	t.Helper()
	dir := filepath.Join(h.paths.Logs, id, run)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), u.JSON(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadUsage(t *testing.T, h *harness) []usage.Record {
	t.Helper()
	recs, err := usage.Load(h.paths.UsageLog(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// A finished local run is one line of usage.jsonl: its ticket, project,
// stage, run, host and models, at the tick's time, and whether its
// transcript was read only in part.
func TestAFinishedLocalRunRecordsItsUsage(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "triage", "triage", nil)
	h.local.running["t0001-t1"] = true
	h.local.reports["t0001-t1"] = []byte(`{"decision": "reject", "reason": "dup"}`)
	spent := usage.Tokens{Input: 10, Output: 20, CacheWrite: 30, CacheRead: 40, Messages: 2}
	writeUsage(t, h, string(id), "t0001-t1", usage.Usage{Models: map[string]usage.Tokens{"claude-opus": spent}, Partial: true})
	h.tick()
	recs := loadUsage(t, h)
	if len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
	r := recs[0]
	if r.Ticket != string(id) || r.Project != "p" || r.Stage != "triage" || r.Run != "t0001-t1" || r.Host != "local" ||
		r.Models["claude-opus"] != spent || r.At != stamp(h.clock.Now()) || !r.Partial {
		t.Errorf("record %+v", r)
	}
}

// A collected helper job records its usage with its host; a run with no
// usage.json (Pi, a crash before the first message) records nothing and
// finishes as before.
func TestARemoteRunRecordsItsUsageAndOneWithoutRecordsNothing(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	writeUsage(t, h, string(id), "t0001-x1", usage.Usage{Models: map[string]usage.Tokens{"claude-haiku": {Input: 5, Messages: 1}}})
	collected(t, h, id, map[string][]byte{"report.json": []byte(`{"decision": "reject", "reason": "dup"}`)})
	recs := loadUsage(t, h)
	if len(recs) != 1 || recs[0].Host != "pi" || recs[0].Run != "t0001-x1" || recs[0].Models["claude-haiku"].Input != 5 {
		t.Fatalf("records %+v", recs)
	}

	h2 := newHarness(t)
	id2 := remoteAgent(h2, "triage", "triage", "")
	got := collected(t, h2, id2, map[string][]byte{"report.json": []byte(`{"decision": "reject", "reason": "dup"}`)})
	if got.Column != "rejected" {
		t.Errorf("column %s", got.Column)
	}
	if recs := loadUsage(t, h2); len(recs) != 0 {
		t.Errorf("records %+v", recs)
	}
}

// A run stopped at its timeout or a usage limit still records what it
// spent.
func TestATimedOutLocalRunRecordsItsUsage(t *testing.T) {
	h := newHarness(t)
	id := localAgent(h, "triage", "triage", nil)
	h.local.running["t0001-t1"] = true
	h.clock.Add(31 * time.Minute)
	writeUsage(t, h, string(id), "t0001-t1", usage.Usage{Models: map[string]usage.Tokens{"claude-opus": {Output: 9, Messages: 1}}})
	h.tick()
	if got := h.load(id); got.Column != "needs_human" {
		t.Fatalf("column %s", got.Column)
	}
	if recs := loadUsage(t, h); len(recs) != 1 || recs[0].Models["claude-opus"].Output != 9 {
		t.Errorf("records %+v", recs)
	}
}

// A helper job removed before it was done (past its timeout while still
// running, gone, or its host failing past the timeout) records the usage
// the removal archived: the helper reads the job's home before deleting it.
func TestARemoteRunRemovedBeforeItWasDoneRecordsItsUsage(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(h *harness)
	}{
		{"timed out", func(h *harness) { h.clock.Add(31 * time.Minute) }},
		{"gone", func(h *harness) { h.helpers["pi"].state = "missing" }},
		{"host failing past the timeout", func(h *harness) {
			h.helpers["pi"].pollErr = errors.New("factory@pi status failed (255): no route to host")
			h.clock.Add(31 * time.Minute)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			id := remoteAgent(h, "triage", "triage", "")
			c.setup(h)
			h.helpers["pi"].onRemove = func(rid ticket.ID, name string) {
				writeUsage(t, h, string(rid), name, usage.Usage{Models: map[string]usage.Tokens{"claude-opus": {Output: 77, Messages: 3}}})
			}
			h.tick()
			if got := h.load(id); got.Agent.Present() || got.Column != "needs_human" {
				t.Fatalf("agent %+v column %s", got.Agent, got.Column)
			}
			recs := loadUsage(t, h)
			if len(recs) != 1 || recs[0].Run != "t0001-x1" || recs[0].Host != "pi" || recs[0].Models["claude-opus"].Output != 77 {
				t.Errorf("records %+v", recs)
			}
		})
	}
}
