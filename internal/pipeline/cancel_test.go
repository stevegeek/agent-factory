package pipeline

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// a cancel injected before every adapter call of a start,
// a finish, a gate and a PR open leaves the ticket's file as some commit
// point wrote it (never a half-made step), and the ticks after it bring the
// ticket to where an uninterrupted run left it. The one exception is a
// cancel on the agent start itself, after the write-ahead agent.starting
// save: that run is lost, and the next tick reconciles the record to the
// desk as agent_lost. Nothing is stranded.

type scenario struct {
	name  string
	setup func(h *harness) (ticket.ID, func() Deps)
}

func scenarios() []scenario {
	return []scenario{
		{"start", func(h *harness) (ticket.ID, func() Deps) {
			h.local.alive = true
			return h.file("p", "Fix it", "ready", nil), h.deps
		}},
		{"finish", func(h *harness) (ticket.ID, func() Deps) {
			h.local.alive = true
			h.repo.BranchWith("bot/x", map[string]string{"lib/thing.rb": "def thing; 2; end\n"})
			id := localAgent(h, "work", "working", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
			h.local.running["t0001-w1"] = true
			h.local.reports["t0001-w1"] = []byte(`{"status": "done", "summary": "did it", "category": "fix"}`)
			return id, h.deps
		}},
		{"gate", func(h *harness) (ticket.ID, func() Deps) {
			h.gate.result = passedGate()
			return h.file("p", "Fix it", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") }), h.deps
		}},
		{"pr open", func(h *harness) (ticket.ID, func() Deps) {
			h.config(strings.Replace(withGitHub, "app_client_id = \"Iv1\"\n", "app_client_id = \"Iv1\"\nopen_prs = \"auto\"\n", 1))
			gh := &ghFake{c: h.calls, pr: map[string]any{"number": 7, "html_url": "https://github.com/o/gem/pull/7", "state": "open",
				"draft": false, "merged": false, "node_id": "PR_7", "head": map[string]any{"ref": "bot/x", "sha": "abc"}}}
			id := h.file("p", "Fix it", "ready_for_pr", func(t *ticket.Ticket) {
				t.Branch = ticket.Some("bot/x")
				t.Gate = ticket.Some(passedGate())
				t.Reports = append(t.Reports, reportMember("work", `{"status": "done", "summary": "did it"}`))
			})
			return id, func() Deps { return withFlow(h, gh) }
		}},
	}
}

func reportMember(stage, report string) jsonx.Member {
	v, err := jsonx.Parse([]byte(report))
	if err != nil {
		panic(err)
	}
	return jsonx.Member{Key: stage, Value: v}
}

func TestACancelBetweenAnyTwoCallsLeavesACommitPoint(t *testing.T) {
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			h := newHarness(t)
			id, deps := sc.setup(h)
			file := h.paths.TicketFile(string(id))
			states := map[string]bool{h.raw(id): true}
			h.calls.hook = func(string) {
				if b, err := os.ReadFile(file); err == nil {
					states[string(b)] = true
				}
			}
			if err := Tick(context.Background(), deps()); err != nil {
				t.Fatal(err)
			}
			states[h.raw(id)] = true
			n := len(h.calls.names())
			// the reference ends where as many ticks as the recovery's
			// leave it
			h.calls.hook = nil
			for r := 0; r < 3; r++ {
				if err := Tick(context.Background(), deps()); err != nil {
					t.Fatal(err)
				}
			}
			final := h.raw(id)
			if n < 3 {
				t.Fatalf("only %d adapter calls: %v", n, h.calls.names())
			}
			for k := 1; k <= n; k++ {
				h := newHarness(t)
				id, deps := sc.setup(h)
				ctx, cancel := context.WithCancel(context.Background())
				i, at := 0, ""
				h.calls.hook = func(op string) {
					if i++; i == k {
						at = op
						cancel()
					}
				}
				_ = Tick(ctx, deps())
				cancel()
				if got := h.raw(id); !states[got] {
					t.Errorf("cancel before call %d (%s): the file is no commit point:\n%s", k, at, got)
					continue
				}
				h.calls.hook = nil
				for r := 0; r < 3; r++ {
					if err := Tick(context.Background(), deps()); err != nil {
						t.Fatalf("recovery tick: %v", err)
					}
				}
				if got := h.raw(id); got != final {
					// the one cancel point allowed to end elsewhere: on the agent
					// start, after the write-ahead agent.starting save.
					// The next tick finds no agent behind the record and
					// reconciles it to agent_lost.
					rec := h.load(id)
					if at != "local.start" || rec.Column != string(model.ColNeedsHuman) || moveReason(lastMove(rec)) != model.ReasonAgentLost {
						t.Errorf("cancel before call %d (%s): after recovery column %s, %+v, not the uninterrupted run's", k, at, rec.Column, lastMove(rec))
					}
				}
			}
		})
	}
}
