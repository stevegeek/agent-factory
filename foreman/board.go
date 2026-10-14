package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The ticket as bin/factory writes it. Agent reports stay loose: agents write them.
type Ticket struct {
	ID                string            `json:"id"`
	Project           string            `json:"project"`
	Title             string            `json:"title"`
	Body              string            `json:"body"`
	Door              string            `json:"door"`
	Column            string            `json:"column"`
	Kind              string            `json:"kind"`
	Branch            string            `json:"branch"`
	Agent             *Agent            `json:"agent"`
	Reports           map[string]Report `json:"reports"`
	Created           string            `json:"created"`
	Updated           string            `json:"updated"`
	History           []Hist            `json:"history"`
	Source            *Source           `json:"source"`
	Original          *Original         `json:"original"`
	Gate              *Gate             `json:"gate"`
	PR                *PR               `json:"pr"`
	SecurityTier      *int              `json:"security_tier"`
	SecurityQuestions []string          `json:"security_questions"`
	Design            string            `json:"design"`
	Proposed          []Proposed        `json:"proposed"`
	Feedback          []Feedback        `json:"feedback"`
	Proposals         []Proposal        `json:"proposals"`
	Votes             []Vote            `json:"votes"`
	PanelResult       *PanelResult      `json:"panel_result"`
	Proposal          *Proposal         `json:"proposal"`
	Panel             *Panel            `json:"panel"`
	Parent            string            `json:"parent"`
	Children          []string          `json:"children"`
	Result            string            `json:"result"`
	Notes             []Note            `json:"notes"`
	Ask               *Ask              `json:"ask"`
	Host              string            `json:"host"`
	AgentLastHost     string            `json:"agent_last_host"`
}

type Agent struct {
	Name    string `json:"name"`
	Stage   string `json:"stage"`
	Started string `json:"started"`
	Host    string `json:"host"`
}

type Report map[string]any

type Hist struct {
	At     string `json:"at"`
	Event  string `json:"event"`
	Detail string `json:"detail"`
	Rule   string `json:"rule"`
}

type Source struct {
	Issue int    `json:"issue"`
	URL   string `json:"url"`
}

type Original struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type Gate struct {
	ChecksPass      bool     `json:"checks_pass"`
	Files           []string `json:"files"`
	ChangedLines    int      `json:"changed_lines"`
	WithinSize      bool     `json:"within_size"`
	Outside         []string `json:"outside_allowed_paths"`
	RuntimeBehavior any      `json:"runtime_behaviour"` // copied from the reviewer's report: any JSON
	AutoEligible    bool     `json:"auto_eligible"`
	Blockers        []string `json:"blockers"`
}

type PR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Draft  bool   `json:"draft"`
}

type Proposed struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type Feedback struct {
	At   string `json:"at"`
	Note string `json:"note"`
}

type Proposal struct {
	Title   string `json:"title"`
	Noticed string `json:"noticed"`
	Change  string `json:"change"`
	Why     string `json:"why"`
	Cost    string `json:"cost"`
}

// A prospect's ballot: proposal index ("1".."5") -> score and reason.
type Vote struct {
	Voter  string               `json:"voter"`
	Host   string               `json:"host"`
	Scores map[string]VoteScore `json:"scores"`
}

type VoteScore struct {
	Score  int    `json:"score"`
	Reason string `json:"reason"`
}

type PanelResult struct {
	Kept           []string `json:"kept"`
	Dropped        []string `json:"dropped"`
	DroppedTickets []string `json:"dropped_tickets"`
}

type Panel struct {
	Scores   []int       `json:"scores"`
	Mean     float64     `json:"mean"`
	Spread   float64     `json:"spread"`
	Keen     int         `json:"keen"`
	Priority float64     `json:"priority"`
	Survives bool        `json:"survives"`
	Votes    []PanelVote `json:"votes"`
}

// One voter's score for one proposal, as the dispatcher files it on the proposal ticket.
type PanelVote struct {
	Voter  string `json:"voter"`
	Host   string `json:"host"`
	Score  int    `json:"score"`
	Reason string `json:"reason"`
}

type Note struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

type Ask struct {
	Kind        string `json:"kind"`
	Question    string `json:"question"`
	Why         string `json:"why"`
	Since       string `json:"since"`
	RetryColumn string `json:"retry_column"`
}

var ticketID = regexp.MustCompile(`^T[0-9]{4,6}$`)

func validID(id string) bool { return ticketID.MatchString(id) }

func (r Report) str(k string) string {
	if r == nil {
		return ""
	}
	switch v := r[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64, bool:
		b, _ := json.Marshal(v)
		return string(b)
	}
	return ""
}

func (r Report) list(k string) []any {
	if r == nil {
		return nil
	}
	l, _ := r[k].([]any)
	return l
}

func (t *Ticket) report(stage string) Report { return t.Reports[stage] }

func (t *Ticket) closed() bool { return t.Column == "done" || t.Column == "rejected" }

// The board: ticket files re-read only when they change.
type Board struct {
	dir   string
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	mod  time.Time
	size int64
	t    *Ticket
}

func NewBoard(root string) *Board {
	return &Board{dir: filepath.Join(root, "board", "tickets"), cache: map[string]cached{}}
}

// Load returns every ticket, oldest id first. A file that does not parse is logged and skipped.
func (b *Board) Load() []*Ticket {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		log.Printf("board: %v", err)
		return nil
	}
	seen := map[string]bool{}
	var out []*Ticket
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || !validID(strings.TrimSuffix(name, ".json")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		seen[name] = true
		c, ok := b.cache[name]
		if !ok || !c.mod.Equal(info.ModTime()) || c.size != info.Size() {
			data, err := os.ReadFile(filepath.Join(b.dir, name))
			if err != nil {
				continue
			}
			t, err := decodeTicket(data)
			if err != nil || !validID(t.ID) {
				log.Printf("board: skipping %s: %v", name, err)
				if ok {
					// the last good read beats dropping the ticket; not re-read until the file changes
					b.cache[name] = cached{info.ModTime(), info.Size(), c.t}
					out = append(out, c.t)
				}
				continue
			}
			c = cached{info.ModTime(), info.Size(), t}
			b.cache[name] = c
		}
		out = append(out, c.t)
	}
	for name := range b.cache {
		if !seen[name] {
			delete(b.cache, name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return idLess(out[i].ID, out[j].ID) })
	return out
}

// decodeTicket reads a ticket file. Agents write some fields, so a field of an unexpected
// type is left empty (and logged) instead of losing the whole ticket.
func decodeTicket(data []byte) (*Ticket, error) {
	var t Ticket
	err := json.Unmarshal(data, &t)
	if err == nil {
		return &t, nil
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return nil, err
	}
	t = Ticket{}
	var bad []string
	for k, v := range raw {
		one, _ := json.Marshal(map[string]json.RawMessage{k: v})
		var f Ticket
		if json.Unmarshal(one, &f) != nil {
			bad = append(bad, k)
			continue
		}
		json.Unmarshal(one, &t)
	}
	sort.Strings(bad)
	log.Printf("board: %s: ignoring fields of an unexpected type: %s", t.ID, strings.Join(bad, ", "))
	return &t, nil
}

// boardVersion changes whenever any ticket does: the dispatcher stamps `updated` on every save,
// and the counts catch a writer that does not.
func boardVersion(ts []*Ticket) string {
	h := sha256.New()
	for _, t := range ts {
		agent := ""
		if t.Agent != nil {
			agent = t.Agent.Name
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%d\x00%s\n", t.ID, t.Updated, t.Column, len(t.History), len(t.Notes), agent)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func byID(ts []*Ticket) map[string]*Ticket {
	m := make(map[string]*Ticket, len(ts))
	for _, t := range ts {
		m[t.ID] = t
	}
	return m
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
