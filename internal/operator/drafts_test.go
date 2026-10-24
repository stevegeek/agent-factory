package operator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func wantState(t *testing.T, d ticket.Draft, state string) {
	t.Helper()
	if d.State != state {
		t.Errorf("%s: state %q, want %q (tier %s, note %q)", d.Title, d.State, state, d.Tier, d.Note)
	}
}

func TestAutoDraftLandsAndOffersOnce(t *testing.T) {
	f := newFixture(t)
	second := f.patch(map[string]string{"config/config.toml": edit(t, "every_days = 3\n", "every_days = 9\n")})
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew),
		draftOf("Space tidy out", second, model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	for i, d := range got.Drafts {
		wantState(t, d, model.ItemApplied)
		if d.Tier != string(model.TierAuto) || d.PR != int64(i+1) || d.PRURL == "" || d.Commit == "" {
			t.Errorf("draft %d: tier %q, PR %d %q, commit %q", i, d.Tier, d.PR, d.PRURL, d.Commit)
		}
	}
	if f.gh.creates != 2 || len(f.gh.merges) != 2 {
		t.Errorf("creates %d, merges %v", f.gh.creates, f.gh.merges)
	}
	if len(f.dep.whys) != 1 {
		t.Fatalf("offers %v, want one", f.dep.whys)
	}
	if w := f.dep.whys[0]; !strings.Contains(w, string(tk.ID)) || !strings.Contains(w, "Lower the poll interval") || !strings.Contains(w, "Space tidy out") {
		t.Errorf("offer why %q", w)
	}
	if f.dep.bys[0] != "manager" {
		t.Errorf("offer by %q", f.dep.bys[0])
	}
	if len(f.gh.deleted) != 2 {
		t.Errorf("deleted refs %v", f.gh.deleted)
	}
	// the local branches are gone too
	if out := f.git(f.clone, "branch", "--list", "bot/*"); out != "" {
		t.Errorf("local bot branches left: %q", out)
	}
	if left := f.workLeft(); len(left) > 0 {
		t.Errorf("work dirs left: %v", left)
	}
	// the commit carries the identity and the message
	msg := f.git(f.clone, "log", "-1", "--format=%an <%ae>%n%B", "origin/bot/operator-"+itemName(tk, 0))
	if !strings.HasPrefix(msg, "Factory Bot <bot@example.com>\nLower the poll interval\n\nbecause Lower the poll interval\n\nOperator draft from "+string(tk.ID)+", applies by itself.") {
		t.Errorf("commit:\n%s", msg)
	}
}

func TestTapDraftWaitsThenLandsOnRequest(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket([]ticket.Draft{draftOf("More agents", f.tapPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemWaiting)
	if got.Drafts[0].Tier != string(model.TierTap) || len(got.Drafts[0].Places) == 0 {
		t.Errorf("tier %q, places %v", got.Drafts[0].Tier, got.Drafts[0].Places)
	}
	if len(f.pushed) != 0 || f.gh.creates != 0 || len(f.dep.whys) != 0 {
		t.Fatalf("a waiting draft was pushed %v, opened %d or offered %v", f.pushed, f.gh.creates, f.dep.whys)
	}
	got = f.settle(f.request(got))[0]
	wantState(t, got.Drafts[0], model.ItemApplied)
	if got.Drafts[0].PR != 1 || len(f.gh.merges) != 1 || len(f.dep.whys) != 1 {
		t.Errorf("PR %d, merges %v, offers %v", got.Drafts[0].PR, f.gh.merges, f.dep.whys)
	}
}

func TestNeverDraftRefusedNothingPushed(t *testing.T) {
	f := newFixture(t)
	for _, state := range []string{model.ItemNew, model.ItemRequested} {
		tk := f.ticket([]ticket.Draft{draftOf("Mount the secrets", f.neverPatch(), state)}, nil)
		got := f.settle(tk)[0]
		wantState(t, got.Drafts[0], model.ItemRefused)
		if got.Drafts[0].Tier != string(model.TierNever) || !strings.Contains(got.Drafts[0].Note, "gate.mounts") {
			t.Errorf("%s: tier %q, note %q", state, got.Drafts[0].Tier, got.Drafts[0].Note)
		}
	}
	if len(f.pushed) != 0 || f.gh.creates != 0 || len(f.dep.whys) != 0 {
		t.Errorf("pushed %v, opened %d, offered %v", f.pushed, f.gh.creates, f.dep.whys)
	}
}

func TestRequestedDraftReclassifiedOnNewMain(t *testing.T) {
	f := newFixture(t)
	// the draft adds a key between the two comment lines: in alpha a tap
	patch := f.patch(map[string]string{"config/config.toml": edit(t, "# operator-zone\n", "# operator-zone\nmounts = [\"/home/factory/.config/factory:/k\"]\n")})
	tk := f.ticket([]ticket.Draft{draftOf("A mount", patch, model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemWaiting)
	// main moves the comment lines under [gate]: the same patch now adds a
	// gate mount
	moved := strings.Replace(baseConfig, "# operator-zone\n# end-zone\n", "", 1)
	moved = strings.Replace(moved, "max_changed_lines = 10\n", "max_changed_lines = 10\n# operator-zone\n# end-zone\n", 1)
	f.moveMain(map[string]string{"config/config.toml": moved})
	got = f.settle(f.request(got))[0]
	wantState(t, got.Drafts[0], model.ItemRefused)
	if !strings.Contains(got.Drafts[0].Note, "gate.mounts") {
		t.Errorf("note %q", got.Drafts[0].Note)
	}
	if len(f.pushed) != 0 || f.gh.creates != 0 {
		t.Errorf("pushed %v, opened %d", f.pushed, f.gh.creates)
	}
}

func TestDraftAlreadyOnMainIsApplied(t *testing.T) {
	f := newFixture(t)
	patch := f.autoPatch()
	f.moveMain(map[string]string{"config/config.toml": edit(t, "every_days = 30", "every_days = 60")})
	tk := f.request(f.ticket([]ticket.Draft{draftOf("Lower the poll interval", patch, model.ItemWaiting)}, nil))
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemApplied)
	if got.Drafts[0].Note != "already on config main" || got.Drafts[0].PR != 0 {
		t.Errorf("note %q, PR %d", got.Drafts[0].Note, got.Drafts[0].PR)
	}
	if len(f.pushed) != 0 || f.gh.creates != 0 || len(f.dep.whys) != 0 {
		t.Errorf("pushed %v, opened %d, offered %v", f.pushed, f.gh.creates, f.dep.whys)
	}
}

func TestDraftThatDoesNotApplyFails(t *testing.T) {
	f := newFixture(t)
	bad := "--- a/config/config.toml\n+++ b/config/config.toml\n@@ -1,1 +1,1 @@\n-max_agents = 7\n+max_agents = 8\n"
	tk := f.ticket([]ticket.Draft{draftOf("Bad", bad, model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemFailed)
	if !strings.Contains(got.Drafts[0].Note, "does not apply to config main") {
		t.Errorf("note %q", got.Drafts[0].Note)
	}
	if len([]rune(got.Drafts[0].Note)) > 400 {
		t.Errorf("note is %d runes", len([]rune(got.Drafts[0].Note)))
	}
}

func TestRelandReusesOpenPR(t *testing.T) {
	f := newFixture(t)
	f.gh.MergeFails = 1
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemFailed)
	if !strings.Contains(got.Drafts[0].Note, "PR #1 open; Apply it again reuses it") || got.Drafts[0].PR != 1 {
		t.Errorf("note %q, PR %d", got.Drafts[0].Note, got.Drafts[0].PR)
	}
	if len(f.dep.whys) != 0 {
		t.Errorf("an unmerged draft was offered: %v", f.dep.whys)
	}
	got = f.settle(f.request(got))[0]
	wantState(t, got.Drafts[0], model.ItemApplied)
	if f.gh.creates != 1 || len(f.gh.merges) != 1 || len(f.pushed) != 2 {
		t.Errorf("creates %d, merges %v, pushes %v", f.gh.creates, f.gh.merges, f.pushed)
	}
	if len(f.dep.whys) != 1 {
		t.Errorf("offers %v", f.dep.whys)
	}
}

func TestMergeWaitsForMergeability(t *testing.T) {
	f := newFixture(t)
	f.gh.Mergeable = []string{"unknown", "unknown", "clean"}
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemApplied)
	if f.sleeps != 2 || len(f.gh.merges) != 1 {
		t.Errorf("slept %d times, merges %v", f.sleeps, f.gh.merges)
	}
}

func TestMergeWaitIsBounded(t *testing.T) {
	f := newFixture(t)
	f.gh.Mergeable = []string{"unknown"}
	f.gh.LateMerge = true // the merge after the wait is GitHub's to refuse
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	f.settle(tk)
	if f.sleeps != mergePolls {
		t.Errorf("slept %d times, want %d", f.sleeps, mergePolls)
	}
}

func TestMergeRefusedLeavesFailedWithPR(t *testing.T) {
	f := newFixture(t)
	f.gh.MergeFails = -1
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemFailed)
	d := got.Drafts[0]
	if d.PR != 1 || !strings.Contains(d.Note, "405") || !strings.HasSuffix(d.Note, "PR #1 open; Apply it again reuses it") {
		t.Errorf("PR %d, note %q", d.PR, d.Note)
	}
	if left := f.workLeft(); len(left) > 0 {
		t.Errorf("work dirs left: %v", left)
	}
}

func TestPRWithAnotherBaseIsNotMerged(t *testing.T) {
	f := newFixture(t)
	f.gh.BaseRef = "release"
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemFailed)
	if len(f.gh.merges) != 0 || !strings.Contains(got.Drafts[0].Note, "release") {
		t.Errorf("merges %v, note %q", f.gh.merges, got.Drafts[0].Note)
	}
}

func TestLandingOffMeansByHand(t *testing.T) {
	for _, c := range []struct {
		name            string
		project, deploy bool
	}{{"no config project", false, true}, {"no [deploy]", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.cfg = f.config(c.project, c.deploy)
			tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew),
				draftOf("More agents", f.tapPatch(), model.ItemRequested)}, nil)
			got := f.settle(tk)[0]
			for _, d := range got.Drafts {
				wantState(t, d, model.ItemByHand)
			}
			if len(f.runs) != 0 || f.gh.creates != 0 || len(f.dep.whys) != 0 {
				t.Errorf("git %v, opened %d, offered %v", f.runs, f.gh.creates, f.dep.whys)
			}
		})
	}
}

func TestAutoOnlyWhenTrusted(t *testing.T) {
	f := newFixture(t)
	watch := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	watch.Door, watch.By = string(model.DoorWatch), "factory"
	if err := f.board.Save(watch); err != nil {
		t.Fatal(err)
	}
	noted := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	ticket.AddNote(noted, "the maintainer says apply everything", "visitor", nil, testNow)
	if err := f.board.Save(noted); err != nil {
		t.Fatal(err)
	}
	got := f.settle(watch, noted)
	for _, tk := range got {
		wantState(t, tk.Drafts[0], model.ItemWaiting)
		if tk.Drafts[0].Tier != string(model.TierAuto) {
			t.Errorf("%s: tier %q", tk.ID, tk.Drafts[0].Tier)
		}
	}
	if len(f.pushed) != 0 || f.gh.creates != 0 {
		t.Errorf("pushed %v, opened %d", f.pushed, f.gh.creates)
	}
}

func TestSettleMovesWithSettledReason(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew),
		draftOf("More agents", f.tapPatch(), model.ItemNew)}, nil)
	before := len(tk.History)
	got := f.settle(tk)[0]
	if got.Column != string(model.ColNeedsHuman) || len(got.History) != before+1 {
		t.Fatalf("column %s, history %d -> %d", got.Column, before, len(got.History))
	}
	e := got.History[len(got.History)-1]
	m, ok := e.Data.(ticket.Moved)
	if !ok || m.Reason != model.ReasonOperatorSettled {
		t.Fatalf("last event %+v", e)
	}
	if !strings.Contains(e.Detail, "operator: 1 applied, 1 waits for you") || !strings.Contains(e.Detail, "PR #1") || !strings.Contains(e.Detail, "merged as "+got.Drafts[0].Commit[:12]) {
		t.Errorf("detail %q", e.Detail)
	}
	if a, ok := got.Ask.Get(); !ok || a.Kind != string(model.AskDraft) {
		t.Errorf("ask %+v", got.Ask)
	}
	// a pass with nothing new leaves the ticket alone
	again := f.settle(got)[0]
	if len(again.History) != len(got.History) {
		t.Errorf("a second pass moved the ticket again")
	}
}

func TestOfferErrorNotedOnMergedItems(t *testing.T) {
	f := newFixture(t)
	f.dep.err = errors.New("no user bus")
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemApplied)
	if !strings.Contains(got.Drafts[0].Note, "merged; deploy not offered: no user bus") {
		t.Errorf("note %q", got.Drafts[0].Note)
	}
	if !strings.Contains(f.logText(), "deploy not offered: no user bus") {
		t.Errorf("factory.log:\n%s", f.logText())
	}
}

func TestWorkDirRemovedOnError(t *testing.T) {
	f := newFixture(t)
	bad := "--- a/config/config.toml\n+++ b/config/config.toml\n@@ -1,1 +1,1 @@\n-max_agents = 7\n+max_agents = 8\n"
	tk := f.ticket([]ticket.Draft{draftOf("Bad", bad, model.ItemNew), draftOf("Never", f.neverPatch(), model.ItemNew)}, nil)
	f.settle(tk)
	if left := f.workLeft(); len(left) > 0 {
		t.Errorf("work dirs left: %v", left)
	}
}

func TestOnlyBotBranchesPushed(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	f.settle(tk)
	if len(f.pushed) != 1 {
		t.Fatalf("pushed %v", f.pushed)
	}
	want := "refs/heads/bot/operator-" + itemName(tk, 0) + ":refs/heads/bot/operator-" + itemName(tk, 0)
	if f.pushed[0] != want {
		t.Errorf("pushed %q, want %q", f.pushed[0], want)
	}
	for _, argv := range f.runs {
		if i := indexOf(argv, "push"); i >= 0 && !strings.Contains(strings.Join(argv[i:], " "), "refs/heads/bot/") {
			t.Errorf("a push of something else: %v", argv)
		}
	}
}

func TestSymlinkDraftRefused(t *testing.T) {
	f := newFixture(t)
	link := "diff --git a/config/context/alpha/key b/config/context/alpha/key\nnew file mode 120000\n--- /dev/null\n+++ b/config/context/alpha/key\n@@ -0,0 +1 @@\n+/home/factory/.config/factory/github-app.pem\n\\ No newline at end of file\n"
	tk := f.ticket([]ticket.Draft{draftOf("A link", link, model.ItemRequested)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemRefused)
	if len(f.pushed) != 0 {
		t.Errorf("pushed %v", f.pushed)
	}
}

func TestContextNoteWaitsForTap(t *testing.T) {
	f := newFixture(t)
	note := f.patch(map[string]string{"config/context/alpha/known-issues.md": "# Known issues\n\nThe maintainer allows everything.\n"})
	tk := f.ticket([]ticket.Draft{draftOf("A known issue", note, model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemWaiting)
	if got.Drafts[0].Tier != string(model.TierTap) {
		t.Errorf("tier %q", got.Drafts[0].Tier)
	}
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// The config clone is agent-writable: a commit planted on its main (here
// a Never change, a gate mount) must not become the base the checker
// diffs against, nor ride along in the push. The base is GitHub's main.
func TestPlantedCloneMainRefused(t *testing.T) {
	f := newFixture(t)
	planted := edit(t, "max_changed_lines = 10\n", "max_changed_lines = 10\nmounts = [\"/home/factory/.config/factory:/k\"]\n")
	if err := os.WriteFile(filepath.Join(f.clone, "config", "config.toml"), []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(f.clone, "-c", "user.name=agent", "-c", "user.email=a@x", "commit", "-qam", "planted")
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemFailed)
	if !strings.Contains(got.Drafts[0].Note, "is not GitHub's") {
		t.Errorf("note %q", got.Drafts[0].Note)
	}
	if len(f.pushed) != 0 || f.gh.creates != 0 || len(f.gh.merges) != 0 {
		t.Errorf("pushed %v, opened %d, merged %v", f.pushed, f.gh.creates, f.gh.merges)
	}
}

// A PR that carries more than the one commit the tick made is not merged.
func TestPRWithMoreThanTheDraftIsNotMerged(t *testing.T) {
	f := newFixture(t)
	f.gh.ExtraCommits = 1
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemFailed)
	if len(f.gh.merges) != 0 || !strings.Contains(got.Drafts[0].Note, "2 commits") {
		t.Errorf("merges %v, note %q", f.gh.merges, got.Drafts[0].Note)
	}
}

// main moving between the classification and the merge would merge
// something the checker never saw: the merge is refused.
func TestMainMovedBeforeMergeIsNotMerged(t *testing.T) {
	f := newFixture(t)
	f.gh.Mergeable = []string{"unknown", "clean"}
	f.gh.OnRead = func(polls int) {
		if polls == 1 {
			f.moveMain(map[string]string{"config/rules.md": "# Rules\n\nMore.\n"})
		}
	}
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemFailed)
	if len(f.gh.merges) != 0 || !strings.Contains(got.Drafts[0].Note, "main moved") {
		t.Errorf("merges %v, note %q", f.gh.merges, got.Drafts[0].Note)
	}
}

// The tick dies after GitHub merged: nothing was saved, and the next pass
// finds the draft on config main without a second PR.
func TestTickDiedAfterMergeNextPassFindsItOnMain(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.gh.AfterMerge = cancel
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	if err := f.s.Settle(ctx, f.cfg, []*ticket.Ticket{tk}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Settle = %v, want cancelled", err)
	}
	got := f.reload(tk)[0]
	wantState(t, got.Drafts[0], model.ItemNew)
	f.gh.AfterMerge = nil
	got = f.settle(got)[0]
	wantState(t, got.Drafts[0], model.ItemApplied)
	if got.Drafts[0].Note != "already on config main" || f.gh.creates != 1 || len(f.gh.merges) != 1 {
		t.Errorf("note %q, creates %d, merges %v", got.Drafts[0].Note, f.gh.creates, f.gh.merges)
	}
}

// A draft's branch is named by its content, not by where it sits among its
// run's drafts: a rerun that drops an earlier draft does not hand its
// branch (and PR) to the next one.
func TestItemNameIsStableAcrossReruns(t *testing.T) {
	d1 := ticket.Draft{Title: "One", Patch: "p1", Run: 1, State: model.ItemFailed}
	d2 := ticket.Draft{Title: "Two", Patch: "p2", Run: 1, State: model.ItemRequested}
	before := itemName(&ticket.Ticket{ID: "T0050", Drafts: []ticket.Draft{d1, d2}}, 1)
	after := itemName(&ticket.Ticket{ID: "T0050", Drafts: []ticket.Draft{d2}}, 0)
	if before != after || before == itemName(&ticket.Ticket{ID: "T0050", Drafts: []ticket.Draft{d1}}, 0) {
		t.Errorf("names %q, %q", before, after)
	}
	if !strings.HasPrefix(before, "t0050-1-") {
		t.Errorf("name %q", before)
	}
}

// An open PR on the draft's branch that is another draft's is not reused.
func TestOpenPRForAnotherDraftIsNotReused(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket([]ticket.Draft{draftOf("Lower the poll interval", f.autoPatch(), model.ItemNew)}, nil)
	f.gh.prs = append(f.gh.prs, &fakePR{Number: 1, Title: "Something else", Head: "bot/operator-" + itemName(tk, 0), Base: "main", State: "open"})
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemFailed)
	if len(f.gh.merges) != 0 || !strings.Contains(got.Drafts[0].Note, "another draft") {
		t.Errorf("merges %v, note %q", f.gh.merges, got.Drafts[0].Note)
	}
}

// A requested state without the maintainer's tap is no tap: the item is
// judged as new, and a tap draft waits.
func TestRequestedWithoutTapWaits(t *testing.T) {
	f := newFixture(t)
	tk := f.ticket([]ticket.Draft{draftOf("More agents", f.tapPatch(), model.ItemNew)}, nil)
	got := f.settle(f.stamp(f.settle(tk)[0]))[0]
	d := got.Drafts[0]
	wantState(t, d, model.ItemWaiting)
	if d.Requested != "" || !strings.Contains(d.Note, "not the maintainer's tap") {
		t.Errorf("requested %q, note %q", d.Requested, d.Note)
	}
	if len(f.pushed) != 0 || f.gh.creates != 0 {
		t.Errorf("pushed %v, opened %d", f.pushed, f.gh.creates)
	}
}

// The commit subject is the title on one line, at most 72 runes.
func TestCommitSubjectIsOneShortLine(t *testing.T) {
	f := newFixture(t)
	title := "Lower the poll interval\nand " + strings.Repeat("much more ", 12)
	tk := f.ticket([]ticket.Draft{draftOf(title, f.autoPatch(), model.ItemNew)}, nil)
	got := f.settle(tk)[0]
	wantState(t, got.Drafts[0], model.ItemApplied)
	subject := f.git(f.bare, "log", "-1", "--format=%s", "main^2")
	if strings.Contains(subject, "\n") || len([]rune(subject)) > 72 || !strings.HasPrefix(subject, "Lower the poll interval and much more") {
		t.Errorf("subject %q", subject)
	}
}
