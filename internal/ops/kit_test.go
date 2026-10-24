package ops_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// now is the scenarios' frozen clock: gen.py's FACTORY_TEST_NOW.
var now = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

// logRec records the factory log lines a store writes.
type logRec struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRec) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRec) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	l.lines = nil
	return out
}

// wakes counts the ticks a verb asked for.
type wakes struct {
	mu sync.Mutex
	n  int
}

func (w *wakes) Wake(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n++
	return nil
}

func (w *wakes) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// fakePR stands in for prflow.Sync: Hold and Open record that they ran and
// change the ticket as the test says.
type fakePR struct {
	holds, opens []ticket.ID
	holdErr      error
	openURL      string
	openErr      error
}

func (f *fakePR) Hold(_ context.Context, cfg *config.Config, t *ticket.Ticket) error {
	f.holds = append(f.holds, t.ID)
	if f.holdErr != nil {
		ticket.Event(t, "error", "github: could not turn PR back to draft: "+f.holdErr.Error(), "", ticket.Errored{Text: f.holdErr.Error()}, now)
		return f.holdErr
	}
	if pr, ok := t.PR.Get(); ok && pr.Number > 0 && cfg.HasGitHub() {
		pr.Draft = ticket.Some(true)
		t.PR = ticket.Some(pr)
		ticket.Event(t, "pr", fmt.Sprintf("PR #%d back to draft while it is reworked", pr.Number), "", ticket.PRNote{Action: ticket.PRDraft, Number: pr.Number}, now)
	}
	return nil
}

func (f *fakePR) Open(_ context.Context, _ *config.Config, t *ticket.Ticket) (string, error) {
	f.opens = append(f.opens, t.ID)
	if f.openErr != nil {
		return "", f.openErr
	}
	return f.openURL, nil
}

// world is one test's factory: a root, its board and a service on it.
type world struct {
	t     testing.TB
	env   *testkit.Env
	paths paths.Paths
	board *store.Board
	svc   *ops.Service
	log   *logRec
	wakes *wakes
	pr    *fakePR
}

func newWorld(t testing.TB, configTOML string) *world {
	t.Helper()
	env := testkit.Root(t)
	env.WriteConfig(t, configTOML)
	p := paths.FromEnv(env.Vars)
	w := &world{t: t, env: env, paths: p, log: &logRec{}, wakes: &wakes{}, pr: &fakePR{}}
	fixed := testkit.Fixed{T: now}
	w.board = &store.Board{Reader: &boardread.Reader{Paths: p}, Paths: p, Clock: fixed, Log: w.log}
	w.svc = &ops.Service{Board: w.board, Config: &config.Source{Path: p.Config}, Clock: fixed, Wake: w.wakes,
		PR: w.pr, Panel: pyPanel{}}
	return w
}

// file writes a ticket file as given.
func (w *world) file(name, data string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.paths.Tickets, name), []byte(data), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// new files a ticket as new_ticket does and lets change edit it before it
// is saved again.
func (w *world) new(in ticket.NewIn, change func(t *ticket.Ticket)) *ticket.Ticket {
	w.t.Helper()
	t, err := w.board.FileTicket(context.Background(), in)
	if err != nil {
		w.t.Fatal(err)
	}
	if change != nil {
		change(t)
		if err := w.board.Save(t); err != nil {
			w.t.Fatal(err)
		}
	}
	w.log.take()
	return t
}

// load reads a ticket back.
func (w *world) load(id ticket.ID) *ticket.Ticket {
	w.t.Helper()
	t, err := w.board.Reader.Load(id)
	if err != nil {
		w.t.Fatal(err)
	}
	return t
}

// files is every ticket file and the id counter, by name.
func (w *world) files() map[string]string {
	w.t.Helper()
	out := map[string]string{}
	names, _ := filepath.Glob(filepath.Join(w.paths.Tickets, "T*.json"))
	names = append(names, filepath.Join(w.paths.Tickets, ".last-id"))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			continue
		}
		out[filepath.Base(n)] = string(b)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pyPanel is the blind panel's rules as Python writes them (panel_tally,
// panel_votes, panel_rule, file_proposal): the test's stand-in for package
// panel, which ops may not import in its tests.
type pyPanel struct{}

type tally struct {
	scores             []int
	mean, spread, prio float64
	keen               int
	survives           bool
}

func pyTally(i int, votes []ticket.Vote) tally {
	var scores []int
	for _, v := range votes {
		if s, _, ok := v.Score(i); ok {
			scores = append(scores, s)
		}
	}
	if len(scores) == 0 {
		return tally{}
	}
	sum := 0.0
	keen := 0
	for _, s := range scores {
		sum += float64(s)
		if s >= model.PanelKeenScore {
			keen++
		}
	}
	mean := sum / float64(len(scores))
	sq := 0.0
	for _, s := range scores {
		sq += (float64(s) - mean) * (float64(s) - mean)
	}
	spread := math.Sqrt(sq / float64(len(scores)))
	r2 := func(f float64) float64 { v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 2, 64), 64); return v }
	return tally{scores: scores, mean: r2(mean), spread: r2(spread), prio: r2(mean - 0.5*spread), keen: keen,
		survives: keen >= 2 || mean >= 3.5}
}

func pyFloat(f float64) string { return string(jsonx.EncodePython(jsonx.Float(f), -1)) }

func pyInts(xs []int) string {
	var s []string
	for _, x := range xs {
		s = append(s, strconv.Itoa(x))
	}
	return "[" + strings.Join(s, ", ") + "]"
}

func (pyPanel) Survives(i int, votes []ticket.Vote) bool { return pyTally(i, votes).survives }

func (pyPanel) Votes(i int, votes []ticket.Vote) []ticket.PanelVote {
	out := []ticket.PanelVote{}
	for _, v := range votes {
		if s, reason, ok := v.Score(i); ok {
			host := v.Host
			if host == "" {
				host = model.LocalHost
			}
			out = append(out, ticket.PanelVote{Voter: v.Voter, Host: host, Score: s, Reason: reason})
		}
	}
	return out
}

func (p pyPanel) FileProposal(id ticket.ID, t *ticket.Ticket, i int, at string, at0 time.Time) *ticket.Ticket {
	tl := pyTally(i, t.Votes)
	votes := p.Votes(i, t.Votes)
	var reasons []string
	var vs []jsonx.Value
	for _, v := range votes {
		reasons = append(reasons, strconv.Itoa(v.Score)+": "+v.Reason)
		vs = append(vs, jsonx.ObjectOf(jsonx.Member{Key: "voter", Value: jsonx.String(v.Voter)},
			jsonx.Member{Key: "host", Value: jsonx.String(v.Host)}, jsonx.Member{Key: "score", Value: jsonx.Int(int64(v.Score))},
			jsonx.Member{Key: "reason", Value: jsonx.String(v.Reason)}))
	}
	pr := t.Proposals[i-1]
	text := fmt.Sprintf("### Proposal %d: %s\n\n- Noticed: %s\n- Change: %s\n- Why it is worth it: %s\n- Cost: %s",
		i, pr.Title, pr.Noticed, pr.Change, pr.Why, pr.Cost)
	body := strings.SplitN(text, "\n", 3)[2] + "\n\nPanel: " + strings.Join(reasons, "; ")
	column := string(model.ColRejected)
	if tl.survives {
		column = string(model.ColNeedsHuman)
	}
	proposal, _ := t.Tree().Get("proposals").Array()
	var scores []jsonx.Value
	for _, s := range tl.scores {
		scores = append(scores, jsonx.Int(int64(s)))
	}
	panel := jsonx.ObjectOf(jsonx.Member{Key: "scores", Value: jsonx.ArrayOf(scores...)},
		jsonx.Member{Key: "mean", Value: jsonx.Float(tl.mean)}, jsonx.Member{Key: "spread", Value: jsonx.Float(tl.spread)},
		jsonx.Member{Key: "keen", Value: jsonx.Int(int64(tl.keen))}, jsonx.Member{Key: "priority", Value: jsonx.Float(tl.prio)},
		jsonx.Member{Key: "survives", Value: jsonx.Bool(tl.survives)}, jsonx.Member{Key: "votes", Value: jsonx.ArrayOf(vs...)})
	c := ticket.New(id, ticket.NewIn{Project: t.Project, Title: pr.Title, Body: body, Door: string(model.DoorClock),
		Column: column, Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("proposal")},
			{Key: "parent", Value: jsonx.String(string(t.ID))}, {Key: "proposal", Value: proposal[i-1].Clone()},
			{Key: "panel", Value: panel}}}, at0)
	keenText := "no voter scored 4 or more"
	if tl.keen > 0 {
		keenText = fmt.Sprintf("%d of %d voters scored 4 or more (2 needed)", tl.keen, len(tl.scores))
	}
	meanText := "mean " + pyFloat(tl.mean) + " (3.5 needed)"
	rule := "dropped: " + keenText + "; " + meanText
	if tl.survives {
		rule = "kept: " + meanText + "; " + keenText
		if tl.keen >= 2 {
			rule = "kept: " + keenText + "; " + meanText
		}
	}
	ticket.Event(c, "panel", "mean "+pyFloat(tl.mean)+", spread "+pyFloat(tl.spread)+", scores "+pyInts(tl.scores), rule, ticket.PanelScored{Kept: tl.survives}, at0)
	if at != "" {
		c.Created = at
		for k := range c.History {
			c.History[k].At = at
		}
		if a, ok := c.Ask.Get(); ok {
			a.Since = at
			c.SetAsk(&a)
		}
	}
	return c
}
