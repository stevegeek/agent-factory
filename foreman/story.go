package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// "How it got here": the ticket's history, reports, votes and gate as a few lines for a person.
type Step struct {
	At, End time.Time
	When    string
	Who     string
	What    string
	Quotes  []Quote
	Tone    string // "now", "flag" or ""
}

type Quote struct {
	Score int
	Text  string
	Who   string
}

type Story struct {
	Steps  []Step
	Caveat string
}

var roles = map[string]string{"triage": "Triage", "work": "Worker", "review": "Reviewer", "security": "Security",
	"task": "Task agent", "design": "Designer", "prospect": "Prospector", "vote": "Voter"}

var columnLabels = map[string]string{"triage": "Triage", "ready": "Ready", "working": "Working", "voting": "Voting",
	"review": "Review", "security": "Security", "gate": "the gate", "ready_for_pr": "Ready for PR", "pr_open": "PR open",
	"needs_human": "Needs you", "done": "Done", "rejected": "Closed"}

// ticketColumnLabel: a proposal the panel or the maintainer turned down reads as dropped.
func ticketColumnLabel(t *Ticket) string {
	if t.Column == "rejected" && t.Kind == "proposal" {
		return "Dropped"
	}
	return columnLabel(t.Column)
}

func columnLabel(c string) string {
	if l, ok := columnLabels[c]; ok {
		return l
	}
	return c
}

var startDetail = regexp.MustCompile(`^(\w+) (\S+)(?: on \S+)?(?: @(\S+))?$`)

func BuildStory(t *Ticket, all map[string]*Ticket) Story {
	var s Story
	if t.Kind == "proposal" {
		if p := all[t.Parent]; p != nil {
			s.Steps = append(s.Steps, prospectOrigin(p)...)
		}
	}
	h := t.History
	used := make([]bool, len(h))
	lastStart := map[string]int{}
	for i, e := range h {
		if e.Event == "start" {
			if m := startDetail.FindStringSubmatch(e.Detail); m != nil {
				lastStart[m[1]] = i
			}
		}
	}
	for i, e := range h {
		if used[i] {
			continue
		}
		var st *Step
		switch e.Event {
		case "created":
			st = createdStep(t, e)
		case "start":
			m := startDetail.FindStringSubmatch(e.Detail)
			if m == nil {
				break
			}
			if m[1] == "vote" {
				st = voteGroup(t, h, i, used)
				break
			}
			st = &Step{At: parseTime(e.At), Who: roles[m[1]]}
			if st.Who == "" {
				st.Who = capFirst(m[1])
			}
			if m[3] != "" {
				st.Who += " on " + m[3]
			}
			if j := finishedAt(h, i, m[1]+" "+m[2]); j >= 0 {
				used[j] = true
				st.End = parseTime(h[j].At)
				if lastStart[m[1]] == i {
					st.What = stageWhat(t, m[1])
				} else {
					st.What = "An earlier attempt."
				}
			} else if t.Agent != nil && t.Agent.Name == m[2] {
				st.What, st.Tone = "Running now.", "now"
			} else {
				st.What, st.Tone = "Stopped without a report.", "flag"
			}
		case "move":
			st = moveStep(t, e)
		case "error":
			n := 1
			for j := i + 1; j < len(h) && h[j].Event == "error" && h[j].Detail == e.Detail; j++ {
				used[j] = true
				n++
			}
			st = &Step{At: parseTime(e.At), Who: "Dispatcher", What: sentence("error: " + clip(e.Detail, 300)), Tone: "flag"}
			if n > 1 {
				st.What += fmt.Sprintf(" (%d times)", n)
			}
		case "note":
			st = &Step{At: parseTime(e.At), Who: "You", What: "Note: " + clip(e.Detail, 400)}
		case "asked":
			s.Steps = append(s.Steps, askSteps(e, all)...)
			continue
		case "panel":
			s.Steps = append(s.Steps, panelSteps(t, all, e)...)
			continue
		}
		if st == nil && e.Rule != "" && e.Event != "move" {
			st = &Step{At: parseTime(e.At), Who: "Dispatcher"}
		}
		if st == nil {
			continue
		}
		if e.Rule != "" && (st.Who == "Dispatcher" || st.Who == "Gate") {
			st.What = sentence(e.Rule)
		}
		s.Steps = append(s.Steps, *st)
	}
	if a := AskFor(t); a != nil {
		s.Steps = append(s.Steps, Step{Who: "Waiting for you", What: waitingWhat(t, a), Tone: "now", When: "now"})
	}
	if v := panelVotes(t, all); len(v) > 1 {
		host := v[0].Host
		for _, x := range v {
			if x.Host != host {
				host = ""
			}
		}
		if host != "" {
			s.Caveat = fmt.Sprintf("All %d voters ran on %s, so matching scores say less than independent setups agreeing would.", len(v), host)
		}
	}
	formatWhen(s.Steps)
	return s
}

func finishedAt(h []Hist, from int, what string) int {
	for j := from + 1; j < len(h); j++ {
		if h[j].Event == "finished" && h[j].Detail == what {
			return j
		}
	}
	return -1
}

func createdStep(t *Ticket, e Hist) *Step {
	st := &Step{At: parseTime(e.At)}
	switch {
	case t.Kind == "proposal":
		return nil
	case t.Door == "github":
		st.Who = "GitHub sweep"
		if t.Source != nil && t.Source.Issue > 0 {
			st.What = fmt.Sprintf("Filed issue #%d for triage.", t.Source.Issue)
		} else {
			st.What = "Filed an issue for triage."
		}
	case t.Door == "clock":
		st.Who = "Clock"
		switch {
		case t.Kind == "prospect":
			st.What = "Started a prospecting run: " + t.Title
		case t.Project == noticeProject:
			st.What = "Filed a notice: " + t.Title
		default:
			st.What = "Filed a scheduled check: " + t.Title
		}
	case t.Parent != "":
		st.Who = "You"
		st.What = "Approved it in " + t.Parent + "."
	case t.Door == "peer":
		st.Who, st.What = "Peer", "Filed it."
	default:
		st.Who = "You"
		switch t.Kind {
		case "intent":
			st.What = "Filed the idea."
		case "task":
			st.What = "Asked: " + t.Title
		default:
			st.What = "Filed it."
		}
	}
	return st
}

func stageWhat(t *Ticket, stage string) string {
	r := t.report(stage)
	switch stage {
	case "triage":
		reason := clip(r.str("reason"), 400)
		switch r.str("decision") {
		case "accept":
			return strings.TrimSpace("Accepted. " + reason)
		case "reject":
			return strings.TrimSpace("Advised closing it. " + reason)
		}
		return strings.TrimSpace("Could not decide. " + reason)
	case "work":
		if r.str("status") != "done" {
			return strings.TrimSpace("Blocked. " + clip(r.str("summary"), 300))
		}
		return clip(r.str("summary"), 400)
	case "review":
		v := "Asked for changes."
		if r.str("verdict") == "pass" {
			v = "Pass."
		}
		if b := r.str("runtime_behaviour"); b != "" {
			v += " Changes runtime behaviour: " + b + "."
		}
		return strings.TrimSpace(v + " " + clip(r.str("summary"), 300))
	case "security":
		return securityLine(t)
	case "task":
		if r.str("status") != "done" {
			return strings.TrimSpace("Blocked. " + clip(r.str("summary"), 300))
		}
		return strings.TrimSpace("Answered. " + clip(r.str("summary"), 300))
	case "design":
		return strings.TrimSpace(clip(r.str("summary"), 300) + " " + sentence("proposed "+plural(len(t.Proposed), "ticket")))
	case "prospect":
		return sentence(fmt.Sprintf("proposed %s with evidence", plural(len(t.Proposals), "change")))
	}
	return sentence(clip(r.str("summary"), 300))
}

// securityLine: tier, questions asked, worst concern among the answers.
func securityLine(t *Ticket) string {
	r := t.report("security")
	answers := r.list("answers")
	worst := -1
	for _, a := range answers {
		m, _ := a.(map[string]any)
		c, _ := m["concern"].(string)
		for i, k := range []string{"none", "low", "medium", "high"} {
			if strings.EqualFold(c, k) && i > worst {
				worst = i
			}
		}
	}
	var b strings.Builder
	if t.SecurityTier != nil {
		fmt.Fprintf(&b, "Tier %d (%s): ", *t.SecurityTier, tierWhat(*t.SecurityTier))
	}
	b.WriteString(plural(len(answers), "question"))
	switch worst {
	case -1:
	case 0, 1:
		b.WriteString(", none above low")
	default:
		b.WriteString(", worst " + []string{"none", "low", "medium", "high"}[worst])
	}
	b.WriteString(".")
	if r.str("verdict") != "" && r.str("verdict") != "pass" {
		b.WriteString(" Verdict: " + r.str("verdict") + ".")
	}
	return b.String()
}

func tierWhat(n int) string {
	return map[int]string{0: "text only", 1: "inert code", 2: "input reaches a sink", 3: "build or dependencies", 4: "several kinds"}[n]
}

var prRef = regexp.MustCompile(`/pull/(\d+)`)

func moveStep(t *Ticket, e Hist) *Step {
	head, detail, _ := strings.Cut(e.Detail, ": ")
	_, to, _ := strings.Cut(head, " -> ")
	st := &Step{At: parseTime(e.At), Who: "Dispatcher"}
	human := strings.HasPrefix(detail, "by human")
	switch {
	case human:
		st.Who = "You"
		switch to {
		case "done":
			st.What = "Closed it."
		case "rejected":
			st.What = "Dropped it."
		default:
			st.What = "Moved it to " + columnLabel(to) + "."
		}
	case to == "pr_open":
		n := ""
		if m := prRef.FindStringSubmatch(detail); m != nil {
			n = " #" + m[1]
		}
		if strings.HasPrefix(detail, "draft") {
			st.What = "Opened draft PR" + n + " and asked for review."
		} else {
			st.What = "Opened PR" + n + "."
		}
	case strings.HasPrefix(detail, "PR merged by "):
		st.Who, st.What = "GitHub", "Merged by "+strings.TrimPrefix(detail, "PR merged by ")+"."
	case strings.HasPrefix(detail, "PR closed"):
		st.Who, st.What, st.Tone = "GitHub", "The PR was closed without merging.", "flag"
	case strings.HasPrefix(detail, "triage accepted"):
		switch {
		case to == "ready":
			st.What = "Triage accepted it, so it went straight to Ready."
		case t.Door == "github":
			st.What = "Issues from GitHub wait for your go-ahead after triage, so it stopped here."
		default:
			st.What = "Triage accepted it; it waits for you to start it."
		}
	case strings.HasPrefix(detail, "triage unsure"), strings.HasPrefix(detail, "design ready"),
		strings.HasPrefix(detail, "task done"), strings.HasSuffix(detail, "blind panel next"):
		return nil
	case strings.HasPrefix(detail, "triage: "):
		st.What, st.Tone = "Closed it, as triage advised.", "flag"
	case strings.HasPrefix(detail, "gate:"):
		st.Who, st.What, st.Tone = "Gate", sentence(strings.TrimSpace(strings.TrimPrefix(detail, "gate:"))), "flag"
	case to == "ready_for_pr" && detail == "auto-merge eligible":
		st.Who, st.What = "Gate", "Tests and lint pass, no blockers: auto-merge eligible."
	case to == "ready_for_pr" && strings.HasPrefix(detail, "needs human review: "):
		// the same plain words as the draft callout: a list of paths becomes a count
		why := strings.Join(draftReasons(strings.Split(strings.TrimPrefix(detail, "needs human review: "), "; ")), "; ")
		st.Who, st.What = "Gate", sentence("Tests and lint pass. Needs a maintainer: "+why)
	case detail == "review asked for changes":
		st.Who, st.What = "Reviewer", "Asked for changes; back to the worker."
	case strings.HasPrefix(detail, "redirected: "):
		st.Who, st.What = "You", "Sent the design back: "+strings.TrimPrefix(detail, "redirected: ")
	case strings.HasPrefix(detail, "design approved; filed "):
		st.Who, st.What = "You", "Approved the design; filed "+strings.TrimPrefix(detail, "design approved; filed ")+"."
	case strings.HasPrefix(detail, "proposal approved; filed "):
		st.Who, st.What = "You", "Filed it as a change: "+strings.TrimPrefix(detail, "proposal approved; filed ")+"."
	case strings.HasPrefix(detail, "panel kept "):
		st.What = "The panel kept " + strings.TrimPrefix(detail, "panel kept ") + "."
	case strings.HasPrefix(detail, "prospecting found nothing"):
		st.Who, st.What = "Prospector", "Found nothing worth proposing."
	case strings.HasPrefix(detail, "task blocked"), strings.HasPrefix(detail, "agent "), strings.HasPrefix(detail, "worker "),
		strings.HasPrefix(detail, "designer "), strings.HasPrefix(detail, "security:"), strings.HasPrefix(detail, "review still"),
		strings.Contains(detail, "changed the branch"), detail == "dispatcher error":
		st.What, st.Tone = sentence(clip(detail, 300)), "flag"
	case detail == "":
		if e.Rule == "" {
			return nil
		}
	default:
		// a person's own words from `factory move --note`
		st.Who, st.What = "You", "Moved it to "+columnLabel(to)+": "+clip(detail, 300)
	}
	return st
}

func voteGroup(t *Ticket, h []Hist, i int, used []bool) *Step {
	st := &Step{At: parseTime(h[i].At), Who: "Panel"}
	end, n := i, 0
	for j := i; j < len(h); j++ {
		if (h[j].Event == "start" && strings.HasPrefix(h[j].Detail, "vote ")) ||
			(h[j].Event == "finished" && strings.HasPrefix(h[j].Detail, "vote ")) || h[j].Event == "vote" {
			used[j] = true
			end = j
			if h[j].Event == "finished" {
				n++
			}
		}
	}
	st.End = parseTime(h[end].At)
	st.What = sentence(fmt.Sprintf("%s scored each proposal 1–5, blind to each other", plural(n, "voter")))
	if t.Agent != nil && t.Agent.Stage == "vote" {
		st.Tone = "now"
		st.What += " Still voting."
	}
	return st
}

// prospectOrigin: the first steps of a proposal's story, from the prospect that found it.
func prospectOrigin(p *Ticket) []Step {
	var out []Step
	for i, e := range p.History {
		switch {
		case e.Event == "created":
			out = append(out, Step{At: parseTime(e.At), Who: "Clock", What: "Started a prospecting run (" + p.ID + ")."})
		case e.Event == "start" && strings.HasPrefix(e.Detail, "prospect "):
			st := Step{At: parseTime(e.At), Who: "Prospector", What: stageWhat(p, "prospect")}
			name := strings.Fields(e.Detail)[1]
			if j := finishedAt(p.History, i, "prospect "+name); j >= 0 {
				st.End = parseTime(p.History[j].At)
			}
			out = append(out, st)
		}
	}
	return out
}

func panelSteps(t *Ticket, all map[string]*Ticket, e Hist) []Step {
	votes := panelVotes(t, all)
	at := parseTime(e.At)
	panel := Step{At: at, Who: "Panel"}
	if p := all[t.Parent]; p != nil {
		// times come from the prospect: a proposal filed later (a migrated board) keeps the panel's own
		for _, x := range p.History {
			switch {
			case x.Event == "start" && strings.HasPrefix(x.Detail, "vote ") && panel.End.IsZero():
				panel.At, panel.End = parseTime(x.At), at
			case x.Event == "finished" && strings.HasPrefix(x.Detail, "vote "):
				panel.End = parseTime(x.At)
			case x.Event == "move" && strings.Contains(x.Detail, "panel kept"):
				at = parseTime(x.At)
			}
		}
	}
	var scores []string
	for _, v := range votes {
		scores = append(scores, strconv.Itoa(v.Score))
		panel.Quotes = append(panel.Quotes, Quote{Score: v.Score, Text: v.Reason, Who: v.Voter})
	}
	if len(scores) == 0 && t.Panel != nil {
		for _, s := range t.Panel.Scores {
			scores = append(scores, strconv.Itoa(s))
		}
	}
	panel.What = "Scored " + strings.Join(scores, " · ") + "."
	rule := e.Rule
	if rule == "" && t.Panel != nil {
		rule = panelRule(t.Panel.Scores)
	}
	verdict := Step{At: at, Who: "Dispatcher", What: sentence(rule)}
	if t.Column == "rejected" && strings.HasPrefix(rule, "dropped") {
		verdict.Tone = "flag"
		verdict.What += " It never reached your desk."
	}
	return []Step{panel, verdict}
}

// panelTime: when the prospect's panel decided, or zero.
func panelTime(p *Ticket) time.Time {
	for _, x := range p.History {
		if x.Event == "move" && strings.Contains(x.Detail, "panel kept") {
			return parseTime(x.At)
		}
	}
	return time.Time{}
}

// panelRule mirrors the dispatcher's keep rule: two keen voters (4+) or a mean of 3.5.
func panelRule(scores []int) string {
	if len(scores) == 0 {
		return "dropped: no scores"
	}
	keen, sum := 0, 0
	for _, s := range scores {
		sum += s
		if s >= 4 {
			keen++
		}
	}
	mean := float64(sum) / float64(len(scores))
	tail := fmt.Sprintf("mean %.1f (3.5 needed)", mean)
	if keen >= 2 || mean >= 3.5 {
		return fmt.Sprintf("kept: %d of %d voters scored 4 or more (2 needed); %s", keen, len(scores), tail)
	}
	if keen == 0 {
		return "dropped: no voter scored 4 or more; " + tail
	}
	return fmt.Sprintf("dropped: %d of %d voters scored 4 or more (2 needed); %s", keen, len(scores), tail)
}

// panelVotes: the votes filed on the proposal, or, on older boards, read back from its prospect.
func panelVotes(t *Ticket, all map[string]*Ticket) []PanelVote {
	if t.Kind != "proposal" {
		return nil
	}
	if t.Panel != nil && len(t.Panel.Votes) > 0 {
		return t.Panel.Votes
	}
	p := all[t.Parent]
	if p == nil || t.Proposal == nil {
		return nil
	}
	return votesFor(p, proposalIndex(p, t.Proposal.Title))
}

func proposalIndex(p *Ticket, title string) int {
	for i, x := range p.Proposals {
		if x.Title == title {
			return i + 1
		}
	}
	return 0
}

func votesFor(p *Ticket, idx int) []PanelVote {
	if idx == 0 {
		return nil
	}
	var out []PanelVote
	for _, v := range p.Votes {
		if s, ok := v.Scores[strconv.Itoa(idx)]; ok {
			out = append(out, PanelVote{Voter: v.Voter, Host: v.Host, Score: s.Score, Reason: s.Reason})
		}
	}
	return out
}

// droppedProposals: proposals a panel dropped that have no ticket of their own (boards from
// before the dispatcher filed them), as closed proposal tickets without an id.
func droppedProposals(ts []*Ticket) []*Ticket {
	filed := map[string]bool{}
	for _, t := range ts {
		if t.Kind == "proposal" && t.Proposal != nil {
			filed[t.Parent+"\x00"+t.Proposal.Title] = true
		}
	}
	var out []*Ticket
	for _, p := range ts {
		if p.Kind != "prospect" || p.PanelResult == nil || len(p.Votes) == 0 {
			continue
		}
		at := ""
		for _, h := range p.History {
			if h.Event == "move" && strings.Contains(h.Detail, "panel kept") {
				at = h.At
			}
		}
		for i, prop := range p.Proposals {
			if filed[p.ID+"\x00"+prop.Title] {
				continue
			}
			votes := votesFor(p, i+1)
			var scores []int
			for _, v := range votes {
				scores = append(scores, v.Score)
			}
			rule := panelRule(scores)
			if !strings.HasPrefix(rule, "dropped") {
				continue
			}
			prop := prop
			out = append(out, &Ticket{Project: p.Project, Title: prop.Title, Door: "clock", Column: "rejected",
				Kind: "proposal", Parent: p.ID, Proposal: &prop, Panel: &Panel{Scores: scores, Votes: votes},
				Created: at, Updated: at, History: []Hist{{At: at, Event: "panel", Rule: rule}}})
		}
	}
	return out
}

// askSteps: the question, then its answer (or where the answer is) from the task it filed.
func askSteps(e Hist, all map[string]*Ticket) []Step {
	id, q, _ := strings.Cut(e.Detail, ": ")
	steps := []Step{{At: parseTime(e.At), Who: "You", What: "Asked: " + clip(q, 400)}}
	c := all[id]
	switch {
	case c == nil:
	case c.Result != "":
		steps = append(steps, Step{At: parseTime(c.Updated), Who: "Answer", What: clip(c.Result, 900)})
	case c.Agent != nil:
		steps = append(steps, Step{Who: "Answer", What: "An agent is working on it (" + id + ").", Tone: "now", When: "now"})
	case c.closed():
		steps = append(steps, Step{At: parseTime(c.Updated), Who: "Answer", What: "None came back (" + id + " closed).", Tone: "flag"})
	default:
		steps = append(steps, Step{Who: "Answer", What: "Queued (" + id + "); an agent picks it up on the next tick.", Tone: "now", When: "soon"})
	}
	return steps
}

// recentSteps: how many of the latest steps show before "earlier steps" folds the rest away.
const recentSteps = 5

// Earlier and Recent split the steps so the page opens on what happened last.
func (s Story) Earlier() []Step {
	if len(s.Steps) <= recentSteps+1 {
		return nil
	}
	return s.Steps[:len(s.Steps)-recentSteps]
}

func (s Story) Recent() []Step {
	if len(s.Steps) <= recentSteps+1 {
		return s.Steps
	}
	return s.Steps[len(s.Steps)-recentSteps:]
}

func waitingWhat(t *Ticket, a *Ask) string {
	switch a.Kind {
	case "review":
		if mergeable(t) {
			return "It passed every check for automatic approval. Merge it, or read it on GitHub first."
		}
		return "Merge it on GitHub, or send it back with a note."
	case "start":
		return "Start it, or not now."
	case "decide":
		return "Start it anyway, drop it, or ask triage again with a note."
	case "design":
		return "Approve some or all of the tickets, or send it back with a note."
	case "proposal":
		return "File it as a change, or drop it."
	case "notice":
		return "Close it when you have read it."
	case "question":
		return "The agent waits for your answer."
	}
	return "Retry it, retry with a note, or drop it."
}

// formatWhen writes each step's time: the date only when it changes.
func formatWhen(steps []Step) {
	last := ""
	for i := range steps {
		s := &steps[i]
		if s.When != "" || s.At.IsZero() {
			continue
		}
		at := s.At.Local()
		day := at.Format("2 Jan")
		s.When = at.Format("15:04")
		if day != last {
			s.When = day + " " + s.When
			last = day
		}
		if !s.End.IsZero() && s.End.Sub(s.At) >= time.Minute {
			s.When += "–" + s.End.Local().Format("15:04")
		}
	}
}
