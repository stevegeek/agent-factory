package gate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// holdRig is a project that is a clone of the rig's repository (its
// origin), with the ticket's branch b gated at its head on main.
func holdRig(t *testing.T) (*rig, string, *ticket.Ticket) {
	t.Helper()
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"lib/new.rb": "x\n"})
	clone := filepath.Join(t.TempDir(), "clone")
	r.git(r.repo.Dir, "clone", "-q", r.repo.Dir, clone)
	r.git(clone, "branch", "-q", "b", "origin/b")
	tk := gateTicket(t, `{}`)
	g := ticket.Gate{ChecksPass: true, AutoEligible: true, Files: []string{"lib/new.rb"}, Blockers: []string{},
		Head: r.git(clone, "rev-parse", "b"), Base: r.git(clone, "rev-parse", "main")}
	tk.Gate = ticket.Some(g)
	return r, clone, tk
}

func (r *rig) holdConfig(clone string) HoldIn {
	cfg, p := r.config("", "")
	p.Path = clone
	return HoldIn{Config: cfg, Project: p, Ticket: nil}
}

// An old ticket gated before the base was recorded merges as before.
func TestStillHoldsWithNoGatedBase(t *testing.T) {
	r := newRig(t)
	r.runner.Exec = failExec(t)
	tk := gateTicket(t, `{}`)
	tk.Gate = ticket.Some(ticket.Gate{Head: "abc"})
	cfg, p := r.config("", "")
	ok, out, err := r.runner.StillHoldsOnMain(context.Background(), HoldIn{Config: cfg, Project: p, Ticket: tk})
	if !ok || out != "" || err != nil {
		t.Errorf("= %v %q %v", ok, out, err)
	}
	tk.Gate = ticket.Opt[ticket.Gate]{}
	if ok, _, err := r.runner.StillHoldsOnMain(context.Background(), HoldIn{Config: cfg, Project: p, Ticket: tk}); !ok || err != nil {
		t.Errorf("no gate: %v %v", ok, err)
	}
}

// The base has not moved since the gate: no checks run.
func TestStillHoldsWhenTheBaseIsWhereItWas(t *testing.T) {
	r, clone, tk := holdRig(t)
	in := r.holdConfig(clone)
	in.Ticket = tk
	ok, out, err := r.runner.StillHoldsOnMain(context.Background(), in)
	if !ok || out != "" || err != nil {
		t.Errorf("= %v %q %v", ok, out, err)
	}
	if len(r.podman.runs) != 0 {
		t.Error("the checks ran")
	}
}

// port: TF GithubOutTest.test_main_moved_since_the_gate_means_checks_again_with_it_merged_in
//
// (gate part) main moved on origin: the base is refreshed, the checks run
// on the gated head with the new base merged in, and on a pass the gate
// records the new base with a gate event.
func TestMainMovedMeansChecksAgainWithItMergedIn(t *testing.T) {
	r, clone, tk := holdRig(t)
	r.repo.Write("README.md", "moved\n")
	r.repo.Commit("main moves")
	moved := r.git(r.repo.Dir, "rev-parse", "main")
	in := r.holdConfig(clone)
	in.Ticket = tk
	wt := filepath.Join(r.paths.Gate, "T0001")
	var merged bool
	r.podman.inspect = func(g podman.Gate) {
		merged = r.git(wt, "rev-parse", "HEAD^2") == moved && g.Name == "factory-merge-T0001"
	}
	before := len(tk.History)
	ok, _, err := r.runner.StillHoldsOnMain(context.Background(), in)
	if !ok || err != nil {
		t.Fatalf("= %v %v", ok, err)
	}
	if !merged {
		t.Error("the checks did not run on the gated head with the new main merged in")
	}
	g, _ := tk.Gate.Get()
	if g.Base != moved {
		t.Errorf("gate base %q, want %q", g.Base, moved)
	}
	if len(tk.History) != before+1 {
		t.Fatalf("history %+v", tk.History)
	}
	e := tk.History[len(tk.History)-1]
	if e.Event != "gate" || e.Detail != "main moved; the checks pass again with it merged in ("+moved[:7]+")" || e.At != "2026-10-17T09:00:00+00:00" {
		t.Errorf("event %+v", e)
	}
	if r.git(clone, "rev-parse", "main") != moved {
		t.Error("the clone's base was not refreshed")
	}
}

// The checks fail with the base merged in: not held, nothing recorded.
func TestMainMovedAndTheChecksFail(t *testing.T) {
	r, clone, tk := holdRig(t)
	r.repo.Write("README.md", "moved\n")
	r.repo.Commit("main moves")
	in := r.holdConfig(clone)
	in.Ticket = tk
	r.podman.ok, r.podman.out = false, "1 failure"
	gated, _ := tk.Gate.Get()
	before := len(tk.History)
	ok, out, err := r.runner.StillHoldsOnMain(context.Background(), in)
	if ok || out != "1 failure" || err != nil {
		t.Errorf("= %v %q %v", ok, out, err)
	}
	if g, _ := tk.Gate.Get(); g.Base != gated.Base || len(tk.History) != before {
		t.Error("a failed merge check changed the ticket")
	}
}

// The base cannot be refreshed (no origin): logged, and the local base is
// used.
func TestStillHoldsUsesTheLocalBaseWhenItCannotRefresh(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"lib/new.rb": "x\n"})
	tk := gateTicket(t, `{}`)
	base := r.git(r.repo.Dir, "rev-parse", "main")
	tk.Gate = ticket.Some(ticket.Gate{Head: r.git(r.repo.Dir, "rev-parse", "b"), Base: base})
	cfg, p := r.config("", "")
	ok, _, err := r.runner.StillHoldsOnMain(context.Background(), HoldIn{Config: cfg, Project: p, Ticket: tk})
	if !ok || err != nil {
		t.Errorf("= %v %v", ok, err)
	}
	if len(r.log.lines) != 1 || !strings.HasPrefix(r.log.lines[0], "merge check for T0001: could not refresh main, using the local one: ") {
		t.Errorf("log %q", r.log.lines)
	}
	// main moved locally: the checks run against it.
	r.repo.Write("README.md", "moved\n")
	r.repo.Commit("main moves")
	if ok, _, err := r.runner.StillHoldsOnMain(context.Background(), HoldIn{Config: cfg, Project: p, Ticket: tk}); !ok || err != nil {
		t.Errorf("= %v %v", ok, err)
	}
	if len(r.podman.runs) != 1 {
		t.Errorf("%d runs", len(r.podman.runs))
	}
}

// Quirk 2: the merge check is a gate on this host, so it needs room
// there: one of max_agents' slots, and the host to itself when the
// project's gate memory is heavy. Without room it does not run, and says
// so with ErrNoRoom; an agent count that cannot be read is no room.
func TestTheMergeCheckNeedsRoomOnThisHost(t *testing.T) {
	for _, c := range []struct {
		name    string
		project string
		agents  int
		err     error
		room    bool
	}{
		{name: "a free slot", agents: 1, room: true},
		{name: "every slot taken", agents: 2},
		{name: "heavy beside an agent", project: `gate_memory = "1800m"`, agents: 1},
		{name: "heavy on an idle host", project: `gate_memory = "1800m"`, agents: 0, room: true},
		{name: "the agents unknown", agents: 0, err: errors.New("scion is down")},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, clone, tk := holdRig(t)
			r.repo.Write("README.md", "moved\n")
			r.repo.Commit("main moves")
			cfg, p := r.config(c.project, "max_agents = 2\n")
			p.Path = clone
			r.runner.Agents = func(context.Context, *config.Config) (int, error) { return c.agents, c.err }
			gated, _ := tk.Gate.Get()
			before := len(tk.History)
			ok, _, err := r.runner.StillHoldsOnMain(context.Background(), HoldIn{Config: cfg, Project: p, Ticket: tk})
			if c.room {
				if !ok || err != nil || len(r.podman.runs) != 1 {
					t.Fatalf("= %v %v, %d runs", ok, err, len(r.podman.runs))
				}
				return
			}
			if ok || !errors.Is(err, ErrNoRoom) || len(r.podman.runs) != 0 {
				t.Fatalf("= %v %v, %d runs", ok, err, len(r.podman.runs))
			}
			if g, _ := tk.Gate.Get(); g.Base != gated.Base || len(tk.History) != before {
				t.Error("a merge check with no room changed the ticket")
			}
		})
	}
}

func TestFits(t *testing.T) {
	cfg := mustConfig(t, "max_agents = 2\n[projects.small]\npath = \"/x\"\n[projects.bigproj]\npath = \"/y\"\ngate_memory = \"1800m\"\n")
	small, _ := cfg.Project("small")
	bigproj, _ := cfg.Project("bigproj")
	for _, c := range []struct {
		p      *config.Project
		agents int
		want   bool
	}{
		{small, 0, true}, {small, 1, true}, {small, 2, false},
		{bigproj, 0, true}, {bigproj, 1, false}, {bigproj, 2, false},
	} {
		if got := Fits(cfg, *c.p, c.agents); got != c.want {
			t.Errorf("%s beside %d agents: %v", c.p.Key, c.agents, got)
		}
	}
}

// A cancelled context is not a refresh failure to log past.
func TestStillHoldsStopsWhenCancelled(t *testing.T) {
	r, clone, tk := holdRig(t)
	in := r.holdConfig(clone)
	in.Ticket = tk
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.runner.StillHoldsOnMain(ctx, in); err == nil {
		t.Error("no error")
	}
}
