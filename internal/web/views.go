package web

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/desk"
	"github.com/stevegeek/agent-factory/internal/mdhtml"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Pick is one proposal of a design, ticked by default.
type Pick struct {
	N           int
	Title, Body string
}

// FindingView is one finding of a hunt: ticked by default and filed as its
// default kind when it is still open on the findings ask (Choose), greyed
// with its verdict when it was not confirmed.
type FindingView struct {
	N                     int
	Title, Body, Evidence string
	Meta                  string // "confirmed · low bug · changes behaviour", "duplicate of PR #27"
	Open, Choose          bool
	DefaultAs             string // change or intent
}

// findingViews are a hunt's findings for its page.
func findingViews(t *ticket.Ticket, a *ticket.Ask) []FindingView {
	asked := a != nil && a.Kind == string(model.AskFindings)
	var out []FindingView
	for i, f := range t.Findings {
		meta := []string{ticket.VerdictLabel(f)}
		if f.Confirmed() {
			meta = append(meta, strings.TrimSpace(f.Severity+" "+f.Kind))
			if f.BehaviourChange {
				meta = append(meta, "changes behaviour")
			}
			if f.Filed != "" {
				meta = append(meta, "filed as "+f.Filed+" ("+f.FiledAs+")")
			}
		}
		out = append(out, FindingView{N: i + 1, Title: f.Title, Body: f.Body, Evidence: f.Evidence, Meta: strings.Join(meta, " · "),
			Open: f.Open(), Choose: asked && f.Open(), DefaultAs: f.DefaultKind()})
	}
	return out
}

// More is a disclosure: "Read the design", the notes, the original report.
type More struct {
	Summary, Text string
}

// AnswerView is the latest answer to a question asked while the ask waits.
type AnswerView struct {
	ID, Question, Lead, Full string
}

// TicketView is a ticket's page, or the detail pane beside the desk.
type TicketView struct {
	T                       *ticket.Ticket
	Ask                     *ticket.Ask
	Kind, KindLabel, Glyph  string
	Headline, Subtitle, Why string
	Age, Filter             string
	Facts                   []present.Fact
	Answers                 []Answer
	Picks                   []Pick
	Findings                []FindingView
	FindingsAsk             bool // the findings are ticked and filed from the answer form
	NoteLabel, NoteSub      string
	Callout                 *present.Callout
	Pending                 *present.Callout // an intent the tick has still to carry out
	Next                    string
	CanNote                 bool
	Story                   present.Story
	More                    []More
	Children                []string
	ChildrenLabel           string
	ParentID                string
	NoteFirst               bool
	Since                   string
	Answer                  *AnswerView
	AgentName               string        // the agent running now, if any
	Logs                    []string      // archived agent runs, by name
	Tokens                  *TicketTokens // what those runs spent, when any recorded it
	// Reply: the open board question takes a typed answer (beside any
	// option buttons), sent through the ticket API
	Reply     bool
	Questions []QuestionView // a board ticket's questions, oldest first
	Walls     []string       // the files the change touches in its project's walls
	// Operator is a manager ticket's drafts and actions, each with the
	// tier the tick found and where it stands
	Operator []present.OperatorItem
	// Pass: the ticket may be given to one of People; Assignee is whose it
	// is now ("" nobody's)
	Pass     bool
	People   []string
	Assignee string
	// Nudge is why this ticket is in the viewer's For you, until they put
	// it away
	Nudge *desk.ForYouItem
	// GateFail is the failed gate's last output lines, shown above the
	// answers
	GateFail *present.GateFailure
}

// forPerson adds to a ticket view what is the viewer's: who it may be
// passed to, and the nudge that put it in front of them.
func (s *Server) forPerson(r *http.Request, tv *TicketView) {
	if tv == nil {
		return
	}
	dv := s.deskViewer(r)
	if cfg := s.cfg(); cfg != nil {
		tv.People = cfg.People() // whom ops.Pass takes
	}
	tv.Assignee = tv.T.Assignee.Or("")
	tv.Pass = s.svc.Passer != nil && len(tv.People) > 0 && !model.Closed(tv.T.Column)
	for _, n := range desk.ForYou(desk.Nudges([]*ticket.Ticket{tv.T}, dv), s.now()) {
		tv.Nudge = &n
	}
}

// QuestionView is one question an agent asked on a board ticket, and its
// answer.
type QuestionView struct {
	N                    int
	By, To, Text         string
	Options              []string
	Answer, AnsweredBy   string
	Open, Withdrawn, Now bool // Now: the one the ask puts
}

func questionViews(t *ticket.Ticket, a *ticket.Ask) []QuestionView {
	now := 0
	if q := present.Asked(t, a); q != nil {
		now = q.N
	}
	var out []QuestionView
	for _, q := range t.Questions {
		out = append(out, QuestionView{N: q.N, By: q.By, To: q.Recipient, Text: q.Text, Options: q.Options,
			Answer: q.Answer.Or(""), AnsweredBy: q.AnsweredBy.Or(""), Open: q.Open(),
			Withdrawn: !q.Answer.Present() && !q.Open(), Now: q.N == now})
	}
	return out
}

// latestAnswer is the newest of your questions on t that has an answer,
// asked since the ask arrived (an answer about an earlier decision stays in
// the story).
func latestAnswer(t *ticket.Ticket, a *ticket.Ask, all map[ticket.ID]*ticket.Ticket) *AnswerView {
	for i := len(t.History) - 1; i >= 0; i-- {
		h := t.History[i]
		if h.Event != "asked" {
			continue
		}
		if a != nil && present.ParseTime(h.At).Before(present.ParseTime(a.Since)) {
			return nil
		}
		id, q := present.AskOf(h)
		c := all[ticket.ID(id)]
		if c == nil {
			continue
		}
		if res := c.Result.Or(""); res != "" {
			lead := present.ReportField(c, "task", "summary")
			if lead == "" {
				lead = mdhtml.Lead(res, 600)
			}
			return &AnswerView{ID: id, Question: q, Lead: lead, Full: res}
		}
	}
	return nil
}

// boardAnswers are the buttons for an agent's question on a board ticket:
// one for each option it offered (the typed answer has its own button), or
// none when board answers are off.
func (s *Server) boardAnswers(a *ticket.Ask) []Answer {
	if s.svc.Tickets == nil || a == nil {
		return nil
	}
	var out []Answer
	for i, o := range a.Options.Or(nil) {
		out = append(out, Answer{Value: optionAnswer + strconv.Itoa(i+1), Label: o, Primary: i == 0})
	}
	return out
}

func (s *Server) answersFor(t *ticket.Ticket, a *ticket.Ask) []Answer {
	if t.Board() {
		return s.boardAnswers(a)
	}
	if s.svc.Answers == nil {
		return nil
	}
	out := s.svc.Answers.Answers(t, a)
	for i := range out {
		// a link built from ticket data is only ever http(s)
		if out[i].Href != "" {
			out[i].Href = present.SafeURL(out[i].Href)
		}
	}
	return out
}

func (s *Server) ticketView(t *ticket.Ticket, ts []*ticket.Ticket, filter string) *TicketView {
	all := present.ByID(ts)
	a := present.AskFor(t)
	v := &TicketView{T: t, Ask: a, Filter: filter, Story: present.BuildStory(t, all), Facts: present.Facts(t, a, all),
		Answers: s.answersFor(t, a), Callout: present.PRCallout(t), Pending: present.Pending(t), Children: t.Children, ChildrenLabel: "Filed",
		ParentID: t.Parent.Or(""), CanNote: !model.Closed(t.Column) && !t.Board(), Walls: present.Walls(t), Operator: present.Operator(t),
		GateFail: present.GateFailed(t, a)}
	if pr, ok := t.PanelResult.Get(); ok {
		// a prospect's proposals, kept and dropped, each a ticket of its own
		v.ChildrenLabel, v.Children = "Proposals", nil
		for _, id := range append(append([]string{}, pr.Kept...), pr.DroppedTickets...) {
			if validID(id) {
				v.Children = append(v.Children, id)
			}
		}
	}
	result := t.Result.Or("")
	if a != nil {
		v.Kind, v.KindLabel, v.Glyph = a.Kind, present.KindLabel(a.Kind), present.KindGlyph(a.Kind)
		v.Headline, v.Why, v.Since = present.Headline(t, a), a.Why, a.Since
		if a.Kind == string(model.AskNotice) && result != "" {
			// an answer is a Markdown document: its opening paragraph, not a
			// run of its first sentences
			v.Why = mdhtml.Lead(result, 320)
		}
		v.Next = present.WaitingWhat(t, a)
		v.Age = present.AgeSince(a.Since, s.now())
		if !strings.Contains(v.Headline, t.Title) {
			v.Subtitle = t.Title
		}
		v.Answer = latestAnswer(t, a, all)
		if q := present.Asked(t, a); q != nil {
			v.Why = q.By + " asks " + q.Recipient + ", on " + t.Project + "."
			v.Reply = s.svc.Tickets != nil
		}
	} else {
		v.Kind = "closed"
		v.KindLabel = present.TicketColumnLabel(t)
		v.Headline = t.Title
		v.Age = present.AgeSince(t.Updated, s.now())
	}
	switch {
	case a != nil && a.Kind == string(model.AskDesign):
		v.NoteLabel = "Note for the designer"
		for i, p := range t.Proposed {
			v.Picks = append(v.Picks, Pick{N: i + 1, Title: p.Title, Body: p.Body})
		}
	case a != nil && a.Kind == string(model.AskQuestion):
		v.NoteLabel = "Your answer"
	case a != nil && a.Kind == string(model.AskFindings):
		v.NoteLabel = "A note or a question"
		v.FindingsAsk = true
	default:
		v.NoteLabel = "A note or a question"
	}
	v.Findings = findingViews(t, a)
	agent, running := t.Agent.Get()
	switch {
	case t.Board():
		v.Questions = questionViews(t, a)
		v.NoteSub = "The agent reads your answer through the ticket API; it is recorded as yours. With an option, what you type goes with it."
	case a != nil:
		v.NoteSub = "Ask the agent: one reads the change and answers on your desk in a few minutes; nothing changes. " +
			"Save note: kept for the next agent that works on it; nothing happens now."
	case running:
		v.NoteSub = "Ask the agent: an answer comes to your desk. Save note: kept for the next agent, not the one running now."
	default:
		v.NoteSub = "Ask the agent: an answer comes to your desk. Save note: kept for the next agent that works on it."
	}
	if running {
		v.AgentName = agent.Name
	}
	for _, x := range v.Answers {
		if x.NeedsNote && x.NoteFor != "" {
			v.NoteSub = "“" + x.Label + "” sends this note. " + v.NoteSub
			break
		}
	}
	for _, x := range v.Answers {
		// on a phone the field must come before the button that needs it
		v.NoteFirst = v.NoteFirst || x.NeedsNote
	}
	v.Logs = s.agentLogs(string(t.ID))
	v.Tokens = s.ticketTokens(string(t.ID), v.Logs)
	if d := t.Design.Or(""); d != "" {
		v.More = append(v.More, More{"Read the design", d})
	}
	if result != "" && t.Board() {
		v.More = append(v.More, More{"Read the result", result})
	} else if result != "" {
		v.More = append(v.More, More{"Read the answer", result})
	}
	p, isProposal := t.Proposal.Get()
	if isProposal {
		v.More = append(v.More, More{"Read the proposal", "Noticed: " + p.Noticed + "\n\nChange: " + p.Change + "\n\nWhy: " + p.Why + "\n\nCost: " + p.Cost})
	}
	if t.Body != "" && !isProposal {
		v.More = append(v.More, More{"Read the ticket", t.Body})
	}
	if o, ok := t.Original.Get(); ok && o.Body != "" {
		v.More = append(v.More, More{"Read the original report (untrusted)", o.Title + "\n\n" + o.Body})
	}
	if len(t.Notes) > 0 {
		var b strings.Builder
		for _, n := range t.Notes {
			fmt.Fprintf(&b, "%s  %s\n\n", present.ParseTime(n.At).Local().Format("2 Jan 15:04"), n.Text)
		}
		v.More = append(v.More, More{present.Plural(len(t.Notes), "note") + " for the agents", strings.TrimSpace(b.String())})
	}
	return v
}

// agentLogs are the archived agent runs of a ticket, oldest first: the
// directories under logs/<id>/ named as the dispatcher names agents.
func (s *Server) agentLogs(id string) []string {
	if s.paths.Logs == "" || !validID(id) {
		return nil
	}
	ents, err := os.ReadDir(filepath.Join(s.paths.Logs, id))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && present.ValidAgentName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func running(ts []*ticket.Ticket) int {
	n := 0
	for _, t := range ts {
		if t.Agent.Present() {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- boards

// BoardSummary is one project on the Boards tab.
type BoardSummary struct {
	Name, Href                       string
	Open, Asks, PRs, Closed, Running int
	// Paused: `enabled = false` in config, or paused from Foreman, so no
	// agents, gates, schedules or sweeps run for it
	Paused bool
	// Switchable: the viewer (the maintainer) may switch this factory
	// project on or off; SwitchNote says who switched it, when it is
	// switched ("" when it follows config), and ConfigState is config's
	// own "enabled" or "paused", for "Follow config".
	Switchable              bool
	SwitchNote, ConfigState string
	// Board: a board project (`mode = "board"`), worked by agents through
	// the ticket API; Doing and Waiting count its open tickets in those columns
	Board          bool
	Doing, Waiting int
}

// Lane is one column of a project's board.
type Lane struct {
	Column, Label string
	Tickets       []LaneTicket
	More          int // tickets left out, newest kept (a board's Done)
}

// LaneTicket is one ticket in a lane.
type LaneTicket struct {
	ID, Title, Ask, AskLabel, Agent, Wait string
	Assignee                              string   // board tickets
	Labels                                []string // board tickets
}

// BoardView is a project's board: open lanes, then the closed ones folded.
// A board project's lanes are its columns, every one shown, Dropped folded.
type BoardView struct {
	Name   string
	Paused bool
	Board  bool
	Lanes  []Lane
	Closed []Lane
}

// ProjectState is how config has a project the viewer may see.
type ProjectState struct {
	Paused bool // `enabled = false`, or switched off
	Board  bool // `mode = "board"`
	// Note says who switched it and when (config.Project.SwitchNote), ""
	// when it follows config; Config is config's own "enabled" or "paused".
	Note, Config string
}

// maxDoneCards is how many of a board's Done tickets its lane shows, newest
// first; search finds the rest.
const maxDoneCards = 30

// summaries are the projects on the board, by name, factory projects first:
// those with tickets, and every configured project the viewer may see
// (projects says how each is configured; nil when config can't be read). A
// project is a board project when config says so or, unconfigured, when its
// tickets are board tickets.
func summaries(ts []*ticket.Ticket, projects map[string]ProjectState) []BoardSummary {
	m := map[string]*BoardSummary{}
	summary := func(name string) *BoardSummary {
		b := m[name]
		if b == nil {
			st := projects[name]
			b = &BoardSummary{Name: name, Href: "/b/" + url.PathEscape(name), Paused: st.Paused && !st.Board, Board: st.Board,
				SwitchNote: st.Note, ConfigState: st.Config}
			m[name] = b
		}
		return b
	}
	for name := range projects {
		summary(name)
	}
	for _, t := range ts {
		b := summary(t.Project)
		if t.Board() {
			if _, configured := projects[t.Project]; !configured {
				b.Board = true
			}
			if !b.Board {
				continue // a board ticket left on a project now in factory mode
			}
			if model.Closed(t.Column) {
				b.Closed++
			} else {
				b.Open++
			}
			switch t.Column {
			case string(model.BoardDoing):
				b.Doing++
			case string(model.BoardWaiting):
				b.Waiting++
			}
			if present.AskFor(t) != nil {
				b.Asks++
			}
			continue
		}
		if b.Board {
			continue
		}
		if model.Closed(t.Column) {
			b.Closed++
		} else {
			b.Open++
		}
		if present.AskFor(t) != nil {
			b.Asks++
		}
		if t.Column == string(model.ColPROpen) {
			b.PRs++
		}
		if t.Agent.Present() {
			b.Running++
		}
	}
	var out []BoardSummary
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Board != out[j].Board {
			return !out[i].Board
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// boardLanes is a board project's board: a lane for each board column in
// order, empty ones too, newest id first; Done shows its newest
// maxDoneCards, and Dropped is folded. ok is false when the project has no
// board ticket the viewer may see.
func boardLanes(name string, ts []*ticket.Ticket) (*BoardView, bool) {
	bv, all := &BoardView{Name: name, Board: true}, present.ByID(ts)
	found := false
	for _, col := range model.BoardColumns() {
		c := string(col.Name)
		lane := Lane{Column: c, Label: col.Label}
		for i := len(ts) - 1; i >= 0; i-- {
			t := ts[i]
			if t.Project != name || t.Column != c || !t.Board() {
				continue
			}
			found = true
			if col.Name == model.BoardDone && len(lane.Tickets) == maxDoneCards {
				lane.More++
				continue
			}
			lt := LaneTicket{ID: string(t.ID), Title: t.Title, Assignee: t.Assignee.Or(""), Labels: t.Labels, Wait: present.WaitLine(t, all)}
			if a := present.AskFor(t); a != nil {
				lt.Ask, lt.AskLabel = a.Kind, present.KindLabel(a.Kind)
			}
			lane.Tickets = append(lane.Tickets, lt)
		}
		if col.Name == model.BoardDropped {
			if len(lane.Tickets) > 0 {
				bv.Closed = append(bv.Closed, lane)
			}
			continue
		}
		bv.Lanes = append(bv.Lanes, lane)
	}
	return bv, found
}

// boardView is one project's lanes in model column order, newest id first;
// ok is false when the project has no factory ticket the viewer may see.
func boardView(name string, ts []*ticket.Ticket) (*BoardView, bool) {
	bv, all := &BoardView{Name: name}, present.ByID(ts)
	found := false
	for _, col := range model.Columns() {
		c := string(col.Name)
		lane := Lane{Column: c, Label: col.Label}
		if col.Name == model.ColNeedsHuman {
			lane.Label = "Needs you"
		}
		for i := len(ts) - 1; i >= 0; i-- {
			t := ts[i]
			if t.Project != name || t.Column != c || t.Board() {
				continue
			}
			found = true
			lt := LaneTicket{ID: string(t.ID), Title: t.Title, Wait: present.WaitLine(t, all)}
			if a := present.AskFor(t); a != nil {
				lt.Ask, lt.AskLabel = a.Kind, present.KindLabel(a.Kind)
			}
			if ag, ok := t.Agent.Get(); ok {
				lt.Agent = present.RoleLabel(ag.Stage) + " running"
			}
			lane.Tickets = append(lane.Tickets, lt)
		}
		if len(lane.Tickets) == 0 {
			continue
		}
		if model.Closed(c) {
			bv.Closed = append(bv.Closed, lane)
		} else {
			bv.Lanes = append(bv.Lanes, lane)
		}
	}
	return bv, found
}

// ---------------------------------------------------------------- live

// LiveAgent is one running agent on the Live tab.
type LiveAgent struct {
	ID, Title, Project, Role, Host, Name, Age, Href string
}

// LiveView is the Live tab: In flight for everyone; the screens (Agents)
// and the log only for the maintainer (Screens).
type LiveView struct {
	InFlight present.InFlight
	Stalled  string // the tick has stopped writing its snapshot
	Screens  bool
	Agents   []LiveAgent
}

func liveAgents(ts []*ticket.Ticket, now time.Time) []LiveAgent {
	var out []LiveAgent
	for _, t := range ts {
		a, ok := t.Agent.Get()
		if !ok {
			continue
		}
		host := a.Host
		if host == model.LocalHost {
			host = ""
		}
		out = append(out, LiveAgent{ID: string(t.ID), Title: t.Title, Project: t.Project, Role: present.RoleLabel(a.Stage), Host: host,
			Name: a.Name, Age: present.AgeSince(a.Started, now), Href: "/t/" + string(t.ID)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
