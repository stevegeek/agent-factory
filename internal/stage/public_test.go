package stage

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const hygieneTOML = "[projects.gem]\nhygiene = true\n[projects.widgets]\n"

func reading(t *testing.T, extra string) *ticket.Ticket {
	if extra != "" {
		extra = ", " + extra
	}
	return running(t, "T0001", "public", "public", `"head_before_public": "`+head+`"`+extra)
}

// moved is a typed move entry, as history JSON.
func moved(from, to string, r model.Reason) string {
	return `{"at": "x", "event": "move", "detail": "` + from + ` -> ` + to + `", "type": "moved", "from": "` + from + `", "to": "` + to +
		`", "reason": "` + string(r) + `", "by": "factory"}`
}

func TestSecurityPassGoesToPublicReviewOnAHygieneProject(t *testing.T) {
	pass := `{"verdict": "pass", "answers": [{"answer": "fine", "concern": "none"}]}`
	for _, c := range []struct {
		toml, want string
	}{{hygieneTOML, "public"}, {"[projects.gem]\n", "gate"}, {"", "gate"}} {
		tk := securing(t, `"security_tier": 0, "security_questions": ["only one"]`)
		out := finish(t, FinishIn{Ticket: tk, Report: report(pass), Facts: Facts{Head: head}, Config: cfg(t, c.toml)})
		if out.Ticket.Column != c.want || reason(out.Ticket) != model.ReasonSecurityPassed {
			t.Errorf("%q: %s %s", c.toml, out.Ticket.Column, reason(out.Ticket))
		}
	}
}

func TestPublicPassGoesToTheGateWithTheHeadItRead(t *testing.T) {
	out := finish(t, FinishIn{Ticket: reading(t, ""), Report: report(`{"verdict": "pass", "findings": [], "summary": "ok"}`),
		Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML)})
	if out.Ticket.Column != "gate" || reason(out.Ticket) != model.ReasonPublicPassed || out.Ticket.PublicHead.Or("") != head {
		t.Errorf("%s %s %v", out.Ticket.Column, reason(out.Ticket), out.Ticket.PublicHead)
	}
	// a pass that still lists a finding is a block
	out = finish(t, FinishIn{Ticket: reading(t, ""), Report: report(`{"verdict": "pass", "findings": [{"where": "commit abc message", "why": "x"}]}`),
		Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML)})
	if out.Ticket.Column != "ready" || out.Ticket.PublicHead.Set {
		t.Errorf("a pass with findings: %s %v", out.Ticket.Column, out.Ticket.PublicHead)
	}
}

func TestPublicAgentThatCommitsIsDiscarded(t *testing.T) {
	out := finish(t, FinishIn{Ticket: reading(t, ""), Report: report(`{"verdict": "pass"}`), Facts: Facts{Head: "f00"}, Config: cfg(t, hygieneTOML)})
	if out.Ticket.Column != "needs_human" || reason(out.Ticket) != model.ReasonReviewerCommitted || out.Ticket.PublicHead.Set {
		t.Errorf("%s %s %v", out.Ticket.Column, reason(out.Ticket), out.Ticket.PublicHead)
	}
}

func TestPublicBlockGoesBackToWorkOnceThenToAPerson(t *testing.T) {
	block := `{"verdict": "block", "findings": [{"where": "commit abc1234 lib/gem.rb:3", "why": "names another project"}]}`
	out := finish(t, FinishIn{Ticket: reading(t, ""), Report: report(block), Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML)})
	tk := out.Ticket
	if tk.Column != "ready" || reason(tk) != model.ReasonPublicChanges || tk.PublicHead.Set {
		t.Fatalf("first block: %s %s", tk.Column, reason(tk))
	}
	// the worker is told where and why, and to rewrite its commits
	p := workPrompt(PromptIn{Ticket: tk})
	for _, want := range []string{"commit abc1234 lib/gem.rb:3: names another project", "amend or squash", "Do not describe the removal"} {
		if !strings.Contains(p, want) {
			t.Errorf("work prompt lacks %q:\n%s", want, p)
		}
	}
	// a passed review on the way back does not reset the count
	tk = reading(t, `"history": [`+moved("public", "ready", model.ReasonPublicChanges)+`, `+moved("review", "security", model.ReasonReviewPassed)+`]`)
	out = finish(t, FinishIn{Ticket: tk, Report: report(block), Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML)})
	if out.Ticket.Column != "needs_human" || reason(out.Ticket) != model.ReasonPublicBlocked {
		t.Errorf("second block: %s %s", out.Ticket.Column, reason(out.Ticket))
	}
	// an opened PR starts the count again: a rework a GitHub review asked for
	tk = reading(t, `"history": [`+moved("public", "ready", model.ReasonPublicChanges)+`, `+moved("ready_for_pr", "pr_open", model.ReasonPROpened)+`]`)
	out = finish(t, FinishIn{Ticket: tk, Report: report(block), Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML)})
	if out.Ticket.Column != "ready" {
		t.Errorf("after a PR opened: %s", out.Ticket.Column)
	}
}

// shown are the texts a public review was shown: the fixed ones, and the
// summary and a title made from it, which the worker can change.
func shown(titleFixed bool) *PublicIn {
	return &PublicIn{Texts: []PublicText{{"branch", "bot/x", true}, {"pr-title", "T", titleFixed}, {"ticket", "Fixes #7", true},
		{"summary", "Did it.", false}, {"footer", "By a bot.", true}}}
}

func TestPublicFindingInFixedTextGoesStraightToAPerson(t *testing.T) {
	for _, where := range []string{"branch", "pr-title", "ticket", "footer", "PR-Title: the second word"} {
		r := `{"verdict": "block", "findings": [{"where": "commit abc lib/x.rb:1", "why": "a"}, {"where": "` + where + `", "why": "b"}]}`
		out := finish(t, FinishIn{Ticket: reading(t, ""), Report: report(r), Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML), Public: shown(true)})
		if out.Ticket.Column != "needs_human" || reason(out.Ticket) != model.ReasonPublicBlocked {
			t.Errorf("%s: %s %s", where, out.Ticket.Column, reason(out.Ticket))
		}
	}
	// the summary is the worker's to change, and so is a title made from it
	for _, where := range []string{"summary", "pr-title"} {
		r := `{"verdict": "block", "findings": [{"where": "` + where + `", "why": "b"}]}`
		out := finish(t, FinishIn{Ticket: reading(t, ""), Report: report(r), Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML), Public: shown(false)})
		if out.Ticket.Column != "ready" || reason(out.Ticket) != model.ReasonPublicChanges {
			t.Errorf("%s: %s %s", where, out.Ticket.Column, reason(out.Ticket))
		}
	}
}

// A finding whose where is neither a commit nor a text the review was
// shown cannot be routed: the report is one that cannot be read, read
// again, and never a rework the worker cannot act on.
func TestPublicFindingInAPlaceItWasNotShownCannotBeRead(t *testing.T) {
	for _, where := range []string{"pr title", "the branch name", "PR title of the change", "lib/x.rb:1", "lint"} {
		r := `{"verdict": "block", "findings": [{"where": "` + where + `", "why": "b"}]}`
		out := finish(t, FinishIn{Ticket: reading(t, ""), Report: report(r), Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML), Public: shown(true)})
		if out.Ticket.Column != "public" || ticket.PublicRounds(out.Ticket) != 0 || last(out.Ticket).Event != "error" {
			t.Errorf("%s: %s %+v", where, out.Ticket.Column, last(out.Ticket))
		}
	}
}

func TestPublicMalformedReportReadsAgainThenGoesToAPerson(t *testing.T) {
	start := `{"at": "x", "event": "start", "detail": "public t0001-o1", "type": "started", "stage": "public", "agent": "t0001-o1", "host": "local"}`
	for _, bad := range []string{`not json`, `{"verdict": "maybe"}`, `{"verdict": "block"}`, `{"verdict": "block", "findings": "lots"}`,
		`{"verdict": "block", "findings": [{"why": "no where"}]}`, `{"verdict": "pass", "findings": ["x"]}`} {
		tk := reading(t, `"history": [`+moved("security", "public", model.ReasonSecurityPassed)+`, `+start+`]`)
		out := finish(t, FinishIn{Ticket: tk, Report: report(bad), Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML)})
		if out.Ticket.Column != "public" || reason(out.Ticket) != model.ReasonSecurityPassed || out.Ticket.PublicHead.Set {
			t.Errorf("%s, first: %s %s", bad, out.Ticket.Column, reason(out.Ticket))
		}
		tk = reading(t, `"history": [`+moved("security", "public", model.ReasonSecurityPassed)+`, `+start+`, `+start+`]`)
		out = finish(t, FinishIn{Ticket: tk, Report: report(bad), Facts: Facts{Head: head}, Config: cfg(t, hygieneTOML)})
		if out.Ticket.Column != "needs_human" || reason(out.Ticket) != model.ReasonNoReport || out.Ticket.PublicHead.Set {
			t.Errorf("%s, second: %s %s", bad, out.Ticket.Column, reason(out.Ticket))
		}
		// a person may have it read again: no findings to send back, and
		// public review's one rework is still to come
		a, _ := out.Ticket.Ask.Get()
		if a.Kind != "unstick" || a.RetryColumn.Or("") != "public" || ticket.PublicBlocked(out.Ticket) || ticket.PublicRounds(out.Ticket) != 0 {
			t.Errorf("%s, ask %+v", bad, a)
		}
	}
}

func TestPublicPromptReadsEveryCommitTheTextsAndThePrivateValues(t *testing.T) {
	tk := reading(t, `"public_head": "an older pass"`)
	Begin(tk, model.StagePublic, BeginIn{Head: head, Texts: "digest"})
	in := PromptIn{Ticket: tk, Project: gemProject(t), Public: &PublicIn{
		Texts:   []PublicText{{"branch", "bot/fix-it", true}, {"summary", "Fixed it.\n```\nnot a fence\n```", false}, {"footer", "By a bot.", true}},
		Private: []string{"widgets", "hosty"},
	}}
	got := For(model.StagePublic).Prompt(in)
	for _, want := range []string{"against `main`, at " + head, "git log -p --text --diff-merges=remerge", "main..HEAD", "published already", "### branch\n```\nbot/fix-it\n```",
		"### summary\n````\nFixed it.", "### footer", "- widgets\n- hosty", "this line is not published", "never a finding:\nTask: t"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	// a new read starts with no pass: a pass belongs to the read that made it
	if tk.HeadBeforePublic.Or("") != head || tk.TextsBeforePublic.Or("") != "digest" || tk.PublicHead.Or("") != "" {
		t.Errorf("begin: %v %v %v", tk.HeadBeforePublic, tk.TextsBeforePublic, tk.PublicHead)
	}
	// with no private values the rules still stand
	in.Public.Private = nil
	if got := For(model.StagePublic).Prompt(in); !strings.Contains(got, "every rule in your instructions still applies") || strings.Contains(got, "Private values:") {
		t.Errorf("prompt with no private values:\n%s", got)
	}
}

// The worker is told public review's findings only right after it sent the
// change back: a later rework (a review's changes) does not repeat them.
func TestWorkPromptCarriesPublicFindingsOnlyAfterPublicChanges(t *testing.T) {
	rep := `"reports": {"public": {"verdict": "block", "findings": [{"where": "summary", "why": "names a host"}]}}`
	for _, c := range []struct {
		r    model.Reason
		want bool
	}{{model.ReasonPublicChanges, true}, {model.ReasonReviewChanges, false}, {model.ReasonByHuman, false}} {
		tk := running(t, "T0001", "work", "ready", rep+`, "history": [`+moved("x", "ready", c.r)+`]`)
		if got := strings.Contains(workPrompt(PromptIn{Ticket: tk}), "names a host"); got != c.want {
			t.Errorf("%s: findings shown %v", c.r, got)
		}
	}
}
