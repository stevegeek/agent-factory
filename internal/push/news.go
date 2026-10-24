package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Snap is the part of a ticket whose change might be news.
type Snap struct {
	Column string `json:"c"`
	Ask    string `json:"a,omitempty"`
	Result string `json:"r,omitempty"`
}

// SnapOf is what the watcher remembers of t.
func SnapOf(t *ticket.Ticket) Snap {
	s := Snap{Column: t.Column, Result: t.Result.Or("")}
	if a := present.AskFor(t); a != nil {
		s.Ask = a.Kind + " " + a.Since
	}
	return s
}

// askedBy is the ticket whose Ask filed this task, or nil (a task with a
// parent is not always a question).
func askedBy(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) *ticket.Ticket {
	p := all[ticket.ID(t.Parent.Or(""))]
	if p == nil {
		return nil
	}
	for _, h := range p.History {
		if h.Event != "asked" {
			continue
		}
		if id, _ := present.AskOf(h); id == string(t.ID) {
			return p
		}
	}
	return nil
}

// mover is the person whose own doing the ticket's last move was, "" when
// it was the factory's: a person's move names who made it (older history,
// which names nobody, is the maintainer's); a merge on the go-ahead does
// not say whose, so it stays the maintainer's.
func mover(t *ticket.Ticket, maintainer string) string {
	for i := len(t.History) - 1; i >= 0; i-- {
		h := t.History[i]
		if h.Event != "move" {
			continue
		}
		switch {
		case present.MoveOf(h).Human:
			if m, ok := h.Data.(ticket.Moved); ok && m.By != "" {
				return m.By
			}
			return maintainer
		case strings.HasSuffix(h.Detail, "on your go-ahead"):
			return maintainer
		}
		return ""
	}
	return ""
}

// News compares the board with what was seen before: a new ask on the
// desk, the answer to a question you asked, and a PR ticket that closed
// without you (merged or closed on GitHub). What a person did themselves
// is not news to their own devices, only to everyone else's (D19);
// maintainer is the maintainer's name on the board, the first of [board]
// people. On a board ticket only an agent's new question to a person is.
// More than three items the maintainer hears become one summary for them,
// each item still going to other people's devices.
func News(prev map[string]Snap, ts []*ticket.Ticket, maintainer string) []Message {
	all := present.ByID(ts)
	desk := present.Waiting(present.Desk(ts, present.Viewer{By: maintainer, Maintainer: true}))
	var out []Message
	for _, t := range ts {
		id := string(t.ID)
		if t.Board() {
			was, seen := prev[id]
			if now := SnapOf(t); now.Ask != "" && (!seen || was.Ask != now.Ask) {
				out = append(out, boardQuestion(t))
			}
			continue
		}
		was, seen := prev[id]
		now := SnapOf(t)
		if seen && was == now {
			continue
		}
		result := t.Result.Or("")
		switch p := askedBy(t, all); {
		case t.KindName() == string(model.KindTask) && p != nil && result != "" && (!seen || was.Result != result):
			out = append(out, Message{Title: "Answer to your question on " + string(p.ID), Body: present.Clip(result, 220),
				URL: "/t/" + string(p.ID), Tag: id, Project: p.Project})
		case now.Ask != "" && (!seen || was.Ask != now.Ask):
			a := present.AskFor(t)
			m := Message{Title: present.KindLabel(a.Kind) + ": " + present.Clip(present.Headline(t, a), 80), Body: present.Clip(t.Title, 160),
				URL: "/t/" + id, Tag: id, Project: t.Project, by: mover(t, maintainer)}
			if a.Kind == string(model.AskQuestion) {
				// an agent's question opens the conversation, reply box ready
				m.Title, m.URL = id+" asks: "+present.Clip(a.Question, 90), "/t/"+id+"/talk#reply"
			}
			out = append(out, m)
		case seen && model.Closed(t.Column) && was.Column != t.Column && t.PR.Present():
			pr, _ := t.PR.Get()
			verb := "closed"
			if t.Column == string(model.ColDone) {
				verb = "done"
			}
			out = append(out, Message{Title: fmt.Sprintf("%s %s: PR #%d", id, verb, pr.Number), Body: present.Clip(t.Title, 160),
				URL: "/t/" + id, Tag: id, Project: t.Project, by: mover(t, maintainer)})
		}
	}
	var heard []Message // what the maintainer's devices hear
	for i := range out {
		out[i].Badge = desk
		if out[i].by != maintainer {
			heard = append(heard, out[i])
		}
	}
	if len(heard) > 3 {
		batch := []Message{{Title: fmt.Sprintf("%d updates from the factory", len(heard)), Body: heard[0].Title + "; " + heard[1].Title + "; …",
			URL: "/", Tag: "batch", Badge: desk}}
		for _, m := range out {
			m.forOthers = true
			batch = append(batch, m)
		}
		return batch
	}
	return out
}

// boardQuestion is the news of an agent's question on a board ticket: who
// asks, on which project, opening the conversation with the reply box.
func boardQuestion(t *ticket.Ticket) Message {
	id := string(t.ID)
	a := present.AskFor(t)
	who := "An agent"
	if q := present.Asked(t, a); q != nil {
		who = q.By
	}
	return Message{Title: who + " asks on " + t.Project + ": " + present.Clip(a.Question, 90), Body: id + " · " + present.Clip(t.Title, 160),
		URL: "/t/" + id + "/talk#reply", Tag: id, Project: t.Project}
}

// Board is the board as the watcher reads it (boardread.Snapshot).
type Board interface {
	Load() (*boardread.View, error)
}

// Watch reads the board every interval until ctx ends and pushes what is
// news; it always makes one pass first. What it saw is kept on disk, so a
// change made while Foreman was down is still announced; with nothing kept,
// the first read is the baseline. A board that reads as empty is a failed
// read, not every ticket gone: what was seen stays. Nothing is computed
// while no device listens.
func (p *Pusher) Watch(ctx context.Context, b Board, every time.Duration) {
	var prev map[string]Snap
	var last []byte
	if data, err := os.ReadFile(p.seenFile()); err == nil && json.Unmarshal(data, &prev) == nil {
		last = data
	} else {
		prev = nil
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		v, err := b.Load()
		if err != nil {
			p.logf("push: %v", err)
		}
		if ts := v.Tickets; err == nil && len(ts) > 0 {
			if prev != nil && p.Count() > 0 {
				for _, m := range News(prev, ts, p.maintainerName()) {
					p.Send(ctx, m)
				}
			}
			prev = make(map[string]Snap, len(ts))
			for _, t := range ts {
				prev[string(t.ID)] = SnapOf(t)
			}
			if data, err := json.Marshal(prev); err == nil && !bytes.Equal(data, last) {
				if err := fsx.WriteAtomic(p.seenFile(), data, 0o600); err != nil {
					p.logf("push: %v", err)
				} else {
					last = data
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
