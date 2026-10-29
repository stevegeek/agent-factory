package ops_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const twoProjects = `[projects.gem]
path = "/srv/gem"
base = "main"

[projects.widgets]
path = "/srv/widgets"
base = "main"
`

// Each verb reads the config as it is now: a project removed from
// config, or switched to board mode, refuses the next add; a paused project
// still takes tickets, as in Python (the tick starts none of them).
func TestAddFollowsTheConfig(t *testing.T) {
	w := newWorld(t, twoProjects)
	ctx := context.Background()
	add := func(project string) error {
		_, err := w.svc.Add(ctx, ops.AddIn{Project: project, Title: "x"})
		return err
	}
	if err := add("widgets"); err != nil {
		t.Fatal(err)
	}
	rewrite := func(text string) {
		t.Helper()
		// a new inode and size, as an editor's save gives
		tmp := w.paths.Config + ".new"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, w.paths.Config); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("[projects.gem]\npath = \"/srv/gem\"\nbase = \"main\"\n")
	if err := add("widgets"); err == nil || !strings.HasPrefix(err.Error(), "unknown project widgets; known: gem") {
		t.Errorf("a removed project: %v", err)
	}
	rewrite("[projects.gem]\npath = \"/srv/gem\"\nbase = \"main\"\n\n[projects.widgets]\nmode = \"board\"\n")
	if err := add("widgets"); err == nil || err.Error() != "widgets is a board project: use `factory ticket create`" {
		t.Errorf("a project switched to board mode: %v", err)
	}
	rewrite("[projects.gem]\npath = \"/srv/gem\"\nbase = \"main\"\nenabled = false\n")
	if err := add("gem"); err != nil {
		t.Errorf("a paused project still takes tickets: %v", err)
	}
	// a config that stops parsing keeps the last good one
	rewrite("[projects.gem\n")
	if err := add("gem"); err != nil {
		t.Errorf("a broken config keeps the last good one: %v", err)
	}
}

// --body-file wins over --body, reads text with universal newlines, and
// {github} is replaced in it.
func TestAddReadsTheBodyFile(t *testing.T) {
	w := newWorld(t, "[projects.gem]\npath = \"/srv/gem\"\nbase = \"main\"\ngithub = \"o/gem\"\n")
	f := filepath.Join(t.TempDir(), "body.md")
	os.WriteFile(f, []byte("Line one\r\nLine two\rsee {github}\n"), 0o644)
	id, err := w.svc.Add(context.Background(), ops.AddIn{Project: "gem", Title: "x", Body: "ignored", BodyFile: f})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.load(id).Body; got != "Line one\nLine two\nsee o/gem\n" {
		t.Errorf("body %q", got)
	}
	if _, err := w.svc.Add(context.Background(), ops.AddIn{Project: "gem", Title: "x", BodyFile: f + ".missing"}); err == nil {
		t.Errorf("a missing body file")
	}
	os.WriteFile(f, []byte("bad \xff"), 0o644)
	if _, err := w.svc.Add(context.Background(), ops.AddIn{Project: "gem", Title: "x", BodyFile: f}); err == nil {
		t.Errorf("a body file that is not UTF-8")
	}
}

// An unknown --host is refused before anything is filed (Python filed the
// ticket first and then refused the host: a deliberate fix).
func TestAddRefusesAnUnknownHostBeforeFiling(t *testing.T) {
	w := newWorld(t, twoProjects+"\n[hosts.helper]\nssh = \"h\"\n")
	_, err := w.svc.Add(context.Background(), ops.AddIn{Project: "gem", Title: "x", Host: "mars"})
	if err == nil || err.Error() != "unknown host mars; known: auto, local, helper" {
		t.Fatalf("%v", err)
	}
	if len(w.files()) != 0 {
		t.Errorf("a ticket was filed: %v", sortedKeys(w.files()))
	}
}

// Foreman's ＋ page files through the same checks.
func TestFileFromForeman(t *testing.T) {
	w := newWorld(t, twoProjects)
	ctx := context.Background()
	for _, c := range []struct{ project, kind, title, body, wantKind, err string }{
		{"gem", "change", "Release 1.2.0", "Bump **version**", "", ""},
		{"widgets", "intent", "-weird title", "", "intent", ""},
		{"factory", "task", "What is new?", "-dash", "task", ""},
		{"factory", "manage", "A morning check", "", "manage", ""},
		{"factory", "change", "x", "", "", "unknown project factory"},
		{"gem", "prospect", "x", "", "", "a new ticket is"},
	} {
		id, err := w.svc.File(ctx, c.project, c.kind, c.title, c.body, "", "")
		if c.err != "" {
			if err == nil || !strings.HasPrefix(err.Error(), c.err) {
				t.Errorf("%+v: %v", c, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		got := w.load(id)
		if got.KindName() != c.wantKind || got.Title != c.title || got.Body != c.body || got.By != "maintainer" || got.Door != "human" {
			t.Errorf("%+v: %+v", c, got)
		}
	}
}

// port: TF KindAskTest.test_note_reaches_the_next_agent
func TestNoteReachesTheNextAgent(t *testing.T) {
	w := newWorld(t, twoProjects)
	w.new(ticket.NewIn{Project: "gem", Title: "Fix", Body: "body", Door: "human", Column: "ready"}, nil)
	if err := w.svc.Note(context.Background(), "T0001", "keep the old API"); err != nil {
		t.Fatal(err)
	}
	got := w.load("T0001")
	if len(got.Notes) != 1 || got.Notes[0].Text != "keep the old API" || got.History[len(got.History)-1].Event != "note" {
		t.Errorf("note: %+v", got.Notes)
	}
}

type boardNotes struct{ notes []string }

func (b *boardNotes) BoardNote(_ context.Context, id ticket.ID, text string) error {
	b.notes = append(b.notes, string(id)+" "+text)
	return nil
}

// A note on a board ticket joins its conversation through the ticket API.
func TestANoteOnABoardTicketGoesToTheTicketAPI(t *testing.T) {
	w := newWorld(t, twoProjects)
	w.new(ticket.NewIn{Project: "notes", Title: "Item", Body: "b", Door: "human", Column: "inbox", Mode: model.ModeBoard}, nil)
	if err := w.svc.Note(context.Background(), "T0001", "x"); err == nil {
		t.Errorf("a board note with no ticket API")
	}
	bn := &boardNotes{}
	w.svc.Notes = bn
	if err := w.svc.Note(context.Background(), "t0001", "a comment"); err != nil || strings.Join(bn.notes, ";") != "T0001 a comment" {
		t.Errorf("board note: %v %v", bn.notes, err)
	}
	if len(w.load("T0001").Notes) != 0 {
		t.Errorf("a board ticket got a pipeline note")
	}
}

// A CLI change gives up while the tick holds the ticket, saving nothing; a
// name that is not an id is no ticket.
//
// port: TF TickRaceTest.test_a_cli_change_gives_up_while_the_tick_holds_the_ticket
func TestAChangeGivesUpWhileTheTickHoldsTheTicket(t *testing.T) {
	w := newWorld(t, twoProjects)
	w.new(ticket.NewIn{Project: "gem", Title: "Fix", Body: "b", Door: "human", Column: "gate"}, nil)
	ctx := context.Background()
	held, err := w.board.Lock(ctx, "T0001", store.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	w.svc.Wait = 300 * time.Millisecond
	err = w.svc.Note(ctx, "T0001", "x")
	var busy *store.BusyError
	if !errors.As(err, &busy) || !strings.Contains(err.Error(), "busy") {
		t.Errorf("busy: %v", err)
	}
	held.Unlock()
	if len(w.load("T0001").Notes) != 0 {
		t.Errorf("a busy change saved")
	}
	var nf *store.NotFoundError
	if err := w.svc.Note(ctx, "../T0001", "x"); !errors.As(err, &nf) {
		t.Errorf("not an id: %v", err)
	}
}

// A send-back from pr_open holds the PR (back to draft) in the same save;
// a hold that fails warns that the PR is still open for merging.
//
// port: TF GithubOutTest.test_sending_a_pr_back_turns_it_back_to_draft
func TestSendingAPRBackHoldsIt(t *testing.T) {
	w := newWorld(t, deskConfig)
	desk(w)
	ctx := context.Background()
	if out, err := w.svc.Move(ctx, "T0001", "ready", ""); err != nil || out != "" {
		t.Fatalf("%q %v", out, err)
	}
	got := w.load("T0001")
	if !got.PR.V.Draft.Or(false) || len(w.pr.holds) != 1 || got.History[len(got.History)-1].Event != "pr" {
		t.Errorf("held: %+v %v", got.PR.V, w.pr.holds)
	}
	// done and rejected leave the PR alone, as does a move within pr_open
	w = newWorld(t, deskConfig)
	desk(w)
	w.svc.Move(ctx, "T0001", "done", "")
	if len(w.pr.holds) != 0 {
		t.Errorf("a close held the PR")
	}
	w = newWorld(t, deskConfig)
	desk(w)
	w.pr.holdErr = errors.New("502")
	out, err := w.svc.Move(ctx, "T0001", "triage", "")
	if err != nil || out != "warning: PR #23 is still open for merging; it could not be turned back to draft" {
		t.Errorf("failed hold: %q %v", out, err)
	}
	if got := w.load("T0001"); got.Column != "triage" || got.History[len(got.History)-1].Event != "error" {
		t.Errorf("the move stands and the error is on the ticket: %s", got.Column)
	}
}

// Foreman's intents: the send-back records hold_requested, the open
// records open_requested, and the save that records one wakes the tick.
// Nothing in Intents talks to GitHub.
func TestIntentsRecordWhatTheTickDoes(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	w.svc.PR = ops.Intents{Clock: fixedClock{}}
	desk(w)
	if _, err := w.svc.Move(ctx, "T0001", "ready", ""); err != nil {
		t.Fatal(err)
	}
	pr := w.load("T0001").PR.V
	if pr.HoldRequested.Or("") != "2026-10-18T10:00:00+00:00" || w.wakes.count() != 1 {
		t.Errorf("hold intent %+v, wakes %d", pr, w.wakes.count())
	}
	if pr.Draft.Or(false) != true {
		t.Errorf("the intent leaves the PR's state to the tick")
	}
	// nothing to hold: no PR number, or GitHub off
	w = newWorld(t, twoProjects)
	w.svc.PR = ops.Intents{Clock: fixedClock{}}
	w.new(ticket.NewIn{Project: "gem", Title: "x", Body: "b", Door: "human", Column: "pr_open"}, func(t *ticket.Ticket) {
		t.PR = ticket.Some(ticket.PR{Number: 4})
	})
	w.svc.Move(ctx, "T0001", "ready", "")
	if w.load("T0001").PR.V.HoldRequested.Present() || w.wakes.count() != 0 {
		t.Errorf("an intent where hold_pr does nothing")
	}

	// open: only ready_for_pr with a gate, on a GitHub project
	w = newWorld(t, deskConfig)
	w.svc.PR = ops.Intents{Clock: fixedClock{}}
	w.new(ticket.NewIn{Project: "widgets", Title: "Out", Body: "b", Door: "human", Column: "gate"}, func(t *ticket.Ticket) {
		t.Gate = ticket.Some(ticket.Gate{ChecksPass: true})
		ticket.Move(t, "ready_for_pr", model.ReasonGatePassed, "gate passed", "", "", now)
	})
	w.new(ticket.NewIn{Project: "widgets", Title: "Early", Body: "b", Door: "human", Column: "ready"}, nil)
	url, err := w.svc.OpenPR(ctx, "T0001")
	if err != nil || url != "" {
		t.Fatalf("open: %q %v", url, err)
	}
	if w.load("T0001").PR.V.OpenRequested.Or("") == "" || w.wakes.count() != 1 {
		t.Errorf("open intent not recorded or not woken")
	}
	if _, err := w.svc.OpenPR(ctx, "T0002"); err == nil || err.Error() != "T0002 is not ready for a PR" {
		t.Errorf("open too early: %v", err)
	}
	var cfg *config.Config
	if _, err := (ops.Intents{}).Open(ctx, mustConfig(t, twoProjects, &cfg), w.load("T0001")); err == nil {
		t.Errorf("open on a project with no GitHub repository")
	}
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return now }

func mustConfig(t *testing.T, text string, out **config.Config) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	*out = c
	return c
}

// Approving a panel's proposal files one change ticket, ready for work.
//
// port: TF PanelTest.test_approving_a_proposal_files_a_change_ticket
func TestApprovingAProposalFilesAChangeTicket(t *testing.T) {
	w := newWorld(t, twoProjects)
	prop := w.new(ticket.NewIn{Project: "widgets", Title: "Faster", Body: "b", Door: "clock", Column: "needs_human",
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("proposal")}, {Key: "parent", Value: jsonx.String("T0099")},
			{Key: "proposal", Value: jsonx.ObjectOf(jsonx.Member{Key: "title", Value: jsonx.String("Faster")},
				jsonx.Member{Key: "noticed", Value: jsonx.String("n")}, jsonx.Member{Key: "change", Value: jsonx.String("c")},
				jsonx.Member{Key: "why", Value: jsonx.String("It is slow.")}, jsonx.Member{Key: "cost", Value: jsonx.String("small")})}}}, nil)
	ids, err := w.svc.Approve(context.Background(), string(prop.ID), nil)
	if err != nil || len(ids) != 1 {
		t.Fatalf("approve: %v %v", ids, err)
	}
	if got := w.load(prop.ID); len(got.Children) != 1 || got.Children[0] != string(ids[0]) || got.Column != "done" {
		t.Errorf("the proposal: children %v, column %s", got.Children, got.Column)
	}
	change := w.load(ids[0])
	stage, _ := model.StageForColumn(model.Column(change.Column))
	if change.Column != "ready" || change.KindName() != "" || stage != model.StageWork {
		t.Errorf("the change: column %s, kind %q, stage %s", change.Column, change.KindName(), stage)
	}
}

// Python's verbs refuse a board ticket with the factory_ticket code, and
// leave it where it was.
//
// port: TT ModeTest.test_pipeline_commands_refuse_a_board_ticket
func TestBoardTicketsAreRefused(t *testing.T) {
	w := newWorld(t, twoProjects)
	w.new(ticket.NewIn{Project: "notes", Title: "Item", Body: "b", Door: "human", Column: "inbox", Mode: model.ModeBoard}, nil)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"move":     func() error { _, err := w.svc.Move(ctx, "T0001", "ready", ""); return err },
		"reply":    func() error { _, err := w.svc.Reply(ctx, "T0001", "x"); return err },
		"ask":      func() error { _, err := w.svc.Ask(ctx, "T0001", "x"); return err },
		"approve":  func() error { _, err := w.svc.Approve(ctx, "T0001", nil); return err },
		"redirect": func() error { return w.svc.Redirect(ctx, "T0001", "x") },
		"merge":    func() error { _, err := w.svc.RequestMerge(ctx, "T0001"); return err },
		"pr":       func() error { _, err := w.svc.OpenPR(ctx, "T0001"); return err },
		"host":     func() error { return w.svc.Host(ctx, "T0001", "local") },
	} {
		var r *ops.Refused
		if err := call(); !errors.As(err, &r) || r.Code != ops.FactoryTicket ||
			!strings.Contains(r.Message, "is on a board project; use `factory ticket") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := w.load("T0001"); got.Column != "inbox" {
		t.Errorf("the board ticket moved to %s", got.Column)
	}
}

// A pin to a host whose kinds rule leaves out the project's kind is
// refused, on add and on host; a host that takes the kind is pinned.
func TestAPinToAHostThatExcludesTheKindIsRefused(t *testing.T) {
	w := newWorld(t, twoProjects+"language = \"Go module\"\n\n[hosts.hostx]\ntransport = \"pull\"\nnot_kinds = [\"go-module\"]\n")
	ctx := context.Background()
	want := "hostx does not take go-module projects ([hosts.hostx] never go-module)"
	var r *ops.Refused
	if _, err := w.svc.Add(ctx, ops.AddIn{Project: "widgets", Title: "x", Host: "hostx"}); !errors.As(err, &r) || !strings.Contains(r.Message, want) {
		t.Fatalf("add: %v", err)
	}
	id, err := w.svc.Add(ctx, ops.AddIn{Project: "widgets", Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Host(ctx, string(id), "hostx"); !errors.As(err, &r) || !strings.Contains(r.Message, want) {
		t.Fatalf("host: %v", err)
	}
	if got := w.load(id); got.Host.Present() {
		t.Fatalf("pinned anyway: %+v", got.Host)
	}
	if _, err := w.svc.Add(ctx, ops.AddIn{Project: "gem", Title: "y", Host: "hostx"}); err != nil {
		t.Fatalf("a gem: %v", err)
	}
}

// Approving a design files its tickets in their numbers' order, each
// waiting for the earlier ones it names and for open tickets; a done ticket
// needs no wait, and a proposed ticket left out is said in the body.
func TestApprovingADesignFilesItsTicketsWithTheirOrder(t *testing.T) {
	for _, tc := range []struct {
		only  []int
		want  map[string][]string // title -> titles or ids it waits for
		unmet string
	}{
		{nil, map[string][]string{"A": nil, "B": {"A", "T0001"}, "C": {"B"}}, ""},
		{[]int{3, 2}, map[string][]string{"B": {"T0001"}, "C": {"B"}}, "proposed ticket 1"},
	} {
		w := newWorld(t, twoProjects)
		w.new(ticket.NewIn{Project: "widgets", Title: "Open", Body: "b", Door: "human", Column: "ready"}, nil)
		w.new(ticket.NewIn{Project: "widgets", Title: "Shipped", Body: "b", Door: "human", Column: "done"}, nil)
		intent := w.new(ticket.NewIn{Project: "widgets", Title: "Idea", Body: "b", Door: "human", Column: "needs_human",
			Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("intent")}}}, func(t *ticket.Ticket) {
			t.Design = ticket.Some("## Approach")
			t.Proposed = []ticket.Proposed{{Title: "A", Body: "a"}, {Title: "B", Body: "b", After: []string{"1", "T0001", "T0002"}},
				{Title: "C", Body: "c", After: []string{"2"}}}
			ticket.Move(t, "needs_human", model.ReasonDesignReady, "design ready (3 tickets proposed): x", "", "", now)
		})
		ids, err := w.svc.Approve(context.Background(), string(intent.ID), tc.only)
		if err != nil || len(ids) != len(tc.want) {
			t.Fatalf("approve %v: %v %v", tc.only, ids, err)
		}
		byTitle := map[string]ticket.ID{}
		for _, id := range ids {
			byTitle[w.load(id).Title] = id
		}
		for _, id := range ids {
			got := w.load(id)
			var want []ticket.ID
			for _, x := range tc.want[got.Title] {
				if strings.HasPrefix(x, "T") {
					want = append(want, ticket.ID(x))
				} else {
					want = append(want, byTitle[x])
				}
			}
			if !slices.Equal(got.After, want) {
				t.Errorf("only %v: %s waits for %v, want %v", tc.only, got.Title, got.After, want)
			}
			if mentions := strings.Contains(got.Body, "which was not filed with it"); mentions != (tc.unmet != "" && got.Title == "B") ||
				(mentions && !strings.Contains(got.Body, tc.unmet)) {
				t.Errorf("only %v: %s body %q", tc.only, got.Title, got.Body)
			}
		}
	}
}
