// Package ticket is the ticket entity and its pure rules: the typed views
// over a ticket file's ordered JSON tree, the history entries and their
// typed payloads, the one set of readers for legacy history (Interpret), the
// mutators that record history, and the asks a waiting ticket puts to a
// person (AskFor).
//
// The tree as read is the source of truth. Decode reads typed views from it
// leniently; Encode writes them back into it path by path, so that key order
// and keys no view knows survive at every depth, and a ticket nobody changed
// is written back byte for byte.
//
// The package does no I/O and never asks for the time: callers pass it in.
// Mutators do not log; they queue lines (Logs) that the store writes after
// the file is saved.
package ticket

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
)

// Ticket is one ticket file. The exported fields are the typed views, in the
// order Python inserts the keys of a ticket that grows through the pipeline
// (a key a save adds is appended in this order).
type Ticket struct {
	ID      ID          `json:"id"`
	Schema  Opt[int]    `json:"schema"` // board tickets: 1
	Mode    Opt[string] `json:"mode"`   // "board", or absent for a factory ticket
	Project string      `json:"project"`
	Title   string      `json:"title"`
	Body    string      `json:"body"`
	Door    string      `json:"door"`
	By      string      `json:"by"`
	Column  string      `json:"column"` // a model.Column or, on a board ticket, a model.BoardColumn
	Kind    Opt[string] `json:"kind"`   // absent or null: a change

	// board fields (Labels, Links and Parent also on factory tickets filed through the API)
	Assignee  Opt[string]     `json:"assignee"`
	Labels    []string        `json:"labels"`
	Parent    Opt[string]     `json:"parent"`
	Children  []string        `json:"children"`
	Links     []Link          `json:"links"`
	Waiting   Opt[Waiting]    `json:"waiting"`
	Questions []BoardQuestion `json:"questions"`
	Comments  []Comment       `json:"comments"`
	Result    Opt[string]     `json:"result"`

	Branch  Opt[string]  `json:"branch"`
	Agent   Opt[Agent]   `json:"agent"`
	Reports jsonx.Object `json:"reports"` // stage -> the agent's report, verbatim and in insertion order
	Created string       `json:"created"`
	History []Entry      `json:"history"`
	Ask     Opt[Ask]     `json:"ask"`
	Updated string       `json:"updated"`

	// pipeline fields
	Source             Opt[Source]      `json:"source"`
	Host               Opt[string]      `json:"host"` // pinned host
	TimeoutMinutes     Opt[int]         `json:"timeout_minutes"`
	Ref                Opt[string]      `json:"ref"`
	AgentLastHost      Opt[string]      `json:"agent_last_host"`
	Original           Opt[Original]    `json:"original"`
	Question           Opt[Question]    `json:"question"` // an agent's question to the maintainer
	Design             Opt[string]      `json:"design"`
	Proposed           []Proposed       `json:"proposed"`
	Feedback           []Feedback       `json:"feedback"`
	Drafts             []Draft          `json:"drafts"`
	Actions            []Action         `json:"actions,omitempty"` // the manager's typed actions beside its drafts
	Proposals          []Proposal       `json:"proposals"`
	Votes              []Vote           `json:"votes"`
	Panel              Opt[Panel]       `json:"panel"`
	PanelResult        Opt[PanelResult] `json:"panel_result"`
	Proposal           Opt[Proposal]    `json:"proposal"`
	HeadBeforeReview   Opt[string]      `json:"head_before_review"`
	ReviewedHead       Opt[string]      `json:"reviewed_head"`
	HeadBeforeSecurity Opt[string]      `json:"head_before_security"`
	SecurityTier       Opt[int]         `json:"security_tier"`
	SecurityQuestions  []string         `json:"security_questions"`
	Gate               Opt[Gate]        `json:"gate"`
	PR                 Opt[PR]          `json:"pr"`
	Notes              []Note           `json:"notes"`
	Migrated           Opt[bool]        `json:"migrated"`
	Idempotency        Opt[Idempotency] `json:"idempotency"`

	// the findings flow: a finding task asked for structured findings
	// (FindingsAsked), the verifier's findings on it (Findings), and on a
	// ticket filed from one, which finding it was (FoundIn)
	FindingsAsked Opt[bool]       `json:"findings_asked"`
	Findings      []Finding       `json:"findings"`
	FoundIn       Opt[FindingRef] `json:"found_in"`

	// a board ticket imported from another tracker: where it came from and
	// what the import last wrote, so that running it again refreshes the
	// ticket instead of filing another
	Imported Opt[Imported] `json:"imported"`

	// a held deploy's Deploy? ticket on the notice project: what it would
	// deploy and why, and the maintainer's go-ahead
	Deploy Opt[Deploy] `json:"deploy"`

	tree jsonx.Value // the ordered object as read, built by New, or as last encoded
	raw  []byte      // its bytes
	logs []string    // queued log lines, written by the store after the save
}

// Agent is the record of the agent running on a ticket.
type Agent struct {
	Name     string         `json:"name"`
	Stage    string         `json:"stage"`
	Started  string         `json:"started"`
	Host     string         `json:"host"`
	Starting Opt[bool]      `json:"starting"` // write-ahead: true until the agent has started; the record that replaces it has no key
	Memory   Opt[MemoryRef] `json:"memory"`
	// Gate is set on a gate job (Stage "gate"): Gate 1's checks running on
	// another host, decided here on the heads it was started with.
	Gate Opt[GateJob] `json:"gate"`
}

// GateJob is what a gate job was started on: the gated commit and the
// base's head at the time.
type GateJob struct {
	Head string `json:"head"`
	Base string `json:"base"`
}

// MemoryRef is the memory a run started from.
type MemoryRef struct {
	Role    string `json:"role"`
	Base    string `json:"base"`
	Dir     string `json:"dir,omitempty"`
	Context bool   `json:"context"`
	Settled bool   `json:"settled,omitempty"`
}

// Ask is what a waiting ticket asks a person.
type Ask struct {
	Kind        string        `json:"kind"` // a model.AskKind
	Question    string        `json:"question"`
	Why         string        `json:"why"`
	Since       string        `json:"since"`
	RetryColumn Opt[string]   `json:"retry_column"` // null when a retry has nowhere to go
	Options     Opt[[]string] `json:"options"`      // board asks
	N           Opt[int]      `json:"n"`            // board asks: the question's number
}

// PR is the ticket's pull request, and the intents Foreman records on it.
type PR struct {
	Number         int         `json:"number,omitempty"`
	URL            string      `json:"url,omitempty"`
	Draft          Opt[bool]   `json:"draft"`
	ReviewsSeen    []int64     `json:"reviews_seen,omitempty"`
	MergeWait      Opt[string] `json:"merge_wait"`
	MergeRequested Opt[string] `json:"merge_requested"`
	OpenRequested  Opt[string] `json:"open_requested"`
	HoldRequested  Opt[string] `json:"hold_requested"`
	// Review is the latest decision a configured reviewer gave on GitHub,
	// as the tick last read it; absent while nobody has read the reviews.
	Review Opt[PRReview] `json:"review"`
	// MergeBlocked is why GitHub last refused the factory's merge, when
	// that is one waiting will not fix by itself: MergeBlockedApproval.
	MergeBlocked Opt[string] `json:"merge_blocked"`
	// MergeCommit is the commit GitHub made when it merged the PR, once
	// the factory has seen the merge.
	MergeCommit string `json:"merge_commit,omitempty"`
}

// PRReview is a reviewer's decision on a PR: State is ReviewApproved,
// ReviewChanges or ReviewNone, and Commit the commit it was given on (""
// for none).
type PRReview struct {
	State  string `json:"state"`
	Commit string `json:"commit,omitempty"`
}

// The review decisions on a ticket's pr.review, and the merge refusal on
// pr.merge_blocked.
const (
	ReviewApproved = "approved"
	ReviewChanges  = "changes"
	ReviewNone     = "none"
	// MergeBlockedApproval: GitHub's rules want an approving review of the
	// PR's head (from someone other than the App, which pushed it).
	MergeBlockedApproval = "approval"
)

// NeedsApproval reports an open PR that GitHub will not merge until the
// maintainer approves its current head (the commit the gate checked): its
// reviews were read and the latest decision is not an approval of that
// commit, or they were not read and GitHub refused a merge for want of one.
// A PR whose reviews nobody has read, and that GitHub never refused, is not
// known to need one.
func NeedsApproval(t *Ticket) bool {
	pr, ok := t.PR.Get()
	if t.Column != string(model.ColPROpen) || !ok || pr.Number == 0 {
		return false
	}
	if r, read := pr.Review.Get(); read {
		g, _ := t.Gate.Get()
		return r.State != ReviewApproved || r.Commit == "" || r.Commit != g.Head
	}
	return pr.MergeBlocked.Or("") == MergeBlockedApproval
}

// Source is the GitHub issue a ticket was swept from.
type Source struct {
	Issue int64  `json:"issue"`
	URL   string `json:"url"`
}

// Original is a ticket's title and body before triage rewrote them.
type Original struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Note is the maintainer's note. Question and Answer are set when it answers
// an agent's question.
type Note struct {
	At       string      `json:"at"`
	Text     string      `json:"text"`
	Question Opt[string] `json:"question"`
	Answer   Opt[string] `json:"answer"`
}

// Question is an agent's question to the maintainer.
type Question struct {
	Text     string `json:"text"`
	Stage    string `json:"stage"`
	Agent    string `json:"agent"`
	At       string `json:"at"`
	Answered string `json:"answered,omitempty"`
}

// BoardQuestion is a question on a board ticket, to a person or an agent.
type BoardQuestion struct {
	N          int         `json:"n"`
	By         string      `json:"by"`
	Recipient  string      `json:"recipient"`
	ToPerson   bool        `json:"to_person"`
	Text       string      `json:"text"`
	Options    []string    `json:"options"`
	At         string      `json:"at"`
	Answer     Opt[string] `json:"answer"`
	AnsweredBy Opt[string] `json:"answered_by"`
	AnsweredAt Opt[string] `json:"answered_at"`
	Withdrawn  Opt[string] `json:"withdrawn"`
}

// Open reports a question nobody has answered, on a ticket not closed since
// (closing withdraws it): is_open.
func (q BoardQuestion) Open() bool {
	return !q.Answer.Present() && !pyTruthyString(q.Withdrawn)
}

// Comment is a comment or a result on a board ticket.
type Comment struct {
	N    int    `json:"n"`
	At   string `json:"at"`
	By   string `json:"by"`
	Type string `json:"type"` // comment | result
	Text string `json:"text"`
}

// Link is a board ticket's link to a URL or another ticket.
type Link struct {
	Rel    string `json:"rel"`
	URL    string `json:"url,omitempty"`
	Ticket string `json:"ticket,omitempty"`
	Title  string `json:"title,omitempty"`
}

// Waiting says why a board ticket waits.
type Waiting struct {
	Reason   string `json:"reason"` // question | other
	Question int    `json:"question,omitempty"`
	From     string `json:"from"`
	Since    string `json:"since"`
	Text     string `json:"text"`
}

// keyOrder: Python writes a question wait as reason, question, from, since,
// text and any other as reason, text, from, since.
func (w Waiting) keyOrder() []string {
	if w.Reason == "question" {
		return []string{"reason", "question", "from", "since", "text"}
	}
	return []string{"reason", "text", "from", "since", "question"}
}

// Gate is gate 1's result.
type Gate struct {
	ChecksPass       bool        `json:"checks_pass"`
	Files            []string    `json:"files"`
	ChangedLines     int         `json:"changed_lines"`
	WithinSize       bool        `json:"within_size"`
	OutsideAllowed   []string    `json:"outside_allowed_paths"`
	RuntimeBehaviour jsonx.Value `json:"runtime_behaviour"` // copied from the reviewer's report: any JSON
	ReasoningStated  bool        `json:"reasoning_stated"`
	Head             string      `json:"head"`
	Base             string      `json:"base"`
	AutoEligible     bool        `json:"auto_eligible"`
	Blockers         []string    `json:"blockers"`
	Host             string      `json:"host,omitempty"`  // where the checks ran, when not here (a gate job)
	Walls            []string    `json:"walls,omitempty"` // the files that touch the project's walls
	// HygieneFindings counts the public-repo hygiene hits, for Record;
	// not stored (the hits live in gate-output.txt).
	HygieneFindings int `json:"-"`
}

// Proposed is a ticket a design proposes.
type Proposed struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Feedback is a design sent back, with the maintainer's note.
type Feedback struct {
	At     string `json:"at"`
	Design string `json:"design"`
	Note   string `json:"note"`
}

// Draft is a change to the factory's own config that the manager wrote.
// Title, Why and Patch are the manager's; the rest is the operator's
// record of it: the run that proposed it, the rights tier and places
// the tick's checker found, its state (a model.Item* constant; "" on a
// draft saved before the operator, which is by hand), a note on the outcome, when
// the maintainer asked to apply it, and the PR and merge commit it landed
// as.
type Draft struct {
	Title     string   `json:"title"`
	Why       string   `json:"why"`
	Patch     string   `json:"patch"`
	Run       int      `json:"run,omitempty"`
	Tier      string   `json:"tier,omitempty"`
	State     string   `json:"state,omitempty"`
	Places    []string `json:"places,omitempty"`
	Walls     []string `json:"walls,omitempty"`
	Note      string   `json:"note,omitempty"`
	Requested string   `json:"requested,omitempty"`
	PR        int64    `json:"pr,omitempty"`
	PRURL     string   `json:"pr_url,omitempty"`
	Commit    string   `json:"commit,omitempty"`
}

// Action is a typed operation the manager proposed (a model.ActionKind,
// its target host or project, hours for a pause, and why), with the
// operator's record of it like a Draft's. Its Tier comes from the model's
// table, never from the manager.
type Action struct {
	Kind      string `json:"kind,omitempty"`
	Target    string `json:"target,omitempty"`
	Hours     int    `json:"hours,omitempty"`
	Why       string `json:"why,omitempty"`
	Run       int    `json:"run,omitempty"`
	Tier      string `json:"tier,omitempty"`
	State     string `json:"state,omitempty"`
	Note      string `json:"note,omitempty"`
	Requested string `json:"requested,omitempty"`
}

// Proposal is a prospector's proposal.
type Proposal struct {
	Title   string `json:"title"`
	Noticed string `json:"noticed"`
	Change  string `json:"change"`
	Why     string `json:"why"`
	Cost    string `json:"cost"`
}

// Vote is one voter's ballot on a prospect: Scores maps the proposal's
// number ("1".."5") to {"score", "reason"}, in the order the voter gave them.
type Vote struct {
	Voter  string       `json:"voter"`
	Host   string       `json:"host"`
	Scores jsonx.Object `json:"scores"`
}

// Score is one voter's score for proposal i (1-based), if the ballot has one.
func (v Vote) Score(i int) (score int, reason string, ok bool) {
	s, found := v.Scores.Lookup(fmt.Sprint(i))
	if !found {
		return 0, "", false
	}
	n, isInt := s.Get("score").Int64()
	r, _ := s.Get("reason").Str()
	return int(n), r, isInt
}

// Panel is the panel's tally on a proposal ticket.
type Panel struct {
	Scores   []int       `json:"scores"`
	Mean     float64     `json:"mean"`
	Spread   float64     `json:"spread"`
	Keen     int         `json:"keen"`
	Priority float64     `json:"priority"`
	Survives bool        `json:"survives"`
	Votes    []PanelVote `json:"votes,omitempty"`
}

// PanelVote is one voter's score for one proposal, filed on its ticket.
type PanelVote struct {
	Voter  string `json:"voter"`
	Host   string `json:"host"`
	Score  int    `json:"score"`
	Reason string `json:"reason"`
}

// PanelResult is what a prospect's panel kept and dropped.
type PanelResult struct {
	Kept           []string `json:"kept"`
	Dropped        []string `json:"dropped"`
	DroppedTickets []string `json:"dropped_tickets,omitempty"`
}

// Idempotency is the key a create was made with.
type Idempotency struct {
	By     string `json:"by"`
	Key    string `json:"key"`
	Digest string `json:"digest"`
}

// Deploy is, on a Deploy? ticket, the held deploy of core main: Rev, the
// main it last offered (for display only: a deploy always takes main's
// head when it runs), Why, each offer's reason, Requested, the stamp of
// the maintainer's Deploy it (an intent the tick's Drain carries out), and
// Started, when the tick started that deploy.
type Deploy struct {
	Rev       string      `json:"rev"`
	Why       []string    `json:"why"`
	Requested Opt[string] `json:"requested"`
	Started   Opt[string] `json:"started"`
}

// Imported is, on a ticket an import filed, the card it came from and
// what the import last wrote. A rerun finds the ticket by From and Card,
// refreshes it only when Digest (of the title, body, labels and place it
// wrote) changes, replaces only the labels it wrote, and copies only the
// comments not in Comments.
type Imported struct {
	From     string   `json:"from"`   // the tracker: "fizzy"
	Card     string   `json:"card"`   // the card's id there
	Number   int      `json:"number"` // its number there
	Board    string   `json:"board"`  // its board's id there
	URL      string   `json:"url,omitempty"`
	Digest   string   `json:"digest"`
	Labels   []string `json:"labels"`
	Comments []string `json:"comments"` // the ids of the comments copied
}

// Board reports a board project's ticket.
func (t *Ticket) Board() bool { return t.Mode.Or("") == "board" }

// KindName is the kind, "" for a change (absent or null).
func (t *Ticket) KindName() string { return t.Kind.Or("") }

// Logs are the log lines the mutators queued since the last TakeLogs.
func (t *Ticket) Logs() []string { return append([]string(nil), t.logs...) }

// TakeLogs returns the queued log lines and forgets them: the store calls
// it after a save.
func (t *Ticket) TakeLogs() []string {
	out := t.logs
	t.logs = nil
	return out
}

// ErrNotObject is a ticket file whose JSON is not an object.
var ErrNotObject = errors.New("ticket file is not a JSON object")

// Decode reads a ticket file. A file that is not JSON, or not an object, is
// an error. Inside it decoding is lenient: a field of the wrong type reads
// as its zero value, is reported as a Problem and stays in the tree as it
// was.
func Decode(data []byte) (*Ticket, []Problem, error) {
	v, err := jsonx.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("ticket: %w", err)
	}
	t, problems, err := fromTree(v)
	if err != nil {
		return nil, nil, err
	}
	t.raw = append([]byte(nil), data...)
	return t, problems, nil
}

func fromTree(v jsonx.Value) (*Ticket, []Problem, error) {
	o, ok := v.Object()
	if !ok {
		return nil, nil, ErrNotObject
	}
	t := &Ticket{tree: v}
	var d decoder
	d.decodeFields("", o, reflect.ValueOf(t).Elem())
	return t, d.problems, nil
}

// Tree is the ticket as it would be saved now: the stored tree with the
// typed views merged in. Neither the ticket nor its tree is changed.
func (t *Ticket) Tree() jsonx.Value {
	o, _ := t.tree.Object()
	return mergeFields(o, true, reflect.ValueOf(t).Elem()).Value()
}

// Encode is the file to save: Python's json.dump(t, f, indent=2) of Tree.
// A ticket whose tree did not change gets back the bytes it was read from.
// The encoded tree becomes the ticket's stored tree.
func Encode(t *Ticket) []byte {
	m := t.Tree()
	if t.raw != nil && jsonx.Equal(m, t.tree) {
		return append([]byte(nil), t.raw...)
	}
	b := jsonx.EncodePython(m, 2)
	t.tree, t.raw = m, b
	return append([]byte(nil), b...)
}

// Clone is a deep copy: its views, its stored tree and its queued logs, so a
// stage can change it and the caller can throw it away.
func Clone(t *Ticket) *Ticket {
	c, _, err := fromTree(t.Tree().Clone())
	if err != nil {
		// t.tree is always an object: Decode and New make sure of it
		panic(err)
	}
	c.tree = t.tree.Clone()
	c.raw = t.raw
	c.logs = append([]string(nil), t.logs...)
	return c
}
