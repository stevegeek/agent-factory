package main

import (
	"strings"
	"sync/atomic"
)

// The dispatcher types its history: `type` and the facts beside it (EVENT_TYPES in bin/factory, in model.json).
// Boards from before that have only the sentence in `detail`; these readers take the fields when they are
// there and read the old wording otherwise. fallbacks counts the second kind, so a test can show that typed
// history never needs it.
var fallbacks atomic.Int64

func fellBack() { fallbacks.Add(1) }

// runOf: a start or finished event's stage, agent and host ("" for the dispatcher's own host).
func runOf(h Hist) (stage, agent, host string, ok bool) {
	switch {
	case h.Type == "started" || h.Type == "finished":
		host = h.Host
		if host == "local" {
			host = ""
		}
		return h.Stage, h.Agent, host, h.Stage != "" && h.Agent != ""
	case h.Event == "start":
		fellBack()
		if m := startDetail.FindStringSubmatch(h.Detail); m != nil {
			return m[1], m[2], m[3], true
		}
	case h.Event == "finished":
		fellBack()
		stage, agent, _ := strings.Cut(h.Detail, " ")
		return stage, agent, "", stage != "" && agent != ""
	}
	return "", "", "", false
}

// stageOf: the stage of a start or finished event, else "".
func stageOf(h Hist) string {
	if h.Event != "start" && h.Event != "finished" {
		return ""
	}
	stage, _, _, _ := runOf(h)
	return stage
}

type moveFacts struct {
	From, To, Reason string
	Text             string // the words after the columns: a summary, a PR link, a person's note
	Human            bool   // moved by hand (factory move)
}

func moveOf(h Hist) moveFacts {
	head, text, _ := strings.Cut(h.Detail, ": ")
	if h.Type == "moved" {
		return moveFacts{From: h.From, To: h.To, Reason: h.Reason, Text: text, Human: h.Reason == "by_human"}
	}
	fellBack()
	from, to, _ := strings.Cut(head, " -> ")
	return moveFacts{From: from, To: to, Text: text, Human: strings.HasPrefix(text, "by human")}
}

// is: the typed reason when there is one, else what the old wording said.
func (m moveFacts) is(reason string, worded bool) bool {
	if m.Reason != "" {
		return m.Reason == reason
	}
	return worded
}

// askOf: the task an asked or answered event names, and its question or answer.
func askOf(h Hist) (id, text string) {
	if h.Type == "asked" || h.Type == "answered" {
		return h.Ticket, h.Text
	}
	fellBack()
	id, text, _ = strings.Cut(h.Detail, ": ")
	return id, text
}

// panelDecided: the prospect's move that revealed the panel's scores.
func panelDecided(h Hist) bool {
	return h.Event == "move" && moveOf(h).is("panel_done", strings.Contains(h.Detail, "panel kept"))
}
