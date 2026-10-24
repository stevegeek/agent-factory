package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var testNowCLI = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

// hunt files a verified bug hunt on the golden root's gem: two confirmed
// findings (the second changes behaviour) and a duplicate.
func hunt(t *testing.T, f *fixture) ticket.ID {
	t.Helper()
	tk := file(t, f, "gem", "ready", func(tk *ticket.Ticket) {
		tk.Title = "Bug hunt"
		tk.Kind = ticket.Some("task")
		tk.Findings = []ticket.Finding{
			{Title: "Raise a format error for nil", Severity: "low", Kind: "bug", Verdict: ticket.VerdictConfirmed, Body: "b", Evidence: "seen"},
			{Title: "Keep hex leading zeros", Severity: "medium", Kind: "bug", BehaviourChange: true, Verdict: ticket.VerdictConfirmed, Body: "b"},
			{Title: "Make decode_hex errors consistent", Verdict: ticket.VerdictDuplicate, Duplicate: "PR #27", Body: "b"},
		}
		ticket.Move(tk, "needs_human", model.ReasonFindingsVerified, "findings verified: 2 of 3 confirmed", "", "", testNowCLI)
	})
	return tk.ID
}

func TestFindingsListsThenFilesWithKinds(t *testing.T) {
	f := goldenRoot(t)
	id := hunt(t, f)
	code, out, errs := f.run("", "findings", string(id))
	if code != 0 || !strings.Contains(out, "1. Raise a format error for nil") || !strings.Contains(out, "duplicate of PR #27") ||
		!strings.Contains(out, "factory findings "+string(id)+" --file 1,2") {
		t.Fatalf("list: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	code, out, errs = f.run("", "findings", string(id), "--file", "1,2", "--as", "2=change")
	if code != 0 || out != "T0021 T0022\n" {
		t.Fatalf("file: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if a, b := load(t, f, "T0021"), load(t, f, "T0022"); a.Column != "triage" || b.Column != "triage" || b.KindName() != "" ||
		a.Door != "finding" || a.Parent.Or("") != string(id) {
		t.Errorf("filed %s/%s %s/%s", a.Column, a.KindName(), b.Column, b.KindName())
	}
	if h := load(t, f, id); h.Column != "done" || strings.Join(h.Children, ",") != "T0021,T0022" {
		t.Errorf("hunt %s %v", h.Column, h.Children)
	}
	// again: refused, nothing filed
	if code, out, errs := f.run("", "findings", string(id), "--file", "1"); code != 1 || out != "" || !strings.Contains(errs, "no findings waiting") {
		t.Errorf("again: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

func TestFindingsFlagsAndRefusals(t *testing.T) {
	f := goldenRoot(t)
	id := hunt(t, f)
	for argv, want := range map[string]string{
		"--file 3":           "finding 3 was not confirmed (duplicate of PR #27); it is not filed",
		"--file 1 --as 1=x":  `finding 1: file it as a change or an intent, not "x"`,
		"--file 1 --as one":  "--as takes N=change or N=intent, comma-separated: one",
		"--file x":           "invalid literal for int() with base 10: 'x'",
		"--file 1 --none":    "--file, --none and --verify are different answers; pick one",
		"--as 1=intent":      "--as goes with --file",
		"--file all --none":  "--file, --none and --verify are different answers; pick one",
		"--verify --file 1":  "--file, --none and --verify are different answers; pick one",
		"--verify":           "verified already",
		"--file 1,2 --as 3=": "--as takes N=change or N=intent, comma-separated: 3=",
	} {
		code, out, errs := f.run("", append([]string{"findings", string(id)}, strings.Fields(argv)...)...)
		if code != 1 || out != "" || !strings.Contains(errs, want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", argv, code, out, errs)
		}
	}
	// --file all files every open confirmed finding by its default kind
	code, out, errs := f.run("", "findings", string(id), "--file", "all")
	if code != 0 || out != "T0021 T0022\n" || load(t, f, "T0022").KindName() != "intent" {
		t.Fatalf("all: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

func TestFindingsNoneAndVerify(t *testing.T) {
	f := goldenRoot(t)
	id := hunt(t, f)
	if code, out, errs := f.run("", "findings", string(id), "--none"); code != 0 || out != "" {
		t.Fatalf("none: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if h := load(t, f, id); h.Column != "done" || len(h.Children) != 0 {
		t.Errorf("none: %s %v", h.Column, h.Children)
	}
	// a task whose result is prose only (T0030) goes to the verifier
	task := file(t, f, "gem", "needs_human", func(tk *ticket.Ticket) {
		tk.Kind = ticket.Some("task")
		tk.Result = ticket.Some("## Confirmed bugs\n\n### 1. Hex loses leading zeros")
	})
	if code, out, errs := f.run("", "findings", string(task.ID), "--verify"); code != 0 || out != "" {
		t.Fatalf("verify: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if tk := load(t, f, task.ID); tk.Column != "verifying" {
		t.Errorf("verify: %s", tk.Column)
	}
	if code, out, _ := f.run("", "findings", string(task.ID)); code != 0 || !strings.Contains(out, "with the verifier") {
		t.Errorf("while verifying: %q", out)
	}
}

func TestAddFindingsAsksATaskForThem(t *testing.T) {
	f := goldenRoot(t)
	code, out, errs := f.run("", "add", "gem", "Hunt", "--task", "--findings")
	if code != 0 || out != "T0020\n" || !load(t, f, "T0020").FindingsAsked.Or(false) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}
