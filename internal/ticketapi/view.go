package ticketapi

import (
	"sort"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/mdout"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// member builds one key of an answer.
func member(k string, v jsonx.Value) jsonx.Member { return jsonx.Member{Key: k, Value: v} }

// columnLabel is column_label(): a board or factory column's label, else
// the column itself.
func columnLabel(column jsonx.Value) jsonx.Value {
	s, ok := column.Str()
	if !ok {
		return column
	}
	return jstr(model.ColumnLabel(s))
}

// factoryFacts is factory_facts(): a factory ticket's column and PR.
func factoryFacts(tr jsonx.Value) jsonx.Value {
	pr := jsonx.Null()
	if p := tr.Get("pr"); p.Kind() == jsonx.KindObject && jsonx.Truthy(p.Get("number")) {
		pr = jsonx.ObjectOf(member("number", p.Get("number")), member("url", p.Get("url")))
	}
	return jsonx.ObjectOf(member("column", tr.Get("column")), member("column_label", columnLabel(tr.Get("column"))), member("pr", pr))
}

// summary is summary(): a ticket's line in a list, from its index entry.
func summary(id string, e boardread.Entry) jsonx.Value {
	ev := e.Object().Value()
	return jsonx.ObjectOf(member("id", jstr(id)), member("project", ev.Get("project")), member("kind", ev.Get("kind")),
		member("title", ev.Get("title")), member("column", ev.Get("column")), member("assignee", ev.Get("assignee")),
		member("labels", or(ev.Get("labels"), emptyList())), member("updated", ev.Get("updated")),
		member("waiting_reason", ev.Get("waiting_reason")), member("open_questions", getOr(ev, "open_questions", jsonx.Int(0))))
}

// summaryOf is summary_of(): the summary of a ticket in hand.
func summaryOf(t *ticket.Ticket) jsonx.Value {
	tr := t.Tree()
	return summary(sval(tr.Get("id")), boardread.EntryOf(tr))
}

// conversation is conversation(): comments, questions, answers, results
// and (on a factory ticket) the maintainer's notes, in time order; things
// said within the same second keep their order in the history.
func (cx *call) conversation(tr jsonx.Value) jsonx.Value {
	kinds := map[string]string{"commented": "comment", "question_asked": "question", "question_answered": "answer", "noted": "note"}
	pos := map[string]int{}
	noteBy := map[string]jsonx.Value{}
	for i, h := range list(tr.Get("history")) {
		k := kinds[sval(h.Get("type"))]
		if k == "" || h.Get("type").Kind() != jsonx.KindString {
			continue
		}
		at := h.Get("n")
		if k == "note" {
			at = h.Get("note")
			noteBy[pyKey(at)] = h.Get("by")
		}
		pos[k+"\x00"+pyKey(at)] = i
	}
	type said struct {
		at, by, typ, text, n jsonx.Value
		hasN                 bool
		pos                  int
	}
	place := func(k string, v jsonx.Value) int {
		if p, ok := pos[k+"\x00"+pyKey(v)]; ok {
			return p
		}
		return -1
	}
	var out []said
	for _, c := range list(tr.Get("comments")) {
		if c.Kind() != jsonx.KindObject {
			continue
		}
		out = append(out, said{at: c.Get("at"), by: c.Get("by"), typ: getOr(c, "type", jstr("comment")), text: getOr(c, "text", jstr("")),
			n: c.Get("n"), hasN: true, pos: place("comment", c.Get("n"))})
	}
	for _, q := range list(tr.Get("questions")) {
		if q.Kind() != jsonx.KindObject {
			continue
		}
		out = append(out, said{at: q.Get("at"), by: q.Get("by"), typ: jstr("question"), text: getOr(q, "text", jstr("")),
			n: q.Get("n"), hasN: true, pos: place("question", q.Get("n"))})
		if !q.Get("answer").IsNull() {
			out = append(out, said{at: q.Get("answered_at"), by: q.Get("answered_by"), typ: jstr("answer"), text: q.Get("answer"),
				n: q.Get("n"), hasN: true, pos: place("answer", q.Get("n"))})
		}
	}
	if !isStr(tr.Get("mode"), string(model.ModeBoard)) {
		for i, n := range list(tr.Get("notes")) {
			if n.Kind() != jsonx.KindObject {
				continue
			}
			by := or(noteBy[pyKey(jsonx.Int(int64(i)))], jstr(cx.cfg.Maintainer()))
			out = append(out, said{at: n.Get("at"), by: by, typ: jstr("note"), text: getOr(n, "text", jstr("")),
				pos: place("note", jsonx.Int(int64(i)))})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := pyStrOr(out[i].at), pyStrOr(out[j].at)
		if a != b {
			return a < b
		}
		return out[i].pos < out[j].pos
	})
	var items []jsonx.Value
	for _, s := range out {
		o := jsonx.Object{}
		for _, m := range []jsonx.Member{member("at", s.at), member("by", s.by), member("type", s.typ), member("text", s.text)} {
			if !m.Value.IsNull() || m.Key == "text" {
				o = append(o, m)
			}
		}
		if s.hasN && !s.n.IsNull() {
			o = append(o, member("n", s.n))
		}
		items = append(items, o.Value())
	}
	return jsonx.ArrayOf(items...)
}

// view is ticket_view(): the API's shape of a ticket, never the raw file.
// On a factory ticket the reports, gate and security findings, drafts and
// votes stay out: they are not for project agents. Children and linked
// tickets the caller cannot see are left out or shown bare.
func (cx *call) view(t *ticket.Ticket) jsonx.Value {
	tr := t.Tree()
	board := isStr(tr.Get("mode"), string(model.ModeBoard))
	var children []jsonx.Value
	for _, cid := range list(or(tr.Get("children"), emptyList())) {
		c := cx.tryLoad(cid)
		if c != nil && cx.c.seesTicket(c) {
			ct := c.Tree()
			children = append(children, jsonx.ObjectOf(member("id", ct.Get("id")), member("title", ct.Get("title")),
				member("column", ct.Get("column"))))
		}
	}
	var links []jsonx.Value
	firstFactory := jsonx.Null()
	found := false
	for _, l := range list(or(tr.Get("links"), emptyList())) {
		o, isObj := l.Object()
		if !isObj {
			links = append(links, l)
			continue
		}
		o = o.Clone()
		if jsonx.Truthy(o.Get("ticket")) {
			other := cx.tryLoad(o.Get("ticket"))
			if other != nil && cx.c.seesTicket(other) && !isStr(other.Tree().Get("mode"), string(model.ModeBoard)) {
				facts, _ := factoryFacts(other.Tree()).Object()
				for _, m := range facts {
					o.Set(m.Key, m.Value)
				}
				if isStr(o.Get("rel"), "factory") && !found {
					firstFactory, found = factoryFacts(other.Tree()), true
				}
			}
		}
		links = append(links, o.Value())
	}
	hist := list(getOr(tr, "history", emptyList()))
	if n := cx.lim.History; len(hist) > int(n) {
		hist = hist[len(hist)-int(n):]
	}
	var history []jsonx.Value
	for _, h := range hist {
		if h.Kind() != jsonx.KindObject {
			continue
		}
		item := jsonx.Object{member("at", h.Get("at"))}
		if jsonx.Truthy(h.Get("type")) {
			item = append(item, member("type", h.Get("type")))
		} else {
			item = append(item, member("event", h.Get("event")))
		}
		for _, k := range []string{"by", "from", "to", "reason"} {
			if v := h.Get(k); !v.IsNull() {
				item.Set(k, v)
			}
		}
		item.Set("detail", getOr(h, "detail", jstr("")))
		history = append(history, item.Value())
	}
	kind := jstr("change")
	if board {
		kind = jstr("item")
	}
	facts := factoryFacts(tr)
	if board {
		facts = firstFactory
	}
	return jsonx.ObjectOf(
		member("id", tr.Get("id")), member("project", tr.Get("project")), member("mode", getOr(tr, "mode", jstr("factory"))),
		member("kind", or(tr.Get("kind"), kind)), member("title", tr.Get("title")), member("body", or(tr.Get("body"), jstr(""))),
		member("column", tr.Get("column")), member("column_label", columnLabel(tr.Get("column"))),
		member("assignee", tr.Get("assignee")), member("labels", or(tr.Get("labels"), emptyList())), member("parent", tr.Get("parent")),
		member("children", jsonx.ArrayOf(children...)), member("links", jsonx.ArrayOf(links...)),
		member("created", tr.Get("created")), member("updated", tr.Get("updated")), member("by", tr.Get("by")),
		member("waiting", tr.Get("waiting")), member("questions", or(tr.Get("questions"), emptyList())),
		member("conversation", cx.conversation(tr)), member("result", tr.Get("result")),
		member("history", jsonx.ArrayOf(history...)), member("factory", facts),
	)
}

// written is written_markdown(): a write's answer for an agent (MCP), the
// ticket as Markdown (after a note, the conversation's tail) with only the
// facts a reply is worded from left in "ticket".
func (cx *call) written(verb string, out jsonx.Object) jsonx.Object {
	v := out.Get("ticket")
	md := mdout.Written(verb, mdView(v), cx.mdOpts())
	slim := jsonx.Object{}
	for _, k := range []string{"id", "project", "mode", "column", "column_label", "assignee"} {
		slim = append(slim, member(k, v.Get(k)))
	}
	res := out.Clone()
	res.Set("ticket", slim.Value())
	res.Set("markdown", jstr(md))
	return res
}

func (cx *call) mdOpts() mdout.Opts {
	return mdout.Opts{Me: cx.c.name, Today: cx.now(), MaxBytes: cx.lim.Markdown, Conversation: cx.lim.Conversation}
}

// mdView is the view as mdout renders it: str() of what the Markdown
// prints, and "" for what it tests the truth of.
func mdView(v jsonx.Value) mdout.View {
	ifTrue := func(x jsonx.Value) string {
		if jsonx.Truthy(x) {
			return jsonx.PyStr(x)
		}
		return ""
	}
	pr := func(x jsonx.Value) *mdout.PRRef {
		if !jsonx.Truthy(x) {
			return nil
		}
		return &mdout.PRRef{Number: jsonx.PyStr(x.Get("number")), URL: pyStrOr(x.Get("url"))}
	}
	out := mdout.View{ID: jsonx.PyStr(v.Get("id")), Title: pyStrOr(v.Get("title")), Project: ifTrue(v.Get("project")),
		Kind: ifTrue(v.Get("kind")), Mode: jsonx.PyStr(v.Get("mode")), ColumnLabel: jsonx.PyStr(v.Get("column_label")),
		Assignee: ifTrue(v.Get("assignee")), Parent: ifTrue(v.Get("parent")), Body: pyStrOr(v.Get("body")),
		Result: ifTrue(v.Get("result"))}
	for _, l := range list(v.Get("labels")) {
		out.Labels = append(out.Labels, jsonx.PyStr(l))
	}
	for _, c := range list(v.Get("children")) {
		out.Children = append(out.Children, mdout.Child{ID: jsonx.PyStr(c.Get("id")), Column: jsonx.PyStr(c.Get("column"))})
	}
	for _, l := range list(v.Get("links")) {
		out.Links = append(out.Links, mdout.Link{Rel: jsonx.PyStr(l.Get("rel")), URL: ifTrue(l.Get("url")), Title: ifTrue(l.Get("title")),
			Ticket: jsonx.PyStr(l.Get("ticket")), ColumnLabel: ifTrue(l.Get("column_label")), PR: pr(l.Get("pr"))})
	}
	if f := v.Get("factory"); jsonx.Truthy(f) {
		out.Factory = &mdout.Facts{PR: pr(f.Get("pr"))}
	}
	if w := v.Get("waiting"); jsonx.Truthy(w) {
		out.Waiting = &mdout.Waiting{Reason: jsonx.PyStr(w.Get("reason")), Question: jsonx.PyStr(w.Get("question")),
			Since: pyStrOr(w.Get("since")), Text: pyStrOr(w.Get("text"))}
	}
	for _, q := range list(v.Get("questions")) {
		mq := mdout.Question{N: jsonx.PyStr(q.Get("n")), By: jsonx.PyStr(q.Get("by")), Recipient: jsonx.PyStr(q.Get("recipient")),
			At: pyStrOr(q.Get("at")), Text: pyStrOr(q.Get("text")), Withdrawn: jsonx.Truthy(q.Get("withdrawn")),
			AnsweredBy: jsonx.PyStr(q.Get("answered_by")), AnsweredAt: pyStrOr(q.Get("answered_at"))}
		if jsonx.Truthy(q.Get("options")) {
			for _, o := range list(q.Get("options")) {
				mq.Options = append(mq.Options, jsonx.PyStr(o))
			}
		}
		if a := q.Get("answer"); !a.IsNull() {
			s := jsonx.PyStr(a)
			mq.Answer = &s
		}
		out.Questions = append(out.Questions, mq)
	}
	for _, c := range list(v.Get("conversation")) {
		out.Conversation = append(out.Conversation, mdout.Said{Type: jsonx.PyStr(c.Get("type")), N: jsonx.PyStr(c.Get("n")),
			At: pyStrOr(c.Get("at")), By: jsonx.PyStr(c.Get("by")), Text: pyStrOr(c.Get("text"))})
	}
	return out
}

// mdSummary is a summary as mdout's list line reads it.
func mdSummary(s jsonx.Value) mdout.Summary {
	out := mdout.Summary{ID: jsonx.PyStr(s.Get("id")), Column: jsonx.PyStr(s.Get("column")), Title: jsonx.PyStr(s.Get("title"))}
	if jsonx.Truthy(s.Get("assignee")) {
		out.Assignee = jsonx.PyStr(s.Get("assignee"))
	}
	if jsonx.Truthy(s.Get("waiting_reason")) {
		out.WaitingReason = jsonx.PyStr(s.Get("waiting_reason"))
	}
	for _, l := range list(s.Get("labels")) {
		out.Labels = append(out.Labels, jsonx.PyStr(l))
	}
	if n, ok := s.Get("open_questions").Int64(); ok {
		out.OpenQuestions = int(n)
	}
	return out
}
