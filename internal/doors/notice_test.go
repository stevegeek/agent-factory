package doors

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func newIn(project, title, column string) ticket.NewIn {
	return ticket.NewIn{Project: project, Title: title, Body: "b", Door: "human", Column: column}
}

// port: TF UpkeepTest.test_newer_release_files_one_notice_and_tick_ignores_it
// (the notice half: one open ticket per title; the version check that files
// it is upkeep's test, and the tick ignoring it is pipeline's)
func TestNoticeFilesOneOpenTicketPerTitle(t *testing.T) {
	f := newFixture(t)
	tk, err := f.s.Notice(t.Context(), "Update Claude Code 2.1.296 -> 2.1.300", "body")
	if err != nil || tk == nil {
		t.Fatalf("notice %v %v", tk, err)
	}
	if tk.Project != "factory" || tk.Column != "needs_human" || tk.Door != "clock" || tk.By != "factory" || tk.Body != "body" {
		t.Fatalf("notice %+v", tk)
	}
	again, err := f.s.Notice(t.Context(), "Update Claude Code 2.1.296 -> 2.1.300", "other")
	if err != nil || again != nil {
		t.Fatalf("a second notice with an open one: %v %v", again, err)
	}
	ts := f.tickets(t)
	if len(ts) != 1 || ts[0].History[len(ts[0].History)-1].Event != "created" {
		t.Fatalf("tickets %+v", ts)
	}
}

func TestNoticeAgainOnceTheOldOneIsClosed(t *testing.T) {
	for _, col := range []string{"done", "rejected"} {
		t.Run(col, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.s.Board.FileTicket(t.Context(), newIn("p", "Same title", col)); err != nil {
				t.Fatal(err)
			}
			tk, err := f.s.Notice(t.Context(), "Same title", "b")
			if err != nil || tk == nil {
				t.Fatalf("closed ticket blocked the notice: %v %v", tk, err)
			}
		})
	}
	t.Run("any project, any open column", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.s.Board.FileTicket(t.Context(), newIn("p", "Same title", "triage")); err != nil {
			t.Fatal(err)
		}
		if tk, err := f.s.Notice(t.Context(), "Same title", "b"); err != nil || tk != nil {
			t.Fatalf("an open ticket of another project did not stop it: %v %v", tk, err)
		}
	})
}

// A held deploy is one Deploy? ticket: a second ask updates its rev and
// adds its reason instead of filing another.
func TestDeployAskFilesOnce(t *testing.T) {
	f := newFixture(t)
	tk, err := f.s.DeployAsk(t.Context(), "0123456789ab", "merged #12")
	if err != nil || tk == nil {
		t.Fatalf("ask %v %v", tk, err)
	}
	if tk.Project != "factory" || tk.Column != "needs_human" || tk.Door != "clock" || tk.Title != "Deploy the factory?" {
		t.Fatalf("ticket %+v", tk)
	}
	last := tk.History[len(tk.History)-1]
	if m, _ := last.Data.(ticket.Moved); m.Reason != model.ReasonDeployHeld {
		t.Errorf("last %+v", last)
	}
	if a := tk.Ask.V; a.Kind != string(model.AskDeploy) || a.Question != "Deploy main (now 0123456789ab) to the factory?" {
		t.Errorf("ask %+v", a)
	}
	again, err := f.s.DeployAsk(t.Context(), "fedcba987654", "merged #13")
	if err != nil || again == nil || again.ID != tk.ID {
		t.Fatalf("second ask %v %v", again, err)
	}
	ts := f.tickets(t)
	if len(ts) != 1 {
		t.Fatalf("%d tickets", len(ts))
	}
	d, _ := ts[0].Deploy.Get()
	if d.Rev != "fedcba987654" || strings.Join(d.Why, "|") != "merged #12|merged #13" {
		t.Errorf("deploy %+v", d)
	}
	if a := ts[0].Ask.V; a.Question != "Deploy main (now fedcba987654) to the factory?" {
		t.Errorf("ask %+v", a)
	}
	if e := ts[0].History[len(ts[0].History)-1]; !strings.HasSuffix(e.Detail, "now fedcba987654") {
		t.Errorf("history %+v", e)
	}

	// once closed, the next ask files a new one
	if err := f.s.Board.Changing(t.Context(), string(tk.ID), store.NoWait, func(x *ticket.Ticket) error {
		ticket.Move(x, "rejected", model.ReasonByHuman, "", "", "", f.clock.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	third, err := f.s.DeployAsk(t.Context(), "fedcba987654", "")
	if err != nil || third == nil || third.ID == tk.ID {
		t.Fatalf("after closing: %v %v", third, err)
	}
	if d, _ := third.Deploy.Get(); len(d.Why) != 0 || d.Why == nil {
		t.Errorf("an ask without a reason: %+v", d)
	}
}

// A long hold keeps the last MaxDeployWhy reasons, not every merge's.
func TestDeployAskKeepsTheLastReasons(t *testing.T) {
	f := newFixture(t)
	for i := 1; i <= MaxDeployWhy+5; i++ {
		if _, err := f.s.DeployAsk(t.Context(), "0123456789ab", "merged #"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	ts := f.tickets(t)
	if len(ts) != 1 {
		t.Fatalf("%d tickets", len(ts))
	}
	d, _ := ts[0].Deploy.Get()
	if len(d.Why) != MaxDeployWhy || d.Why[0] != "merged #6" || d.Why[len(d.Why)-1] != "merged #"+strconv.Itoa(MaxDeployWhy+5) {
		t.Errorf("why %q", d.Why)
	}
}

// A board that does not read in full files no ask: the open one may be
// the ticket it skipped, and a second ask breaks the one-ticket rule.
func TestDeployAskRefusesAnUnreadBoard(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.p.Tickets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.p.Tickets, "T9999.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tk, err := f.s.DeployAsk(t.Context(), "0123456789ab", "merged #12"); err == nil || tk != nil {
		t.Errorf("ask %v %v", tk, err)
	}
	ts, _ := f.s.Board.Reader.List()
	if len(ts) != 0 {
		t.Errorf("filed %d tickets", len(ts))
	}
}
