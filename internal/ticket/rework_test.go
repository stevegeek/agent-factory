package ticket

import (
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
)

func TestReworkWhyIsTheLastSendBackSinceTheWorkWasDone(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	tk := &Ticket{}
	if got := ReworkWhy(tk); got != "" {
		t.Errorf("first run: %q", got)
	}
	Move(tk, string(model.ColReady), model.ReasonConflicts, "", "", "", at)
	Move(tk, string(model.ColReview), model.ReasonWorkDone, "", "", "", at)
	if got := ReworkWhy(tk); got != "" {
		t.Errorf("after work done: %q", got)
	}
	Move(tk, string(model.ColReady), model.ReasonMainMoved, "", "", "", at)
	if got := ReworkWhy(tk); got != "because the checks failed on the latest base branch" {
		t.Errorf("main moved: %q", got)
	}
}

func TestUpdateLines(t *testing.T) {
	got := UpdateLines([]Update{
		{At: "2026-10-17T09:05:00+00:00", Why: "after feedback", Text: "Renamed\nthe flag."},
		{At: "not a time", Text: "Fixed a typo."},
	})
	want := []string{"- 2026-10-17 09:05 UTC, after feedback: Renamed the flag.", "- not a time: Fixed a typo."}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("%q", got)
	}
}

func TestKeepSummaryOnlyFillsAMissingOrBlankSummary(t *testing.T) {
	tk := &Ticket{}
	parse := func(s string) jsonx.Value {
		v, err := jsonx.Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := KeepSummary(tk, parse(`{"status": "done"}`)); func() bool { o, _ := got.Object(); _, has := o.Lookup("summary"); return has }() {
		t.Error("no stored report, a summary appeared")
	}
	tk.Reports.Set("work", parse(`{"summary": "Old."}`))
	for _, c := range []struct{ in, want string }{
		{`{"status": "done"}`, "Old."}, {`{"summary": "  "}`, "Old."}, {`{"summary": "New."}`, "New."},
	} {
		if s, _ := KeepSummary(tk, parse(c.in)).Get("summary").Str(); s != c.want {
			t.Errorf("%s: %q", c.in, s)
		}
	}
}

// A send-back from public review records no update: what it removed must not
// be described anywhere that is published. An update's text is clipped.
func TestPublicReworkRecordsNoUpdateAndUpdatesAreClipped(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	tk := &Ticket{}
	Move(tk, string(model.ColReady), model.ReasonConflicts, "", "", "", at)
	Move(tk, string(model.ColReady), model.ReasonPublicChanges, "", "", "", at)
	if got := ReworkWhy(tk); got != "" {
		t.Errorf("public send-back: %q", got)
	}
	AddUpdate(tk, "after feedback", strings.Repeat("x", 2*maxUpdate), at)
	if n := len(tk.Updates[0].Text); n > maxUpdate {
		t.Errorf("an update of %d bytes", n)
	}
}

// A ticket that leaves "PR open" leaves what its merge waited on behind,
// so a rework back to the same PR does not show the old wait.
func TestLeavingPROpenClearsTheMergeWait(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	tk := &Ticket{Column: string(model.ColPROpen)}
	tk.PR = Some(PR{Number: 5, MergeWait: Some("main moved"), Queue: Some(PRQueue{State: "room"})})
	Move(tk, string(model.ColPROpen), model.ReasonPROpened, "", "", "", at)
	if !tk.PR.V.MergeWait.Present() {
		t.Error("a move within PR open cleared the wait")
	}
	Move(tk, string(model.ColReady), model.ReasonConflicts, "", "", "", at)
	if tk.PR.V.MergeWait.Present() || tk.PR.V.Queue.Present() {
		t.Errorf("after leaving PR open: %+v", tk.PR.V)
	}
}
