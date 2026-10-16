package main

import (
	"fmt"
	"sort"
	"strings"
)

type kindInfo struct {
	Label, Glyph string
}

// Desk order: an agent waiting mid-run first, then what is stuck, then what is ready to land.
var askOrder = []string{"question", "unstick", "review", "open", "start", "design", "proposal", "decide", "notice"}

var askKinds = map[string]kindInfo{
	"question": {"Question", "?"},
	"unstick":  {"Unstick", "!"},
	"review":   {"Review", "PR"},
	"open":     {"Open PR", "PR"},
	"start":    {"Start", "GO"},
	"design":   {"Design", "DS"},
	"proposal": {"Proposal", "PP"},
	"decide":   {"Decide", "··"},
	"notice":   {"Notice", "NB"},
}

func askRank(kind string) int {
	for i, k := range askOrder {
		if k == kind {
			return i
		}
	}
	return len(askOrder)
}

const noticeProject = "factory"

// AskFor is the ticket's ask: the dispatcher's when it wrote one, else derived the same way from
// the column and the move that brought the ticket there (boards from before asks existed).
func AskFor(t *Ticket) *Ask {
	if t.Column != "needs_human" && t.Column != "pr_open" && t.Column != "ready_for_pr" {
		return nil
	}
	if t.Ask != nil && t.Ask.Kind != "" {
		return t.Ask
	}
	return deriveAsk(t)
}

// lastArrival is the move into the ticket's current column: old column, detail, when.
func lastArrival(t *Ticket) (old, detail, at string) {
	for i := len(t.History) - 1; i >= 0; i-- {
		h := t.History[i]
		if h.Event == "created" && strings.Contains(h.Detail, "column="+t.Column) {
			return "", "", h.At
		}
		if h.Event != "move" {
			continue
		}
		head, rest, _ := strings.Cut(h.Detail, ": ")
		from, to, ok := strings.Cut(head, " -> ")
		if ok && to == t.Column {
			return from, rest, h.At
		}
	}
	return "", "", t.Updated
}

func deriveAsk(t *Ticket) *Ask {
	old, detail, since := lastArrival(t)
	a := &Ask{Since: since}
	title := quoted(t.Title)
	switch {
	case t.Column == "pr_open":
		a.Kind = "review"
		n := 0
		if t.PR != nil {
			n = t.PR.Number
		}
		a.Question = fmt.Sprintf("Review and merge PR #%d?", n)
		a.Why = t.report("work").str("summary")
	case t.Column == "ready_for_pr":
		a.Kind = "open"
		a.Question = "Open a PR for this?"
		a.Why = firstNonEmpty(t.report("work").str("summary"), detail)
	case t.Kind == "proposal":
		a.Kind = "proposal"
		a.Question = "File this as a change: " + t.Title + "?"
		if t.Proposal != nil {
			a.Why = t.Proposal.Why
		}
	case t.Kind == "task" || (t.Project == noticeProject && t.Kind != "intent"):
		a.Kind = "notice"
		if t.Kind == "task" {
			a.Question = "Read the answer to " + title + " and close it?"
			a.Why = firstNonEmpty(t.Result, t.report("task").str("summary"), detail)
		} else {
			a.Question = "Seen this notice: " + t.Title + "?"
			a.Why = t.Body
		}
	case t.Kind == "intent" && t.Design != "" && len(t.Proposed) > 0:
		a.Kind = "design"
		a.Question = fmt.Sprintf("Approve the design for %s and its %s?", title, plural(len(t.Proposed), "ticket"))
		a.Why = firstNonEmpty(t.report("design").str("summary"), detail)
	case strings.HasPrefix(detail, "triage accepted"):
		a.Kind = "start"
		a.Question = "Start work on " + subject(t) + "?"
		a.Why = t.report("triage").str("reason")
	case strings.HasPrefix(detail, "triage unsure"):
		a.Kind = "decide"
		a.Question = "Triage could not decide on " + subject(t) + ". Start it, or drop it?"
		a.Why = firstNonEmpty(t.report("triage").str("reason"), strings.TrimPrefix(detail, "triage unsure: "))
	default:
		a.Kind = "unstick"
		a.RetryColumn = retryColumn(old)
		a.Question = unstickQuestion(detail)
		a.Why = unstickWhy(t, old, detail)
	}
	return a
}

func subject(t *Ticket) string {
	if t.Source != nil && t.Source.Issue > 0 {
		return fmt.Sprintf("issue #%d, %s", t.Source.Issue, quoted(t.Title))
	}
	return quoted(t.Title)
}

func quoted(s string) string { return "“" + s + "”" }

// retryColumn: where a retry starts. A running stage retries from the column it waits in.
func retryColumn(old string) string {
	switch old {
	case "working":
		return "ready"
	case "", "needs_human", "done", "rejected", "pr_open":
		return ""
	}
	return old
}

// Headline: the ask's question short enough to read at a glance; the ticket's title goes under it.
// The full question stays in the ask (the CLI and older pages use it).
func Headline(t *Ticket, a *Ask) string {
	issue := ""
	if t.Source != nil && t.Source.Issue > 0 {
		issue = fmt.Sprintf(" issue #%d", t.Source.Issue)
	}
	switch a.Kind {
	case "review":
		if mergeable(t) {
			return fmt.Sprintf("Merge PR #%d?", t.PR.Number)
		}
	case "design":
		return fmt.Sprintf("Approve this design and its %s?", plural(len(t.Proposed), "ticket"))
	case "proposal":
		return "File this proposal as a change?"
	case "start":
		if issue != "" {
			return "Start work on" + issue + "?"
		}
		return "Start work on this?"
	case "decide":
		if issue != "" {
			return "Start" + issue + ", or drop it?"
		}
		return "Start it, or drop it?"
	case "notice":
		if t.Kind == "task" {
			return "Read the answer and close it?"
		}
		return "Seen this notice?"
	}
	return a.Question
}

func unstickQuestion(detail string) string {
	switch {
	case strings.HasPrefix(detail, "gate:"):
		return "Tests or lint failed at the gate. Run the gate again?"
	case strings.Contains(detail, "timed out"):
		return "An agent ran out of time. Try again?"
	case strings.Contains(detail, "disappeared") || strings.Contains(detail, "without a report"):
		return "An agent stopped without a report. Try again?"
	case strings.HasPrefix(detail, "security:"):
		return "The security review raised a concern. Retry, or drop it?"
	case strings.HasPrefix(detail, "review still asks"):
		return "Review still wants changes after the last attempt. Retry with a note, or drop it?"
	case strings.Contains(detail, "changed the branch"):
		return "A reviewing agent changed the branch, which it must not. Run that check again?"
	case strings.HasPrefix(detail, "worker blocked"), strings.Contains(detail, "no commits"):
		return "The worker could not finish. Retry with a note, or drop it?"
	case strings.HasPrefix(detail, "designer blocked"):
		return "The designer could not finish. Retry with a note, or drop it?"
	case strings.HasPrefix(detail, "dispatcher error"):
		return "The dispatcher hit an error on this ticket. Retry?"
	}
	return "This ticket is stuck. Retry, or drop it?"
}

var stageOfColumn = map[string]string{"working": "work", "review": "review", "security": "security", "triage": "triage", "voting": "vote"}

func unstickWhy(t *Ticket, old, detail string) string {
	parts := []string{}
	if detail != "" && detail != "by human" {
		parts = append(parts, sentence(detail))
	}
	if strings.HasPrefix(detail, "gate:") && t.Gate != nil && len(t.Gate.Blockers) > 0 {
		parts = append(parts, sentence("Gate blockers: "+strings.Join(t.Gate.Blockers, "; ")))
	}
	stage := stageOfColumn[old]
	if t.Kind == "intent" && old == "working" {
		stage = "design"
	}
	if s := t.report(stage).str("summary"); s != "" {
		parts = append(parts, sentence(clip(s, 400)))
	}
	if detail == "dispatcher error" {
		for i := len(t.History) - 1; i >= 0; i-- {
			if t.History[i].Event == "error" {
				parts = append(parts, sentence("Error: "+clip(t.History[i].Detail, 300)))
				break
			}
		}
	}
	return strings.Join(parts, " ")
}

func sentence(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), " ,;:")
	if s == "" {
		return s
	}
	s = capFirst(s)
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}

func capFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

// Action: what the ask wants, in a few words, for the desk list (the title says what it is about).
func Action(t *Ticket, a *Ask) string {
	pr := 0
	if t.PR != nil {
		pr = t.PR.Number
	}
	issue := ""
	if t.Source != nil && t.Source.Issue > 0 {
		issue = fmt.Sprintf(" · issue #%d", t.Source.Issue)
	}
	switch a.Kind {
	case "review":
		if mergeable(t) {
			return fmt.Sprintf("Merge PR #%d?", pr)
		}
		return fmt.Sprintf("Review PR #%d", pr)
	case "open":
		if pr > 0 {
			return fmt.Sprintf("Update PR #%d?", pr)
		}
		return "Open a PR?"
	case "design":
		return "Approve design · " + plural(len(t.Proposed), "ticket")
	case "proposal":
		return "File as a change?"
	case "start":
		return "Start work?" + issue
	case "decide":
		return "Start or drop?" + issue
	case "notice":
		if t.Kind == "task" {
			return "Read the answer"
		}
		return "Read the notice"
	case "question":
		return "Answer the agent"
	case "unstick":
		return "Stuck: retry or drop?"
	}
	return kindLabel(a.Kind)
}

type DeskItem struct {
	T   *Ticket
	Ask *Ask
	// Responded: the last thing on the ticket is the maintainer's own "note" or "asked", made after
	// the ask arrived. The item waits lower down until the factory does anything new with it.
	Responded, RespondedAt string
}

// responded: the factory's own notes (a GitHub review, a rebase request) come with a move off the
// desk, so a note or a question that is the ticket's last event is the maintainer's.
func responded(t *Ticket, a *Ask) (string, string) {
	if len(t.History) == 0 {
		return "", ""
	}
	h := t.History[len(t.History)-1]
	if (h.Event != "note" && h.Event != "asked") || parseTime(h.At).Before(parseTime(a.Since)) {
		return "", ""
	}
	return h.Event, h.At
}

// Waiting counts the asks nobody has responded to yet: the desk's number, the badge's and the floor's.
func Waiting(items []DeskItem) int {
	n := 0
	for _, it := range items {
		if it.Responded == "" {
			n++
		}
	}
	return n
}

// Desk lists the tickets with an ask: the ones still waiting for a response first, then by kind in
// desk order, then oldest first.
func Desk(ts []*Ticket) []DeskItem {
	var out []DeskItem
	for _, t := range ts {
		if a := AskFor(t); a != nil {
			r, at := responded(t, a)
			out = append(out, DeskItem{t, a, r, at})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Responded == "") != (out[j].Responded == "") {
			return out[i].Responded == ""
		}
		ri, rj := askRank(out[i].Ask.Kind), askRank(out[j].Ask.Kind)
		if ri != rj {
			return ri < rj
		}
		si, sj := parseTime(out[i].Ask.Since), parseTime(out[j].Ask.Since)
		if !si.Equal(sj) {
			return si.Before(sj)
		}
		return idLess(out[i].T.ID, out[j].T.ID)
	})
	return out
}
