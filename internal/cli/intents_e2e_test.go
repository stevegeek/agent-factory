package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/pipeline"
	"github.com/stevegeek/agent-factory/internal/prflow"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Foreman's answer records an intent, and the next tick carries
// it out. Foreman answers through ops with ops.Intents as its PRActions
// (cmd/foreman wires exactly that; its wiring test checks the web half),
// and the CLI's own tick, with its real wiring and a fake GitHub, drains
// the intent: "Send back" from pr_open turns the PR back to draft and the
// save that records it clears hold_requested. The import table lets no
// package hold both web and the pipeline, so the two halves meet here, on
// one root.
func TestForemansSendBackIsDrainedByTheNextTick(t *testing.T) {
	f := githubRoot(t)
	tk := file(t, f, "gem", "pr_open", func(tk *ticket.Ticket) {
		tk.Branch = ticket.Some("bot/fix-it")
		tk.PR = ticket.Some(ticket.PR{Number: 5, URL: "https://github.com/example/gem/pull/5", Draft: ticket.Some(false)})
	})
	gh := &fakeGitHub{answer: func(method, path, body string) (int, string) {
		switch {
		case method == "GET" && path == "/repos/example/gem/pulls/5":
			return 200, `{"number":5,"state":"open","draft":false,"node_id":"PR_5","html_url":"https://github.com/example/gem/pull/5","head":{"ref":"bot/fix-it","sha":"abc"}}`
		case method == "POST" && path == "/graphql":
			return 200, `{"data":{"convertPullRequestToDraft":{"pullRequest":{"isDraft":true}}}}`
		}
		return 404, `{"message":"Not Found"}`
	}}
	wakeFile := wakeLog(f)
	e := &Env{Environ: f.env.Vars, Exec: &fakeExec{}}
	e.init()
	cfg, err := e.Config()
	if err != nil {
		t.Fatal(err)
	}

	// Foreman's answer: the move and an intent, no GitHub call
	svc, err := opsCLI{HTTP: gh}.service(e)
	if err != nil {
		t.Fatal(err)
	}
	svc.PR = ops.Intents{Clock: e.Clock}
	if _, err := svc.Move(t.Context(), string(tk.ID), "ready", "split it"); err != nil {
		t.Fatal(err)
	}
	got := load(t, f, tk.ID)
	if got.Column != "ready" || !got.PR.V.HoldRequested.Present() || len(gh.seen()) != 0 {
		t.Fatalf("after the answer: column %s, pr %+v, GitHub calls %q", got.Column, got.PR.V, gh.seen())
	}
	if n := wakes(t, wakeFile); n != 1 {
		t.Errorf("the answer woke the tick %d times", n)
	}

	// the next tick: its GitHub pass drains the intent first
	deps := tickDeps(e, cfg)
	deps.GitHub.(*prflow.Flow).GitHub = opsCLI{HTTP: gh}.githubClient(e, cfg)
	if err := pipeline.Tick(t.Context(), deps); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gh.seen(), "POST /graphql") || !strings.Contains(strings.Join(gh.bodies, ""), "convertPullRequestToDraft") {
		t.Fatalf("the tick did not turn the PR back to draft: %q", gh.seen())
	}
	got = load(t, f, tk.ID)
	if got.PR.V.HoldRequested.Present() || !got.PR.V.Draft.Or(false) {
		t.Fatalf("after the tick: pr %+v", got.PR.V)
	}
	if ev := events(got); !slices.Contains(ev, "pr: PR #5 back to draft while it is reworked") {
		t.Errorf("events %q", ev)
	}
}
