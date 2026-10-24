package present

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Fact is one line of evidence beside an ask.
type Fact struct {
	Label, Value string
	Tone         string // good, warn, bad
	URL          string // only ever an http(s) link (SafeURL)
	Votes        []int
}

// Facts are what a person reads before answering the ask a (nil for a
// ticket that asks nothing): the panel's scores, the design's size, the
// issue, triage's verdict, the PR, the gate, the review and the security
// check. all holds every ticket, for a proposal's prospect.
func Facts(t *ticket.Ticket, a *ticket.Ask, all map[ticket.ID]*ticket.Ticket) []Fact {
	return new(reader).facts(t, a, all)
}

func (r *reader) facts(t *ticket.Ticket, a *ticket.Ask, all map[ticket.ID]*ticket.Ticket) []Fact {
	if t.Board() {
		return boardFacts(t)
	}
	var f []Fact
	kind := ""
	if a != nil {
		kind = a.Kind
	}
	tkind := t.KindName()
	switch {
	case kind == string(model.AskProposal) || tkind == string(model.KindProposal):
		var scores []int
		for _, v := range panelVotes(t, all) {
			scores = append(scores, v.Score)
		}
		if p, ok := t.Panel.Get(); ok && len(scores) == 0 {
			scores = p.Scores
		}
		if len(scores) > 0 {
			f = append(f, Fact{Label: "Panel scores", Votes: scores})
			f = append(f, Fact{Label: "Panel", Value: PanelRule(scores)})
		}
		if p, ok := t.Proposal.Get(); ok && p.Cost != "" {
			f = append(f, Fact{Label: "Cost", Value: Clip(p.Cost, 160)})
		}
	case kind == string(model.AskDesign) || tkind == string(model.KindIntent):
		f = append(f, Fact{Label: "Proposed", Value: Plural(len(t.Proposed), "ticket")})
		if n := len(t.Feedback); n > 0 {
			f = append(f, Fact{Label: "Sent back", Value: Plural(n, "time")})
		}
	case kind == string(model.AskNotice) || tkind == string(model.KindTask):
		k := "Notice"
		if tkind == string(model.KindTask) {
			k = "Task"
		}
		f = append(f, Fact{Label: "Kind", Value: k})
		if h := t.AgentLastHost.Or(""); h != "" {
			f = append(f, Fact{Label: "Ran on", Value: h})
		}
	case tkind == string(model.KindProspect):
		f = append(f, Fact{Label: "Proposals", Value: fmt.Sprint(len(t.Proposals))})
		if pr, ok := t.PanelResult.Get(); ok {
			f = append(f, Fact{Label: "Kept", Value: fmt.Sprint(len(pr.Kept)), Tone: "good"})
			f = append(f, Fact{Label: "Dropped", Value: fmt.Sprint(len(pr.Dropped))})
		}
	}
	if s, ok := t.Source.Get(); ok && s.Issue > 0 {
		f = append(f, Fact{Label: "From", Value: fmt.Sprintf("issue #%d (GitHub)", s.Issue), URL: SafeURL(s.URL)})
	}
	if tr := reportOf(t, "triage"); tr != nil && (kind == string(model.AskStart) || kind == string(model.AskDecide)) {
		d := map[string]string{"accept": "accepted", "reject": "advised closing"}[tr.str("decision")]
		if d == "" {
			d = "unsure"
		}
		f = append(f, Fact{Label: "Triage", Value: d})
	}
	if kind == string(model.AskUnstick) {
		if c := a.RetryColumn.Or(""); c != "" {
			f = append(f, Fact{Label: "Retry from", Value: ColumnLabel(c)})
		}
	}
	if pr, ok := t.PR.Get(); ok {
		v := fmt.Sprintf("#%d", pr.Number)
		if prDraft(pr) {
			v += " (draft)"
		}
		f = append(f, Fact{Label: "Pull request", Value: v, URL: SafeURL(pr.URL)})
	}
	if g, ok := t.Gate.Get(); ok {
		if !g.ChecksPass {
			f = append(f, Fact{Label: "Tests and lint", Value: "fail", Tone: "bad"})
		}
		tone := "good"
		if !g.WithinSize {
			tone = "warn"
		}
		f = append(f, Fact{Label: "Size", Value: fmt.Sprintf("%d lines, %s", g.ChangedLines, Plural(len(g.Files), "file")), Tone: tone})
		if g.AutoEligible {
			f = append(f, Fact{Label: "Auto-merge", Value: "eligible", Tone: "good"})
		} else if g.ChecksPass && PRCallout(t) == nil {
			f = append(f, Fact{Label: "Auto-merge", Value: "no: needs a maintainer", Tone: "warn"})
		}
	}
	if rv := reportOf(t, "review"); rv != nil {
		if b := rv.str("runtime_behaviour"); b != "" {
			tone := "warn"
			if b == "no" {
				tone = "good"
			}
			f = append(f, Fact{Label: "Changes behaviour", Value: b, Tone: tone})
		}
		v := rv.str("verdict")
		tone := ""
		if v != "pass" {
			tone = "warn"
		}
		f = append(f, Fact{Label: "Review", Value: v, Tone: tone})
	}
	if reportOf(t, "security") != nil {
		f = append(f, Fact{Label: "Security", Value: strings.TrimSuffix(SecurityLine(t), ".")})
	}
	f = append(f, publicFacts(t)...)
	if kind == "" && len(f) == 0 {
		f = append(f, Fact{Label: "Column", Value: TicketColumnLabel(t)})
	}
	return f
}

// boardFacts are a board ticket's column, why it waits, who has it, its
// labels and who filed it.
func boardFacts(t *ticket.Ticket) []Fact {
	f := []Fact{{Label: "Column", Value: ColumnLabel(t.Column)}}
	if w, ok := t.Waiting.Get(); ok && w.Reason != "question" && w.Text != "" {
		f = append(f, Fact{Label: "Waiting on", Value: Clip(w.Text, 160)})
	}
	if as := t.Assignee.Or(""); as != "" {
		f = append(f, Fact{Label: "Assignee", Value: as})
	}
	if len(t.Labels) > 0 {
		f = append(f, Fact{Label: "Labels", Value: strings.Join(t.Labels, ", ")})
	}
	if t.By != "" {
		f = append(f, Fact{Label: "Filed by", Value: t.By})
	}
	return f
}

// Callout is what a person must know before answering, set apart from the
// facts.
type Callout struct {
	Tone, Title string
	Lines       []string
}

// PRCallout says why an open PR is a draft (the gate's blockers, in plain
// words), that GitHub refused to merge it before the maintainer's approval,
// or that it passed everything; nil for a ticket with no open PR and gate.
func PRCallout(t *ticket.Ticket) *Callout {
	pr, okPR := t.PR.Get()
	g, okG := t.Gate.Get()
	if t.Column != string(model.ColPROpen) || !okPR || !okG {
		return nil
	}
	if pr.MergeBlocked.Or("") == ticket.MergeBlockedApproval && WaitsForApproval(t) {
		return &Callout{Tone: "warn", Title: "Waiting for your approval on GitHub", Lines: []string{
			fmt.Sprintf("GitHub refused the merge: PR #%d needs your approval of its latest commit.", pr.Number),
			"Approve it on GitHub; the factory merges it by itself on the next tick after that."}}
	}
	if !prDraft(pr) || len(g.Blockers) == 0 {
		return &Callout{Tone: "good", Title: "Passed every check for automatic approval"}
	}
	return &Callout{Tone: "warn", Title: "Why this PR is a draft", Lines: DraftReasons(g.Blockers)}
}

// Pending says what the tick is doing for an intent Foreman recorded:
// Foreman holds no GitHub credentials, so "Open the PR", "Merge"
// and "Send back" wait for the next tick. A failure since the request
// (the latest error event after it) is the "still trying" line. nil when
// nothing waits.
func Pending(t *ticket.Ticket) *Callout {
	pr, ok := t.PR.Get()
	if !ok {
		return nil
	}
	var c *Callout
	since := ""
	switch {
	case pr.OpenRequested.Present():
		since = pr.OpenRequested.Or("")
		c = &Callout{Tone: "info", Title: "Opening the PR; it shows here when it is open"}
		if e := lastErrorSince(t, since); e != "" {
			c.Lines = []string{"Still trying to open the PR: " + Clip(e, 200)}
		}
	case pr.MergeRequested.Present() && !ticket.NeedsApproval(t):
		// a merge GitHub refused for want of your approval is dropped, not
		// retried; one left on a PR that waits for it is not merging either
		since = pr.MergeRequested.Or("")
		c = &Callout{Tone: "info", Title: "Merging…"}
		if e := lastErrorSince(t, since); e != "" {
			c.Lines = []string{"Still trying to merge: " + Clip(e, 200)}
		}
	case pr.HoldRequested.Present():
		since = pr.HoldRequested.Or("")
		c = &Callout{Tone: "info", Title: "Making the PR a draft again"}
		if e := lastErrorSince(t, since); e != "" {
			c.Lines = []string{"Still trying to make the PR a draft: " + Clip(e, 200)}
		}
	}
	return c
}

// lastErrorSince is the detail of the latest error event at or after
// since (a stamp), or "".
func lastErrorSince(t *ticket.Ticket, since string) string {
	for i := len(t.History) - 1; i >= 0; i-- {
		e := t.History[i]
		if e.At < since {
			break
		}
		if e.Event == "error" {
			return e.Detail
		}
	}
	return ""
}

// linesRe is the gate's size blocker.
var linesRe = regexp.MustCompile(`^(\d+) changed lines \(limit (\d+)\)$`) // re: go

// DraftReasons are the gate's blockers in plain words; a list of paths
// becomes a count. Each is clipped to 200 characters.
func DraftReasons(blockers []string) []string {
	var out []string
	for _, b := range blockers {
		head, list, ok := strings.Cut(b, ": ")
		switch {
		case ok && strings.HasPrefix(head, "files outside"):
			b = CapFirst(Plural(len(strings.Split(list, ", ")), "file")) + " outside the paths that may merge automatically (docs, tests, config)"
		case strings.HasPrefix(b, "reviewer says runtime behaviour changes"):
			b = "The reviewer says it changes how the code behaves at runtime"
		default:
			if m := linesRe.FindStringSubmatch(b); m != nil {
				b = m[1] + " changed lines; the limit for automatic approval is " + m[2]
			} else {
				b = CapFirst(b)
			}
		}
		out = append(out, Clip(b, 200))
	}
	return out
}

// SecurityLine is the security check in one sentence: the tier, the
// questions asked and the worst concern among the answers, and the verdict
// when it did not pass.
func SecurityLine(t *ticket.Ticket) string {
	r := reportOf(t, "security")
	answers := r.list("answers")
	levels := []string{"none", "low", "medium", "high"}
	worst := -1
	for _, a := range answers {
		c := a["concern"]
		for i, k := range levels {
			if strings.EqualFold(c, k) && i > worst {
				worst = i
			}
		}
	}
	var b strings.Builder
	if tier, ok := t.SecurityTier.Get(); ok {
		fmt.Fprintf(&b, "Tier %d (%s): ", tier, tierName(tier))
	}
	b.WriteString(Plural(len(answers), "question"))
	switch worst {
	case -1:
	case 0, 1:
		b.WriteString(", none above low")
	default:
		b.WriteString(", worst " + levels[worst])
	}
	b.WriteString(".")
	if v := r.str("verdict"); v != "" && v != "pass" {
		b.WriteString(" Verdict: " + v + ".")
	}
	return b.String()
}
