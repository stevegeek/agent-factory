package ticket

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Finding is one finding of a finding task (a bug hunt, a hygiene check) as
// its verifier left it: rewritten in the verifier's words, with its verdict
// and the verifier's own evidence, and, once filed, the ticket it became.
type Finding struct {
	Title           string `json:"title"`
	Severity        string `json:"severity"` // low, medium or high
	Kind            string `json:"kind"`     // bug, hygiene, compat, security, docs or other
	BehaviourChange bool   `json:"behaviour_change"`
	Verdict         string `json:"verdict"`             // VerdictConfirmed, VerdictNotReproduced or VerdictDuplicate
	Duplicate       string `json:"duplicate,omitempty"` // what it duplicates: "T0042", "PR #27"
	Evidence        string `json:"evidence"`            // what the verifier ran and saw
	Body            string `json:"body"`                // the ticket's body, ready to file
	Filed           string `json:"filed,omitempty"`     // the ticket it was filed as
	FiledAs         string `json:"filed_as,omitempty"`  // model.KindChange or model.KindIntent
}

// FindingRef is, on a ticket filed from a finding, the hunt it came from
// and the finding's number (1-based).
type FindingRef struct {
	Ticket string `json:"ticket"`
	N      int    `json:"n"`
}

// The verifier's verdicts.
const (
	VerdictConfirmed     = "confirmed"
	VerdictNotReproduced = "not_reproduced"
	VerdictDuplicate     = "duplicate"
)

// Confirmed reports a finding the verifier reproduced on the base branch
// and found nowhere else on the board or the tracker.
func (f Finding) Confirmed() bool { return f.Verdict == VerdictConfirmed }

// DefaultKind is how a finding is filed unless the person says otherwise:
// one that changes behaviour people rely on is designed first, as an
// intent; anything else is a change.
func (f Finding) DefaultKind() string {
	if f.BehaviourChange {
		return string(model.KindIntent)
	}
	return string(model.KindChange)
}

// Open reports a confirmed finding not filed yet: one still to decide.
func (f Finding) Open() bool { return f.Confirmed() && f.Filed == "" }

// OpenFindings counts the findings still to decide.
func OpenFindings(t *Ticket) int {
	n := 0
	for _, f := range t.Findings {
		if f.Open() {
			n++
		}
	}
	return n
}

// FileFinding is finding n (1-based) of hunt as a new ticket with id,
// filed as kind by by: a change goes to triage, an intent to ready, for its
// designer; both come in through the finding door, untrusted, so triage
// rewrites a change in its own words and every agent is told the text is
// data. The new ticket and the hunt are linked both ways, and the finding
// records what it became. The caller saves both; a finding that is not
// confirmed, or already filed, is an error and changes nothing.
func FileFinding(id ID, hunt *Ticket, n int, kind, by string, now time.Time) (*Ticket, error) {
	if n < 1 || n > len(hunt.Findings) {
		return nil, fmt.Errorf("%s has no finding %d", hunt.ID, n)
	}
	f := hunt.Findings[n-1]
	switch {
	case f.Filed != "":
		return nil, fmt.Errorf("finding %d of %s is filed already, as %s", n, hunt.ID, f.Filed)
	case !f.Confirmed():
		return nil, fmt.Errorf("finding %d of %s was not confirmed (%s)", n, hunt.ID, VerdictLabel(f))
	case kind != string(model.KindChange) && kind != string(model.KindIntent):
		return nil, fmt.Errorf("a finding is filed as a change or an intent, not %q", kind)
	}
	column := model.ColTriage
	if kind == string(model.KindIntent) {
		column = model.ColReady
	}
	c := New(id, NewIn{Project: hunt.Project, Title: textx.Prefix(f.Title, 120), Body: FindingBody(hunt, f),
		Door: string(model.DoorFinding), Column: string(column), By: by}, now)
	if kind == string(model.KindIntent) {
		c.Kind = Some(kind)
	}
	c.Parent = Some(string(hunt.ID))
	c.FoundIn = Some(FindingRef{Ticket: string(hunt.ID), N: n})

	hunt.Findings[n-1].Filed, hunt.Findings[n-1].FiledAs = string(id), kind
	hunt.Children = append(hunt.Children, string(id))
	as := "a change"
	if kind == string(model.KindIntent) {
		as = "an idea to design first"
	}
	Event(hunt, "filed", fmt.Sprintf("%s: finding %d, %s: %s", id, n, as, textx.Clip(f.Title, 120)), "",
		Filed{Ticket: string(id)}, now)
	return c, nil
}

// FindingBody is a filed finding's ticket body: the verifier's write-up,
// its evidence, and where it came from.
func FindingBody(hunt *Ticket, f Finding) string {
	var b strings.Builder
	b.WriteString(textx.RStrip(f.Body))
	if e := strings.TrimSpace(f.Evidence); e != "" {
		b.WriteString("\n\n## Verification\n\n" + e)
	}
	facts := []string{"Severity: " + orDash(f.Severity), orDash(f.Kind)}
	if f.BehaviourChange {
		facts = append(facts, "changes behaviour people may rely on")
	}
	fmt.Fprintf(&b, "\n\nFound by %s (%s) and confirmed by an independent verifier. %s.", hunt.ID, hunt.Title, strings.Join(facts, " · "))
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// VerdictLabel is a finding's verdict in words: "confirmed", "not
// reproduced", "duplicate of PR #27".
func VerdictLabel(f Finding) string {
	switch f.Verdict {
	case VerdictConfirmed:
		return "confirmed"
	case VerdictDuplicate:
		if f.Duplicate != "" {
			return "duplicate of " + f.Duplicate
		}
		return "duplicate"
	}
	return "not reproduced"
}

// CloseFindings closes a hunt once the person has chosen, or nothing is
// left to choose: done, with the tickets its findings became listed on the
// move (findings_filed), or with none (findings_dismissed). rule says which
// rule closed it, when no person did.
func CloseFindings(hunt *Ticket, by, rule string, now time.Time) {
	var filed []string
	for _, f := range hunt.Findings {
		if f.Filed != "" {
			filed = append(filed, f.Filed+" ("+f.FiledAs+")")
		}
	}
	if len(filed) == 0 {
		Move(hunt, string(model.ColDone), model.ReasonFindingsDismissed, "no findings filed", rule, by, now)
		return
	}
	Move(hunt, string(model.ColDone), model.ReasonFindingsFiled, "findings filed: "+strings.Join(filed, ", "), rule, by, now)
}

// findingsQuestion is the findings ask's question.
func findingsQuestion(t *Ticket) string {
	open := OpenFindings(t)
	title := "“" + t.Title + "”"
	if open == 0 {
		return "The findings of " + title + " are checked, with nothing left to file: close it?"
	}
	noun := "finding"
	if open != 1 {
		noun += "s"
	}
	return "File " + strconv.Itoa(open) + " confirmed " + noun + " of " + title + " as tickets?"
}
