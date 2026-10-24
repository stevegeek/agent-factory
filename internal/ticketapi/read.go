package ticketapi

import (
	"sort"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/mdout"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// get is t_get: one ticket, as JSON or as Markdown.
func (cx *call) get(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"id", "str"}, {"format", "str"}}, "id")
	if err != nil {
		return nil, err
	}
	t, err := cx.visible(o.Get("id"))
	if err != nil {
		return nil, err
	}
	if err := cx.c.need("read"); err != nil {
		return nil, err
	}
	v := cx.view(t)
	format := getOr(o.Value(), "format", jstr("json"))
	if isStr(format, "md") {
		return jsonx.Object{member("markdown", jstr(mdout.TicketMarkdown(mdView(v), cx.mdOpts())))}, nil
	}
	if !isStr(format, "json") {
		return nil, bad("format", "format is json or md")
	}
	return jsonx.Object{member("ticket", v)}, nil
}

type row struct {
	id string
	e  boardread.Entry
}

func num(id string) int { return ticket.ID(id).Num() }

// listed is listed(): the index entries the caller sees that the filters
// keep. Closed tickets are left out unless a column or include_closed asks
// for them.
func (cx *call) listed(ix boardread.Index, req jsonx.Object) []row {
	me := cx.c.name
	var out []row
	for _, id := range ix.IDs() {
		e, _ := ix.Get(id)
		ev := e.Object().Value()
		if !cx.c.sees(ev.Get("project"), id, ev.Get("by")) {
			continue
		}
		if p := req.Get("project"); jsonx.Truthy(p) && !pyEq(ev.Get("project"), p) {
			continue
		}
		col := req.Get("column")
		if jsonx.Truthy(col) && !pyEq(ev.Get("column"), col) {
			continue
		}
		if !jsonx.Truthy(col) && !jsonx.Truthy(req.Get("include_closed")) && model.Closed(sval(ev.Get("column"))) && ev.Get("column").Kind() == jsonx.KindString {
			continue
		}
		if a, ok := req.Lookup("assignee"); ok {
			want := a
			if isStr(a, "me") {
				want = jstr(me)
			}
			if !pyEq(ev.Get("assignee"), want) {
				continue
			}
		}
		if l := req.Get("label"); jsonx.Truthy(l) && !pyIn(sval(l), ev.Get("labels")) {
			continue
		}
		if k := req.Get("kind"); jsonx.Truthy(k) && !pyEq(ev.Get("kind"), k) {
			continue
		}
		if since := req.Get("updated_since"); jsonx.Truthy(since) && pyStrOr(ev.Get("updated")) < sval(since) {
			continue
		}
		out = append(out, row{id, e})
	}
	return out
}

// list is t_list: the caller's tickets, newest first, a page at a time. A
// text search reads ticket files (title and body, every word, case-folded)
// within a budget per call, then hands back a cursor where it stopped.
func (cx *call) list(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"project", "str"}, {"column", "str"}, {"assignee", "str?"}, {"label", "str"}, {"kind", "str"},
		{"text", "str"}, {"updated_since", "str"}, {"include_closed", "bool"}, {"limit", "int"}, {"cursor", "str"}, {"format", "str"}})
	if err != nil {
		return nil, err
	}
	if err := cx.c.need("read"); err != nil {
		return nil, err
	}
	limit := int64(50)
	if v, ok := o.Lookup("limit"); ok {
		n, fits := intOf(v)
		if !fits || n < 1 || n > 200 {
			return nil, bad("limit", "limit is 1 to 200")
		}
		limit = n
	}
	if c, ok := o.Lookup("cursor"); ok && !c.IsNull() && !isTicketID(sval(c)) {
		return nil, bad("cursor", "not a cursor from this list")
	}
	if s, ok := o.Lookup("updated_since"); ok && !s.IsNull() {
		at, aware, err := clock.ParseISO(strings.ReplaceAll(sval(s), "Z", "+00:00"))
		if err != nil {
			return nil, bad("updated_since", "updated_since is an RFC 3339 time")
		}
		_ = aware // a time without an offset is UTC here (Python: the box's local time, UTC)
		o = o.Clone()
		o.Set("updated_since", jstr(clock.Stamp(at.UTC())))
	}
	ix, err := cx.index()
	if err != nil {
		return nil, err
	}
	rows := cx.listed(ix, o)
	sort.SliceStable(rows, func(i, j int) bool { return num(rows[i].id) > num(rows[j].id) })
	if c := o.Get("cursor"); jsonx.Truthy(c) {
		var keep []row
		for _, r := range rows {
			if num(r.id) < num(sval(c)) {
				keep = append(keep, r)
			}
		}
		rows = keep
	}
	words := strings.FieldsFunc(textx.Lower(pyStrOr(o.Get("text"))), textx.IsSpace)
	var page []jsonx.Value
	var spent int64
	files, last, cut := 0, "", false
	for _, r := range rows {
		if int64(len(page)) == limit {
			break
		}
		if len(words) > 0 {
			if files > 0 && (files >= cx.lim.SearchFiles || spent >= cx.lim.SearchBytes) {
				cut = true
				break
			}
			files++
			if n, ok := r.e.Get("size").Int64(); ok {
				spent += n
			}
			body := ""
			if t := cx.tryLoad(jstr(r.id)); t != nil {
				body = jsonx.PyStr(getOr(t.Tree(), "body", jstr("")))
			}
			hay := textx.Lower(jsonx.PyStr(getOr(r.e.Object().Value(), "title", jstr(""))) + "\n" + body)
			last = r.id
			all := true
			for _, w := range words {
				all = all && strings.Contains(hay, w)
			}
			if !all {
				continue
			}
		}
		last = r.id
		page = append(page, summary(r.id, r.e))
	}
	next := jsonx.Null()
	if cut {
		next = jstr(last)
	} else if int64(len(page)) == limit && len(page) > 0 {
		lastID := sval(page[len(page)-1].Get("id"))
		for _, r := range rows {
			if num(r.id) < num(lastID) {
				next = jstr(lastID)
				break
			}
		}
	}
	if isStr(o.Get("format"), "md") {
		var lines []string
		for _, s := range page {
			lines = append(lines, mdout.SummaryLine(mdSummary(s), cx.c.name))
		}
		if len(lines) == 0 {
			lines = []string{"No tickets."}
			if cut {
				lines = []string{"No match among these."}
			}
		}
		if cut {
			lines = append(lines, "\n(searched "+strconv.Itoa(files)+" tickets, the most one call reads; older ones are on the next page)")
		}
		md := strings.Join(lines, "\n")
		if !next.IsNull() {
			md += "\n\nMore: cursor=" + sval(next)
		}
		return jsonx.Object{member("markdown", jstr(md+"\n"))}, nil
	}
	return jsonx.Object{member("tickets", jsonx.ArrayOf(page...)), member("next", next)}, nil
}

// mineGroups are MINE_GROUPS and their MINE_HEADINGS.
func mineGroups() []struct{ name, heading string } {
	return []struct{ name, heading string }{{"answered", "Answered (your questions)"}, {"asked_of_me", "Asked of you"},
		{"doing", "Doing"}, {"next", "Next"}, {"unassigned", "Unassigned"}, {"waiting", "Waiting"}}
}

// newlyAnswered is newly_answered(): an answer to one of my questions is
// new until I do anything on the ticket after it, by history order.
func newlyAnswered(tr jsonx.Value, me string) bool {
	var mine []jsonx.Value
	for _, q := range list(or(tr.Get("questions"), emptyList())) {
		if isStr(q.Get("by"), me) && !q.Get("answer").IsNull() {
			mine = append(mine, q.Get("n"))
		}
	}
	answered, acted := -1, -1
	for i, h := range list(getOr(tr, "history", emptyList())) {
		inMine := false
		for _, n := range mine {
			inMine = inMine || pyEq(n, h.Get("n"))
		}
		if isStr(h.Get("type"), "question_answered") && inMine {
			answered = i
		} else if isStr(h.Get("by"), me) {
			acted = i
		}
	}
	return answered > acted
}

// mine is t_mine: the caller's open board tickets, in groups, each ticket
// in one group only.
func (cx *call) mine(req jsonx.Value) (jsonx.Object, error) {
	o, err := take(req, []spec{{"project", "str"}, {"limit", "int"}, {"format", "str"}})
	if err != nil {
		return nil, err
	}
	if err := cx.c.need("read"); err != nil {
		return nil, err
	}
	limit := 20
	if v, ok := o.Lookup("limit"); ok {
		n, fits := intOf(v)
		if !fits || n < 1 || n > 50 {
			return nil, bad("limit", "limit is 1 to 50")
		}
		limit = int(n)
	}
	me := cx.c.name
	ix, err := cx.index()
	if err != nil {
		return nil, err
	}
	var rows []row
	for _, id := range ix.IDs() {
		e, _ := ix.Get(id)
		ev := e.Object().Value()
		if !cx.c.sees(ev.Get("project"), id, ev.Get("by")) {
			continue
		}
		if p := o.Get("project"); jsonx.Truthy(p) && !pyEq(ev.Get("project"), p) {
			continue
		}
		if !isStr(ev.Get("mode"), string(model.ModeBoard)) {
			continue
		}
		if c := ev.Get("column"); isStr(c, string(model.BoardDone)) || isStr(c, string(model.BoardDropped)) {
			continue
		}
		rows = append(rows, row{id, e})
	}
	sort.SliceStable(rows, func(i, j int) bool { return num(rows[i].id) < num(rows[j].id) })
	groups := map[string][]jsonx.Value{}
	taken := map[string]bool{}
	put := func(group string, picked []row, keepOrder bool) {
		if !keepOrder {
			sort.SliceStable(picked, func(i, j int) bool {
				return pyStrOr(picked[i].e.Get("updated")) < pyStrOr(picked[j].e.Get("updated"))
			})
		}
		for _, r := range picked {
			if !taken[r.id] && len(groups[group]) < limit {
				taken[r.id] = true
				groups[group] = append(groups[group], summary(r.id, r.e))
			}
		}
	}
	pick := func(keep func(row) bool) []row {
		var out []row
		for _, r := range rows {
			if keep(r) {
				out = append(out, r)
			}
		}
		return out
	}
	mineIs := func(r row) bool { return isStr(r.e.Get("assignee"), me) }
	free := func(r row) bool { return !jsonx.Truthy(r.e.Get("assignee")) }
	in := func(r row, col string) bool { return isStr(r.e.Get("column"), col) }
	put("answered", pick(func(r row) bool {
		if !pyIn(me, r.e.Get("answers_for")) {
			return false
		}
		tr := jsonx.ObjectOf()
		if t := cx.tryLoad(jstr(r.id)); t != nil {
			tr = t.Tree()
		}
		return newlyAnswered(tr, me)
	}), false)
	put("asked_of_me", pick(func(r row) bool { return pyIn(me, r.e.Get("open_for")) }), false)
	put("doing", pick(func(r row) bool { return mineIs(r) && in(r, "doing") }), false)
	put("next", pick(func(r row) bool { return mineIs(r) && in(r, "next") }), true)
	put("unassigned", pick(func(r row) bool { return free(r) && in(r, "next") }), true)
	put("unassigned", pick(func(r row) bool { return free(r) && in(r, "inbox") }), true)
	put("waiting", pick(func(r row) bool { return mineIs(r) && in(r, "waiting") }), false)
	if isStr(o.Get("format"), "md") {
		var parts []string
		for _, g := range mineGroups() {
			if len(groups[g.name]) == 0 {
				continue
			}
			var lines []string
			for _, s := range groups[g.name] {
				lines = append(lines, mdout.SummaryLine(mdSummary(s), me))
			}
			parts = append(parts, "## "+g.heading+"\n"+strings.Join(lines, "\n"))
		}
		md := "Nothing for you. `search_tickets` to look around."
		if len(parts) > 0 {
			md = strings.Join(parts, "\n\n")
		}
		return jsonx.Object{member("markdown", jstr(md+"\n"))}, nil
	}
	out := jsonx.Object{}
	for _, g := range mineGroups() {
		out = append(out, member(g.name, jsonx.ArrayOf(groups[g.name]...)))
	}
	return jsonx.Object{member("groups", out.Value())}, nil
}

// projects is t_projects: the caller's projects, each with its mode, its
// columns and kinds, and what the caller may do there.
func (cx *call) projects(req jsonx.Value) (jsonx.Object, error) {
	if _, err := take(req, nil); err != nil {
		return nil, err
	}
	var out []jsonx.Value
	for _, p := range cx.c.projects {
		mode, _ := cx.cfg.ProjectMode(p)
		var info model.ModeInfo
		for _, m := range model.Modes() {
			if string(m.Mode) == mode {
				info = m
			}
		}
		kinds := []string{"change"}
		if mode == string(model.ModeBoard) {
			kinds = nil
			for _, k := range info.Kinds {
				kinds = append(kinds, string(k))
			}
		}
		out = append(out, jsonx.ObjectOf(member("name", jstr(p)), member("mode", jstr(mode)), member("columns", strs(info.Columns)),
			member("kinds", strs(kinds)), member("can", strs(cx.c.can))))
	}
	return jsonx.Object{member("projects", jsonx.ArrayOf(out...))}, nil
}

// whoami is t_whoami.
func (cx *call) whoami(req jsonx.Value) (jsonx.Object, error) {
	if _, err := take(req, nil); err != nil {
		return nil, err
	}
	return jsonx.Object{member("name", jstr(cx.c.name)), member("projects", strs(cx.c.projects)), member("can", strs(cx.c.can)),
		member("expires", cx.c.expires)}, nil
}

// reindex is t_reindex: a person rebuilds the index.
func (cx *call) reindex(req jsonx.Value) (jsonx.Object, error) {
	if _, err := take(req, nil); err != nil {
		return nil, err
	}
	if !cx.c.person {
		return nil, refuse(Forbidden, "", "only a person rebuilds the index")
	}
	n, err := cx.s.Board.Reindex(cx.ctx)
	if err != nil {
		return nil, err
	}
	return jsonx.Object{member("count", jsonx.Int(int64(n)))}, nil
}
