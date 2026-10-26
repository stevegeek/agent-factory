package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
)

func TestSummariesListConfiguredProjectsAndWhichArePaused(t *testing.T) {
	ts := []*ticket.Ticket{{ID: "T0001", Project: "live", Column: "ready"}}
	got := summaries(ts, map[string]ProjectState{"live": {}, "resting": {Paused: true}})
	if len(got) != 2 || got[0].Name != "live" || got[1].Name != "resting" {
		t.Fatalf("summaries %+v", got)
	}
	if got[0].Paused || got[0].Open != 1 || !got[1].Paused || got[1].Open != 0 {
		t.Errorf("summaries %+v", got)
	}
	if got := summaries(ts, nil); len(got) != 1 || got[0].Paused {
		t.Errorf("without config: %+v", got)
	}
}

// Board projects come after the factory ones, configured or not, with
// their open, doing, waiting, closed and asking tickets counted.
func TestSummariesCountBoardProjectsAfterFactoryOnes(t *testing.T) {
	board := func(id, project, column string) *ticket.Ticket {
		return &ticket.Ticket{ID: ticket.ID(id), Project: project, Column: column, Mode: ticket.Some("board")}
	}
	asking := board("T0005", "notes", "waiting")
	asking.Questions = []ticket.BoardQuestion{{N: 1, By: "example-agent", Recipient: "alice", ToPerson: true, Text: "?", At: "2026-10-09T10:00:00+00:00"}}
	ts := []*ticket.Ticket{{ID: "T0001", Project: "zeta", Column: "ready"}, board("T0002", "notes", "inbox"), board("T0003", "notes", "doing"),
		board("T0004", "notes", "done"), asking, board("T0006", "notes", "dropped"), board("T0007", "loose", "next")}
	got := summaries(ts, map[string]ProjectState{"zeta": {}, "notes": {Board: true}, "empty": {Board: true}})
	var names []string
	for _, b := range got {
		names = append(names, fmt.Sprintf("%s:%v", b.Name, b.Board))
	}
	if strings.Join(names, " ") != "zeta:false empty:true loose:true notes:true" {
		t.Fatalf("order: %v", names)
	}
	n := got[3]
	if n.Open != 3 || n.Doing != 1 || n.Waiting != 1 || n.Closed != 2 || n.Asks != 1 || n.Paused {
		t.Errorf("notes: %+v", n)
	}
	if got[1].Open != 0 || got[2].Open != 1 {
		t.Errorf("empty, loose: %+v %+v", got[1], got[2])
	}
}

func TestBoardsShowPausedProjects(t *testing.T) {
	h := newHarness(t, false)
	cfg, err := config.Parse([]byte(`
[projects.resting]
path = "/x"
enabled = false

[projects.busy]
path = "/y"
`))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.svc.Config = fakeConfig{cfg}
	c := h.login()
	body := h.do("GET", "/boards", nil, c).Body.String()
	i := strings.Index(body, `href="/b/resting"`)
	if i < 0 {
		t.Fatalf("a configured project with no tickets is missing:\n%s", body)
	}
	if card := body[i:]; !strings.Contains(card[:strings.Index(card, "</a>")], ">paused<") {
		t.Errorf("resting is not marked paused")
	}
	j := strings.Index(body, `href="/b/busy"`)
	if j < 0 {
		t.Fatal("busy is missing")
	}
	if card := body[j:]; strings.Contains(card[:strings.Index(card, "</a>")], ">paused<") {
		t.Errorf("busy is marked paused")
	}
	w := h.do("GET", "/b/resting", nil, c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "enabled = false") {
		t.Errorf("a paused project's board: %d", w.Code)
	}
	if w := h.do("GET", "/b/nosuch", nil, c); w.Code != http.StatusNotFound {
		t.Errorf("an unknown project: %d", w.Code)
	}
	if strings.Contains(body, "Boards for agents") {
		t.Errorf("a factory-only config lists boards for agents")
	}
}

const boardConfig = `
[board]
people = ["alice", "bob"]

[projects.widgets]
path = "/x"

[projects.notes]
mode = "board"

[projects.empty]
mode = "board"
`

// boardTicket is a board ticket's file: the fields the ticket API writes.
func boardTicket(id, column, title, extra string) string {
	return fmt.Sprintf(`{"id": %q, "schema": 1, "mode": "board", "project": "notes", "title": %q, "body": "The body of %s.",
		"door": "agent", "by": "example-agent", "column": %q, "kind": "item", "assignee": null, "labels": [], "parent": null,
		"children": [], "links": [], "waiting": null, "questions": [], "comments": [], "result": null, "notes": [],
		"created": "2026-10-09T09:00:00+00:00", "history": [{"at": "2026-10-09T09:00:00+00:00", "event": "created",
		"detail": "door=agent column=inbox", "type": "created", "door": "agent", "column": "inbox", "by": "example-agent"}],
		"updated": "2026-10-09T09:00:00+00:00"%s}`, id, title, id, column, extra)
}

const openQuestion = `, "questions": [{"n": 1, "by": "example-agent", "recipient": "alice", "to_person": true,
	"text": "Which locale?", "options": ["en-GB", "en-US"], "at": "2026-10-09T10:00:00+00:00",
	"answer": null, "answered_by": null, "answered_at": null}],
	"waiting": {"reason": "question", "question": 1, "from": "doing", "since": "2026-10-09T10:00:00+00:00", "text": "Which locale?"}`

// boardHarness is the harness with board projects in config and a board
// ticket in each board column (T0104 waits on a question to a person).
func boardHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, false)
	cfg, err := config.Parse([]byte(boardConfig))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.svc.Config = fakeConfig{cfg}
	for _, f := range [][2]string{
		{"T0101", boardTicket("T0101", "inbox", "Inbox card", `, "labels": ["ui", "docs"]`)},
		{"T0102", boardTicket("T0102", "next", "Next card", "")},
		{"T0103", boardTicket("T0103", "doing", "Doing card", `, "assignee": "example-agent"`)},
		{"T0104", boardTicket("T0104", "waiting", "Date picker", `, "assignee": "example-agent", "labels": ["ui"]`+openQuestion)},
		{"T0105", boardTicket("T0105", "done", "Done card", `, "result": "Shipped it."`)},
		{"T0106", boardTicket("T0106", "dropped", "Dropped card", "")},
	} {
		h.env.WriteFile(t, "board/tickets/"+f[0]+".json", strings.ReplaceAll(f[1], "\n", " "))
	}
	return h
}

// cardOf is the Boards card linking to href.
func cardOf(t *testing.T, body, href string) string {
	t.Helper()
	i := strings.Index(body, `href="`+href+`"`)
	if i < 0 {
		t.Fatalf("no card for %s:\n%s", href, body)
	}
	card := body[i:]
	return card[:strings.Index(card, "</a>")]
}

func TestBoardsListBoardProjectsWithABadge(t *testing.T) {
	h := boardHarness(t)
	c := h.login()
	body := h.do("GET", "/boards", nil, c).Body.String()
	agents := strings.Index(body, "Boards for agents")
	if agents < 0 || strings.Index(body, `href="/b/widgets"`) > agents || strings.Index(body, `href="/b/notes"`) < agents {
		t.Fatalf("board projects are not grouped after the factory ones:\n%s", body)
	}
	notes := cardOf(t, body, "/b/notes")
	for _, want := range []string{">board<", "1 need you", "4 open", "1 doing", "1 waiting", "2 closed"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the notes card lacks %q: %s", want, notes)
		}
	}
	if empty := cardOf(t, body, "/b/empty"); !strings.Contains(empty, ">board<") || !strings.Contains(empty, "0 open") {
		t.Errorf("a configured board with no tickets: %s", empty)
	}
	if strings.Contains(cardOf(t, body, "/b/widgets"), ">board<") {
		t.Errorf("a factory project is badged board")
	}
}

func TestABoardProjectsLanesAreItsColumns(t *testing.T) {
	h := boardHarness(t)
	c := h.login()
	w := h.do("GET", "/b/notes", nil, c)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, ">board<") {
		t.Fatalf("/b/notes: %d", w.Code)
	}
	last := -1
	for _, lane := range []string{"<h3>Inbox <span>1", "<h3>Next <span>1", "<h3>Doing <span>1", "<h3>Waiting <span>1", "<h3>Done <span>1"} {
		i := strings.Index(body, lane)
		if i < 0 || i < last {
			t.Fatalf("lane %q missing or out of order:\n%s", lane, body)
		}
		last = i
	}
	if strings.Contains(body, "<h3>Dropped") {
		t.Errorf("Dropped is a lane")
	}
	d := strings.Index(body, "<details")
	if d < 0 || !strings.Contains(body[d:], "Dropped · 1") || !strings.Contains(body[d:], "T0106") || strings.Index(body, "T0106") < d {
		t.Errorf("Dropped is not folded with its count:\n%s", body)
	}
	for _, want := range []string{`<span class="card-meta">ui, docs</span>`, `<span class="card-meta">example-agent</span>`,
		`<span class="card-meta">example-agent · ui</span>`, `k-question`} {
		if !strings.Contains(body, want) {
			t.Errorf("the cards lack %q", want)
		}
	}
	// a configured board with no tickets shows its empty lanes
	w = h.do("GET", "/b/empty", nil, c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<h3>Inbox <span>0") || !strings.Contains(w.Body.String(), "<h3>Done <span>0") {
		t.Errorf("/b/empty: %d\n%s", w.Code, w.Body.String())
	}
	// the factory board is as it was: its own columns, no board tickets
	fb := h.do("GET", "/b/widgets", nil, c).Body.String()
	if strings.Contains(fb, ">board<") || strings.Contains(fb, "T0104") || strings.Contains(fb, "<h3>Inbox") {
		t.Errorf("the factory board shows board things")
	}
}

// A board's Done lane shows its newest cards; search has the rest.
func TestABoardsDoneLaneIsCapped(t *testing.T) {
	var ts []*ticket.Ticket
	for i := 1; i <= maxDoneCards+5; i++ {
		ts = append(ts, &ticket.Ticket{ID: ticket.FormatID(i), Project: "notes", Column: "done", Mode: ticket.Some("board")})
	}
	bv, ok := boardLanes("notes", ts)
	if !ok || len(bv.Lanes) != 5 {
		t.Fatalf("lanes: %+v", bv)
	}
	done := bv.Lanes[4]
	if done.Label != "Done" || len(done.Tickets) != maxDoneCards || done.More != 5 || done.Tickets[0].ID != string(ticket.FormatID(maxDoneCards+5)) {
		t.Errorf("done: %d shown, %d more, first %s", len(done.Tickets), done.More, done.Tickets[0].ID)
	}
}

func TestABoardTicketPageShowsItsWork(t *testing.T) {
	h := boardHarness(t)
	h.change("T0105", func(tk *ticket.Ticket) {
		tk.Comments = []ticket.Comment{{N: 1, At: "2026-10-09T11:00:00+00:00", By: "example-agent", Type: "result", Text: "Shipped it."}}
		tk.Questions = []ticket.BoardQuestion{{N: 1, By: "example-agent", Recipient: "alice", ToPerson: true, Text: "Ship on Friday?",
			At: "2026-10-09T10:00:00+00:00", Answer: ticket.Some("Yes"), AnsweredBy: ticket.Some("alice"), AnsweredAt: ticket.Some("2026-10-09T10:30:00+00:00")}}
		at := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
		ticket.Event(tk, "question", "example-agent asks alice: Ship on Friday?", "", ticket.QuestionAsked{By: "example-agent", Recipient: "alice", N: 1}, at)
		ticket.Event(tk, "answer", "alice answered question 1: Yes", "", ticket.QuestionAnswered{By: "alice", N: 1}, at.Add(30*time.Minute))
		ticket.Event(tk, "comment", "example-agent: Shipped it.", "", ticket.Commented{By: "example-agent", N: 1}, at.Add(time.Hour))
	})
	c := h.login()
	w := h.do("GET", "/t/T0105", nil, c)
	page := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("/t/T0105: %d", w.Code)
	}
	for _, want := range []string{"Done card", "Column", "Done", "Filed by", "Read the ticket", "The body of T0105.", "Read the result", "Shipped it.",
		"Questions", "1. example-agent asked alice", "Ship on Friday?", "alice answered", `href="/b/notes"`, "Asked alice: Ship on Friday?"} {
		if !strings.Contains(page, want) {
			t.Errorf("the board ticket page lacks %q", want)
		}
	}
	if strings.Contains(page, `name="note"`) || strings.Contains(page, `name="answer"`) {
		t.Errorf("a closed board ticket offers answer forms")
	}
	talk := h.do("GET", "/t/T0105/talk", nil, c).Body.String()
	for _, want := range []string{"Question for alice:", "Ship on Friday?", "Yes", "Shipped it."} {
		if !strings.Contains(talk, want) {
			t.Errorf("the conversation lacks %q", want)
		}
	}
	if strings.Contains(talk, `id="reply"`) {
		t.Errorf("a board ticket with nothing to answer offers a reply")
	}
	// an open board ticket names its assignee and labels
	page = h.do("GET", "/t/T0103", nil, c).Body.String()
	if !strings.Contains(page, "Assignee") || !strings.Contains(page, "example-agent") || !strings.Contains(page, "Doing") {
		t.Errorf("T0103 lacks its assignee or column")
	}
}

// fakeTickets stands in for the ticket API's verbs and records each call.
type fakeTickets struct {
	mu    sync.Mutex
	calls []string
	fail  error
}

func (f *fakeTickets) Do(ctx context.Context, c ticketapi.Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	who := "token"
	switch c {
	case ticketapi.ForPerson():
		who = "maintainer"
	case ticketapi.AsPerson("robin"):
		who = "robin"
	}
	f.calls = append(f.calls, who+" "+verb+" "+string(jsonx.Format{Indent: -1}.Encode(req)))
	if f.fail != nil {
		return nil, f.fail
	}
	return jsonx.Object{}, nil
}

func (f *fakeTickets) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

func TestABoardQuestionIsAnAskOnTheDesk(t *testing.T) {
	h := boardHarness(t)
	tickets := &fakeTickets{}
	h.srv.svc.Tickets = tickets
	c := h.login()
	desk := h.do("GET", "/", nil, c).Body.String()
	i := strings.Index(desk, `href="/t/T0104"`)
	if i < 0 {
		t.Fatalf("the board question is not on the desk")
	}
	row := desk[i:]
	row = row[:strings.Index(row, "</a>")]
	for _, want := range []string{"notes", "Date picker", "Answer example-agent"} {
		if !strings.Contains(row, want) {
			t.Errorf("the desk row lacks %q: %s", want, row)
		}
	}
	page := h.do("GET", "/t/T0104", nil, c).Body.String()
	for _, want := range []string{"Which locale?", "example-agent asks alice, on notes.", "example-agent asked alice and waits for the answer.",
		`value="option-1"`, `value="option-2"`, ">en-GB<", ">en-US<", `value="answer"`, `name="note"`, `name="since" value="2026-10-09T10:00:00&#43;00:00"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the board ask lacks %q", want)
		}
	}
	if strings.Contains(page, `value="ask"`) || strings.Contains(page, "Save note") {
		t.Errorf("a board ask offers the factory's note box")
	}
	// without the ticket API there is nothing to answer with
	h.srv.svc.Tickets = nil
	if page := h.do("GET", "/t/T0104", nil, c).Body.String(); strings.Contains(page, `name="answer"`) {
		t.Errorf("a board ask offers answers with board answers off")
	}
}

func TestABoardQuestionIsAnsweredFromTheWeb(t *testing.T) {
	h := boardHarness(t)
	tickets := &fakeTickets{}
	h.srv.svc.Tickets = tickets
	c := h.login()
	a := present.AskFor(find(h.fixtures(), "T0104"))
	post := func(form url.Values) *http.Response {
		t.Helper()
		form.Set("csrf", h.csrf(c))
		return h.do("POST", "/t/T0104/act", form, c).Result()
	}
	// an option, with a typed note under it
	res := post(url.Values{"answer": {"option-1"}, "note": {"  and 24-hour times "}, "ask": {a.Kind}, "since": {a.Since}})
	if res.StatusCode != 303 || res.Header.Get("Location") == "/t/T0104" {
		t.Errorf("answer: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	want := `maintainer answer {"id": "T0104", "n": 1, "answer": "en-GB\n\nand 24-hour times"}`
	if got := tickets.take(); len(got) != 1 || got[0] != want {
		t.Errorf("calls %q, want %q", got, want)
	}
	// typed only, from the conversation
	res = post(url.Values{"answer": {"talk"}, "view": {"talk"}, "note": {"Use en-US"}, "ask": {a.Kind}, "since": {a.Since}})
	if res.Header.Get("Location") != "/t/T0104/talk" {
		t.Errorf("reply from the conversation went to %s", res.Header.Get("Location"))
	}
	if got := tickets.take(); len(got) != 1 || !strings.Contains(got[0], `"answer": "Use en-US"`) {
		t.Errorf("reply calls %q", got)
	}
	// nothing typed, a bad option, an ask that moved on: nothing is sent
	for _, form := range []url.Values{
		{"answer": {"answer"}, "note": {"  "}, "ask": {a.Kind}, "since": {a.Since}},
		{"answer": {"option-3"}, "ask": {a.Kind}, "since": {a.Since}},
		{"answer": {"option-1"}, "ask": {a.Kind}, "since": {"2026-10-09T09:00:00+00:00"}},
	} {
		if res := post(form); res.StatusCode != 303 || res.Header.Get("Location") != "/t/T0104" {
			t.Errorf("%v: %d %s", form, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if got := tickets.take(); len(got) != 0 {
		t.Errorf("a refused answer reached the ticket API: %q", got)
	}
	// the ticket API's refusal is the flash
	tickets.fail = &ticketapi.Refused{Code: ticketapi.Conflict, Message: "question 1 is answered already"}
	w := h.do("POST", "/t/T0104/act", url.Values{"csrf": {h.csrf(c)}, "answer": {"option-2"}, "ask": {a.Kind}, "since": {a.Since}}, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || f.Text != "T0104: Question 1 is answered already." {
		t.Errorf("refusal flash %+v", f)
	}
	// a note, an ask or a factory answer on a board ticket is not taken
	for _, v := range []string{"note", "ask", "start"} {
		if w := h.do("POST", "/t/T0104/act", url.Values{"csrf": {h.csrf(c)}, "answer": {v}, "note": {"x"}}, c); w.Code != http.StatusNotFound {
			t.Errorf("%s on a board ticket: %d", v, w.Code)
		}
	}
	if len(h.ops.takeAnswers()) != 0 {
		t.Errorf("a board answer reached ops")
	}
	// another user answers as a person of their own
	tickets.take()
	tickets.fail = nil
	secret, err := h.users.Add("robin", []string{"notes"})
	if err != nil {
		t.Fatal(err)
	}
	rc := h.loginWith(secret, "/")
	h.do("POST", "/t/T0104/act", url.Values{"csrf": {h.csrf(rc)}, "answer": {"option-2"}, "ask": {a.Kind}, "since": {a.Since}}, rc)
	if got := tickets.take(); len(got) != 1 || !strings.HasPrefix(got[0], "robin answer ") {
		t.Errorf("robin's answer: %q", got)
	}
}

// A ticket id searched on its own opens a board ticket as it opens any.
func TestSearchOpensABoardTicketByID(t *testing.T) {
	h := boardHarness(t)
	c := h.login()
	if w := h.do("GET", "/search?q=T0104", nil, c); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/t/T0104" {
		t.Errorf("id search: %d %s", w.Code, w.Header().Get("Location"))
	}
	if body := h.do("GET", "/search?q=locale", nil, c).Body.String(); !strings.Contains(body, "T0104") {
		t.Errorf("text search misses a board question")
	}
}

func TestSummariesCarryTheProjectsLabels(t *testing.T) {
	got := summaries(nil, map[string]ProjectState{"widgets": {Title: "Widget shop", Description: "Parts and orders", Group: "Tools"}, "plain": {}})
	if len(got) != 2 || got[0].Name != "plain" || got[1].Name != "widgets" {
		t.Fatalf("summaries %+v", got)
	}
	if w := got[1]; w.Title != "Widget shop" || w.Description != "Parts and orders" || w.Group != "Tools" {
		t.Errorf("widgets: %+v", w)
	}
	if p := got[0]; p.Title != "" || p.Description != "" || p.Group != "" {
		t.Errorf("plain: %+v", p)
	}
}

func TestTheNoticeProjectHasDefaultLabels(t *testing.T) {
	ts := []*ticket.Ticket{{ID: "T0001", Project: config.NoticeProject, Column: "ready"}}
	got := summaries(ts, nil)
	if len(got) != 1 || got[0].Title != config.NoticeTitle || got[0].Description != config.NoticeDescription || got[0].Group != "Factory" {
		t.Fatalf("unconfigured: %+v", got)
	}
	got = summaries(ts, map[string]ProjectState{config.NoticeProject: {Title: "Notices"}})
	if len(got) != 1 || got[0].Title != "Notices" || got[0].Description != "" || got[0].Group != "" {
		t.Errorf("configured: %+v", got)
	}
}

const groupConfig = `
[projects.plain]
path = "/x"

[projects.widgets]
path = "/y"
title = "Widget shop"
description = "Parts, orders and the stock count"
group = "Tools"

[projects.notes]
mode = "board"
title = "Shared notes"
group = "Tools"

[projects.loose]
mode = "board"

[projects.old]
path = "/z"
group = "Archive"
`

func TestBoardsListAGroupInItsOwnSection(t *testing.T) {
	h := newHarness(t, false)
	cfg, err := config.Parse([]byte(groupConfig))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.svc.Config = fakeConfig{cfg}
	h.srv.svc.Switch = &fakeSwitcher{}
	c := h.login()
	body := h.do("GET", "/boards", nil, c).Body.String()
	last := -1
	for _, want := range []string{`href="/b/plain"`, "Boards for agents", `href="/b/loose"`,
		`<h3 class="boards-h">Archive</h3>`, `href="/b/old"`, `<h3 class="boards-h">Tools</h3>`, `href="/b/widgets"`} {
		i := strings.Index(body, want)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order:\n%s", want, body)
		}
		last = i
	}
	tools := body[strings.Index(body, `<h3 class="boards-h">Tools</h3>`):]
	if !strings.Contains(tools, `href="/b/notes"`) {
		t.Errorf("the board project is not in its group:\n%s", tools)
	}
	widgets := cardOf(t, body, "/b/widgets")
	for _, want := range []string{"<b>Widget shop</b>", `<span class="key">widgets</span>`, `>Parts, orders and the stock count</span>`, "0 open"} {
		if !strings.Contains(widgets, want) {
			t.Errorf("the widgets card lacks %q: %s", want, widgets)
		}
	}
	if cell := switchCard(body, "widgets"); !strings.Contains(cell, `action="/boards/switch"`) || !strings.Contains(cell, `name="csrf" value="`+h.csrf(c)+`"`) {
		t.Errorf("a grouped factory project has no switch: %s", cell)
	}
	notes := cardOf(t, body, "/b/notes")
	if !strings.Contains(notes, "<b>Shared notes <span") || !strings.Contains(notes, ">board<") || !strings.Contains(notes, `<span class="key">notes</span>`) ||
		strings.Contains(notes, `class="desc"`) {
		t.Errorf("the notes card: %s", notes)
	}
	if strings.Contains(switchCard(body, "notes"), `action="/boards/switch"`) {
		t.Error("a grouped board project has a switch")
	}
	plain := cardOf(t, body, "/b/plain")
	if !strings.Contains(plain, "<b>plain</b>") || strings.Contains(plain, `class="key"`) || strings.Contains(plain, `class="desc"`) {
		t.Errorf("a project with no labels: %s", plain)
	}
}

func TestABoardsHeadingIsItsTitle(t *testing.T) {
	h := newHarness(t, false)
	cfg, err := config.Parse([]byte(groupConfig))
	if err != nil {
		t.Fatal(err)
	}
	h.srv.svc.Config = fakeConfig{cfg}
	c := h.login()
	body := h.do("GET", "/b/widgets", nil, c).Body.String()
	if !strings.Contains(body, "<h3>Widget shop</h3>") || !strings.Contains(body, `<p class="lede">Parts, orders and the stock count</p>`) {
		t.Errorf("/b/widgets:\n%s", body)
	}
	body = h.do("GET", "/b/plain", nil, c).Body.String()
	if !strings.Contains(body, "<h3>plain</h3>") || strings.Contains(body, `<p class="lede">`) {
		t.Errorf("/b/plain:\n%s", body)
	}
}
