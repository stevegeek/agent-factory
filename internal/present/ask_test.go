package present

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestDeskOrderAndAsks(t *testing.T) {
	ts := fixtures(t)
	var got []string
	for _, it := range Desk(ts, you) {
		got = append(got, string(it.T.ID)+":"+it.Ask.Kind)
	}
	want := []string{"T0009:question", "T0012:unstick", "T0007:unstick", "T0001:review", "T0002:start",
		"T0004:design", "T0006:proposal", "T0003:decide", "T0008:notice"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desk order\n got %v\nwant %v", got, want)
	}

	cases := []struct {
		id, kind, question, why, retry string
	}{
		{"T0001", "review", "Review and merge PR #23?", "Changed the widget finder to use the primary key.", ""},
		{"T0002", "start", "Start work on issue #18, “Cache the widget coder”?", "It is slow; caching helps.", ""},
		{"T0003", "decide", "Triage could not decide on issue #15, “Check thread safety”. Start it, or drop it?", "No failing case was reported.", ""},
		{"T0004", "design", "Approve the design for “Make widgets thread safe” and its 3 tickets?", "Freeze state after construction.", ""},
		{"T0006", "proposal", "File this as a change: Proposal 1?", "why 1", ""},
		{"T0007", "unstick", "Tests or lint failed at the gate. Run the gate again?", "", "gate"},
		{"T0008", "notice", "Read the answer to “What is the latest widget release?” and close it?", "1.2.3, released last week.", ""},
		{"T0009", "question", "Keep the old option name as an alias?", "Removing it breaks callers.", ""},
		{"T0012", "unstick", "An agent ran out of time. Try again?", "", "ready"},
	}
	for _, c := range cases {
		a := AskFor(find(ts, c.id))
		if a == nil {
			t.Fatalf("%s: no ask", c.id)
		}
		if a.Kind != c.kind || a.Question != c.question || a.RetryColumn.Or("") != c.retry {
			t.Errorf("%s: got %q %q retry %q", c.id, a.Kind, a.Question, a.RetryColumn.Or(""))
		}
		if c.why != "" && a.Why != c.why {
			t.Errorf("%s: why %q", c.id, a.Why)
		}
	}
	if !strings.Contains(AskFor(find(ts, "T0007")).Why, "Gate: tests or lint failed") {
		t.Errorf("unstick why should carry the move detail: %q", AskFor(find(ts, "T0007")).Why)
	}
	if AskFor(find(ts, "T0007")).Since != "2026-10-08T09:35:00+00:00" {
		t.Errorf("since is the arrival move")
	}
	for _, id := range []string{"T0005", "T0010", "T0011"} {
		if AskFor(find(ts, id)) != nil {
			t.Errorf("%s should have no ask", id)
		}
	}
}

// Only a stored ask asks, and only in the ask columns: a ticket with none
// is what `factory check --asks` lists (D11).
func TestOnlyAStoredAskAsks(t *testing.T) {
	x := tk(t, `{"id": "T0043", "project": "p", "title": "T", "door": "human", "column": "needs_human",
		"ask": {"kind": "start", "question": "Go?", "why": "", "since": "2026-10-16T10:00:00+00:00", "retry_column": null},
		"history": []}`)
	if a := AskFor(x); a == nil || a.Kind != "start" || a.Question != "Go?" {
		t.Errorf("stored ask: %+v", a)
	}
	x.Column = "ready"
	if AskFor(x) != nil {
		t.Errorf("a ticket in ready asks nothing, whatever it stores")
	}
	x.Column = "needs_human"
	x.Ask = ticket.Opt[ticket.Ask]{Set: true, V: ticket.Ask{Kind: ""}}
	if a := AskFor(x); a != nil {
		t.Errorf("an ask with no kind: %+v", a)
	}
	x.Ask = ticket.Opt[ticket.Ask]{}
	if a := AskFor(x); a != nil {
		t.Errorf("no ask: %+v", a)
	}
}

func TestARespondedAskWaitsLowerDownUntilTheFactoryMoves(t *testing.T) {
	ts := fixtures(t)
	all := Desk(ts, you)
	if len(all) < 2 {
		t.Fatalf("fixtures need two asks, have %d", len(all))
	}
	first := all[0].T
	since := ParseTime(all[0].Ask.Since)
	at := func(d time.Duration) string { return since.Add(d).UTC().Format(time.RFC3339) }
	now := since.Add(2 * time.Hour)

	first.History = append(first.History, note(at(time.Hour), "not sure about this"))
	items := Desk(ts, you)
	if last := items[len(items)-1]; last.T != first || last.Responded != "note" {
		t.Errorf("a noted ask should sit last: %s %q", last.T.ID, last.Responded)
	}
	if Waiting(items) != len(items)-1 {
		t.Errorf("waiting %d of %d", Waiting(items), len(items))
	}
	v := DeskPage(items, "", "", now)
	if len(v.Done) != 1 || v.Done[0].ID != string(first.ID) || v.Done[0].Responded != "You left a note 1 h ago · still yours to decide" {
		t.Errorf("done: %+v", v.Done)
	}
	for _, q := range v.Items {
		if q.ID == string(first.ID) {
			t.Errorf("%s is under Needs you and You've responded", q.ID)
		}
	}

	first.History = append(first.History, ticket.Entry{At: at(90 * time.Minute), Event: "asked", Detail: "T0099: why?"})
	if it := Desk(ts, you)[len(items)-1]; it.Responded != "asked" {
		t.Errorf("a question should keep it down: %q", it.Responded)
	}
	// anything new from the factory brings it back up
	first.History = append(first.History, ticket.Entry{At: at(100 * time.Minute), Event: "review requested", Detail: "a-reviewer"})
	if Desk(ts, you)[0].T != first || Waiting(Desk(ts, you)) != len(items) {
		t.Errorf("the factory moved on, so it needs you again")
	}
	// a note from before the ask arrived is old news
	first.History = append(first.History, note(since.Add(-time.Hour).UTC().Format(time.RFC3339), "old"))
	if r, _ := Responded(first, all[0].Ask, you, ByID(ts)); r != "" {
		t.Errorf("an older note responds to nothing: %q", r)
	}
	// a note the factory wrote (typed, by factory) is not yours
	first.History = append(first.History, ticket.Entry{At: at(110 * time.Minute), Event: "note", Detail: "rebase please",
		Type: model.EventNoted, Data: ticket.Noted{By: "factory", Note: 0}})
	if r, _ := Responded(first, all[0].Ask, you, ByID(ts)); r != "" {
		t.Errorf("the factory's own note responds to nothing: %q", r)
	}
}

// D19: a response is its own person's. Another user's note leaves the ask
// waiting on the maintainer's desk, and moves it down only on theirs; a
// question is its asker's, as its task records.
func TestARespondedAskIsOnlyItsOwnPersons(t *testing.T) {
	ts := fixtures(t)
	first := Desk(ts, you)[0]
	at := ParseTime(first.Ask.Since).Add(time.Hour).UTC().Format(time.RFC3339)
	robin := Viewer{By: "robin"}
	noted := func(by string) {
		first.T.History = append(first.T.History, ticket.Entry{At: at, Event: "note", Detail: "a note",
			Type: model.EventNoted, Data: ticket.Noted{By: by}})
	}
	noted("robin")
	if r, _ := Responded(first.T, first.Ask, you, ByID(ts)); r != "" {
		t.Errorf("robin's note is not the maintainer's response: %q", r)
	}
	if r, _ := Responded(first.T, first.Ask, robin, ByID(ts)); r != "note" {
		t.Errorf("robin's own note: %q", r)
	}
	if Waiting(Desk(ts, you)) != Waiting(Desk(ts, robin))+1 {
		t.Errorf("the ask still waits for the maintainer only")
	}
	noted("maintainer")
	if r, _ := Responded(first.T, first.Ask, you, ByID(ts)); r != "note" {
		t.Errorf("the maintainer's own note: %q", r)
	}
	if r, _ := Responded(first.T, first.Ask, robin, ByID(ts)); r != "" {
		t.Errorf("the maintainer's note is not robin's response: %q", r)
	}
	// an untyped note names nobody: the maintainer's, from before D19
	first.T.History = append(first.T.History, note(at, "old style"))
	if r, _ := Responded(first.T, first.Ask, robin, ByID(ts)); r != "" {
		t.Errorf("an unnamed note is not robin's: %q", r)
	}
	child := tk(t, `{"id": "T0998", "kind": "task", "by": "robin", "parent": "`+string(first.T.ID)+`", "column": "ready", "project": "`+first.T.Project+`"}`)
	ts = append(ts, child)
	first.T.History = append(first.T.History, ticket.Entry{At: at, Event: "asked", Detail: "T0998: why?"})
	if r, _ := Responded(first.T, first.Ask, robin, ByID(ts)); r != "asked" {
		t.Errorf("robin's own question: %q", r)
	}
	if r, _ := Responded(first.T, first.Ask, you, ByID(ts)); r != "" {
		t.Errorf("robin's question is not the maintainer's response: %q", r)
	}
}

func TestDeskPageFiltersAndAsks(t *testing.T) {
	ts := fixtures(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v := DeskPage(Desk(ts, you), "unstick", "T0007", now)
	if v.Kind != "unstick" || v.Total != 9 || len(v.Items) != 2 {
		t.Fatalf("filtered desk %+v", v)
	}
	if v.Filters[0].Label != "All" || v.Filters[0].N != 9 || v.Filters[0].On || v.Filters[0].Href != "/" {
		t.Errorf("all chip %+v", v.Filters[0])
	}
	var chips []string
	for _, c := range v.Filters[1:] {
		chips = append(chips, c.Label)
	}
	if !reflect.DeepEqual(chips, []string{"Question", "Unstick", "Review", "Start", "Design", "Proposal", "Decide", "Notice"}) {
		t.Errorf("chips in desk order: %v", chips)
	}
	for _, q := range v.Items {
		if q.Href != "/t/"+q.ID+"?kind=unstick" || q.Glyph != "!" || q.Label != "Unstick" {
			t.Errorf("item %+v", q)
		}
		if q.Current != (q.ID == "T0007") {
			t.Errorf("current marker on %s", q.ID)
		}
	}
	if all := DeskPage(Desk(ts, you), "nonsense", "", now); all.Kind != "" || len(all.Items) != 9 {
		t.Errorf("an unknown kind shows everything: %+v", all)
	}
}

func TestDeskRowsSayWhatItIsAndWhatItWants(t *testing.T) {
	pr := tk(t, `{"id": "T0001", "column": "pr_open", "pr": {"number": 26}}`)
	for _, k := range []struct {
		t    *ticket.Ticket
		kind string
		want string
	}{
		{pr, "review", "Review PR #26"},
		{tk(t, `{"id": "T0001", "column": "ready_for_pr"}`), "open", "Open a PR?"},
		{tk(t, `{"id": "T0001", "proposed": [{}, {}, {}]}`), "design", "Approve design · 3 tickets"},
		{tk(t, `{"id": "T0001", "source": {"issue": 11}}`), "decide", "Start or drop? · issue #11"},
		{tk(t, `{"id": "T0001", "kind": "task"}`), "notice", "Read the answer"},
	} {
		if got := Action(k.t, &ticket.Ask{Kind: k.kind}); got != k.want {
			t.Errorf("%s: %q, want %q", k.kind, got, k.want)
		}
	}
}

func TestMergeOnlyForAnEligiblePROnYourWord(t *testing.T) {
	x := ticket.Clone(find(fixtures(t), "T0001"))
	g, pr := x.Gate.V, x.PR.V
	g.AutoEligible, pr.Draft = true, ticket.Some(false)
	x.Gate, x.PR = ticket.Some(g), ticket.Some(pr)
	if !Mergeable(x) {
		t.Fatalf("an eligible PR is mergeable")
	}
	a := AskFor(x)
	if h := Headline(x, a); h != "Merge PR #23?" {
		t.Errorf("headline %q", h)
	}
	if got := Action(x, a); got != "Merge PR #23?" {
		t.Errorf("action %q", got)
	}
	if !strings.HasPrefix(WaitingWhat(x, a), "It passed every check") {
		t.Errorf("waiting %q", WaitingWhat(x, a))
	}
	draft := find(fixtures(t), "T0001")
	if Mergeable(draft) {
		t.Errorf("a draft PR must not be merged by the factory")
	}
	if h := Headline(draft, AskFor(draft)); h != "Review and merge PR #23?" {
		t.Errorf("a draft's headline is the ask's question: %q", h)
	}
}

// An eligible PR that GitHub will not merge before the maintainer approves
// it asks for that approval on GitHub, not for a merge; approved at its head
// and still open, it asks for the merge again.
func TestAnUnapprovedPRAsksForTheApprovalOnGitHub(t *testing.T) {
	x := ticket.Clone(find(fixtures(t), "T0001"))
	g, pr := x.Gate.V, x.PR.V
	g.AutoEligible, pr.Draft = true, ticket.Some(false)
	pr.Review = ticket.Some(ticket.PRReview{State: ticket.ReviewNone})
	x.Gate, x.PR = ticket.Some(g), ticket.Some(pr)
	a := AskFor(x)
	if h := Headline(x, a); h != "Approve PR #23 on GitHub" {
		t.Errorf("headline %q", h)
	}
	if got := Action(x, a); got != "Approve PR #23 on GitHub" {
		t.Errorf("action %q", got)
	}
	if w := WaitingWhat(x, a); !strings.Contains(w, "the factory merges it by itself on the next tick") || strings.Contains(w, "Merge it") {
		t.Errorf("waiting %q", w)
	}
	if c := PRCallout(x); c == nil || c.Title != "Passed every check for automatic approval" {
		t.Errorf("callout before any refusal: %+v", c)
	}
	// GitHub refused a merge for want of the approval: the callout says so
	pr.MergeBlocked = ticket.Some(ticket.MergeBlockedApproval)
	x.PR = ticket.Some(pr)
	if c := PRCallout(x); c == nil || c.Title != "Waiting for your approval on GitHub" || len(c.Lines) == 0 {
		t.Errorf("callout after a refusal: %+v", c)
	}
	// approved at the head the gate checked: Merge it is back
	pr.Review = ticket.Some(ticket.PRReview{State: ticket.ReviewApproved, Commit: g.Head})
	x.PR = ticket.Some(pr)
	if h := Headline(x, AskFor(x)); h != "Merge PR #23?" {
		t.Errorf("approved: headline %q", h)
	}
	if c := PRCallout(x); c == nil || c.Title != "Passed every check for automatic approval" {
		t.Errorf("approved: callout %+v", c)
	}
}

func TestReadyForPRIsOnTheDesk(t *testing.T) {
	x := ticket.Clone(find(fixtures(t), "T0001"))
	x.Column = "ready_for_pr"
	x.SetAsk(ticket.AskFor(x, "gate", "gate passed", x.Updated, model.ReasonGatePassed))
	a := AskFor(x)
	if a == nil || a.Kind != "open" || a.Question != "Open a PR for this?" {
		t.Fatalf("a ticket waiting to go out must ask: %+v", a)
	}
	if got := Action(x, a); got != "Update PR #23?" {
		t.Errorf("action %q", got)
	}
}

func TestManagerDraftsAreShown(t *testing.T) {
	x := tk(t, `{"id": "T0050", "project": "factory", "kind": "manage", "title": "A morning check", "column": "needs_human",
		"ask": {"kind": "draft", "question": "Apply the manager's 1 config change?", "since": "2026-10-16T10:00:00Z"},
		"drafts": [{"title": "Daily check", "why": "Files a check every morning.", "patch": "+[[schedules]]\n"}]}`)
	a := AskFor(x)
	if got := Action(x, a); got != "Apply the draft? · 1 change" {
		t.Errorf("action: %q", got)
	}
	if got := Headline(x, a); got != "Apply the manager's 1 config change?" {
		t.Errorf("headline: %q", got)
	}
}

func TestAnAnsweredQuestionBringsItsTicketBack(t *testing.T) {
	ts := fixtures(t)
	it := Desk(ts, you)[0]
	p := it.T
	since := ParseTime(it.Ask.Since)
	at := func(d time.Duration) string { return since.Add(d).UTC().Format(time.RFC3339) }
	child := tk(t, `{"id": "T0999", "kind": "task", "parent": "`+string(p.ID)+`", "column": "working", "project": "`+p.Project+`"}`)
	ts = append(ts, child)
	p.History = append(p.History, ticket.Entry{At: at(time.Hour), Event: "asked", Detail: "T0999: why the nil check?"})
	if r, _ := Responded(p, it.Ask, you, ByID(ts)); r != "asked" {
		t.Fatalf("waiting on the answer: %q", r)
	}
	child.Result, child.Column = ticket.Some("Because **lint** says so.\n\nMore detail here."), "done"
	p.History = append(p.History, ticket.Entry{At: at(2 * time.Hour), Event: "answered", Detail: "T0999: Because lint says so."})
	items := Desk(ts, you)
	if items[0].T != p || !items[0].Answered || items[0].Responded != "" {
		t.Errorf("the answer should bring %s back to the top: %+v", p.ID, items[0])
	}
	if id, q := AskOf(p.History[len(p.History)-2]); id != "T0999" || q != "why the nil check?" {
		t.Errorf("asked: %q %q", id, q)
	}
}

// Every ask kind the model knows has its words here (§2.4.3): a label and a
// glyph, a desk position, an action and what the desk waits for.
func TestEveryAskKindHasItsWords(t *testing.T) {
	x := tk(t, `{"id": "T0001", "column": "needs_human"}`)
	seen := map[int]bool{}
	for _, k := range model.AskKinds() {
		kind := string(k.Kind)
		if KindLabel(kind) == "" || KindLabel(kind) != k.Label {
			t.Errorf("%s: label %q", kind, KindLabel(kind))
		}
		if KindGlyph(kind) == "" || KindGlyph(kind) != k.Glyph {
			t.Errorf("%s: glyph %q", kind, KindGlyph(kind))
		}
		if seen[askRank(kind)] || askRank(kind) >= len(model.AskKinds()) {
			t.Errorf("%s: desk position %d", kind, askRank(kind))
		}
		seen[askRank(kind)] = true
		a := &ticket.Ask{Kind: kind, Question: "q"}
		if Action(x, a) == KindLabel(kind) {
			t.Errorf("%s: no action words", kind)
		}
		if kind != string(model.AskUnstick) && WaitingWhat(x, a) == WaitingWhat(x, &ticket.Ask{Kind: "unstick"}) {
			t.Errorf("%s: no waiting words", kind)
		}
	}
	if KindLabel("other") != "Other" || KindGlyph("other") != "?" {
		t.Errorf("an unknown kind: %q %q", KindLabel("other"), KindGlyph("other"))
	}
}

// deployTicket is a held deploy's Deploy? ticket, as doors files it.
func deployTicket(t *testing.T) *ticket.Ticket {
	t.Helper()
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	x := ticket.New("T0060", ticket.NewIn{Project: model.NoticeProject, Title: "Deploy the factory?", Door: "clock", Column: "needs_human"}, at)
	x.Deploy = ticket.Some(ticket.Deploy{Rev: "0123456789ab", Why: []string{"merged #12"}})
	ticket.Move(x, "needs_human", model.ReasonDeployHeld, "deploys are held", "", "", at)
	return x
}

// The desk's words for a held deploy.
func TestDeployAskIsOnTheDesk(t *testing.T) {
	x := deployTicket(t)
	a := AskFor(x)
	if a == nil || a.Kind != "deploy" {
		t.Fatalf("ask %+v", a)
	}
	if got := Headline(x, a); got != "Deploy main to the factory?" {
		t.Errorf("headline %q", got)
	}
	if got := Action(x, a); got != "Deploy main? · now 0123456789ab" {
		t.Errorf("action %q", got)
	}
	if got := WaitingWhat(x, a); got != "Deploy it now, or not now: the next merge asks again." {
		t.Errorf("waiting %q", got)
	}
}

// The story of a held deploy: held, the maintainer's go-ahead, the tick's
// start.
func TestDeployStory(t *testing.T) {
	x := deployTicket(t)
	at := time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)
	ticket.Move(x, "needs_human", model.ReasonDeployHeld, "now fedcba987654", "", "", at)
	ticket.Move(x, "done", model.ReasonDeployRequested, "deploy main", "", "maintainer", at)
	ticket.Move(x, "done", model.ReasonDeployStarted, "deploy started", "", "", at.Add(time.Minute))
	got := fmtSteps(BuildStory(x, map[ticket.ID]*ticket.Ticket{x.ID: x}))
	for _, want := range []string{
		"Dispatcher: Deploys are held, so this deploy of main waits for your go-ahead.",
		"Dispatcher: Another merge offered a deploy; main is now fedcba987654.",
		"You: Asked the factory to deploy main; the next tick starts it.",
		"Dispatcher: Started the deploy of main.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in the story:\n%s", want, got)
		}
	}
}

// A change to the factory's walls says so on the desk, for a review and an
// open ask; one without walls does not.
func TestActionFlagsWalls(t *testing.T) {
	x := ticket.Clone(find(fixtures(t), "T0001"))
	review := AskFor(x)
	if got := Action(x, review); strings.Contains(got, "walls") || len(Walls(x)) != 0 {
		t.Errorf("no walls: %q %q", got, Walls(x))
	}
	g := x.Gate.V
	g.Walls = []string{"internal/gate/gate.go", "deploy.sh"}
	x.Gate = ticket.Some(g)
	if got := Walls(x); !slices.Equal(got, g.Walls) {
		t.Errorf("Walls = %q", got)
	}
	if got := Action(x, review); got != "Review PR #23 · touches the walls" {
		t.Errorf("review action %q", got)
	}
	x.Column = "ready_for_pr"
	x.SetAsk(ticket.AskFor(x, "gate", "gate passed", x.Updated, model.ReasonGatePassed))
	if got := Action(x, AskFor(x)); got != "Update PR #23? · touches the walls" {
		t.Errorf("open action %q", got)
	}
	if got := Action(x, &ticket.Ask{Kind: "notice"}); strings.Contains(got, "walls") {
		t.Errorf("a notice flags the walls: %q", got)
	}
}
