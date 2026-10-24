package prflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// fakeDeployer records the merges it is told of, and fails when err is set.
type fakeDeployer struct {
	got []MergedPR
	err error
}

func (d *fakeDeployer) Merged(_ context.Context, _ config.Project, _ *ticket.Ticket, m MergedPR) error {
	d.got = append(d.got, m)
	return d.err
}

// selfDeployHarness is a harness whose flow has a fake Deployer, and the
// gem project marked as the factory's own core.
func selfDeployHarness(t *testing.T) (*harness, *fakeDeployer, *config.Config, config.Project) {
	h := newHarness(t)
	d := &fakeDeployer{}
	h.flow.Deploys = d
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	p.SelfDeploy = config.SelfDeployCore
	return h, d, cfg, p
}

func TestLandRecordsMergeCommitAndDeploys(t *testing.T) {
	h, d, cfg, p := selfDeployHarness(t)
	tk := prTicket(t, true)
	h.routes(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true, "sha": sha('m')}))...)
	if err := h.flow.MergePR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "done" || tk.PR.V.MergeCommit != sha('m') {
		t.Fatalf("column %s, pr %+v", tk.Column, tk.PR.V)
	}
	if want := []MergedPR{{Commit: sha('m')}}; !slices.Equal(d.got, want) {
		t.Errorf("deploys = %+v, want %+v", d.got, want)
	}
}

func TestCheckPRMergedOnGitHubDeploys(t *testing.T) {
	h, d, cfg, p := selfDeployHarness(t)
	tk := prTicket(t, false)
	h.routes(at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{
		"state": "closed", "merged": true, "merged_by": map[string]any{"login": "maintainer-login"},
		"merge_commit_sha": sha('m'), "head": map[string]any{"sha": sha('a')},
	})))
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "done" || tk.PR.V.MergeCommit != sha('m') {
		t.Fatalf("column %s, pr %+v", tk.Column, tk.PR.V)
	}
	if want := []MergedPR{{Commit: sha('m')}}; !slices.Equal(d.got, want) {
		t.Errorf("deploys = %+v, want %+v", d.got, want)
	}
}

func TestHoldLabelHolds(t *testing.T) {
	for name, labels := range map[string][]any{
		"held":  {map[string]any{"name": "docs"}, map[string]any{"name": HoldDeployLabel}},
		"other": {map[string]any{"name": "docs"}},
	} {
		h, d, cfg, p := selfDeployHarness(t)
		tk := prTicket(t, true)
		pr := openPRAt(sha('a'))
		pr["labels"] = labels
		h.routes(mergeRoutes(pr, ok(map[string]any{"merged": true, "sha": sha('m')}))...)
		if err := h.flow.MergePR(context.Background(), cfg, p, tk); err != nil {
			t.Fatal(err)
		}
		if want := []MergedPR{{Commit: sha('m'), Hold: name == "held"}}; !slices.Equal(d.got, want) {
			t.Errorf("%s: deploys = %+v, want %+v", name, d.got, want)
		}
	}
}

func TestNoDeployForOrdinaryProjects(t *testing.T) {
	h, d, cfg, _ := selfDeployHarness(t)
	p := project(t, cfg, "gem")
	tk := prTicket(t, true)
	h.routes(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true, "sha": sha('m')}))...)
	if err := h.flow.MergePR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if len(d.got) != 0 || tk.PR.V.MergeCommit != "" {
		t.Errorf("deploys %+v, pr %+v", d.got, tk.PR.V)
	}

	// and a flow with no Deployer merges as before
	h, _, cfg, p = selfDeployHarness(t)
	h.flow.Deploys = nil
	tk = prTicket(t, true)
	h.routes(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true, "sha": sha('m')}))...)
	if err := h.flow.MergePR(context.Background(), cfg, p, tk); err != nil || tk.Column != "done" {
		t.Errorf("err %v, column %s", err, tk.Column)
	}
}

func TestDeployRequestFailureKeepsMerge(t *testing.T) {
	h, d, cfg, p := selfDeployHarness(t)
	d.err = errors.New("no [deploy] in config")
	tk := prTicket(t, true)
	h.routes(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true, "sha": sha('m')}))...)
	if err := h.flow.MergePR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "done" || !h.gh.did("DELETE", "/repos/o/gem/git/refs/heads/bot/x") {
		t.Fatalf("column %s, requests %v", tk.Column, h.gh.calls())
	}
	if got := details(tk, "error"); !slices.Equal(got, []string{"deploy not requested: no [deploy] in config"}) {
		t.Errorf("error events = %q", got)
	}
	if len(h.log.lines) != 1 || !strings.Contains(h.log.lines[0], "deploy not requested") {
		t.Errorf("log = %q", h.log.lines)
	}
}

// The label holds on every merge path: the factory's merge on an approving
// review, and a merge seen on GitHub.
func TestHoldLabelHoldsOnEveryPath(t *testing.T) {
	labels := []any{map[string]any{"name": HoldDeployLabel}}
	h, d, cfg, p := selfDeployHarness(t)
	tk := prTicket(t, false)
	h.routes(
		at("GET", "/repos/o/gem/pulls/5/reviews", ok([]any{review(3, "APPROVED", "maintainer-login", sha('a'))})),
		at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{"state": "open", "draft": false, "merged": false,
			"head": map[string]any{"sha": sha('a')}, "node_id": "PR_5", "labels": labels})),
		at("PUT", "/repos/o/gem/pulls/5/merge", ok(map[string]any{"merged": true, "sha": sha('m')})),
	)
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if want := []MergedPR{{Commit: sha('m'), Hold: true}}; tk.Column != "done" || !slices.Equal(d.got, want) {
		t.Errorf("approval: column %s, deploys = %+v, want %+v", tk.Column, d.got, want)
	}

	h, d, cfg, p = selfDeployHarness(t)
	tk = prTicket(t, false)
	h.routes(at("GET", "/repos/o/gem/pulls/5", ok(map[string]any{
		"state": "closed", "merged": true, "merged_by": map[string]any{"login": "maintainer-login"},
		"merge_commit_sha": sha('m'), "head": map[string]any{"sha": sha('a')}, "labels": labels,
	})))
	if err := h.flow.CheckPR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if want := []MergedPR{{Commit: sha('m'), Hold: true}}; !slices.Equal(d.got, want) {
		t.Errorf("merged on GitHub: deploys = %+v, want %+v", d.got, want)
	}
}
