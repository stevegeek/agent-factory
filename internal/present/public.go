package present

import (
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Public review on a public project: what it said, for the facts, the
// story and the thread. Its findings say where private text is and why;
// they are shown here, on the maintainer's own pages, and never sent to
// GitHub.

// PublicDoing is what a running public review is doing, for Live and the
// thread.
const PublicDoing = "reading what it would publish"

// publicFinding is one finding of a public review's report.
type publicFinding struct{ Where, Why string }

func publicFindings(t *ticket.Ticket) []publicFinding {
	return findingsOf(reportOf(t, string(model.StagePublic)))
}

func findingsOf(r *report) []publicFinding {
	var out []publicFinding
	for _, f := range r.list("findings") {
		if w := strings.TrimSpace(f["where"]); w != "" {
			out = append(out, publicFinding{Where: Clip(w, 200), Why: Clip(f["why"], 300)})
		}
	}
	return out
}

// PublicLine is where public review stands, in one sentence: its last
// report (publicRun), or a person's pass of a change it blocked.
func PublicLine(t *ticket.Ticket) string {
	r := reportOf(t, string(model.StagePublic))
	if r == nil {
		return ""
	}
	if who := passedBy(t); who != "" {
		return "Passed by " + who + " at " + shortSHA(t.PublicHead.Or("")) + " (public review had blocked it)."
	}
	return publicRun(r, t.PublicHead.Or(""))
}

// publicRun is one public review report in one sentence: a pass and the
// commit it read (head, "" when not known), the findings of a block, or a
// report it could not read.
func publicRun(r *report, head string) string {
	n := len(findingsOf(r))
	switch {
	case r == nil:
		return ""
	case r.str("verdict") == "pass" && n == 0:
		if head != "" {
			return "Pass: nothing private in what it would publish at " + shortSHA(head) + "."
		}
		return "Pass: nothing private in what it would publish."
	case n > 0:
		return "Blocked: " + Plural(n, "finding") + " in what it would publish."
	}
	return "Its report could not be read."
}

// passedBy is the person whose word is public review's pass of the head
// the ticket holds, "" when that pass is the stage's own (or there is
// none): the last public_passed move, made by a person.
func passedBy(t *ticket.Ticket) string {
	if t.PublicHead.Or("") == "" {
		return ""
	}
	for i := len(t.History) - 1; i >= 0; i-- {
		if m, ok := t.History[i].Data.(ticket.Moved); ok && m.Reason == model.ReasonPublicPassed {
			if m.By == "factory" || m.By == "" {
				return ""
			}
			return m.By
		}
	}
	return ""
}

func shortSHA(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// publicFacts are public review's line and, while a person decides on a
// block, each finding as where and why.
func publicFacts(t *ticket.Ticket) []Fact {
	line := PublicLine(t)
	if line == "" {
		return nil
	}
	tone := ""
	if !strings.HasPrefix(line, "Pass") {
		tone = "bad"
	}
	f := []Fact{{Label: "Public review", Value: strings.TrimSuffix(line, "."), Tone: tone}}
	if ticket.PublicBlocked(t) {
		for _, x := range publicFindings(t) {
			f = append(f, Fact{Label: x.Where, Value: x.Why, Tone: "bad"})
		}
	}
	return f
}
