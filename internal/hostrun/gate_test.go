package hostrun

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// A gate job ships its meta (kind gate, the gated head as its start),
// the checks, a bundle of the base and the branch, and the pinned files:
// no prompt, no role, no memory.
func TestAGateJobShipsTheHeadTheChecksAndThePinnedFiles(t *testing.T) {
	r := newRemote(t)
	r.repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
	r.repo.Write("Gemfile.lock", "GEM\n") // BranchWith committed it on bot/x; main's checkout has it untracked again
	head := r.repo.Git("rev-parse", "bot/x")
	files := r.start(t, Job{Ticket: r.ticket(t, "gate", "bot/x"), Name: "t0003-g1",
		Gate: &GateRun{Head: head, Script: "set -e; true; true; true"}})
	m := meta(t, files)
	if m.Kind != jobproto.KindGate || m.Start != head || m.Branch != "gate" || m.TimeoutMinutes != GateMinutes || m.Bundle {
		t.Errorf("meta %+v", m)
	}
	if !strings.HasSuffix(string(files["meta.json"]), `"kind": "gate"}`) {
		t.Errorf("meta.json %s", files["meta.json"])
	}
	if string(files["checks.sh"]) != "set -e; true; true; true" || string(files["untracked/Gemfile.lock"]) != "GEM\n" {
		t.Errorf("checks %q pinned %q", files["checks.sh"], files["untracked/Gemfile.lock"])
	}
	if heads := bundleHeads(t, files["repo.bundle"]); !reflect.DeepEqual(heads, map[string]bool{"refs/heads/main": true, "refs/heads/bot/x": true}) {
		t.Errorf("bundle %v", heads)
	}
	if len(files) != 4 {
		t.Errorf("members %v", files)
	}
	if err := r.jobs.Start(context.Background(), Job{Ticket: r.ticket(t, "gate", ""), Name: "t0003-g2", Project: r.project,
		Repo: gitRepo{r.project.Path}, Gate: &GateRun{Head: head, Script: "true"}}); err == nil {
		t.Error("a gate job with no branch started")
	}
	if err := r.local().Start(context.Background(), Job{Ticket: r.ticket(t, "gate", "bot/x"), Name: "t0003-g3", Project: r.project,
		Gate: &GateRun{Head: head, Script: "true"}}); err == nil {
		t.Error("a gate job started here")
	}
}
