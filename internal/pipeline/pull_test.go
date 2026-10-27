package pipeline

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/mailbox"
	"github.com/stevegeek/agent-factory/internal/model"
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

// A pull job its host gave back unstarted goes back to its stage at once
// (not to the desk), the reason on the ticket, and the host gets no new
// job for GiveBackPause.
func TestAGivenBackPullJobIsPlacedAgain(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	h.helpers["pi"].state = jobproto.StateGivenBack
	h.helpers["pi"].gaveBack = "worker fw1 did not start it within 3m0s"
	h.tick()
	got := h.load(id)
	if got.Agent.V.Host != "local" || got.Column != "triage" || len(h.helpers["pi"].removed) != 1 {
		t.Fatalf("not placed again here: agent %+v column %s removed %v", got.Agent, got.Column, h.helpers["pi"].removed)
	}
	m := lostMove(got)
	if moveReason(m) != model.ReasonAgentLost ||
		!strings.Contains(m.Detail, "pi gave t0001-x1 back unstarted (worker fw1 did not start it within 3m0s); it goes to another host, and pi gets no new job until ") {
		t.Errorf("move %q", m.Detail)
	}
	if !h.board.Paused("pi") {
		t.Error("pi is not paused")
	}
}

// A job still queued for a pull host that has gone away (no calls, or its
// workers say none can run) is taken back and placed again, without waiting
// out its claim window; the host is not paused, being away already.
func TestAQueuedJobOnAnAwayPullHostIsPlacedAgain(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	h.helpers["pi"].state = jobproto.StateQueued
	h.helpers["pi"].away = true
	h.tick()
	got := h.load(id)
	if got.Agent.V.Host != "local" || got.Column != "triage" || len(h.helpers["pi"].removed) != 1 {
		t.Fatalf("not placed again here: agent %+v column %s removed %v", got.Agent, got.Column, h.helpers["pi"].removed)
	}
	if m := lostMove(got); m.Detail != "triage -> triage: pi went away before it took t0001-x1; it goes to another host" {
		t.Errorf("move %q", m.Detail)
	}
	if h.board.Paused("pi") {
		t.Error("an away host was paused too")
	}
}

// A pull host gets no more jobs than the workers it reports running.
func TestAPullHostGetsNoMoreJobsThanItsRunningWorkers(t *testing.T) {
	h := newHarness(t)
	h.config(oneSlot + `
[hosts.desktop]
transport = "pull"
slots = 3
prefer = ["triage"]
`)
	one := 1
	h.helpers["desktop"] = &fakeHelper{c: h.calls, name: "desktop", state: jobproto.StateRunning, capacity: &one}
	h.local.running["someone-else"] = true
	for range 3 {
		h.file("p", "t", "triage", nil)
	}
	h.tick()
	if n := len(h.helpers["desktop"].started); n != 1 {
		t.Errorf("started %d on a desktop with one worker running", n)
	}
}

// JobHosts reads a pull host's own account of its workers from vitals no
// older than its beat window: down, or none running, is away; fewer running
// than its slots caps it; a stale or missing account changes nothing.
func TestJobHostsReadThePullHostsWorkers(t *testing.T) {
	h := newHarness(t)
	h.config(defaultConfig + "\n[hosts.desktop]\ntransport = \"pull\"\nslots = 3\nbeat_minutes = 5\n")
	cfg, err := (&config.Source{Path: h.paths.Config, Log: h.log}).Current()
	if err != nil {
		t.Fatal(err)
	}
	hosts := JobHosts{Paths: h.paths, Clock: h.clock}
	box := mailbox.Box{Dir: h.paths.Mailbox(), Clock: h.clock}
	post := func(workers string) {
		t.Helper()
		if err := box.PostVitals("desktop", []byte(`{"hostname": "desk"`+workers+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	post(`, "workers": {"up": false, "running": 0, "free": 0, "note": "no lever broker"}`)
	if away := hosts.Away(cfg); !slices.Equal(away, []string{"desktop"}) {
		t.Errorf("lever down: away %q", away)
	}
	post(`, "workers": {"up": true, "running": 0, "free": 0}`)
	if away := hosts.Away(cfg); !slices.Equal(away, []string{"desktop"}) {
		t.Errorf("no worker running: away %q", away)
	}
	post(`, "workers": {"up": true, "running": 2, "free": 1}`)
	if away, c := hosts.Away(cfg), hosts.Capacity(cfg); len(away) != 0 || c["desktop"] != 2 {
		t.Errorf("two running: away %q capacity %v", away, c)
	}
	post(`, "workers": {"up": true, "running": 3, "free": 3}`)
	if c := hosts.Capacity(cfg); len(c) != 0 {
		t.Errorf("all running: capacity %v", c)
	}
	post(``) // an older factory-pull: judged by its calls alone
	if away, c := hosts.Away(cfg), hosts.Capacity(cfg); len(away) != 0 || len(c) != 0 {
		t.Errorf("no account: away %q capacity %v", away, c)
	}
	post(`, "workers": {"up": false}`)
	h.clock.t = h.clock.t.Add(4 * time.Minute)
	box.Claim("desktop") // it still calls in, but its account is going stale
	h.clock.t = h.clock.t.Add(2 * time.Minute)
	if away := hosts.Away(cfg); len(away) != 0 {
		t.Errorf("a stale account still counts: away %q", away)
	}
	if why, ok := hosts.GivenBack(cfg, "desktop", "t0001-k1"); ok {
		t.Errorf("given back %q", why)
	}
}

// lostMove is the ticket's last agent_lost move.
func lostMove(t *ticket.Ticket) ticket.Entry {
	var m ticket.Entry
	for _, e := range t.History {
		if moveReason(e) == model.ReasonAgentLost {
			m = e
		}
	}
	return m
}

// A job still queued for a pull host that was paused since (it gave a job
// back) is taken back too, so one bad worker does not hold the queue.
func TestAQueuedJobOnAPausedPullHostIsPlacedAgain(t *testing.T) {
	h := newHarness(t)
	id := remoteAgent(h, "triage", "triage", "")
	h.helpers["pi"].state = jobproto.StateQueued
	if _, err := h.board.PauseHost("pi", "gave a job back: worker fw1 did not start it", GiveBackPause); err != nil {
		t.Fatal(err)
	}
	h.tick()
	got := h.load(id)
	if got.Agent.V.Host != "local" || len(h.helpers["pi"].removed) != 1 {
		t.Fatalf("agent %+v removed %v", got.Agent, h.helpers["pi"].removed)
	}
	if m := lostMove(got); m.Detail != "triage -> triage: pi was paused before it took t0001-x1; it goes to another host" {
		t.Errorf("move %q", m.Detail)
	}
}
