package ops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

const deskConfig = `[board]
people = ["sam", "alex"]

[github]
installation_id = 7

[projects.widgets]
path = "/nonexistent/widgets"
base = "main"
github = "example/widgets"
`

// desk files the asks of Foreman's fixture board (testdata/board), each
// with the ask the dispatcher stores today:
//
//	T0001 review (pr_open, a draft PR)   T0002 start (an issue)
//	T0003 decide                          T0004 design (3 proposed)
//	T0005 proposal                        T0006 unstick, retry from gate
//	T0007 unstick, retry from ready       T0008 notice (a task's answer)
//	T0009 question (an agent asks)        T0010 done
//	T0011 working, with an agent          T0012 draft (the manager's)
func desk(w *world) {
	moved := func(col string, reason model.Reason, detail string) func(t *ticket.Ticket) {
		return func(t *ticket.Ticket) { ticket.Move(t, col, reason, detail, "", "", now) }
	}
	in := func(title, column string) ticket.NewIn {
		return ticket.NewIn{Project: "widgets", Title: title, Body: "b", Door: "human", Column: column}
	}
	w.new(in("Tidy README", "ready_for_pr"), func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/tidy-readme")
		t.Gate = ticket.Some(ticket.Gate{ChecksPass: true, Blockers: []string{"40 changed lines (limit 10)"}})
		t.PR = ticket.Some(ticket.PR{Number: 23, URL: "https://github.com/example/widgets/pull/23", Draft: ticket.Some(true)})
		ticket.Move(t, "pr_open", model.ReasonPROpened, "draft PR https://github.com/example/widgets/pull/23", "", "", now)
	})
	w.new(in("Fix the parser", "triage"), func(t *ticket.Ticket) {
		t.Source = ticket.Some(ticket.Source{Issue: 18, URL: "https://github.com/example/widgets/issues/18"})
		moved("needs_human", model.ReasonTriageAccepted, "triage accepted; move to ready to start work")(t)
	})
	w.new(in("Vague", "triage"), moved("needs_human", model.ReasonTriageUnsure, "triage unsure: no failing case"))
	w.new(ticket.NewIn{Project: "widgets", Title: "Idea", Body: "b", Door: "human", Column: "working",
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("intent")}}}, func(t *ticket.Ticket) {
		t.Design = ticket.Some("## Approach")
		t.Proposed = []ticket.Proposed{{Title: "A", Body: "a"}, {Title: "B", Body: "b"}, {Title: "C", Body: "c"}}
		moved("needs_human", model.ReasonDesignReady, "design ready (3 tickets proposed): x")(t)
	})
	w.new(ticket.NewIn{Project: "widgets", Title: "Faster", Body: "b", Door: "clock", Column: "needs_human",
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("proposal")}, {Key: "parent", Value: jsonx.String("T0099")},
			{Key: "proposal", Value: jsonx.ObjectOf(jsonx.Member{Key: "title", Value: jsonx.String("Faster")},
				jsonx.Member{Key: "noticed", Value: jsonx.String("n")}, jsonx.Member{Key: "change", Value: jsonx.String("c")},
				jsonx.Member{Key: "why", Value: jsonx.String("It is slow.")}, jsonx.Member{Key: "cost", Value: jsonx.String("small")})}}}, nil)
	w.new(in("Gate failed", "gate"), moved("needs_human", model.ReasonGateFailed, "gate: tests or lint failed"))
	w.new(in("Timed out", "working"), moved("needs_human", model.ReasonTimedOut, "agent t0007-w1 timed out"))
	w.new(ticket.NewIn{Project: "factory", Title: "Version?", Body: "", Door: "human", Column: "working",
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("task")}}}, func(t *ticket.Ticket) {
		t.Result = ticket.Some("1.2.3")
		moved("needs_human", model.ReasonTaskDone, "task done: 1.2.3")(t)
	})
	w.new(in("Asks", "working"), func(t *ticket.Ticket) {
		t.Question = ticket.Some(ticket.Question{Text: "8 or 16?", Stage: "work", Agent: "t0009-w1", At: "2026-10-18T09:00:00+00:00"})
		moved("needs_human", model.ReasonAgentQuestion, "work asks: 8 or 16?")(t)
	})
	w.new(in("Merged", "pr_open"), moved("done", model.ReasonPRMerged, "PR merged by maintainer"))
	w.new(in("Running", "working"), func(t *ticket.Ticket) {
		t.Agent = ticket.Some(ticket.Agent{Name: "t0011-w1", Stage: "work", Started: "2026-10-18T09:00:00+00:00", Host: "local"})
	})
	w.new(ticket.NewIn{Project: "factory", Title: "A morning check", Body: "b", Door: "human", Column: "working",
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.String("manage")}}}, func(t *ticket.Ticket) {
		t.Drafts = []ticket.Draft{{Title: "Daily", Why: "w", Patch: "+[[schedules]]\n"}}
		moved("needs_human", model.ReasonManagerDrafted, "manager drafted 1 change")(t)
	})
}

func askOf(t *ticket.Ticket) (string, string) {
	if a, ok := t.Ask.Get(); ok {
		return a.Kind, a.Since
	}
	return "", ""
}

// Foreman's answers, as the board they leave rather than the argv they ran
// (TestActions): each answer through ops.Answer on a fresh desk.
func TestActionsLeaveTheBoard(t *testing.T) {
	type want struct {
		column string   // "" leaves it where it was
		notes  []string // the ticket's notes after
		out    string
		err    string // a substring of the refusal; nothing is saved
	}
	cases := []struct {
		id, answer, note string
		picks            []int
		want             want
	}{
		{"T0001", "send_back", "split it", nil, want{column: "ready", notes: []string{"split it"}}},
		{"T0001", "send_back", "", nil, want{err: "needs a note"}},
		{"T0002", "start", "", nil, want{column: "ready"}},
		{"T0002", "start", "  mind the cache  ", nil, want{column: "ready", notes: []string{"mind the cache"}}},
		{"T0002", "not_now", "", nil, want{column: "rejected"}},
		{"T0003", "start", "", nil, want{column: "ready"}},
		{"T0003", "drop", "", nil, want{column: "rejected"}},
		{"T0003", "reply", "is it real?", nil, want{column: "triage", notes: []string{"is it real?"}}},
		{"T0003", "reply", "", nil, want{err: "needs a note"}},
		{"T0004", "approve", "", []int{1, 3}, want{column: "done", out: "T0013 T0014"}},
		{"T0004", "approve", "", []int{1, 2, 3}, want{column: "done", out: "T0013 T0014 T0015"}},
		{"T0004", "approve", "", nil, want{err: "tick at least one"}},
		{"T0004", "approve", "", []int{4}, want{err: "pick numbers 1 to 3"}},
		{"T0004", "redirect", "smaller please", nil, want{column: "ready"}},
		{"T0004", "redirect", "-v smaller", nil, want{column: "ready"}},
		{"T0004", "redirect", "", nil, want{err: "needs a note"}},
		{"T0005", "file", "", nil, want{column: "done", out: "T0013"}},
		{"T0005", "drop", "", nil, want{column: "rejected"}},
		{"T0006", "retry", "", nil, want{column: "gate"}},
		{"T0006", "retry_note", "rebase first", nil, want{column: "gate", notes: []string{"rebase first"}}},
		{"T0006", "drop", "", nil, want{column: "rejected"}},
		{"T0007", "retry", "", nil, want{column: "ready"}},
		{"T0008", "done", "", nil, want{column: "done"}},
		{"T0009", "answer", "-yes, keep it", nil, want{column: "ready", out: "T0009: answered; a new work run carries on"}},
		{"T0009", "note", "plain note", nil, want{notes: []string{"plain note"}}},
		{"T0011", "start", "", nil, want{err: "not waiting for an answer"}},
		{"T0010", "note", "too late", nil, want{err: "closed tickets take no notes"}},
		{"T0002", "approve", "", nil, want{err: "not an answer to this ask"}},
		{"T0012", "applied", "", nil, want{column: "done"}},
		{"T0012", "drop", "", nil, want{column: "rejected"}},
		{"T0012", "send_back", "", nil, want{err: "needs a note"}},
		{"T0012", "send_back", "weekdays only", nil, want{column: "ready", notes: []string{"weekdays only"}}},
		{"T0002", "start", "bad\x00note", nil, want{err: "control characters"}},
		{"T0002", "start", strings.Repeat("x", 4001), nil, want{err: "longer than 4000"}},
	}
	for _, tc := range cases {
		t.Run(tc.id+"/"+tc.answer, func(t *testing.T) {
			w := newWorld(t, deskConfig)
			desk(w)
			before := w.files()
			id := ticket.ID(tc.id)
			kind, since := askOf(w.load(id))
			had := len(w.load(id).History)
			out, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: tc.id, Value: tc.answer, Note: tc.note,
				Picks: tc.picks, Kind: kind, Since: since})
			if tc.want.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want.err) {
					t.Fatalf("error %v, want %q", err, tc.want.err)
				}
				if after := w.files(); len(after) != len(before) || after[tc.id+".json"] != before[tc.id+".json"] {
					t.Errorf("a refused answer saved something")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out != tc.want.out {
				t.Errorf("out %q, want %q", out, tc.want.out)
			}
			got := w.load(id)
			if tc.want.column != "" && got.Column != tc.want.column {
				t.Errorf("column %s, want %s", got.Column, tc.want.column)
			}
			var notes []string
			for _, n := range got.Notes {
				notes = append(notes, n.Text)
			}
			if tc.answer != "answer" && strings.Join(notes, "|") != strings.Join(tc.want.notes, "|") {
				t.Errorf("notes %q, want %q", notes, tc.want.notes)
			}
			for _, e := range got.History[had:] {
				if e.Event == "move" || e.Event == "note" {
					if m, ok := e.Data.(ticket.Moved); ok && m.By != "sam" {
						t.Errorf("an answer is the maintainer's, not %q", m.By)
					}
				}
			}
		})
	}
}

// What a design approve, a redirect and a send-back leave, in detail.
func TestAnswersCarryTheirMeaning(t *testing.T) {
	ctx := context.Background()
	answer := func(w *world, id, value, note string, picks ...int) string {
		t.Helper()
		kind, since := askOf(w.load(ticket.ID(id)))
		out, err := w.svc.Answer(ctx, ops.AnswerIn{ID: id, Value: value, Note: note, Picks: picks, Kind: kind, Since: since})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	w := newWorld(t, deskConfig)
	desk(w)
	answer(w, "T0004", "approve", "", 3, 1, 3)
	intent := w.load("T0004")
	if strings.Join(intent.Children, " ") != "T0013 T0014" || w.load("T0013").Title != "C" || w.load("T0014").Title != "A" {
		t.Errorf("picks file in the order ticked: %v", intent.Children)
	}
	if c := w.load("T0013"); c.Parent.Or("") != "T0004" || c.Column != "ready" || !strings.HasSuffix(c.Body, "Part of: Idea") {
		t.Errorf("child %+v", c)
	}

	w = newWorld(t, deskConfig)
	desk(w)
	answer(w, "T0004", "redirect", "-v smaller")
	intent = w.load("T0004")
	if len(intent.Feedback) != 1 || intent.Feedback[0].Note != "-v smaller" || len(intent.Proposed) != 0 || len(intent.Notes) != 0 {
		t.Errorf("a redirect keeps the design as feedback and adds no note: %+v", intent.Feedback)
	}

	// send back from pr_open: a note, back to ready, and the PR held
	w = newWorld(t, deskConfig)
	desk(w)
	answer(w, "T0001", "send_back", "split it")
	got := w.load("T0001")
	if got.Column != "ready" || len(w.pr.holds) != 1 || !got.PR.V.Draft.Or(false) {
		t.Errorf("send back: column %s, holds %v", got.Column, w.pr.holds)
	}
	// one save: the note comes before the move, and the ticket was written once
	var events []string
	for _, e := range got.History {
		events = append(events, e.Event)
	}
	if tail := strings.Join(events[len(events)-3:], ","); tail != "note,move,pr" {
		t.Errorf("history ends %s", tail)
	}

	// an agent's question needs no answer to close
	w = newWorld(t, deskConfig)
	desk(w)
	answer(w, "T0009", "drop", "")
	if got := w.load("T0009"); got.Column != "rejected" || got.Ask.Present() {
		t.Errorf("drop a question: column %s, ask %+v", got.Column, got.Ask)
	}
}

// An answer to an ask that has moved on is refused under the lock, and
// changes nothing; a note, a question and a reply need no matching ask.
func TestAStaleAnswerRunsNothing(t *testing.T) {
	w := newWorld(t, deskConfig)
	desk(w)
	before := w.files()
	_, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: "T0002", Value: "start", Kind: "start",
		Since: "2020-01-01T00:00:00+00:00"})
	if !errors.Is(err, ops.ErrStale) {
		t.Fatalf("stale ask: %v", err)
	}
	if w.files()["T0002.json"] != before["T0002.json"] {
		t.Errorf("a stale answer saved the ticket")
	}
	if _, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: "T0002", Value: "note", Note: "x", Kind: "start",
		Since: "2020-01-01T00:00:00+00:00"}); err != nil {
		t.Errorf("a note needs no matching ask: %v", err)
	}
	// the ask in hand when nobody says which (the CLI without --kind)
	if _, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: "T0003", Value: "drop"}); err != nil {
		t.Errorf("answer without an expected ask: %v", err)
	}
	// a board ticket is answered through the ticket API
	w.new(ticket.NewIn{Project: "notes", Title: "Item", Body: "b", Door: "human", Column: "inbox", Mode: model.ModeBoard}, nil)
	if _, err := w.svc.Answer(context.Background(), ops.AnswerIn{ID: "T0013", Value: "note", Note: "x"}); err == nil {
		t.Errorf("a board ticket took a desk answer")
	}
}

// an answer from another Foreman user records that user as by, on its
// note and its move, and a question's task too; a name the history already
// means something by is refused, and nothing is saved.
func TestAnAnswerRecordsWhoGaveIt(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	desk(w)
	kind, since := askOf(w.load("T0001"))
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "send_back", Note: "split it", Kind: kind, Since: since, By: "robin"}); err != nil {
		t.Fatal(err)
	}
	var bys []string
	for _, e := range w.load("T0001").History {
		switch d := e.Data.(type) {
		case ticket.Noted:
			bys = append(bys, "note "+d.By)
		case ticket.Moved:
			if d.By != "factory" {
				bys = append(bys, "move "+d.By)
			}
		}
	}
	if strings.Join(bys, ", ") != "note robin, move robin" {
		t.Errorf("by: %v", bys)
	}
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0002", Value: ops.AnswerAsk, Note: "why?", By: "robin"}); err != nil {
		t.Fatal(err)
	}
	asked := 0
	for _, f := range w.files() {
		if strings.Contains(f, `"title": "Question on T0002`) {
			asked++
			if !strings.Contains(f, `"by": "robin"`) {
				t.Errorf("the question's task is not robin's:\n%s", f)
			}
		}
	}
	if asked != 1 {
		t.Errorf("%d question tasks", asked)
	}
	for _, name := range []string{"sam", "factory"} {
		before := w.files()
		if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0003", Value: "note", Note: "x", By: name}); err == nil {
			t.Errorf("a Foreman user named %s answered", name)
		}
		if after := w.files(); after["T0003.json"] != before["T0003.json"] {
			t.Errorf("a refused answer saved something")
		}
	}
	id, err := w.svc.File(ctx, "widgets", "change", "From robin", "", "robin")
	if err != nil {
		t.Fatal(err)
	}
	if by := w.load(id).By; by != "robin" {
		t.Errorf("filed by %q", by)
	}
}

// Ask and talk from the desk: the question becomes a task; the reply wakes
// the tick.
func TestDeskAskAndTalk(t *testing.T) {
	w := newWorld(t, deskConfig)
	desk(w)
	ctx := context.Background()
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "ask"}); err == nil || !strings.Contains(err.Error(), "question first") {
		t.Errorf("an empty question: %v", err)
	}
	out, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "ask", Note: "-did steep pass?"})
	if err != nil || out != "T0013" {
		t.Fatalf("ask: %q %v", out, err)
	}
	child := w.load("T0013")
	if child.KindName() != "task" || child.Parent.Or("") != "T0001" || !strings.Contains(child.Body, "-did steep pass?") {
		t.Errorf("the question's task: %+v", child)
	}
	last := w.load("T0001").History
	if e := last[len(last)-1]; e.Event != "asked" || e.Detail != "T0013: -did steep pass?" {
		t.Errorf("the parent records the question: %+v", e)
	}
	if w.wakes.count() != 0 {
		t.Errorf("an ask does not wake the tick (cmd_ask)")
	}
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "talk"}); err == nil {
		t.Errorf("an empty reply")
	}
	if out, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0003", Value: "talk", Note: "why?"}); err != nil || out != "T0014" {
		t.Errorf("a reply with no open question becomes an Ask: %q %v", out, err)
	}
	if w.wakes.count() != 1 {
		t.Errorf("a reply wakes the tick: %d", w.wakes.count())
	}
}

// The merge answer only for an eligible PR on the maintainer's word; the
// open answer through PRActions.
func TestMergeAndOpenAnswers(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, deskConfig)
	desk(w)
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "merge"}); err == nil || !strings.Contains(err.Error(), "not one the factory may merge") {
		t.Errorf("a draft PR must not be merged: %v", err)
	}
	err := w.board.Changing(ctx, "T0001", store.NoWait, func(t *ticket.Ticket) error {
		g := t.Gate.V
		g.AutoEligible = true
		t.Gate = ticket.Some(g)
		pr := t.PR.V
		pr.Draft = ticket.Some(false)
		t.PR = ticket.Some(pr)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "merge"})
	if err != nil || !strings.HasPrefix(out, "PR #23 merges on the next tick") {
		t.Fatalf("merge: %q %v", out, err)
	}
	if !w.load("T0001").PR.V.MergeRequested.Present() || w.wakes.count() != 1 {
		t.Errorf("merge records the request and wakes")
	}

	// a PR GitHub will not merge before your approval records no merge
	err = w.board.Changing(ctx, "T0001", store.NoWait, func(t *ticket.Ticket) error {
		pr := t.PR.V
		pr.MergeRequested = ticket.Opt[string]{}
		pr.Review = ticket.Some(ticket.PRReview{State: ticket.ReviewNone})
		t.PR = ticket.Some(pr)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, try := range []func() error{
		func() error { _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "merge"}); return err },
		func() error { _, err := w.svc.RequestMerge(ctx, "T0001"); return err },
	} {
		if err := try(); err == nil || !strings.Contains(err.Error(), "approve PR #23 on GitHub") {
			t.Errorf("a merge before the approval: %v", err)
		}
	}
	if w.load("T0001").PR.V.MergeRequested.Present() || w.wakes.count() != 1 {
		t.Errorf("a refused merge recorded a request or woke the tick")
	}

	// a ticket waiting to go out: open (no ask) and open (an ask)
	w = newWorld(t, deskConfig)
	w.pr.openURL = "https://github.com/example/widgets/pull/30"
	w.new(ticket.NewIn{Project: "widgets", Title: "Out", Body: "b", Door: "human", Column: "gate"}, func(t *ticket.Ticket) {
		t.Gate = ticket.Some(ticket.Gate{ChecksPass: true})
		ticket.Move(t, "ready_for_pr", model.ReasonGatePassed, "gate passed", "", "", now)
	})
	if out, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "open_pr"}); err != nil || out != w.pr.openURL {
		t.Errorf("open: %q %v", out, err)
	}
	if len(w.pr.opens) != 1 {
		t.Errorf("opens %v", w.pr.opens)
	}
	if _, err := w.svc.Answer(ctx, ops.AnswerIn{ID: "T0001", Value: "send_back"}); !errors.Is(err, ops.ErrNoteNeeded) {
		t.Errorf("send back without a note: %v", err)
	}
}
