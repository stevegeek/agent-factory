package prflow

import (
	"context"
	"testing"
)

const noGitHubConfig = `
[projects.gem]
path = "/nonexistent/gem"
base = "main"
github = "o/gem"
`

// pruneTicket is a closed ticket on bot/fix-it with no PR that asks for its
// branch to be deleted.
func (h *harness) pruneTicket() {
	h.t.Helper()
	h.board.put(h.t, decode(h.t, `{"id": "T0001", "project": "gem", "title": "Fix it", "body": "", "door": "human",
		"column": "rejected", "kind": null, "branch": "bot/fix-it", "agent": null, "reports": {},
		"created": "2026-03-01T11:00:00+00:00", "history": [], "prune_requested": "2026-03-01T11:00:00+00:00"}`))
	h.setHead("bot/fix-it", sha('a'))
}

func TestPassDeletesARequestedBranchHereAndOnGitHub(t *testing.T) {
	h := intentHarness(t)
	h.pruneTicket()
	if err := h.flow.Pass(context.Background(), parseConfig(t, intentConfig), h.board.all(t)); err != nil {
		t.Fatal(err)
	}
	if !h.gh.did("DELETE", "/repos/o/gem/git/refs/heads/bot/fix-it") {
		t.Errorf("GitHub's branch was not deleted: %v", h.gh.calls())
	}
	if len(h.git.deleted) != 1 || h.git.deleted[0] != "bot/fix-it" {
		t.Errorf("local deletions %v", h.git.deleted)
	}
	if tk := h.board.load(t, "T0001"); tk.PruneRequested.Set {
		t.Errorf("the request is kept after the deletion")
	}
}

func TestPassDeletesTheLocalBranchWithoutGitHub(t *testing.T) {
	h := intentHarness(t)
	h.pruneTicket()
	if err := h.flow.Pass(context.Background(), parseConfig(t, noGitHubConfig), h.board.all(t)); err != nil {
		t.Fatal(err)
	}
	if h.gh.any("DELETE") {
		t.Errorf("GitHub was called with no App set up")
	}
	if len(h.git.deleted) != 1 || h.board.load(t, "T0001").PruneRequested.Set {
		t.Errorf("local deletions %v", h.git.deleted)
	}
}

func TestPruneBranchIgnoresABranchGitHubNeverHad(t *testing.T) {
	h := intentHarness(t)
	h.pruneTicket()
	h.routes(at("DELETE", "/repos/o/gem/git/refs/heads/bot/fix-it", refuse(422, "Reference does not exist")))
	tk := h.board.load(t, "T0001")
	if err := h.flow.PruneBranch(context.Background(), parseConfig(t, intentConfig), tk); err != nil {
		t.Fatal(err)
	}
	if len(h.git.deleted) != 1 || len(tk.History) != 0 {
		t.Errorf("deleted %v, history %+v", h.git.deleted, tk.History)
	}
}
