package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The Conversation tab: the same ticket as a thread. The request, each agent's report, your notes,
// questions and answers, in order, with a reply box at the bottom. A reply answers the agent's open
// question (its stage carries on at once) or else asks one; the answer comes back into the thread.

type Msg struct {
	Who, Side, When string // Side: "me", "agent" or "system"
	Text            string // Markdown
	More            string // the whole text, when Text is its lead
	Ref             string // a related ticket
	Tone            string // "question": waits for your answer
}

type TalkView struct {
	T                *Ticket
	Msgs             []Msg
	Waiting          string
	Label, Button    string
	Sub, Placeholder string
	Question         bool
}

const questionPrefix = "Your question: “"

func (s *Server) talk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	ts := s.board.Load()
	t := byID(ts)[id]
	if t == nil {
		http.NotFound(w, r)
		return
	}
	root := ""
	if s.runner != nil {
		root = s.runner.Root
	}
	v := Conversation(t, byID(ts), root, s.now())
	s.render(w, r, "talk", &Page{Title: id + " · conversation", Tab: "desk", Talk: v}, ts)
}

// archivedReport: an agent run's own report, kept beside its log; the ticket keeps only each stage's latest.
func archivedReport(root, id, name string) Report {
	if root == "" || !agentName.MatchString(name) {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, "logs", id, name, "report.json"))
	if err != nil || len(data) > 1<<20 {
		return nil
	}
	var r Report
	if json.Unmarshal(data, &r) != nil {
		return nil
	}
	return r
}

func Conversation(t *Ticket, all map[string]*Ticket, root string, now time.Time) *TalkView {
	v := &TalkView{T: t, Label: "Reply", Button: "Send",
		Sub:         "An agent reads the ticket and your message and answers here, usually in a few minutes.",
		Placeholder: "Ask about it, or tell the agents something"}
	at := func(h Hist) string { return when(parseTime(h.At), now) }
	opening := Msg{Who: "You", Side: "me", Text: "**" + t.Title + "**"}
	switch t.Door {
	case "github":
		opening.Who, opening.Side = "GitHub issue", "system"
	case "clock":
		opening.Who, opening.Side = "Clock", "system"
	}
	if t.Proposal == nil && strings.TrimSpace(t.Body) != "" {
		opening.Text += "\n\n" + t.Body
	}
	if len(t.History) > 0 {
		opening.When = at(t.History[0])
	}
	v.Msgs = append(v.Msgs, opening)
	lastFinished := map[string]int{}
	for i, h := range t.History {
		if h.Event == "finished" {
			stage, _, _ := strings.Cut(h.Detail, " ")
			lastFinished[stage] = i
		}
	}
	note := 0
	for i, h := range t.History {
		switch h.Event {
		case "finished":
			stage, name, _ := strings.Cut(h.Detail, " ")
			if stage == "vote" {
				continue // the panel stays blind; its scores are on the ticket
			}
			rep := archivedReport(root, t.ID, name)
			if rep == nil && lastFinished[stage] == i {
				rep = t.report(stage)
			}
			m := Msg{Who: roles[stage], Side: "agent", When: at(h)}
			if m.Who == "" {
				m.Who = capFirst(stage)
			}
			m.Text, m.More, m.Tone = reportText(t, stage, rep, lastFinished[stage] == i)
			if m.Text == "" {
				m.Text = "Finished."
			}
			v.Msgs = append(v.Msgs, m)
		case "note":
			text := h.Detail
			if note < len(t.Notes) {
				text = t.Notes[note].Text
			}
			note++
			if strings.HasPrefix(text, questionPrefix) {
				if _, ans, ok := strings.Cut(text, "” The maintainer's answer: "); ok {
					text = ans
				}
			}
			v.Msgs = append(v.Msgs, Msg{Who: "You", Side: "me", When: at(h), Text: text})
		case "asked":
			id, q, _ := strings.Cut(h.Detail, ": ")
			v.Msgs = append(v.Msgs, Msg{Who: "You", Side: "me", When: at(h), Text: q, Ref: id})
		case "answered":
			id, sum, _ := strings.Cut(h.Detail, ": ")
			m := Msg{Who: "Task agent", Side: "agent", When: at(h), Text: sum, Ref: id}
			if c := all[id]; c != nil && c.Result != "" {
				if lead := c.report("task").str("summary"); lead != "" {
					m.Text = lead
				}
				if m.Text != c.Result {
					m.More = c.Result
				}
			}
			v.Msgs = append(v.Msgs, m)
		case "move":
			_, rest, _ := strings.Cut(h.Detail, " -> ")
			col, detail, _ := strings.Cut(rest, ": ")
			if strings.Contains(detail, " asks: ") || detail == "answered the agent's question" || detail == "by human" {
				continue
			}
			switch col {
			case "pr_open", "done", "rejected":
				text := ticketColumnLabelFor(col)
				if detail != "" {
					text += ": " + detail
				}
				v.Msgs = append(v.Msgs, Msg{Who: "Factory", Side: "system", When: at(h), Text: sentence(text)})
			case "needs_human":
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
		v.Waiting = Headline(t, a)
		if a.Kind == "question" {
			v.Question = true
			v.Label, v.Button = "Your answer", "Answer"
			v.Sub = "The agent carries on with your answer, usually within a few minutes."
			v.Placeholder = "Your answer to the agent's question"
		}
	} else if t.Agent != nil {
		v.Waiting = roles[t.Agent.Stage] + " is working on it now."
	}
	return v
}

// reportText: what an agent's report says, as a message: its lead, the whole text when there is more.
func reportText(t *Ticket, stage string, r Report, latest bool) (text, more, tone string) {
	if r == nil {
		return "", "", ""
	}
	if r.str("status") == "question" && r.str("question") != "" {
		return "**Question:** " + r.str("question"), r.str("summary"), "question"
	}
	text = r.str("summary")
	switch stage {
	case "triage":
		if d := r.str("decision"); d != "" {
			text = "**" + capFirst(d) + ".** " + r.str("reason")
		}
	case "review":
		if vd := r.str("verdict"); vd != "" {
			text = "**" + capFirst(vd) + ".** " + text
		}
	case "task":
		res := r.str("result")
		if res == "" && latest {
			res = t.Result
		}
		if res != "" && res != text {
			more = res
		}
	case "design":
		if latest && t.Design != "" {
			more = t.Design
		}
	}
	return text, more, ""
}

func ticketColumnLabelFor(col string) string {
	switch col {
	case "pr_open":
		return "PR opened"
	case "done":
		return "Closed as done"
	case "rejected":
		return "Dropped"
	}
	return columnLabel(col)
}

// when: a short time for a message: the time today, else the day.
func when(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	at = at.Local()
	if y, m, d := at.Date(); y == now.Local().Year() && m == now.Local().Month() && d == now.Local().Day() {
		return at.Format("15:04")
	}
	return at.Format("2 Jan 15:04")
}
