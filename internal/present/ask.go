package present

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// AskFor is the ticket's ask: the one the dispatcher stored, in an ask
// column (`factory check --asks` lists a ticket there with none). A board
// ticket asks only while an agent's question to a person is open (BoardAsk).
func AskFor(t *ticket.Ticket) *ticket.Ask {
	if t.Board() {
		return BoardAsk(t)
	}
	switch model.Column(t.Column) {
	case model.ColNeedsHuman, model.ColPROpen, model.ColReadyForPR:
	default:
		return nil
	}
	if a, ok := t.Ask.Get(); ok && a.Kind != "" {
		return &a
	}
	return nil
}

// BoardAsk is a board ticket's ask: its first open question to a person, as
// ticket.BoardAsk makes it for the ticket API to store, worked out afresh
// from the questions, so a stored ask that lags them never shows; nil when no
// question waits on a person or the ticket is closed.
func BoardAsk(t *ticket.Ticket) *ticket.Ask { return ticket.BoardAsk(t, t.Updated) }

// Asked is the board question an ask puts (its N), or nil for any other ask.
func Asked(t *ticket.Ticket, a *ticket.Ask) *ticket.BoardQuestion {
	if a == nil || !t.Board() {
		return nil
	}
	n, ok := a.N.Get()
	if !ok {
		return nil
	}
	for i := range t.Questions {
		if t.Questions[i].N == n {
			return &t.Questions[i]
		}
	}
	return nil
}

// Mergeable reports a change the gate found auto-merge eligible whose PR is
// open and not a draft, so the factory may merge it once the maintainer says
// so. One GitHub will not merge before the maintainer approves it there is
// not: it WaitsForApproval.
func Mergeable(t *ticket.Ticket) bool {
	return eligibleOpen(t) && !ticket.NeedsApproval(t)
}

// WaitsForApproval reports an eligible open PR that GitHub merges only once
// the maintainer approves its head there; the approval is the factory's
// go-ahead too, so it merges by itself on the next tick after it.
func WaitsForApproval(t *ticket.Ticket) bool {
	return eligibleOpen(t) && ticket.NeedsApproval(t)
}

func eligibleOpen(t *ticket.Ticket) bool {
	g, okG := t.Gate.Get()
	pr, okPR := t.PR.Get()
	return t.Column == string(model.ColPROpen) && okG && g.AutoEligible && okPR && pr.Number > 0 && !prDraft(pr)
}

// Headline is the ask's question short enough to read at a glance; the
// ticket's title goes under it. The full question stays in the ask.
func Headline(t *ticket.Ticket, a *ticket.Ask) string {
	issue := ""
	if n := issueOf(t); n > 0 {
		issue = fmt.Sprintf(" issue #%d", n)
	}
	switch model.AskKind(a.Kind) {
	case model.AskReview:
		if Mergeable(t) {
			return fmt.Sprintf("Merge PR #%d?", prNumber(t))
		}
		if WaitsForApproval(t) {
			return fmt.Sprintf("Approve PR #%d on GitHub", prNumber(t))
		}
	case model.AskDesign:
		return fmt.Sprintf("Approve this design and its %s?", Plural(len(t.Proposed), "ticket"))
	case model.AskFindings:
		if n := ticket.OpenFindings(t); n > 0 {
			return fmt.Sprintf("File %s as tickets?", Plural(n, "confirmed finding"))
		}
		return "Nothing left to file: close it?"
	case model.AskProposal:
		return "File this proposal as a change?"
	case model.AskDeploy:
		return "Deploy main to the factory?"
	case model.AskStart:
		if issue != "" {
			return "Start work on" + issue + "?"
		}
		return "Start work on this?"
	case model.AskDecide:
		if issue != "" {
			return "Start" + issue + ", or drop it?"
		}
		return "Start it, or drop it?"
	case model.AskNotice:
		if t.KindName() == string(model.KindTask) {
			return "Read the answer and close it?"
		}
		return "Seen this notice?"
	}
	return a.Question
}

// Walls are the files of the ticket's change that touch its project's
// walls, as Gate 1 recorded them (none before the gate).
func Walls(t *ticket.Ticket) []string {
	if g, ok := t.Gate.Get(); ok {
		return g.Walls
	}
	return nil
}

// Action is what the ask wants, in a few words, for the desk list (the title
// says what it is about). A review or an open ask on a change to the walls
// says so.
func Action(t *ticket.Ticket, a *ticket.Ask) string {
	pr := prNumber(t)
	issue := ""
	if n := issueOf(t); n > 0 {
		issue = fmt.Sprintf(" · issue #%d", n)
	}
	walls := ""
	if len(Walls(t)) > 0 || len(DraftWalls(t)) > 0 {
		walls = " · touches the walls"
	}
	switch model.AskKind(a.Kind) {
	case model.AskReview:
		if Mergeable(t) {
			return fmt.Sprintf("Merge PR #%d?", pr) + walls
		}
		if WaitsForApproval(t) {
			return fmt.Sprintf("Approve PR #%d on GitHub", pr) + walls
		}
		return fmt.Sprintf("Review PR #%d", pr) + walls
	case model.AskOpen:
		if pr > 0 {
			return fmt.Sprintf("Update PR #%d?", pr) + walls
		}
		return "Open a PR?" + walls
	case model.AskDesign:
		return "Approve design · " + Plural(len(t.Proposed), "ticket")
	case model.AskFindings:
		confirmed := 0
		for _, f := range t.Findings {
			if f.Confirmed() {
				confirmed++
			}
		}
		return fmt.Sprintf("File findings · %d of %d confirmed", confirmed, len(t.Findings))
	case model.AskProposal:
		return "File as a change?"
	case model.AskStart:
		return "Start work?" + issue
	case model.AskDecide:
		return "Start or drop?" + issue
	case model.AskNotice:
		if t.KindName() == string(model.KindTask) {
			return "Read the answer"
		}
		return "Read the notice"
	case model.AskQuestion:
		if q := Asked(t, a); q != nil {
			return "Answer " + q.By
		}
		return "Answer the agent"
	case model.AskDraft:
		c := ticket.Items(t)
		switch {
		case c.New+c.Requested > 0:
			return "Applying · " + Plural(c.New+c.Requested, "change") + walls
		case c.ByHand > 0 || len(t.Drafts)+len(t.Actions) == 0:
			return "Apply the draft? · " + Plural(len(t.Drafts), "change") + walls
		case c.Waiting+c.Failed > 0:
			return "Apply it? · " + Plural(c.Waiting+c.Failed, "change") + walls
		}
		return "Read what the manager did"
	case model.AskDeploy:
		if d, ok := t.Deploy.Get(); ok && d.Rev != "" {
			return "Deploy main? · now " + d.Rev
		}
		return "Deploy main?"
	case model.AskUnstick:
		if ticket.PublicBlocked(t) {
			return "Public review blocked it"
		}
		return "Stuck: retry or drop?"
	}
	return KindLabel(a.Kind)
}

// DeskItem is one ticket on the desk with its ask.
type DeskItem struct {
	T   *ticket.Ticket
	Ask *ticket.Ask
	// Responded: the last thing on the ticket is the viewer's own "note" or
	// "asked", made after the ask arrived. The item waits lower down until
	// the factory does anything new with it.
	Responded, RespondedAt string
	Answered               bool // the last thing on it is the answer to your question
}

// Viewer is the person reading the desk: the name their answers record as
// by (the maintainer's is the first of [board] people), and whether they
// are the maintainer. Only a person's own response moves an ask down their
// desk; one whose history names nobody (older boards) is the
// maintainer's.
//
// Their desk holds the asks of their Desk projects (none: every project),
// except that an ask put to one of People by name (a board question's
// recipient, else the ticket's assignee) is on that person's desk alone,
// whatever its project.
type Viewer struct {
	By         string
	Maintainer bool
	Desk       []string
	People     []string
	// Dismissed is, by ticket, the newest mention or pass they put away.
	Dismissed map[string]string
}

// made reports a response by this viewer; by is who made it, "" when the
// history does not say.
func (v Viewer) made(by string) bool {
	if by == "" || by == "maintainer" && v.Maintainer { // config.DefaultPerson: before [board] people named them
		return v.Maintainer
	}
	return by != "factory" && by == v.By
}

// PutTo is the person an ask on t is put to by name, or "": a board
// question's recipient, else the ticket's assignee, when one of people.
func PutTo(t *ticket.Ticket, a *ticket.Ask, people []string) string {
	if q := Asked(t, a); q != nil && q.ToPerson && slices.Contains(people, q.Recipient) {
		return q.Recipient
	}
	if as := t.Assignee.Or(""); as != "" && slices.Contains(people, as) {
		return as
	}
	return ""
}

// Mine reports whether the ask a on t is on this viewer's desk.
func (v Viewer) Mine(t *ticket.Ticket, a *ticket.Ask) bool {
	if p := PutTo(t, a, v.People); p != "" {
		return p == v.By
	}
	return len(v.Desk) == 0 || slices.Contains(v.Desk, t.Project)
}

// Responded is v's response to the ask, if the ticket's last event is one:
// a note or a question ("note" or "asked") and when, made at or after the
// ask arrived, by v. Typed history says who wrote a note, and a question's
// task (found in all) who asked it; on older boards the factory's own notes
// (a GitHub review, a rebase request) come with a move off the desk, so they
// are never the last event.
func Responded(t *ticket.Ticket, a *ticket.Ask, v Viewer, all map[ticket.ID]*ticket.Ticket) (event, at string) {
	if len(t.History) == 0 {
		return "", ""
	}
	h := t.History[len(t.History)-1]
	if (h.Event != "note" && h.Event != "asked") || ParseTime(h.At).Before(ParseTime(a.Since)) {
		return "", ""
	}
	by := byOf(h)
	if id, _ := AskOf(h); h.Event == "asked" && all[ticket.ID(id)] != nil {
		by = all[ticket.ID(id)].By
	}
	if by == "factory" || !v.made(by) {
		return "", ""
	}
	return h.Event, h.At
}

// Waiting counts the asks nobody has responded to yet: the desk's number,
// the badge's and the floor's.
func Waiting(items []DeskItem) int {
	n := 0
	for _, it := range items {
		if it.Responded == "" {
			n++
		}
	}
	return n
}

// Desk lists the tickets with an ask as v sees them: the ones still waiting
// for v's response first, then by kind in desk order, then oldest first,
// then by id.
func Desk(ts []*ticket.Ticket, v Viewer) []DeskItem { return new(reader).desk(ts, v) }

func (r *reader) desk(ts []*ticket.Ticket, v Viewer) []DeskItem {
	var out []DeskItem
	all := ByID(ts)
	for _, t := range ts {
		if a := AskFor(t); a != nil && v.Mine(t, a) {
			resp, at := Responded(t, a, v, all)
			answered := len(t.History) > 0 && t.History[len(t.History)-1].Event == "answered"
			out = append(out, DeskItem{t, a, resp, at, answered})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Responded == "") != (out[j].Responded == "") {
			return out[i].Responded == ""
		}
		ri, rj := askRank(out[i].Ask.Kind), askRank(out[j].Ask.Kind)
		if ri != rj {
			return ri < rj
		}
		si, sj := ParseTime(out[i].Ask.Since), ParseTime(out[j].Ask.Since)
		if !si.Equal(sj) {
			return si.Before(sj)
		}
		return IDLess(out[i].T.ID, out[j].T.ID)
	})
	return out
}

// Chip is one filter of the desk: all, or one ask kind.
type Chip struct {
	Label string
	N     int
	On    bool
	Href  string
}

// QueueItem is one row of the desk.
type QueueItem struct {
	ID, Kind, Label, Glyph, Question, Title, Project, Age, Href string
	Action                                                      string // what the ask wants, in a few words
	Responded                                                   string // "You asked 5 min ago": set when the item waits on the factory, not on you
	Answered                                                    bool   // back on the desk because your question was answered
	Current                                                     bool
	Walls                                                       []string // the files the change touches in its project's walls
}

// DeskView is the desk's queue: the filter chips, what still waits for you,
// and what you have responded to.
type DeskView struct {
	Filters []Chip
	Items   []QueueItem // still waiting for you
	Done    []QueueItem // you have responded; back up top when the factory does anything new
	Kind    string
	Open    bool
	Total   int
}

// DeskPage is the desk's queue for a filter (an ask kind, or "" for all; an
// unknown kind shows all) with the current ticket marked.
func DeskPage(items []DeskItem, kind, current string, now time.Time) *DeskView {
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Ask.Kind]++
	}
	if counts[kind] == 0 {
		kind = ""
	}
	v := &DeskView{Kind: kind, Total: len(items)}
	v.Filters = append(v.Filters, Chip{Label: "All", N: len(items), On: kind == "", Href: "/"})
	for _, info := range model.AskKinds() {
		if k := string(info.Kind); counts[k] > 0 {
			v.Filters = append(v.Filters, Chip{Label: info.Label, N: counts[k], On: kind == k, Href: "/?kind=" + k})
		}
	}
	for _, it := range items {
		if kind != "" && it.Ask.Kind != kind {
			continue
		}
		id := string(it.T.ID)
		q := QueueItem{ID: id, Kind: it.Ask.Kind, Label: KindLabel(it.Ask.Kind), Glyph: KindGlyph(it.Ask.Kind),
			Question: Headline(it.T, it.Ask), Title: it.T.Title, Action: Action(it.T, it.Ask), Project: it.T.Project,
			Age: AgeSince(it.Ask.Since, now), Href: TicketHref(id, kind), Current: id == current, Answered: it.Answered,
			Walls: slices.Concat(Walls(it.T), DraftWalls(it.T))}
		switch it.Responded {
		case "asked":
			q.Responded = "You asked " + AgeSince(it.RespondedAt, now) + " ago · the answer comes here"
		case "note":
			q.Responded = "You left a note " + AgeSince(it.RespondedAt, now) + " ago · still yours to decide"
		}
		if q.Responded != "" {
			v.Done = append(v.Done, q)
			continue
		}
		v.Items = append(v.Items, q)
	}
	return v
}

// TicketHref is a ticket page's path, keeping the desk's filter.
func TicketHref(id, kind string) string {
	if kind != "" {
		return "/t/" + id + "?kind=" + url.QueryEscape(kind)
	}
	return "/t/" + id
}
