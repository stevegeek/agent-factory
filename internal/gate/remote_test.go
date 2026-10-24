package gate

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Checks that ran on another host are decided here from the heads the
// gate job was started on, not the branch as it is now; the result names
// the host, the move says where the checks ran, and the output is kept.
// Nothing runs in a container here.
func TestDecideUsesTheJobsHeadsAndNamesTheHost(t *testing.T) {
	r := newRig(t)
	r.repo.BranchWith("b", map[string]string{"README.md": "x\n"})
	gated := r.git(r.repo.Dir, "rev-parse", "b")
	base := r.git(r.repo.Dir, "rev-parse", "main")
	// the branch moves on after the job started: not what was gated
	r.git(r.repo.Dir, "checkout", "-q", "b")
	r.repo.Write("lib/late.rb", strings.Repeat("late\n", 50))
	r.repo.Commit("late")
	r.git(r.repo.Dir, "checkout", "-q", "main")
	cfg, p := r.config("", "[gate]\nallowed_paths = [\"*.md\"]\n")
	tk := gateTicket(t, `{"work": {"status": "done", "summary": "s", "category": "docs"},
		"review": {"verdict": "pass", "runtime_behaviour": "no", "summary": "fine"}}`)
	g, err := r.runner.Decide(context.Background(), cfg, p, tk, Remote{Head: gated, Base: base, ChecksPass: true, Output: "tests ok\n", Host: "desk"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Head != gated || g.Base != base || !slices.Equal(g.Files, []string{"README.md"}) || g.ChangedLines != 2 || !g.AutoEligible || g.Host != "desk" {
		t.Errorf("gate %+v", g)
	}
	if len(r.podman.runs) != 0 {
		t.Errorf("a container ran: %v", r.podman.runs)
	}
	out, err := os.ReadFile(filepath.Join(r.paths.TicketLogs("T0001"), "gate-output.txt"))
	if err != nil || string(out) != "tests ok\n" {
		t.Errorf("gate-output.txt: %q %v", out, err)
	}
	Record(tk, cfg, g, time.Unix(0, 0))
	if d := tk.History[len(tk.History)-1].Detail; !strings.HasSuffix(d, ": auto-merge eligible (checks on desk)") {
		t.Errorf("move %q", d)
	}

	g, err = r.runner.Decide(context.Background(), cfg, p, gateTicket(t, `{}`), Remote{Head: gated, Base: base, ChecksPass: false, Host: "desk"})
	if err != nil {
		t.Fatal(err)
	}
	failed := gateTicket(t, `{}`)
	Record(failed, cfg, g, time.Unix(0, 0))
	if d := failed.History[len(failed.History)-1].Detail; !strings.HasSuffix(d, "tests or lint failed on a fresh checkout (checks on desk)") {
		t.Errorf("failed move %q", d)
	}
}
