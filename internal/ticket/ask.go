package ticket

import (
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// AskFor is the question a ticket in needs_human, ready_for_pr or pr_open
// puts to the maintainer, or nil anywhere else (ask_for, F:361). old is the
// column it left ("" for a new ticket), detail the move's detail, since the
// stamp the ask is dated. reason is the move's typed reason; older history
// has only the detail, read by its wording ("" reason).
func AskFor(t *Ticket, old, detail, since string, reason model.Reason) *Ask {
	column, kind := t.Column, t.KindName()
	a := func(k model.AskKind, question, why string, retry Opt[string]) *Ask {
		if !retry.Set {
			retry = Nil[string]()
		}
		return &Ask{Kind: string(k), Question: question, Why: textx.Sentence(why, 3), Since: since, RetryColumn: retry}
	}
	said := func(prefix string, reasons ...model.Reason) bool {
		if reason == "" {
			return strings.HasPrefix(detail, prefix)
		}
		for _, r := range reasons {
			if r == reason {
				return true
			}
		}
		return false
	}
	or := func(v jsonx.Value, fallback string) string {
		if jsonx.Truthy(v) {
			return jsonx.PyStr(v)
		}
		return fallback
	}
	none := Opt[string]{}
	if t.Board() {
		return BoardAsk(t, since)
	}
	switch model.Column(column) {
	case model.ColPROpen:
		q := "Review and merge the PR?"
		if pr, ok := t.PR.Get(); ok && pr.Number != 0 {
			q = "Review and merge PR #" + strconv.Itoa(pr.Number) + "?"
		}
		return a(model.AskReview, q, or(t.report("work", "summary"), detail), none)
	case model.ColReadyForPR:
		// through the gate, waiting to go out (open_prs = "manual"); a rework updates its PR by itself
		return a(model.AskOpen, "Open a PR for this?", or(t.report("work", "summary"), detail), none)
	case model.ColNeedsHuman:
	default:
		return nil
	}
	title := "“" + t.Title + "”"
	subject := title
	if src, ok := t.Source.Get(); ok && src.Issue != 0 {
		subject = "issue #" + strconv.FormatInt(src.Issue, 10) + ", " + title
	}
	if kind == string(model.KindProposal) {
		p, panel := t.Proposal.V, t.Panel.V
		scores := make([]string, len(panel.Scores))
		for i, s := range panel.Scores {
			scores[i] = strconv.Itoa(s)
		}
		shown := strings.Join(scores, " · ")
		if shown == "" {
			shown = "nothing"
		}
		mean := "?"
		if t.Panel.Present() {
			mean = jsonx.PyNumber(jsonx.Float(panel.Mean))
		}
		return a(model.AskProposal, "File "+title+" as a change, or drop it?",
			textx.Sentence(p.Why, 2)+" The panel scored it "+shown+", mean "+mean, none)
	}
	// a blocked designer may still leave a draft; that is not one to approve
	if kind == string(model.KindIntent) && t.Design.Or("") != "" && len(t.Proposed) > 0 && !said("designer blocked", model.ReasonDesignerBlocked) {
		return a(model.AskDesign, "Approve the design for "+title+" and its "+strconv.Itoa(len(t.Proposed))+" proposed tickets, or send it back?",
			or(t.report("design", "summary"), detail), none)
	}
	if kind == string(model.KindManage) && said("manager drafted", model.ReasonManagerDrafted) {
		n := len(t.Drafts)
		plural := "s"
		if n == 1 {
			plural = ""
		}
		return a(model.AskDraft, "Apply the manager's "+strconv.Itoa(n)+" config change"+plural+"?",
			or(t.report("manage", "summary"), detail), none)
	}
	if kind == string(model.KindManage) && said("manager answered", model.ReasonManagerAnswered) {
		return a(model.AskNotice, "Read the manager's answer and close it?", orString(t.Result.Or(""), detail), none)
	}
	if len(t.Findings) > 0 && said("findings verified", model.ReasonFindingsVerified) {
		return a(model.AskFindings, findingsQuestion(t), or(t.report(string(model.StageVerify), "summary"), detail), none)
	}
	if q, ok := t.Question.Get(); ok && q.Answered == "" && said(q.Stage+" asks", model.ReasonAgentQuestion) {
		return a(model.AskQuestion, q.Text, or(t.report(q.Stage, "summary"), q.Text), none)
	}
	if d, ok := t.Deploy.Get(); ok && t.Project == model.NoticeProject {
		return a(model.AskDeploy, "Deploy main (now "+d.Rev+") to the factory?", strings.Join(d.Why, "; "), none)
	}
	if (kind == string(model.KindTask) && (said("task ", model.ReasonTaskDone, model.ReasonTaskBlocked) || old == "")) ||
		(t.Project == model.NoticeProject && !t.Kind.Present()) {
		if kind == string(model.KindTask) {
			why := t.Result.Or("")
			if why == "" {
				why = or(t.report("task", "summary"), t.Body)
			}
			return a(model.AskNotice, "Read the task's result and close it?", why, none)
		}
		return a(model.AskNotice, "Read this notice and close it when it is handled?", t.Body, none)
	}
	triage := or(t.report("triage", "reason"), detail)
	if said("triage accepted", model.ReasonTriageAccepted) {
		return a(model.AskStart, "Start work on "+subject+"?", triage, none)
	}
	if said("triage unsure", model.ReasonTriageUnsure) {
		return a(model.AskDecide, "Triage could not decide on "+subject+": start it, or drop it?", triage, none)
	}
	step, isStep := model.StepName(old)
	retry := none
	if isStep {
		retry = Some(model.RetryColumnFor(old))
	}
	stage := old
	switch old {
	case string(model.ColReady):
		stage = string(model.StageWork)
	case string(model.ColVerifying):
		stage = string(model.StageVerify)
	case string(model.ColWorking):
		switch kind {
		case string(model.KindTask):
			stage = string(model.StageTask)
		case string(model.KindIntent):
			stage = string(model.StageDesign)
		default:
			stage = string(model.StageWork)
		}
	}
	var summary jsonx.Value
	if r, ok := t.Reports.Lookup(stage); ok && r.Kind() == jsonx.KindObject {
		summary = r.Get("summary")
	}
	if detail == "" || detail == "by human" {
		detail = "Moved here by hand"
	}
	// the detail often quotes the report already
	why := textx.Sentence(detail, 3)
	if jsonx.Truthy(summary) {
		quoted := []rune(textx.Fold(jsonx.PyStr(summary)))
		if len(quoted) > 80 {
			quoted = quoted[:80]
		}
		if !strings.Contains(textx.Fold(detail), string(quoted)) {
			why += " " + textx.Sentence(jsonx.PyStr(summary), 3)
		}
	}
	if isStep {
		return a(model.AskUnstick, "This stopped at the "+step+" step; retry it, or close it?", why, retry)
	}
	return a(model.AskUnstick, "This needs you: what should happen next?", why, retry)
}

func orString(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// report is reports[stage][key], or null when the report is missing or not
// an object.
func (t *Ticket) report(stage, key string) jsonx.Value {
	r, ok := t.Reports.Lookup(stage)
	if !ok {
		return jsonx.Null()
	}
	return r.Get(key)
}

// BoardAsk is what a board ticket asks a person: only a question put to a
// person, whose options become the answers (board_ask).
func BoardAsk(t *Ticket, since string) *Ask {
	if t.Column == string(model.BoardDone) || t.Column == string(model.BoardDropped) {
		return nil
	}
	for _, q := range t.Questions {
		if !q.Open() || !q.ToPerson {
			continue
		}
		at := q.At
		if at == "" {
			at = since
		}
		options := append([]string{}, q.Options...)
		return &Ask{Kind: string(model.AskQuestion), Question: q.Text, Why: textx.Sentence(q.By+" asks "+q.Recipient, 3),
			Since: at, RetryColumn: Nil[string](), Options: Some(options), N: Some(q.N)}
	}
	return nil
}

// SetAsk stores a (nil for none: "ask": null).
func (t *Ticket) SetAsk(a *Ask) {
	if a == nil {
		t.Ask = Nil[Ask]()
		return
	}
	t.Ask = Some(*a)
}
