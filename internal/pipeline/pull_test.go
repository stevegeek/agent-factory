package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/mailbox"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// A pull host that is away (not called in lately) gets no overflow: it
// goes to the next helper, as for a paused one; a ticket pinned to it
// waits.
func TestAnAwayPullHostGetsNoJobs(t *testing.T) {
	h := newHarness(t)
	h.config(oneSlot + `
[hosts.desktop]
transport = "pull"
slots = 2
prefer = ["triage"]
`)
	h.helpers["desktop"] = &fakeHelper{c: h.calls, name: "desktop", state: jobproto.StateRunning, away: true}
	h.local.running["someone-else"] = true
	first := h.file("p", "t", "triage", nil)
	pinned := h.file("p", "t", "triage", func(t *ticket.Ticket) { t.Host = ticket.Some("desktop") })
	h.tick()
	if got := h.load(first); got.Agent.V.Host != "pi" {
		t.Errorf("overflow went to %q, want pi", got.Agent.V.Host)
	}
	if got := h.load(pinned); got.Agent.Present() || len(h.helpers["desktop"].started) != 0 {
		t.Errorf("an away host was started on: %+v", got.Agent)
	}

	h.helpers["desktop"].away = false
	h.tick()
	if got := h.load(pinned); got.Agent.V.Host != "desktop" {
		t.Errorf("back, the pinned ticket runs there: %+v", got.Agent)
	}
}

// A queued pull job is waited on like a running one, and timed out like
// one past the agent's time.
func TestAQueuedPullJobIsWaitedOn(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	h.helpers["pi"].state = jobproto.StateQueued
	h.tick()
	if got := h.load(id); !got.Agent.Present() || len(h.helpers["pi"].removed) != 0 {
		t.Fatalf("a queued job was dropped: %+v", got.Agent)
	}
	h.clock.t = h.clock.t.Add(2 * time.Hour)
	h.tick()
	if got := h.load(id); got.Agent.Present() || got.Column != "needs_human" || len(h.helpers["pi"].removed) != 1 {
		t.Errorf("past its time: agent %+v column %s", got.Agent, got.Column)
	}
}

// JobHosts runs a pull host's jobs through its mailbox, end to end with
// the forced command's side: start queues, claim and done move it on, and
// collect gives the dispatcher what factory-remote's collect would.
func TestJobHostsRunAPullHostThroughItsMailbox(t *testing.T) {
	h := newHarness(t)
	h.config(defaultConfig + "\n[hosts.desktop]\ntransport = \"pull\"\nclaim_minutes = 20\nbeat_minutes = 5\n")
	cfg, err := (&config.Source{Path: h.paths.Config, Log: h.log}).Current()
	if err != nil {
		t.Fatal(err)
	}
	hosts := JobHosts{Paths: h.paths, Clock: h.clock}
	if away := hosts.Away(cfg); len(away) != 1 || away[0] != "desktop" {
		t.Fatalf("never seen: away = %q", away)
	}
	helper, ok := hosts.Helper(cfg, "desktop")
	if !ok {
		t.Fatal("no desktop helper")
	}
	jobs := helper.(*hostrun.Jobs)
	tar, _ := jobproto.TarFlat([]jobproto.File{{Name: "prompt.md", Data: []byte("p")},
		{Name: "meta.json", Data: jobproto.Meta{Branch: "task", Start: "none", TimeoutMinutes: 30}.Encode()}}, h.clock.Now())
	ctx := context.Background()
	if _, err := jobs.Via.Do(ctx, jobs.Remote, jobproto.VerbStart, "t0001-k1", tar, 0); err != nil {
		t.Fatal(err)
	}
	if st, err := helper.Poll(ctx, "t0001-k1"); err != nil || st != jobproto.StateQueued {
		t.Fatalf("poll %q %v", st, err)
	}
	box := mailbox.Box{Dir: h.paths.Mailbox(), Clock: h.clock}
	if job, _, err := box.Claim("desktop"); err != nil || job != "t0001-k1" {
		t.Fatalf("claim %q %v", job, err)
	}
	if away := hosts.Away(cfg); len(away) != 0 {
		t.Errorf("just claimed: away = %q", away)
	}
	if st, _ := helper.Poll(ctx, "t0001-k1"); st != jobproto.StateRunning {
		t.Errorf("claimed: %q", st)
	}
	posted, _ := jobproto.PackCollect(map[string][]byte{"report.json": []byte(`{"status": "done", "result": "r"}`),
		"agent.log": []byte("log"), "prompt.md": []byte("p")})
	if err := box.Done("desktop", "t0001-k1", posted); err != nil {
		t.Fatal(err)
	}
	if st, _ := helper.Poll(ctx, "t0001-k1"); st != jobproto.StateDone {
		t.Errorf("posted: %q", st)
	}
	res, err := helper.Collect(ctx, "T0001", "t0001-k1")
	if err != nil {
		t.Fatal(err)
	}
	if rep, ok := res.Report(); !ok || string(rep) != `{"status": "done", "result": "r"}` || res.Head() != "unknown" {
		t.Errorf("result %q", res.Files)
	}
	if s, err := jobs.Tail(ctx, "t0001-k1", 10); err != nil || s != "" {
		t.Errorf("tail %q %v", s, err)
	}
	if err := helper.Remove(ctx, "T0001", "t0001-k1"); err != nil {
		t.Fatal(err)
	}
	if names, err := jobs.List(ctx); err != nil || len(names) != 0 {
		t.Errorf("list after remove %q %v", names, err)
	}
	h.clock.t = h.clock.t.Add(6 * time.Minute)
	if away := hosts.Away(cfg); len(away) != 1 {
		t.Errorf("silent past beat_minutes: away = %q", away)
	}
}
