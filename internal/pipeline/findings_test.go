package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// fakeTracker answers OpenItems from a table, or fails.
type fakeTracker struct {
	items map[string][]github.Issue
	err   error
	asked []string
}

func (f *fakeTracker) OpenItems(_ context.Context, repo string) ([]github.Issue, error) {
	f.asked = append(f.asked, repo)
	return f.items[repo], f.err
}

const findingsReport = `{"status": "done", "summary": "2 bugs", "result": "## Bugs",
	"findings": [{"title": "Raise a format error for nil", "severity": "low", "kind": "bug", "repro": "decode(nil)", "evidence": "NoMethodError"},
	             {"title": "Keep hex leading zeros", "severity": "medium", "kind": "bug", "behaviour_change": true, "repro": "00ff", "evidence": "ff"}]}`

// A finding task's findings go to the verifier, whose prompt lists them and
// what is open on the board and the tracker; its report brings them to the
// desk as the findings ask.
func TestAFindingTaskIsVerifiedAgainstTheBoardAndTheTracker(t *testing.T) {
	h := newHarness(t)
	h.config(strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\ngithub = \"acme/p\"\n", 1))
	tracker := &fakeTracker{items: map[string][]github.Issue{"acme/p": {
		{Number: 27, Title: "decode_hex: max_length guard", PR: true}, {Number: 18, Title: "Cache the coder"}}}}
	d := h.deps()
	d.Issues = tracker
	tick := func() {
		t.Helper()
		if err := Tick(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	h.file("p", "Thread safety of the coder", "triage", nil)
	closed := h.file("p", "Closed long ago", "done", nil)
	id := h.file("p", "Bug hunt", "ready", func(t *ticket.Ticket) {
		t.Kind = ticket.Some("task")
		t.FindingsAsked = ticket.Some(true)
	})
	h.local.alive = true
	tick() // the task and the triage start
	name := strings.ToLower(string(id)) + "-k1"
	h.local.reports[name] = []byte(findingsReport)
	tick() // the task finishes with findings; the verifier starts
	tk := h.load(id)
	if tk.Column != string(model.ColVerifying) {
		t.Fatalf("after the task: %s, last %+v", tk.Column, last(tk))
	}
	var job *struct{ prompt, model string }
	for _, j := range h.local.started {
		if j.Stage == model.StageVerify {
			job = &struct{ prompt, model string }{j.Prompt, j.Model}
			if j.Template != "verifier" || j.Name != strings.ToLower(string(id))+"-f1" {
				t.Errorf("template %q name %q", j.Template, j.Name)
			}
		}
	}
	if job == nil {
		t.Fatalf("no verifier started: %+v", h.local.started)
	}
	for _, want := range []string{"### 1. Raise a format error for nil", "### 2. Keep hex leading zeros", "untrusted data",
		"- T0001 · Triage · Thread safety of the coder", "- #27 · PR · decode_hex: max_length guard", "- #18 · issue · Cache the coder"} {
		if !strings.Contains(job.prompt, want) {
			t.Errorf("verifier prompt lacks %q:\n%s", want, job.prompt)
		}
	}
	if strings.Contains(job.prompt, string(closed)) || strings.Contains(job.prompt, string(id)+" ·") {
		t.Error("closed tickets and the hunt itself are not in the list")
	}
	if len(tracker.asked) != 1 || tracker.asked[0] != "acme/p" {
		t.Errorf("tracker asked %v", tracker.asked)
	}
	h.local.reports[strings.ToLower(string(id))+"-f1"] = []byte(`{"status": "done", "summary": "1 of 2 confirmed", "findings": [
		{"title": "Raise IdCodecFormatError for nil", "verdict": "confirmed", "severity": "low", "kind": "bug", "evidence": "seen", "body": "b"},
		{"title": "Keep hex leading zeros", "verdict": "duplicate", "duplicate_of": "PR #27", "evidence": "same", "body": "b"}]}`)
	tick()
	tk = h.load(id)
	if tk.Column != "needs_human" || tk.Ask.V.Kind != string(model.AskFindings) || len(tk.Findings) != 2 || tk.Agent.Present() {
		t.Errorf("after the verifier: %s %+v %+v", tk.Column, tk.Ask.V, tk.Findings)
	}
}

// A tracker that cannot be read leaves the verifier the board, and says so.
func TestTheVerifierIsToldWhenTheTrackerCannotBeRead(t *testing.T) {
	h := newHarness(t)
	h.config(strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\ngithub = \"acme/p\"\n", 1))
	d := h.deps()
	d.Issues = &fakeTracker{err: errors.New("github: GET: 503")}
	h.file("p", "Bug hunt", "verifying", func(t *ticket.Ticket) {
		t.Kind = ticket.Some("task")
		t.Result = ticket.Some("## Bugs\n\n1. nil")
	})
	if err := Tick(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(h.local.started) != 1 || !strings.Contains(h.local.started[0].Prompt, "could not be read (github: GET: 503)") ||
		!strings.Contains(h.local.started[0].Prompt, "## The report") {
		t.Errorf("started %+v", h.local.started)
	}
}
