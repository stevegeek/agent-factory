package prflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func mergedOnGitHub() route {
	return at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{
		"state": "closed", "merged": true, "merged_by": map[string]any{"login": "someone"},
		"merge_commit_sha": sha('m'), "head": map[string]any{"sha": sha('a')},
	}))
}

func TestCheckPRMergedOnGitHubDeletesTheBotBranch(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	h := newHarness(t)
	tk := prTicket(t, false)
	h.routes(mergedOnGitHub())
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "done" || !h.gh.did("DELETE", "/repos/o/gem/git/refs/heads/bot/x") {
		t.Fatalf("column %s, requests %v", tk.Column, h.gh.calls())
	}
}

func TestCheckPRBranchDeleteFailureIsOnlyAnEvent(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	h := newHarness(t)
	tk := prTicket(t, false)
	h.routes(mergedOnGitHub(),
		at("DELETE", "/repos/o/gem/git/refs/heads/bot/x", refuse(422, "Reference does not exist")))
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "done" || lastEntry(tk).Detail != "github: merged, but could not delete bot/x (422)" {
		t.Fatalf("column %s, last %+v", tk.Column, lastEntry(tk))
	}
}

func TestCheckPRKeepsBranchesItDoesNotOwn(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")

	h := newHarness(t)
	tk := prTicket(t, false)
	tk.Branch = ticket.Some("feature")
	h.routes(mergedOnGitHub())
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "done" || h.gh.any("DELETE") {
		t.Fatalf("column %s, requests %v", tk.Column, h.gh.calls())
	}

	h = newHarness(t)
	tk = prTicket(t, false)
	h.routes(at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{
		"state": "closed", "merged": false, "head": map[string]any{"sha": sha('a')},
	})))
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if h.gh.any("DELETE") {
		t.Fatalf("closed without merge: requests %v", h.gh.calls())
	}
}

func mergedTicket(t *testing.T, branch string) *ticket.Ticket {
	t.Helper()
	tk := prTicket(t, true)
	tk.Branch = ticket.Some(branch)
	ticket.Move(tk, string(model.ColDone), model.ReasonPRMerged, "", "", "", time.Unix(1_800_000_000, 0))
	return tk
}

func headAt(branch string) route {
	return at("GET", "/repos/o/gem/git/ref/heads/"+branch, ok(map[string]any{
		"object": map[string]any{"type": "commit", "sha": sha('a')},
	}))
}

func TestMergedBranchesDryRunDeletesNothing(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	h := newHarness(t)
	open := prTicket(t, true)
	open.Branch = ticket.Some("bot/open")
	h.routes(headAt("bot/x"), headAt("bot/open"))
	got, err := h.flow.MergedBranches(context.Background(), cfg, []*ticket.Ticket{mergedTicket(t, "bot/x"), open}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Repo != "o/gem" || got[0].Branch != "bot/x" || got[0].Deleted {
		t.Fatalf("branches = %+v", got)
	}
	if h.gh.any("DELETE") {
		t.Fatalf("requests %v", h.gh.calls())
	}
}

func TestPruneBranchesDryRunPrintsAndDeletesNothing(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	h := newHarness(t)
	h.routes(headAt("bot/x"))
	tk := mergedTicket(t, "bot/x")
	var out strings.Builder
	n, err := h.flow.PruneBranches(context.Background(), cfg, []*ticket.Ticket{tk}, false, &out)
	if err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if want := "o/gem bot/x (" + string(tk.ID) + ")\ndry run: --apply deletes these\n"; out.String() != want {
		t.Fatalf("out = %q, want %q", out.String(), want)
	}
	if h.gh.any("DELETE") {
		t.Fatalf("requests %v", h.gh.calls())
	}
}

func TestMergedBranchesApplyDeletesWhatGitHubStillHas(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	h := newHarness(t)
	h.routes(headAt("bot/x"),
		at("GET", "/repos/o/gem/git/ref/heads/bot/gone", refuse(404, "Not Found")),
		at("DELETE", "/repos/o/gem/git/refs/heads/bot/y", refuse(422, "Reference does not exist")),
		headAt("bot/y"))
	ts := []*ticket.Ticket{mergedTicket(t, "bot/y"), mergedTicket(t, "bot/gone"), mergedTicket(t, "bot/x")}
	got, err := h.flow.MergedBranches(context.Background(), cfg, ts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Branch != "bot/x" || !got[0].Deleted || got[1].Branch != "bot/y" || got[1].Deleted || got[1].Err == nil {
		t.Fatalf("branches = %+v", got)
	}
	if !h.gh.did("DELETE", "/repos/o/gem/git/refs/heads/bot/x") || h.gh.did("DELETE", "/repos/o/gem/git/refs/heads/bot/gone") {
		t.Fatalf("requests %v", h.gh.calls())
	}
}
