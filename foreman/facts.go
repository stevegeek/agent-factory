package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type Fact struct {
	Label, Value string
	Tone         string // good, warn, bad
	URL          string
	Votes        []int
}

// safeURL keeps only http(s) links: ticket text comes from public issue trackers.
func safeURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

func Facts(t *Ticket, a *Ask, all map[string]*Ticket) []Fact {
	var f []Fact
	kind := ""
	if a != nil {
		kind = a.Kind
	}
	switch {
	case kind == "proposal" || t.Kind == "proposal":
		var scores []int
		for _, v := range panelVotes(t, all) {
			scores = append(scores, v.Score)
		}
		if len(scores) == 0 && t.Panel != nil {
			scores = t.Panel.Scores
		}
		if len(scores) > 0 {
			f = append(f, Fact{Label: "Panel scores", Votes: scores})
			f = append(f, Fact{Label: "Panel", Value: panelRule(scores)})
		}
		if t.Proposal != nil && t.Proposal.Cost != "" {
			f = append(f, Fact{Label: "Cost", Value: clip(t.Proposal.Cost, 160)})
		}
	case kind == "design" || t.Kind == "intent":
		f = append(f, Fact{Label: "Proposed", Value: plural(len(t.Proposed), "ticket")})
		if n := len(t.Feedback); n > 0 {
			f = append(f, Fact{Label: "Sent back", Value: plural(n, "time")})
		}
	case kind == "notice" || t.Kind == "task":
		k := "Notice"
		if t.Kind == "task" {
			k = "Task"
		}
		f = append(f, Fact{Label: "Kind", Value: k})
		if t.AgentLastHost != "" {
			f = append(f, Fact{Label: "Ran on", Value: t.AgentLastHost})
		}
	case t.Kind == "prospect":
		f = append(f, Fact{Label: "Proposals", Value: fmt.Sprint(len(t.Proposals))})
		if t.PanelResult != nil {
			f = append(f, Fact{Label: "Kept", Value: fmt.Sprint(len(t.PanelResult.Kept)), Tone: "good"})
			f = append(f, Fact{Label: "Dropped", Value: fmt.Sprint(len(t.PanelResult.Dropped))})
		}
	}
	if t.Source != nil && t.Source.Issue > 0 {
		f = append(f, Fact{Label: "From", Value: fmt.Sprintf("issue #%d (GitHub)", t.Source.Issue), URL: safeURL(t.Source.URL)})
	}
	if tr := t.report("triage"); tr != nil && (kind == "start" || kind == "decide") {
		d := map[string]string{"accept": "accepted", "reject": "advised closing"}[tr.str("decision")]
		if d == "" {
			d = "unsure"
		}
		f = append(f, Fact{Label: "Triage", Value: d})
	}
	if kind == "unstick" && a.RetryColumn != "" {
		f = append(f, Fact{Label: "Retry from", Value: columnLabel(a.RetryColumn)})
	}
	if t.PR != nil {
		v := fmt.Sprintf("#%d", t.PR.Number)
		if t.PR.Draft {
			v += " (draft)"
		}
		f = append(f, Fact{Label: "Pull request", Value: v, URL: safeURL(t.PR.URL)})
	}
	if g := t.Gate; g != nil {
		if !g.ChecksPass {
			f = append(f, Fact{Label: "Tests and lint", Value: "fail", Tone: "bad"})
		}
		tone := "good"
		if !g.WithinSize {
			tone = "warn"
		}
		f = append(f, Fact{Label: "Size", Value: fmt.Sprintf("%d lines, %s", g.ChangedLines, plural(len(g.Files), "file")), Tone: tone})
		if g.AutoEligible {
			f = append(f, Fact{Label: "Auto-merge", Value: "eligible", Tone: "good"})
		} else if g.ChecksPass && PRCallout(t) == nil {
			f = append(f, Fact{Label: "Auto-merge", Value: "no: needs a maintainer", Tone: "warn"})
		}
	}
	if r := t.report("review"); r != nil {
		if b := r.str("runtime_behaviour"); b != "" {
			tone := "warn"
			if b == "no" {
				tone = "good"
			}
			f = append(f, Fact{Label: "Changes behaviour", Value: b, Tone: tone})
		}
		v := r.str("verdict")
		tone := ""
		if v != "pass" {
			tone = "warn"
		}
		f = append(f, Fact{Label: "Review", Value: v, Tone: tone})
	}
	if t.report("security") != nil {
		f = append(f, Fact{Label: "Security", Value: strings.TrimSuffix(securityLine(t), ".")})
	}
	if kind == "" && len(f) == 0 {
		f = append(f, Fact{Label: "Column", Value: ticketColumnLabel(t)})
	}
	return f
}

// Callout: what a person must know before answering, set apart from the facts.
type Callout struct {
	Tone, Title string
	Lines       []string
}

// PRCallout says why an open PR is a draft (the gate's blockers, in plain words) or that it
// passed everything, and where the next step happens.
func PRCallout(t *Ticket) *Callout {
	if t.Column != "pr_open" || t.PR == nil || t.Gate == nil {
		return nil
	}
	if !t.PR.Draft || len(t.Gate.Blockers) == 0 {
		return &Callout{Tone: "good", Title: "Passed every check for automatic approval"}
	}
	return &Callout{Tone: "warn", Title: "Why this PR is a draft", Lines: draftReasons(t.Gate.Blockers)}
}

// draftReasons: the gate's blockers in plain words; a list of paths becomes a count.
func draftReasons(blockers []string) []string {
	var out []string
	for _, b := range blockers {
		head, list, ok := strings.Cut(b, ": ")
		switch {
		case ok && strings.HasPrefix(head, "files outside"):
			b = capFirst(plural(len(strings.Split(list, ", ")), "file")) + " outside the paths that may merge automatically (docs, tests, config)"
		case strings.HasPrefix(b, "reviewer says runtime behaviour changes"):
			b = "The reviewer says it changes how the code behaves at runtime"
		default:
			if m := linesRe.FindStringSubmatch(b); m != nil {
				b = m[1] + " changed lines; the limit for automatic approval is " + m[2]
			} else {
				b = capFirst(b)
			}
		}
		out = append(out, clip(b, 200))
	}
	return out
}

var linesRe = regexp.MustCompile(`^(\d+) changed lines \(limit (\d+)\)$`)
