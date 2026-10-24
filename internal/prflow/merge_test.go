package prflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func mergeRoutes(pr map[string]any, merge reply) []route {
	return []route{
		at("PUT", "/repos/o/gem/pulls/5/merge", merge),
		at("GET", "/repos/o/gem/pulls/5", ok(pr)),
		at("POST", "/graphql", ok(map[string]any{"data": map[string]any{}})),
	}
}

func openPRAt(head string) map[string]any {
	return map[string]any{"state": "open", "draft": false, "head": map[string]any{"sha": head}, "node_id": "PR_5"}
}

// port: TF GithubOutTest.test_merge_on_your_go_ahead_merges_exactly_the_gated_commit
func TestMergeOnYourGoAheadMergesExactlyTheGatedCommit(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	tk := prTicket(t, true)
	pv := tk.PR.V
	pv.MergeWait = ticket.Some("checks")
	tk.PR = ticket.Some(pv)
	h.routes(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true}))...)
	if err := h.flow.MergePR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if got := string(jsonx.EncodePython(h.gh.body(t, "PUT", "/repos/o/gem/pulls/5/merge"), -1)); got != `{"sha": "`+sha('a')+`", "merge_method": "merge"}` {
		t.Errorf("merge = %s", got)
	}
	if !h.gh.did("DELETE", "/repos/o/gem/git/refs/heads/bot/x") {
		t.Errorf("requests = %v", h.gh.calls())
	}
	e := lastEntry(tk)
	if tk.Column != "done" || e.Detail != "pr_open -> done: PR merged by the factory on your go-ahead" ||
		e.Rule != "PR #5 was auto-merge eligible; you confirmed it in Foreman and the factory merged aaaaaaa" ||
		lastMove(t, tk).Reason != model.ReasonPRMerged || tk.PR.V.MergeWait.Set {
		t.Fatalf("column %s, last %+v, pr %+v", tk.Column, e, tk.PR.V)
	}

	// a moved branch or a PR that was not eligible is never merged
	for name, change := range map[string]func(*ticket.Ticket){
		"moved": func(tk *ticket.Ticket) {
			g := tk.Gate.V
			g.Head = sha('b')
			tk.Gate = ticket.Some(g)
		},
		"not eligible": func(tk *ticket.Ticket) {
			g := tk.Gate.V
			g.AutoEligible = false
			tk.Gate = ticket.Some(g)
		},
	} {
		h := newHarness(t)
		tk := prTicket(t, true)
		change(tk)
		h.routes(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true}))...)
		err := h.flow.MergePR(context.Background(), cfg, p, tk)
		var r *Refusal
		if !errors.As(err, &r) || h.gh.any("PUT") || tk.Column != "pr_open" {
			t.Errorf("%s: err %v, requests %v", name, err, h.gh.calls())
		}
	}
}

// merge_pr refuses a PR that is closed or a draft, and says what GitHub
// said when it refuses the merge (§10.7 S14).
func TestMergeRefusals(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	draft := openPRAt(sha('a'))
	draft["draft"] = true
	closed := openPRAt(sha('a'))
	closed["state"] = "closed"
	gone := map[string]any{"draft": false}
	for _, c := range []struct {
		name  string
		pr    map[string]any
		merge reply
		want  string
	}{
		{"draft", draft, reply{}, "PR #5 is a draft; nothing to merge"},
		{"closed", closed, reply{}, "PR #5 is closed; nothing to merge"},
		{"no state", gone, reply{}, "PR #5 is None; nothing to merge"},
		{"not mergeable", openPRAt(sha('a')), refuse(405, "Pull Request is not mergeable"), "GitHub refused the merge of PR #5: Pull Request is not mergeable"},
		{"head moved", openPRAt(sha('a')), refuse(409, "Head branch was modified. Review and try the merge again."),
			"GitHub refused the merge of PR #5: Head branch was modified. Review and try the merge again."},
		{"not merged", openPRAt(sha('a')), ok(map[string]any{"merged": false, "message": "nope"}), "GitHub did not merge PR #5: nope"},
		{"not merged, no word", openPRAt(sha('a')), ok(map[string]any{"merged": false}), "GitHub did not merge PR #5: ?"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			tk := prTicket(t, true)
			merge := c.merge
			if merge.Code == 0 && merge.Value == nil {
				merge = ok(map[string]any{"merged": true})
			}
			h.routes(mergeRoutes(c.pr, merge)...)
			err := h.flow.MergePR(context.Background(), cfg, p, tk)
			var r *Refusal
			if !errors.As(err, &r) || err.Error() != c.want {
				t.Fatalf("err = %v", err)
			}
			if tk.Column != "pr_open" || h.gh.any("DELETE") {
				t.Errorf("column %s, requests %v", tk.Column, h.gh.calls())
			}
		})
	}
	// a 5xx is not a refusal: the merge is tried again
	h := newHarness(t)
	tk := prTicket(t, true)
	h.routes(mergeRoutes(openPRAt(sha('a')), refuse(502, "Bad Gateway"))...)
	err := h.flow.MergePR(context.Background(), cfg, p, tk)
	var r *Refusal
	if errors.As(err, &r) || !github.IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
}

// When the base moved since the gate and the checks fail with it merged in,
// the PR goes back for a rework and nothing merges.
//
// port: TF GithubOutTest.test_main_moved_since_the_gate_means_checks_again_with_it_merged_in
func TestMainMovedAndTheChecksFailSendsItBack(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	for _, pass := range []bool{true, false} {
		h := newHarness(t)
		h.checks.ok, h.checks.out = pass, "lots of output\n"+strings.Repeat("x", 700)+"\n1 failure"
		tk := prTicket(t, true)
		h.routes(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true}))...)
		err := h.flow.MergePR(context.Background(), cfg, p, tk)
		if h.checks.calls != 1 || h.gh.any("PUT") != pass {
			t.Fatalf("pass=%v: checks %d, requests %v", pass, h.checks.calls, h.gh.calls())
		}
		if pass {
			if err != nil || tk.Column != "done" {
				t.Fatalf("err %v, column %s", err, tk.Column)
			}
			continue
		}
		if !errors.Is(err, ErrMainMoved) || err.Error() != "main moved and the checks fail with it merged in; sent back for a rework" {
			t.Fatalf("err = %v", err)
		}
		var r *Refusal
		if !errors.As(err, &r) {
			t.Error("a main-moved merge is a refusal (it is not retried)")
		}
		if tk.Column != "ready" || lastMove(t, tk).Reason != model.ReasonMainMoved {
			t.Fatalf("column %s", tk.Column)
		}
		note := tk.Notes[len(tk.Notes)-1].Text
		if !strings.Contains(note, "main had moved") || !strings.Contains(note, "logs/T0042/merge-check-output.txt") ||
			!strings.Contains(note, "1 failure).") || strings.Contains(note, "lots of output") {
			t.Errorf("note = %q", note)
		}
		out, err := os.ReadFile(filepath.Join(h.logs, "T0042", "merge-check-output.txt"))
		if err != nil || string(out) != h.checks.out {
			t.Errorf("merge-check-output.txt: %v", err)
		}
		mv := details(tk, "move")
		if mv[0] != "pr_open -> ready: not merged: main moved and the checks fail with it" {
			t.Errorf("moves = %q", mv)
		}
		if got := details(tk, "pr"); len(got) != 1 || got[0] != "PR #5 back to draft while it is reworked" {
			t.Errorf("hold: %q", got)
		}
	}
}

// A merge that worked but whose branch could not be deleted is only an
// event (§10.7 S14); a branch that is not bot/* is never deleted.
func TestABranchDeleteFailureIsOnlyAnEvent(t *testing.T) {
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	h := newHarness(t)
	tk := prTicket(t, true)
	h.routes(append(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true})),
		at("DELETE", "/repos/o/gem/git/refs/heads/bot/x", refuse(422, "Reference does not exist")))...)
	if err := h.flow.MergePR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "done" || lastEntry(tk).Detail != "github: merged, but could not delete bot/x (422)" {
		t.Fatalf("column %s, last %+v", tk.Column, lastEntry(tk))
	}

	h = newHarness(t)
	tk = prTicket(t, true)
	tk.Branch = ticket.Some("feature")
	h.routes(mergeRoutes(openPRAt(sha('a')), ok(map[string]any{"merged": true}))...)
	if err := h.flow.MergePR(context.Background(), cfg, p, tk); err != nil {
		t.Fatal(err)
	}
	if tk.Column != "done" || h.gh.any("DELETE") {
		t.Fatalf("column %s, requests %v", tk.Column, h.gh.calls())
	}
}
