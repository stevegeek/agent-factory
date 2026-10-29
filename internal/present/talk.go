package present

import (
	"regexp"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The Conversation tab: the same ticket as a thread. The request, each
// agent's report, your notes, questions and answers, in order, with a reply
// box at the bottom. A reply answers the agent's open question (its stage
// carries on at once) or else asks one; the answer comes back into the
// thread.

// Msg is one message of the thread.
type Msg struct {
	Who, Side, When string // Side: "me", "agent" or "system"
	Text            string // Markdown
	More            string // the whole text, when Text is its lead
	Ref             string // a related ticket
	Tone            string // "question": waits for your answer
}

// TalkView is the thread and its reply box.
type TalkView struct {
	T                *ticket.Ticket
	Msgs             []Msg
	Waiting          string
	Label, Button    string
	Sub, Placeholder string
	Question         bool
	Ask              *ticket.Ask // what the ticket asks now, nil for nothing
}

// Archive reads the report an agent run left beside its log
// (logs/<id>/<agent>/report.json), or nil. The ticket keeps only each
// stage's latest report. Conversation calls it only with an agent name of
// the factory's own form, and ignores more than MaxArchivedReport bytes.
type Archive func(id ticket.ID, agent string) []byte

// MaxArchivedReport is the largest archived report the thread reads.
const MaxArchivedReport = 1 << 20

// agentName is an agent's name as the dispatcher makes them: t0042-w1.
var agentName = regexp.MustCompile(`^t\d{4,}-[a-z]\d{1,3}$`) // re: go

// ValidAgentName reports an agent name of the factory's form, the only kind
// that may name a path under logs/.
func ValidAgentName(name string) bool { return agentName.MatchString(name) }

// questionPrefix is how an older note quoted the agent's question it
// answered.
const questionPrefix = "Your question: “"

// Conversation is t as a thread at now. all holds every ticket (an asked
// question's task); archive may be nil.
func Conversation(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket, archive Archive, now time.Time) *TalkView {
	return new(reader).conversation(t, all, archive, now)
}

func archivedReport(archive Archive, id ticket.ID, name string) *report {
	if archive == nil || !ValidAgentName(name) {
		return nil
	}
	data := archive(id, name)
	if data == nil || len(data) > MaxArchivedReport {
		return nil
	}
	return parseReport(data)
}

func (r *reader) conversation(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket, archive Archive, now time.Time) *TalkView {
	v := &TalkView{T: t, Label: "Reply", Button: "Send",
		Sub:         "An agent reads the ticket and your message and answers here, usually in a few minutes.",
		Placeholder: "Ask about it, or tell the agents something"}
	at := func(h ticket.Entry) string { return When(ParseTime(h.At), now) }
	opening := Msg{Who: "You", Side: "me", Text: "**" + t.Title + "**"}
	switch t.Door {
	case string(model.DoorGitHub):
		opening.Who, opening.Side = "GitHub issue", "system"
	case string(model.DoorClock):
		opening.Who, opening.Side = "Clock", "system"
	}
	if !t.Proposal.Present() && strings.TrimSpace(t.Body) != "" {
		opening.Text += "\n\n" + t.Body
	}
	if len(t.History) > 0 {
		opening.When = at(t.History[0])
	}
	v.Msgs = append(v.Msgs, opening)
	lastFinished := map[string]int{}
	for i, h := range t.History {
		if stage := r.stageOf(h); h.Event == "finished" && stage != "" {
			lastFinished[stage] = i
		}
	}
	note := 0
	for i, h := range t.History {
		switch h.Event {
		case "finished":
			stage, name, _, _ := r.runOf(h)
			if stage == string(model.StageVote) {
				continue // the panel stays blind; its scores are on the ticket
			}
			rep := archivedReport(archive, t.ID, name)
			if rep == nil && lastFinished[stage] == i {
				rep = reportOf(t, stage)
			}
			m := Msg{Who: RoleLabel(stage), Side: "agent", When: at(h)}
			m.Text, m.More, m.Tone = reportText(t, stage, rep, lastFinished[stage] == i)
			if m.Text == "" {
				m.Text = "Finished."
			}
			v.Msgs = append(v.Msgs, m)
		case "note":
			text := h.Detail
			if note < len(t.Notes) {
				n := t.Notes[note]
				text = n.Text
				switch {
				case n.Question.Or("") != "":
					// the answer to an agent's question, which the note's sentence quotes for the agents
					text = n.Answer.Or("")
				case strings.HasPrefix(text, questionPrefix):
					r.legacy++
					if _, ans, ok := strings.Cut(text, "” The maintainer's answer: "); ok {
						text = ans
					}
				}
			}
			note++
			v.Msgs = append(v.Msgs, Msg{Who: "You", Side: "me", When: at(h), Text: text})
		case "comment":
			// an agent's text: shown as theirs, never as the factory's or yours
			v.Msgs = append(v.Msgs, Msg{Who: firstNonEmpty(byOf(h), "Agent"), Side: "agent", When: at(h), Text: commentText(t, h)})
		case "question":
			// a board agent's question: to a person, or to another agent
			m := Msg{Who: firstNonEmpty(byOf(h), "Agent"), Side: "agent", When: at(h), Text: h.Detail}
			if q := boardQuestionOf(t, h); q != nil {
				m.Text = "**Question for " + q.Recipient + ":** " + q.Text
				if len(q.Options) > 0 {
					m.Text += "\n\nOptions: " + strings.Join(q.Options, " · ")
				}
				if q.Open() {
					m.Tone = "question"
				}
			}
			v.Msgs = append(v.Msgs, m)
		case "answer":
			m := Msg{Who: firstNonEmpty(byOf(h), "Someone"), Side: "agent", When: at(h), Text: h.Detail}
			if q := boardQuestionOf(t, h); q != nil {
				m.Text = q.Answer.Or(h.Detail)
				if q.ToPerson {
					m.Side = "me"
				}
			}
			v.Msgs = append(v.Msgs, m)
		case "asked":
			id, q := r.askOf(h)
			v.Msgs = append(v.Msgs, Msg{Who: "You", Side: "me", When: at(h), Text: q, Ref: id})
		case "answered":
			id, sum := r.askOf(h)
			m := Msg{Who: "Task agent", Side: "agent", When: at(h), Text: sum, Ref: id}
			if c := all[ticket.ID(id)]; c != nil && c.Result.Or("") != "" {
				if lead := reportOf(c, string(model.StageTask)).str("summary"); lead != "" {
					m.Text = lead
				}
				if m.Text != c.Result.Or("") {
					m.More = c.Result.Or("")
				}
			}
			v.Msgs = append(v.Msgs, m)
		case "move":
			m := r.moveOf(h)
			col, detail := m.To, m.Text
			// the question and its answer are in the thread already, and a hand move says nothing new
			if m.Is(model.ReasonAgentQuestion, strings.Contains(detail, " asks: ")) ||
				m.Is(model.ReasonQuestionAnswered, detail == "answered the agent's question") || (m.Human && detail == "by human") {
				continue
			}
			switch col {
			case string(model.ColPROpen), string(model.ColDone), string(model.ColRejected):
				text := threadColumnLabel(col)
				if detail != "" {
					text += ": " + detail
				}
				v.Msgs = append(v.Msgs, Msg{Who: "Factory", Side: "system", When: at(h), Text: sentence(text)})
			case string(model.ColNeedsHuman):
				if detail != "" {
					v.Msgs = append(v.Msgs, Msg{Who: "Factory", Side: "system", When: at(h), Text: sentence(detail)})
				}
			}
		case "memory":
			m := Msg{Who: "Memory", Side: "system", When: at(h), Text: h.Detail}
			if h.Rule != "" {
				m.Text += " **" + sentence(h.Rule) + "**"
				m.Tone = "question"
			}
			v.Msgs = append(v.Msgs, m)
		}
	}
	if a := AskFor(t); a != nil {
		v.Ask = a
		v.Waiting = Headline(t, a)
		if a.Kind == string(model.AskQuestion) {
			v.Question = true
			v.Label, v.Button = "Your answer", "Answer"
			v.Sub = "The agent carries on with your answer, usually within a few minutes."
			v.Placeholder = "Your answer to the agent's question"
			if q := Asked(t, a); q != nil {
				v.Sub = q.By + " gets your answer through the ticket API, the next time it looks."
			}
		}
	} else if ag, ok := agentOf(t); ok {
		v.Waiting = roleOf(ag.Stage) + " is working on it now."
		if ag.Stage == string(model.StagePublic) {
			v.Waiting = roleOf(ag.Stage) + " is " + PublicDoing + " now."
		}
	}
	return v
}

// reportText is what an agent's report says, as a message: its lead, and the
// whole text when there is more.
func reportText(t *ticket.Ticket, stage string, r *report, latest bool) (text, more, tone string) {
	if r == nil {
		return "", "", ""
	}
	if r.str("status") == "question" && r.str("question") != "" {
		return "**Question:** " + r.str("question"), r.str("summary"), "question"
	}
	text = r.str("summary")
	switch model.Stage(stage) {
	case model.StageTriage:
		if d := r.str("decision"); d != "" {
			text = "**" + CapFirst(d) + ".** " + r.str("reason")
		}
	case model.StageReview:
		if vd := r.str("verdict"); vd != "" {
			text = "**" + CapFirst(vd) + ".** " + text
		}
	case model.StagePublic:
		// this run's own report; the head it read is known only for the
		// latest run, while no later one has started
		head := ""
		if a, running := agentOf(t); latest && (!running || a.Stage != string(model.StagePublic)) {
			head = t.HeadBeforePublic.Or("")
		}
		text = strings.TrimSpace(publicRunFound(r, head) + " " + text)
	case model.StageTask:
		res := r.str("result")
		if res == "" && latest {
			res = t.Result.Or("")
		}
		if res != "" && res != text {
			more = res
		}
	case model.StageDesign:
		if d := t.Design.Or(""); latest && d != "" {
			more = d
		}
	}
	return text, more, ""
}

// threadColumnLabel is how the thread says a ticket arrived in a column.
func threadColumnLabel(col string) string {
	switch col {
	case string(model.ColPROpen):
		return "PR opened"
	case string(model.ColDone):
		return "Closed as done"
	case string(model.ColRejected):
		return "Dropped"
	}
	return ColumnLabel(col)
}

// When is a short time for a message: the time today, else the day and the
// time.
func When(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	at = at.Local()
	if y, m, d := at.Date(); y == now.Local().Year() && m == now.Local().Month() && d == now.Local().Day() {
		return at.Format("15:04")
	}
	return at.Format("2 Jan 15:04")
}
