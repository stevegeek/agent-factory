package ticketapi

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

// linkRels are LINK_RELS.
func linkRels() []string { return []string{"pr", "issue", "url", "factory", "origin", "related"} }

// boardColumns are BOARD_COLUMNS, in order.
func boardColumns() []string {
	var out []string
	for _, c := range model.BoardColumns() {
		out = append(out, string(c.Name))
	}
	return out
}

// moveCodes is MOVE_CODES: the reason a board move records.
func moveCode(to string) model.Reason {
	switch to {
	case "inbox", "next":
		return model.ReasonParked
	case "doing":
		return model.ReasonStarted
	case "waiting":
		return model.ReasonWaiting
	case "done":
		return model.ReasonFinished
	}
	return model.ReasonDropped
}

func closedBoard(col string) bool {
	return col == string(model.BoardDone) || col == string(model.BoardDropped)
}

// validLabel is LABEL.fullmatch: ^[a-z0-9][a-z0-9-]{0,31}$.
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 32 || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validURL is re.fullmatch(r"https?://[^\s<>\"]+", url).
func validURL(s string) bool {
	rest, ok := strings.CutPrefix(s, "https://")
	if !ok {
		if rest, ok = strings.CutPrefix(s, "http://"); !ok {
			return false
		}
	}
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if textx.IsSpace(r) || r == '<' || r == '>' || r == '"' {
			return false
		}
	}
	return true
}

// cleanLabels is clean_labels(): sorted, unique, at most so many, each a
// label.
func (cx *call) cleanLabels(labels []string, field string) ([]string, error) {
	set := map[string]bool{}
	var out []string
	for _, l := range labels {
		if !set[l] {
			set[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	if len(out) > cx.lim.Labels {
		return nil, bad(field, "at most %d labels", cx.lim.Labels)
	}
	for _, l := range out {
		if !validLabel(l) {
			return nil, bad(field, "label %s: lower-case letters, digits and dashes, at most 32", repr(l))
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// onProject is clean_assignee's and recipient_of's test: a person, or a
// live token on the project.
func (cx *call) onProject(project, name, field string) (person bool, err error) {
	if contains(cx.cfg.People(), name) {
		return true, nil
	}
	ok := false
	if tokens.ValidName(name) {
		rec, live, err := cx.s.Tokens.Live(name)
		if err != nil {
			return false, err
		}
		ok = live && contains(rec.Projects(), project)
	}
	if !ok {
		return false, bad(field, "%s is not a person or an agent on %s", name, project)
	}
	return false, nil
}

// cleanParent is clean_parent(): a ticket the caller sees, on the same
// project, not under tid already, at most so deep.
func (cx *call) cleanParent(project string, pid jsonx.Value, tid string) (jsonx.Value, error) {
	if pid.IsNull() {
		return pid, nil
	}
	p := cx.tryLoad(pid)
	if p == nil || !cx.c.seesTicket(p) {
		return pid, bad("parent", "no ticket %s", jsonx.PyStr(pid))
	}
	if !isStr(p.Tree().Get("project"), project) {
		return pid, bad("parent", "a parent is on the same project")
	}
	seen := 0
	for cur := p; cur != nil; {
		tr := cur.Tree()
		if tid != "" && isStr(tr.Get("id"), tid) {
			return pid, bad("parent", "%s is under %s already; that would make a loop", jsonx.PyStr(pid), tid)
		}
		seen++
		if seen > cx.lim.Depth {
			return pid, bad("parent", "parents go at most %d deep", cx.lim.Depth)
		}
		cur = nil
		if jsonx.Truthy(tr.Get("parent")) {
			cur = cx.tryLoad(tr.Get("parent"))
		}
	}
	return pid, nil
}

// cleanLink is clean_link(): a link with a rel, and a URL or a ticket the
// caller sees.
func (cx *call) cleanLink(link jsonx.Value) (jsonx.Value, error) {
	o, err := take(link, []spec{{"rel", "str"}, {"url", "str"}, {"ticket", "str"}, {"title", "str"}}, "rel")
	if err != nil {
		return jsonx.Value{}, err
	}
	rel := sval(o.Get("rel"))
	if !contains(linkRels(), rel) {
		return jsonx.Value{}, bad("links", "rel is one of %s", strings.Join(linkRels(), ", "))
	}
	if has(link, "url") == has(link, "ticket") {
		return jsonx.Value{}, bad("links", "a link has a url or a ticket")
	}
	out := jsonx.Object{member("rel", jstr(rel))}
	if has(link, "url") {
		u := sval(o.Get("url"))
		if runes(u) > cx.lim.URL || !validURL(u) {
			return jsonx.Value{}, bad("links", "a link's url is http(s), at most 2 KiB")
		}
		out = append(out, member("url", jstr(u)))
	} else {
		t := cx.tryLoad(o.Get("ticket"))
		if t == nil || !cx.c.seesTicket(t) {
			return jsonx.Value{}, bad("links", "no ticket %s", jsonx.PyStr(o.Get("ticket")))
		}
		out = append(out, member("ticket", t.Tree().Get("id")))
	}
	if has(link, "title") {
		out = append(out, member("title", jstr(textx.Prefix(textx.Fold(sval(o.Get("title"))), cx.lim.LinkTitle))))
	}
	return out.Value(), nil
}

// cleanFields is clean_fields(): the one validator for what a caller may set
// on a ticket. The keys it gives back are those it checked, in its order:
// title, body, kind (null for a change), labels, assignee, parent, links.
func (cx *call) cleanFields(project string, fields jsonx.Object, creating bool, tid string) (jsonx.Object, error) {
	out := jsonx.Object{}
	mode, _ := cx.cfg.ProjectMode(project)
	board := mode == string(model.ModeBoard)
	if _, ok := fields.Lookup("title"); ok || creating {
		title := textx.Fold(pyStrOr(fields.Get("title")))
		if n := runes(title); n < 1 || n > cx.lim.Title {
			return nil, bad("title", "a title is 1 to %d characters", cx.lim.Title)
		}
		out = append(out, member("title", jstr(title)))
	}
	if v, ok := fields.Lookup("body"); ok {
		body, err := textIn(sval(v), "body", cx.lim.Body, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, member("body", jstr(body)))
	}
	if _, ok := fields.Lookup("kind"); ok || creating {
		def := "change"
		allowed := []string{"change"}
		if board {
			def, allowed = "item", nil
			for _, m := range model.Modes() {
				if m.Mode == model.ModeBoard {
					for _, k := range m.Kinds {
						allowed = append(allowed, string(k))
					}
				}
			}
		}
		kind := def
		if k := fields.Get("kind"); jsonx.Truthy(k) {
			kind = sval(k)
		}
		if !contains(allowed, kind) {
			return nil, bad("kind", "kind on %s is one of %s", project, strings.Join(allowed, ", "))
		}
		v := jsonx.Null()
		if kind != "change" {
			v = jstr(kind)
		}
		out = append(out, member("kind", v))
	}
	if v, ok := fields.Lookup("labels"); ok {
		labels, err := cx.cleanLabels(strsOf(v), "labels")
		if err != nil {
			return nil, err
		}
		out = append(out, member("labels", strs(labels)))
	}
	if v, ok := fields.Lookup("assignee"); ok {
		if !v.IsNull() {
			if _, err := cx.onProject(project, sval(v), "assignee"); err != nil {
				return nil, err
			}
		}
		out = append(out, member("assignee", v))
	}
	if v, ok := fields.Lookup("parent"); ok {
		p, err := cx.cleanParent(project, v, tid)
		if err != nil {
			return nil, err
		}
		out = append(out, member("parent", p))
	}
	if v, ok := fields.Lookup("links"); ok {
		if len(list(v)) > cx.lim.Links {
			return nil, bad("links", "at most %d links", cx.lim.Links)
		}
		var links []jsonx.Value
		for _, l := range list(v) {
			cl, err := cx.cleanLink(l)
			if err != nil {
				return nil, err
			}
			links = append(links, cl)
		}
		out = append(out, member("links", jsonx.ArrayOf(links...)))
	}
	return out, nil
}

// door is the door a caller files through: a person's, or an agent's
// token (untrusted: a person decides after triage).
func (c *caller) door() string {
	if c.person {
		return string(model.DoorHuman)
	}
	return string(model.DoorAgent)
}

// fileTicket is file_ticket(): a new ticket from a caller's fields, a board
// ticket into inbox, a factory one into triage.
func (cx *call) fileTicket(project string, fields jsonx.Object, key, dig string) (*ticket.Ticket, error) {
	f, err := cx.cleanFields(project, fields, true, "")
	if err != nil {
		return nil, err
	}
	extra := jsonx.Object{member("labels", getOr(f.Value(), "labels", emptyList())), member("links", getOr(f.Value(), "links", emptyList()))}
	after, err := cx.cleanAfter("", fields.Get("after"))
	if err != nil {
		return nil, err
	}
	if len(after) > 0 {
		extra = append(extra, member("after", afterStrs(after)))
	}
	if key != "" {
		extra = append(extra, member("idempotency", jsonx.ObjectOf(member("by", jstr(cx.c.name)), member("key", jstr(key)),
			member("digest", jstr(dig)))))
	}
	if p := f.Get("parent"); jsonx.Truthy(p) {
		extra = append(extra, member("parent", p))
	}
	in := ticket.NewIn{Project: project, Title: sval(f.Get("title")), Body: sval(getOr(f.Value(), "body", jstr(""))),
		Door: cx.c.door(), Column: "triage", By: cx.c.name, Fields: extra}
	if mode, _ := cx.cfg.ProjectMode(project); mode == string(model.ModeBoard) {
		in.Column, in.Mode = "inbox", model.ModeBoard
		in.Fields = append(jsonx.Object{member("kind", f.Get("kind"))}, extra...)
	}
	t, err := cx.s.Board.FileTicket(cx.ctx, in)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// adopt is adopt(): the parent, already locked, takes its new children.
func (cx *call) adopt(parent string, children []string) error {
	p, err := cx.s.Board.Reader.Load(ticket.ID(parent))
	if err != nil {
		return err
	}
	p.Children = uniq(append(append([]string{}, p.Children...), children...))
	ticket.Event(p, "edit", strings.Join(children, ", ")+" filed under it", "",
		ticket.Edited{By: cx.c.name, Fields: []string{"children"}}, cx.now())
	return cx.s.Board.Save(p)
}

// uniq is list(dict.fromkeys(ss)).
func uniq(ss []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// addComment is add_comment(): a comment or a result on a board ticket,
// within the ticket's caps.
func (cx *call) addComment(t *ticket.Ticket, text, kind string) error {
	if len(t.Comments) >= cx.lim.Comments {
		return refuse(TooMany, "", "%s has %d comments, the most a ticket holds", t.ID, cx.lim.Comments)
	}
	field, most := "text", cx.lim.Comment
	if kind != "comment" {
		field, most = "result", cx.lim.Result
	}
	if _, err := textIn(text, field, most, 1); err != nil {
		return err
	}
	total := len(text)
	n := 0
	for _, c := range t.Comments {
		total += len(c.Text)
		n = max(n, c.N)
	}
	if int64(total) > cx.lim.CommentBytes {
		return refuse(TooMany, "", "%s holds %d MiB of comments, the most a ticket holds", t.ID, cx.lim.CommentBytes>>20)
	}
	n++
	by := cx.c.name
	if t.Comments == nil {
		t.Comments = []ticket.Comment{}
	}
	t.Comments = append(t.Comments, ticket.Comment{N: n, At: cx.stamp(), By: by, Type: kind, Text: text})
	ticket.Event(t, "comment", by+": "+textx.Clip(text, 200), "", ticket.Commented{By: by, N: n}, cx.now())
	return nil
}

// setWaiting is t["waiting"] = {...}: Python puts a new object in, so a wait
// that replaces another takes its own key order, not the old one's.
func setWaiting(t *ticket.Ticket, w ticket.Waiting) {
	if t.Tree().Get("waiting").Kind() == jsonx.KindObject {
		t.Waiting = ticket.Nil[ticket.Waiting]()
		ticket.Encode(t)
	}
	t.Waiting = ticket.Some(w)
}

func (cx *call) move(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}, {"to", "str"}, {"reason", "str"}, {"result", "str"}}, "id", "to")
	if err != nil {
		return nil, err
	}
	first, err := cx.visible(o.Get("id"))
	if err != nil {
		return nil, err
	}
	if err := boardOnly(first, FactoryTicket); err != nil {
		return nil, err
	}
	if err := cx.c.need("work"); err != nil {
		return nil, err
	}
	to, why := sval(o.Get("to")), textx.Strip(pyStrOr(o.Get("reason")))
	result, hasResult := o.Get("result").Str()
	if !contains(boardColumns(), to) {
		return nil, bad("to", "to is one of %s", strings.Join(boardColumns(), ", "))
	}
	if runes(why) > cx.lim.Reason {
		return nil, bad("reason", "a reason is at most %d characters", cx.lim.Reason)
	}
	if (to == "waiting" || to == "dropped") && why == "" {
		return nil, bad("reason", "say why it goes to %s", model.ColumnLabel(to))
	}
	if hasResult && to != "done" && to != "waiting" {
		return nil, bad("result", "a result goes with a move to done or waiting")
	}
	if _, err := cx.writable(); err != nil {
		return nil, err
	}
	me := cx.c.name
	t, err := cx.changing(sval(o.Get("id")), func(t *ticket.Ticket) error {
		if err := cx.mayAct(t); err != nil {
			return err
		}
		if err := cx.roomy(t); err != nil {
			return err
		}
		old := t.Column
		if to == old && hasResult && !pyEq(jstr(result), saidLast(t, me, to)) {
			// a retry is fine; a different result would be dropped without a word
			return refuse(Conflict, "", "%s is in %s already; post_update to add to it", t.ID, model.ColumnLabel(to))
		}
		if to == old {
			return nil
		}
		if old == "waiting" {
			t.Waiting = ticket.Nil[ticket.Waiting]()
		}
		if to == "doing" && !jsonx.Truthy(t.Tree().Get("assignee")) {
			t.Assignee = ticket.Some(me)
		}
		if to == "waiting" {
			setWaiting(t, ticket.Waiting{Reason: "other", Text: why, From: old, Since: cx.stamp()})
		}
		if to == "done" && result != "" {
			if _, err := textIn(result, "result", cx.lim.Result, 1); err != nil {
				return err
			}
			t.Result = ticket.Some(result)
			if err := cx.addComment(t, result, "result"); err != nil {
				return err
			}
		}
		if to == "waiting" && result != "" {
			if _, err := textIn(result, "result", cx.lim.Comment, 1); err != nil {
				return err
			}
			if err := cx.addComment(t, result, "comment"); err != nil {
				return err
			}
		}
		if closedBoard(to) {
			// closing withdraws what it still asks: a Done ticket must not wait on anyone
			for i := range t.Questions {
				if t.Questions[i].Open() {
					t.Questions[i].Withdrawn = ticket.Some(cx.stamp())
				}
			}
		}
		reason := moveCode(to)
		if closedBoard(old) && !closedBoard(to) {
			reason = model.ReasonReopened
		}
		ticket.Move(t, to, reason, why, "", me, cx.now())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jsonx.Object{member("ticket", cx.view(t))}, nil
}

// mayAct is may_act(): a ticket someone else holds moves only for its
// holder, a planner or a person.
func (cx *call) mayAct(t *ticket.Ticket) error {
	a := t.Tree().Get("assignee")
	if !a.IsNull() && !isStr(a, cx.c.name) && !cx.c.person && !contains(cx.c.can, "plan") {
		return refuse(Conflict, "", "%s is assigned to %s", t.ID, jsonx.PyStr(a))
	}
	return nil
}

// saidLast is said_last(): what a move to column last left as its result,
// the ticket's result or my latest comment.
func saidLast(t *ticket.Ticket, me, column string) jsonx.Value {
	tr := t.Tree()
	if column == "done" {
		return tr.Get("result")
	}
	last := jsonx.Null()
	for _, c := range list(tr.Get("comments")) {
		if isStr(c.Get("by"), me) {
			last = c.Get("text")
		}
	}
	return last
}

func (cx *call) note(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}, {"text", "str"}}, "id", "text")
	if err != nil {
		return nil, err
	}
	if _, err := cx.visible(o.Get("id")); err != nil {
		return nil, err
	}
	if err := cx.c.need("talk"); err != nil {
		return nil, err
	}
	if _, err := cx.writable(); err != nil {
		return nil, err
	}
	t, err := cx.changing(sval(o.Get("id")), func(t *ticket.Ticket) error {
		if err := cx.roomy(t); err != nil {
			return err
		}
		return cx.addComment(t, sval(o.Get("text")), "comment")
	})
	if err != nil {
		return nil, err
	}
	return jsonx.Object{member("ticket", cx.view(t))}, nil
}

func (cx *call) claim(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}}, "id")
	if err != nil {
		return nil, err
	}
	first, err := cx.visible(o.Get("id"))
	if err != nil {
		return nil, err
	}
	if err := boardOnly(first, FactoryTicket); err != nil {
		return nil, err
	}
	if err := cx.c.need("work"); err != nil {
		return nil, err
	}
	if _, err := cx.writable(); err != nil {
		return nil, err
	}
	me := cx.c.name
	t, err := cx.changing(sval(o.Get("id")), func(t *ticket.Ticket) error {
		if err := cx.roomy(t); err != nil {
			return err
		}
		tr := t.Tree()
		a := tr.Get("assignee")
		switch {
		case t.Column == "doing" && isStr(a, me):
			// a retry by the holder
		case !a.IsNull() && !isStr(a, me):
			return refuse(Conflict, "", "%s is assigned to %s", t.ID, jsonx.PyStr(a))
		case t.Column == "waiting" && isStr(or(tr.Get("waiting"), jsonx.ObjectOf()).Get("reason"), "other"):
			// back from Waiting on something that was not a question (a question comes back with its answer)
			t.Assignee, t.Waiting = ticket.Some(me), ticket.Nil[ticket.Waiting]()
			ticket.Move(t, "doing", model.ReasonStarted, "resumed by "+me, "", me, cx.now())
		case t.Column == "waiting":
			return refuse(Conflict, "", "%s waits on a question; it comes back to Doing when that is answered", t.ID)
		case t.Column != "inbox" && t.Column != "next":
			return refuse(Conflict, "", "%s is in %s, not Inbox, Next or Waiting", t.ID, model.ColumnLabel(t.Column))
		case cx.openAfter(t) != "":
			return refuse(Conflict, "", "%s waits for %s to be done first: pick another ticket", t.ID, cx.openAfter(t))
		default:
			t.Assignee = ticket.Some(me)
			ticket.Event(t, "claim", "claimed by "+me, "", ticket.Claimed{By: me}, cx.now())
			ticket.Move(t, "doing", model.ReasonClaimed, "claimed by "+me, "", me, cx.now())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jsonx.Object{member("ticket", cx.view(t))}, nil
}

func (cx *call) update(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}, {"title", "str"}, {"body", "str"}, {"labels", "strs"}, {"add_labels", "strs"},
		{"remove_labels", "strs"}, {"parent", "str?"}, {"assignee", "str?"}, {"links", "list"}, {"after", "strs"}}, "id")
	if err != nil {
		return nil, err
	}
	first, err := cx.visible(o.Get("id"))
	if err != nil {
		return nil, err
	}
	if err := boardOnly(first, FactoryTicket); err != nil && !cx.afterOnly(o) {
		return nil, err
	}
	if err := cx.c.need("plan"); err != nil {
		return nil, err
	}
	reqParent, parentAsked := o.Lookup("parent")
	if parentAsked && !reqParent.IsNull() && !isTicketID(sval(reqParent)) {
		return nil, bad("parent", "no ticket %s", jsonx.PyStr(reqParent))
	}
	firstParent := first.Tree().Get("parent")
	ids := []string{string(first.ID)}
	if parentAsked {
		for _, p := range []jsonx.Value{firstParent, reqParent} {
			if jsonx.Truthy(p) && isTicketID(sval(p)) {
				ids = append(ids, sval(p))
			}
		}
	}
	if cx.s.afterRead != nil {
		cx.s.afterRead()
	}
	if _, err := cx.writable(); err != nil {
		return nil, err
	}
	unlock, err := cx.held(ids)
	if err != nil {
		return nil, err
	}
	defer unlock()
	t, err := cx.s.Board.Reader.Load(first.ID)
	if err != nil {
		return nil, err
	}
	if err := cx.roomy(t); err != nil {
		return nil, err
	}
	tr := t.Tree()
	if parentAsked && !pyEq(tr.Get("parent"), firstParent) {
		return nil, refuse(Conflict, "", "%s changed meanwhile; try again", t.ID)
	}
	asked := jsonx.Object{}
	for _, k := range []string{"title", "body", "labels", "assignee", "parent", "links"} {
		if v, ok := o.Lookup(k); ok {
			asked = append(asked, member(k, v))
		}
	}
	f, err := cx.cleanFields(sval(tr.Get("project")), asked, false, string(t.ID))
	if err != nil {
		return nil, err
	}
	base := strsOf(or(tr.Get("labels"), emptyList()))
	if v, ok := f.Lookup("labels"); ok {
		base = strsOf(v)
	}
	add, err := cx.cleanLabels(strsOf(o.Get("add_labels")), "add_labels")
	if err != nil {
		return nil, err
	}
	drop := strsOf(o.Get("remove_labels"))
	var keep []string
	for _, l := range append(base, add...) {
		if !contains(drop, l) {
			keep = append(keep, l)
		}
	}
	labels, err := cx.cleanLabels(keep, "labels")
	if err != nil {
		return nil, err
	}
	me := cx.c.name
	var changed []string
	var others []*ticket.Ticket
	linked := false
	for _, k := range []string{"title", "body", "assignee"} {
		v, ok := f.Lookup(k)
		if !ok || pyEq(v, tr.Get(k)) {
			continue
		}
		switch k {
		case "title":
			t.Title = sval(v)
		case "body":
			t.Body = sval(v)
		default:
			t.Assignee = ticket.Some(sval(v))
			if v.IsNull() {
				t.Assignee = ticket.Nil[string]()
			}
		}
		changed = append(changed, k)
	}
	if !pyEq(strs(labels), or(tr.Get("labels"), emptyList())) {
		t.Labels = labels
		changed = append(changed, "labels")
	}
	for _, l := range list(f.Get("links")) {
		known := false
		for _, x := range list(or(tr.Get("links"), emptyList())) {
			known = known || pyEq(l, x)
		}
		for _, x := range t.Links {
			known = known || pyEq(l, linkValue(x))
		}
		if known {
			continue
		}
		if len(t.Links) >= cx.lim.Links {
			return nil, bad("links", "at most %d links", cx.lim.Links)
		}
		link := ticket.Link{Rel: sval(l.Get("rel")), URL: sval(l.Get("url")), Ticket: sval(l.Get("ticket")), Title: sval(l.Get("title"))}
		t.Links = append(t.Links, link)
		linked = true
		target := link.URL
		if target == "" {
			target = link.Ticket
		}
		ticket.Event(t, "link", link.Rel+" "+target, "", ticket.Linked{By: me, Rel: link.Rel, Ticket: link.Ticket, URL: link.URL}, cx.now())
	}
	if np, ok := f.Lookup("parent"); ok && !pyEq(np, tr.Get("parent")) {
		for _, side := range []struct {
			pid jsonx.Value
			add bool
		}{{tr.Get("parent"), false}, {np, true}} {
			if !jsonx.Truthy(side.pid) || !isTicketID(sval(side.pid)) {
				continue
			}
			p, err := cx.s.Board.Reader.Load(ticket.ID(sval(side.pid)))
			if err != nil {
				return nil, err
			}
			kids := []string{}
			for _, c := range p.Children {
				if c != string(t.ID) {
					kids = append(kids, c)
				}
			}
			how := "moved from under"
			if side.add {
				kids, how = append(kids, string(t.ID)), "filed under"
			}
			p.Children = kids
			ticket.Event(p, "edit", string(t.ID)+" "+how+" it", "", ticket.Edited{By: me, Fields: []string{"children"}}, cx.now())
			others = append(others, p)
		}
		t.Parent = ticket.Some(sval(np))
		if np.IsNull() {
			t.Parent = ticket.Nil[string]()
		}
		changed = append(changed, "parent")
	}
	if v, ok := o.Lookup("after"); ok {
		after, err := cx.cleanAfter(string(t.ID), v)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(after, t.After) {
			t.After = after
			changed = append(changed, "after")
		}
	}
	if len(changed) > 0 {
		ticket.Event(t, "edit", me+" changed "+strings.Join(changed, ", "), "", ticket.Edited{By: me, Fields: changed}, cx.now())
	}
	if len(changed) > 0 || linked {
		if err := cx.s.Board.Save(t); err != nil {
			return nil, err
		}
	}
	for _, p := range others {
		if err := cx.s.Board.Save(p); err != nil {
			return nil, err
		}
	}
	return jsonx.Object{member("ticket", cx.view(t))}, nil
}

// linkValue is a typed link as the dict Python compares.
func linkValue(l ticket.Link) jsonx.Value {
	o := jsonx.Object{member("rel", jstr(l.Rel))}
	if l.URL != "" {
		o = append(o, member("url", jstr(l.URL)))
	}
	if l.Ticket != "" {
		o = append(o, member("ticket", jstr(l.Ticket)))
	}
	if l.Title != "" {
		o = append(o, member("title", jstr(l.Title)))
	}
	return o.Value()
}

func (cx *call) ask(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}, {"question", "str"}, {"options", "strs"}, {"recipient", "str"}}, "id", "question")
	if err != nil {
		return nil, err
	}
	first, err := cx.visible(o.Get("id"))
	if err != nil {
		return nil, err
	}
	if err := boardOnly(first, NotOnFactoryTicket); err != nil {
		return nil, err
	}
	if err := cx.c.need("talk"); err != nil {
		return nil, err
	}
	text, err := textIn(sval(o.Get("question")), "question", cx.lim.Question, 1)
	if err != nil {
		return nil, err
	}
	options := strsOf(o.Get("options"))
	bad6 := len(options) > cx.lim.Options
	for _, op := range options {
		bad6 = bad6 || runes(op) < 1 || runes(op) > cx.lim.Option
	}
	if bad6 {
		return nil, bad("options", "at most %d options, each 1 to %d characters", cx.lim.Options, cx.lim.Option)
	}
	if options == nil {
		options = []string{}
	}
	if _, err := cx.writable(); err != nil {
		return nil, err
	}
	me := cx.c.name
	var n int
	var recipient string
	t, err := cx.changing(sval(o.Get("id")), func(t *ticket.Ticket) error {
		if err := cx.roomy(t); err != nil {
			return err
		}
		recipient = cx.cfg.Maintainer()
		if r := o.Get("recipient"); jsonx.Truthy(r) {
			recipient = sval(r)
		}
		person, err := cx.onProject(sval(t.Tree().Get("project")), recipient, "recipient")
		if err != nil {
			return err
		}
		open := 0
		for _, q := range t.Questions {
			if q.Open() {
				open++
			}
		}
		if open >= cx.lim.OpenQuestions {
			return refuse(TooMany, "", "%s has %d open questions; wait for an answer", t.ID, cx.lim.OpenQuestions)
		}
		n = len(t.Questions) + 1
		now := cx.stamp()
		if t.Questions == nil {
			t.Questions = []ticket.BoardQuestion{}
		}
		t.Questions = append(t.Questions, ticket.BoardQuestion{N: n, By: me, Recipient: recipient, ToPerson: person, Text: text,
			Options: options, At: now, Answer: ticket.Nil[string](), AnsweredBy: ticket.Nil[string](), AnsweredAt: ticket.Nil[string]()})
		ticket.Event(t, "question", me+" asks "+recipient+": "+textx.Clip(text, 200), "",
			ticket.QuestionAsked{By: me, Recipient: recipient, N: n}, cx.now())
		from := t.Column
		if w, ok := t.Waiting.Get(); ok && w.From != "" {
			from = w.From
		}
		setWaiting(t, ticket.Waiting{Reason: "question", Question: n, From: from, Since: now, Text: text})
		if t.Column != "waiting" {
			ticket.Move(t, "waiting", model.ReasonQuestionAsked, "question "+strconv.Itoa(n)+" to "+recipient, "", me, cx.now())
		} else {
			t.SetAsk(ticket.BoardAsk(t, now))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jsonx.Object{member("ticket", cx.view(t)), member("n", jsonx.Int(int64(n))), member("recipient", jstr(recipient))}, nil
}

func (cx *call) answer(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}, {"n", "int"}, {"answer", "str"}}, "id", "n", "answer")
	if err != nil {
		return nil, err
	}
	first, err := cx.visible(o.Get("id"))
	if err != nil {
		return nil, err
	}
	if !isStr(first.Tree().Get("mode"), string(model.ModeBoard)) {
		return nil, refuse(Forbidden, "", "%s is a factory ticket: its agents' questions are the maintainer's to answer", first.ID)
	}
	if err := cx.c.need("talk"); err != nil {
		return nil, err
	}
	answer, err := textIn(sval(o.Get("answer")), "answer", cx.lim.Answer, 1)
	if err != nil {
		return nil, err
	}
	if _, err := cx.writable(); err != nil {
		return nil, err
	}
	me := cx.c.name
	t, err := cx.changing(sval(o.Get("id")), func(t *ticket.Ticket) error {
		qi := -1
		for i, q := range list(t.Tree().Get("questions")) {
			if pyEq(q.Get("n"), o.Get("n")) && i < len(t.Questions) {
				qi = i
				break
			}
		}
		if qi < 0 {
			return bad("n", "%s has no question %s", t.ID, jsonx.PyStr(o.Get("n")))
		}
		q := &t.Questions[qi]
		if me != q.Recipient && !(cx.c.person && q.ToPerson) {
			return refuse(Forbidden, "", "question %d was put to %s", q.N, q.Recipient)
		}
		if q.Answer.Present() {
			return refuse(Conflict, "", "question %d is answered already", q.N)
		}
		if q.Withdrawn.Present() && q.Withdrawn.V != "" {
			return refuse(Conflict, "", "question %d was withdrawn when %s closed", q.N, t.ID)
		}
		if err := cx.roomy(t); err != nil {
			return err
		}
		now := cx.stamp()
		q.Answer, q.AnsweredBy, q.AnsweredAt = ticket.Some(answer), ticket.Some(me), ticket.Some(now)
		ticket.Event(t, "answer", me+" answered question "+strconv.Itoa(q.N)+": "+textx.Clip(answer, 200), "",
			ticket.QuestionAnswered{By: me, N: q.N}, cx.now())
		var still []ticket.BoardQuestion
		for _, x := range t.Questions {
			if x.Open() {
				still = append(still, x)
			}
		}
		if w, ok := t.Waiting.Get(); ok && w.Reason == "question" && w.Question == q.N {
			if len(still) > 0 {
				w.Question, w.Text = still[0].N, still[0].Text
				t.Waiting = ticket.Some(w)
			} else {
				t.Waiting = ticket.Nil[ticket.Waiting]()
				if t.Column == "waiting" {
					back := w.From
					if back == "" {
						back = "doing"
					}
					ticket.Move(t, back, model.ReasonQuestionAnswered, "question "+strconv.Itoa(q.N)+" answered", "", me, cx.now())
				}
			}
		}
		t.SetAsk(ticket.BoardAsk(t, now))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jsonx.Object{member("ticket", cx.view(t))}, nil
}

func (cx *call) create(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"project", "str"}, {"title", "str"}, {"body", "str"}, {"kind", "str"}, {"labels", "strs"},
		{"parent", "str?"}, {"links", "list"}, {"after", "strs"}, {"idempotency_key", "str"}}, "project", "title")
	if err != nil {
		return nil, err
	}
	project := sval(o.Get("project"))
	if !cx.c.sees(jstr(project), "", jsonx.Null()) {
		return nil, refuse(NotFound, "project", "no project %s", project)
	}
	if err := cx.c.need("file"); err != nil {
		return nil, err
	}
	key, hasKey := o.Get("idempotency_key").Str()
	if hasKey && !validIdemKey(key) {
		return nil, bad("idempotency_key", "an idempotency key is 1 to 128 of A-Z a-z 0-9 . _ : -")
	}
	dig := digest(o)
	var lock []string
	if p := sval(o.Get("parent")); p != "" && isTicketID(p) {
		lock = []string{p}
	}
	replayed := false
	t, err := func() (*ticket.Ticket, error) {
		release, err := cx.createLock()
		if err != nil {
			return nil, err
		}
		defer release()
		// the parent is held before anything is filed: a busy parent refuses with nothing written
		unlock, err := cx.held(lock)
		if err != nil {
			return nil, err
		}
		defer unlock()
		ix, err := cx.writable()
		if err != nil {
			return nil, err
		}
		if hasKey {
			if id, d, found := findIdem(ix, cx.c.name, key); found {
				if d != dig {
					return nil, refuse(IdempotencyMismatch, "idempotency_key", "that key filed %s from a different request", id)
				}
				old, err := cx.s.Board.Reader.Load(ticket.ID(id))
				if err != nil {
					return nil, err
				}
				replayed = true
				return old, nil
			}
		}
		if err := cx.checkCreates(project, 1, ix); err != nil {
			return nil, err
		}
		t, err := cx.fileTicket(project, o, key, dig)
		if err != nil {
			return nil, err
		}
		if p := sval(t.Tree().Get("parent")); p != "" && jsonx.Truthy(t.Tree().Get("parent")) {
			if err := cx.adopt(p, []string{string(t.ID)}); err != nil {
				return nil, err
			}
		}
		return t, nil
	}()
	if err != nil {
		return nil, err
	}
	return jsonx.Object{member("created", jsonx.Bool(!replayed)), member("ticket", cx.view(t))}, nil
}

func (cx *call) split(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}, {"children", "list"}, {"idempotency_key", "str"}}, "id", "children")
	if err != nil {
		return nil, err
	}
	first, err := cx.visible(o.Get("id"))
	if err != nil {
		return nil, err
	}
	if err := boardOnly(first, FactoryTicket); err != nil {
		return nil, err
	}
	if err := cx.c.need("plan"); err != nil {
		return nil, err
	}
	kids := list(o.Get("children"))
	if len(kids) < 1 || len(kids) > cx.lim.Children {
		return nil, bad("children", "1 to %d children", cx.lim.Children)
	}
	var children []jsonx.Object
	for _, c := range kids {
		co, err := take(c, []spec{{"title", "str"}, {"body", "str"}, {"labels", "strs"}, {"kind", "str"}}, "title")
		if err != nil {
			return nil, err
		}
		children = append(children, co)
	}
	key, hasKey := o.Get("idempotency_key").Str()
	if hasKey && (len(key) > 120 || !validIdemKey(key)) {
		return nil, bad("idempotency_key", "an idempotency key for a split is 1 to 120 of A-Z a-z 0-9 . _ : -")
	}
	me := cx.c.name
	var filed []string
	t, err := func() (*ticket.Ticket, error) {
		release, err := cx.createLock()
		if err != nil {
			return nil, err
		}
		defer release()
		unlock, err := cx.held([]string{string(first.ID)})
		if err != nil {
			return nil, err
		}
		defer unlock()
		ix, err := cx.writable()
		if err != nil {
			return nil, err
		}
		t, err := cx.s.Board.Reader.Load(first.ID)
		if err != nil {
			return nil, err
		}
		if err := cx.roomy(t); err != nil {
			return nil, err
		}
		project := sval(t.Tree().Get("project"))
		withParent := func(c jsonx.Object) jsonx.Object {
			c = c.Clone()
			c.Set("parent", jstr(string(t.ID)))
			return c
		}
		for _, c := range children { // every child checked before any is filed
			if _, err := cx.cleanFields(project, withParent(c), true, ""); err != nil {
				return nil, err
			}
		}
		type todo struct {
			fields       jsonx.Object
			key, dig, id string
		}
		var todos []todo
		fresh := 0
		for i, c := range children {
			td := todo{fields: withParent(c), dig: digest(withParent(c))}
			if hasKey {
				td.key = key + "#" + strconv.Itoa(i+1)
				if id, d, found := findIdem(ix, me, td.key); found {
					if d != td.dig {
						return nil, refuse(IdempotencyMismatch, "idempotency_key", "that key filed %s from a different request", id)
					}
					td.id = id
				}
			}
			if td.id == "" {
				fresh++
			}
			todos = append(todos, td)
		}
		if err := cx.checkCreates(project, fresh, ix); err != nil {
			return nil, err
		}
		var made []string
		for _, td := range todos {
			if td.id != "" {
				filed = append(filed, td.id)
				continue
			}
			n, err := cx.fileTicket(project, td.fields, td.key, td.dig)
			if err != nil {
				return nil, err
			}
			filed = append(filed, string(n.ID))
			made = append(made, string(n.ID))
		}
		t.Children = uniq(append(append([]string{}, t.Children...), filed...))
		if len(made) > 0 {
			ticket.Event(t, "split", me+" split it into "+strings.Join(made, ", "), "", ticket.Split{By: me, Children: made}, cx.now())
		}
		if err := cx.s.Board.Save(t); err != nil {
			return nil, err
		}
		return t, nil
	}()
	if err != nil {
		return nil, err
	}
	var sums []jsonx.Value
	for _, id := range filed {
		c, err := cx.s.Board.Reader.Load(ticket.ID(id))
		if err != nil {
			return nil, err
		}
		sums = append(sums, summaryOf(c))
	}
	return jsonx.Object{member("ticket", cx.view(t)), member("children", jsonx.ArrayOf(sums...))}, nil
}

func (cx *call) send(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}, {"project", "str"}, {"note", "str"}}, "id", "project")
	if err != nil {
		return nil, err
	}
	first, err := cx.visible(o.Get("id"))
	if err != nil {
		return nil, err
	}
	if err := boardOnly(first, FactoryTicket); err != nil {
		return nil, err
	}
	if err := cx.c.need("work"); err != nil {
		return nil, err
	}
	target := sval(o.Get("project"))
	if !cx.c.sees(jstr(target), "", jsonx.Null()) {
		return nil, refuse(NotFound, "project", "no project %s", target)
	}
	if mode, _ := cx.cfg.ProjectMode(target); mode != string(model.ModeFactory) {
		return nil, bad("project", "%s is a board project; send to a factory project", target)
	}
	if err := cx.c.need("file"); err != nil {
		return nil, err
	}
	note, err := textIn(pyStrOr(o.Get("note")), "note", cx.lim.Note, 0)
	if err != nil {
		return nil, err
	}
	me := cx.c.name
	var filed *ticket.Ticket
	t, err := func() (*ticket.Ticket, error) {
		release, err := cx.createLock()
		if err != nil {
			return nil, err
		}
		defer release()
		unlock, err := cx.held([]string{sval(o.Get("id"))})
		if err != nil {
			return nil, err
		}
		defer unlock()
		ix, err := cx.writable()
		if err != nil {
			return nil, err
		}
		if err := cx.checkCreates(target, 1, ix); err != nil {
			return nil, err
		}
		t, err := cx.s.Board.Reader.Load(first.ID)
		if err != nil {
			return nil, err
		}
		if err := cx.roomy(t); err != nil {
			return nil, err
		}
		tr := t.Tree()
		body := textx.RStrip("Sent from " + string(t.ID) + " on " + jsonx.PyStr(tr.Get("project")) + " by " + me + ".\n\n" + pyStrOr(tr.Get("body")))
		if note != "" {
			body += "\n\nNote from " + me + ": " + note
		}
		door := cx.c.door()
		if t.Door == string(model.DoorImport) {
			door = t.Door // imported text stays untrusted in the pipeline
		}
		filed, err = cx.s.Board.FileTicket(cx.ctx, ticket.NewIn{Project: target, Title: t.Title, Body: body, Door: door,
			Column: "triage", By: me, Fields: jsonx.Object{member("labels", emptyList()),
				member("links", jsonx.ArrayOf(jsonx.ObjectOf(member("rel", jstr("related")), member("ticket", jstr(string(t.ID))))))}})
		if err != nil {
			return nil, err
		}
		t.Links = append(t.Links, ticket.Link{Rel: "factory", Ticket: string(filed.ID)})
		ticket.Event(t, "sent", "sent to "+target+" as "+string(filed.ID), "", ticket.SentToFactory{By: me, Ticket: string(filed.ID)}, cx.now())
		ticket.Event(t, "link", "factory "+string(filed.ID), "", ticket.Linked{By: me, Rel: "factory", Ticket: string(filed.ID)}, cx.now())
		if err := cx.s.Board.Save(t); err != nil {
			return nil, err
		}
		return t, nil
	}()
	if err != nil {
		return nil, err
	}
	return jsonx.Object{member("ticket", cx.view(t)), member("factory_ticket", summaryOf(filed))}, nil
}
