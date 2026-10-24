package stage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func managing(t *testing.T) *ticket.Ticket {
	tk := running(t, "T0001", "manage", "working", `"host": "local"`)
	tk.Project, tk.Kind, tk.Branch, tk.Title, tk.Body = "factory", ticket.Some("manage"), ticket.Nil[string](), "A morning check", "Every weekday"
	return tk
}

func TestDraftsWaitForYouAndTheNextRunSeesThem(t *testing.T) {
	// the finish and the prompt:
	// port: TF ManagerTest.test_drafts_wait_for_you_and_a_reply_carries_on
	patch := "--- a/config/config.toml\n+++ b/config/config.toml\n@@ -1 +1,2 @@\n+[[schedules]]\n"
	r := report(fmt.Sprintf(`{"status": "done", "summary": "Drafted a weekday check.", "result": "## Plan\nA schedule.",
		"drafts": [{"title": "Weekday check", "why": "Files it at 05:30.", "patch": %q}, {"title": "empty"}, "junk"]}`, patch))
	out := finish(t, FinishIn{Ticket: managing(t), Report: r})
	got := out.Ticket
	if len(got.Drafts) != 1 || got.Drafts[0] != (ticket.Draft{Title: "Weekday check", Why: "Files it at 05:30.", Patch: patch}) {
		t.Fatalf("drafts %+v", got.Drafts)
	}
	a, _ := got.Ask.Get()
	if got.Column != "needs_human" || a.Kind != "draft" || !strings.Contains(a.Question, "1 config change") || reason(got) != model.ReasonManagerDrafted ||
		last(got).Detail != "working -> needs_human: manager drafted 1 change(s): Drafted a weekday check." {
		t.Errorf("%s %+v %q", got.Column, a, last(got).Detail)
	}
	if got.Result.Or("") != "## Plan\nA schedule." {
		t.Errorf("result %v", got.Result)
	}
	ticket.AddNote(got, "only weekdays", "maintainer", nil, now)
	prompt := For(model.StageManage).Prompt(PromptIn{Ticket: got, Project: gemProject(t)})
	for _, want := range []string{"Your previous answer", "Weekday check", "only weekdays", "what you may read is under /factory"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "## This project") {
		t.Error("the manager is told no project facts")
	}
}

func TestAnAnswerWithoutDraftsIsANotice(t *testing.T) {
	// the finish:
	// port: TF ManagerTest.test_an_answer_without_drafts_is_a_notice
	out := finish(t, FinishIn{Ticket: managing(t), Report: report(`{"status": "done", "summary": "T0042 failed on lint.", "result": "Lint."}`)})
	if a, _ := out.Ticket.Ask.Get(); a.Kind != "notice" || reason(out.Ticket) != model.ReasonManagerAnswered {
		t.Errorf("ask %+v", a)
	}
	if out.Ticket.Drafts == nil || len(out.Ticket.Drafts) != 0 {
		t.Errorf("drafts are written as []: %#v", out.Ticket.Drafts)
	}
	out = finish(t, FinishIn{Ticket: managing(t), Report: report(`{"status": "blocked", "summary": "cannot read"}`)})
	if reason(out.Ticket) != model.ReasonManagerBlocked || out.Ticket.Result.Or("") != "cannot read" {
		t.Errorf("%s %v", reason(out.Ticket), out.Ticket.Result)
	}
}

func TestDraftCaps(t *testing.T) {
	var ds []string
	for i := range 7 {
		ds = append(ds, fmt.Sprintf(`{"title": "%s %d", "why": "%s", "patch": "%s"}`, strings.Repeat("t", 130), i,
			strings.Repeat("w ", 1100), strings.Repeat("p", 50_100)))
	}
	ds = append([]string{`{"patch": "  "}`, `{"patch": 5, "title": null}`}, ds...)
	out := finish(t, FinishIn{Ticket: managing(t), Report: report(`{"status": "done", "drafts": [` + strings.Join(ds, ",") + `]}`)})
	d := out.Ticket.Drafts
	if len(d) != 5 || d[0] != (ticket.Draft{Title: "Untitled", Why: "", Patch: "5"}) {
		t.Fatalf("drafts %d %+v", len(d), d[0])
	}
	if n := len([]rune(d[1].Title)); n != 120 || len([]rune(d[1].Why)) != 2000 || len(d[1].Patch) != 50_000 {
		t.Errorf("caps %d %d %d", n, len([]rune(d[1].Why)), len(d[1].Patch))
	}
	if !strings.HasSuffix(last(out.Ticket).Detail, "manager drafted 5 change(s): ") {
		t.Errorf("%q", last(out.Ticket).Detail)
	}
}
