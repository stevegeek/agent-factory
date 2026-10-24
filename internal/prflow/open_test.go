package prflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func (h *harness) setHead(branch, head string) { h.git.heads["refs/heads/"+branch] = head }

// port: TF GithubOutTest.test_open_pr_pushes_with_the_token_out_of_argv_and_writes_public_text
func TestOpenPRPushesWithTheTokenAndWritesPublicText(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := readyTicket(t, false, sha('a'))
	h.setHead("bot/x", sha('a'))
	h.routes(at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 5, "html_url": "https://github.com/o/gem/pull/5", "draft": true})))
	pr, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.git.pushes) != 1 || h.git.pushes[0] != (push{Repo: "o/gem", Branch: "bot/x", Token: "ghs_secret"}) {
		t.Fatalf("pushes = %+v", h.git.pushes)
	}
	calls := h.gh.calls()
	if len(calls) != 2 || calls[0].String() != "POST /repos/o/gem/pulls" ||
		calls[1].String() != "POST /repos/o/gem/pulls/5/requested_reviewers" {
		t.Fatalf("requests = %v", calls)
	}
	if got := string(jsonx.EncodePython(calls[1].Body, -1)); got != `{"reviewers": ["maintainer-login"]}` {
		t.Errorf("review request = %s", got)
	}
	for _, c := range calls {
		if c.Auth != "Bearer ghs_secret" {
			t.Errorf("%s authorised by %q", c, c.Auth)
		}
	}
	body := calls[0].Body
	if o, _ := body.Object(); len(o) != 5 || o[0].Key != "title" || o[2].Key != "head" || o[3].Key != "base" || o[4].Key != "draft" {
		t.Errorf("create body keys = %s", jsonx.EncodePython(body, -1))
	}
	if d, _ := body.Get("draft").Bool(); !d {
		t.Error("a PR the gate did not call eligible must open as a draft")
	}
	text, _ := body.Get("body").Str()
	for _, want := range []string{"Fixes #7", "12 changed lines"} {
		if !strings.Contains(text, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	if hasTicketID(text) {
		t.Errorf("body names a ticket: %s", text)
	}
	if tk.Column != "pr_open" || tk.PR.V.Number != 5 || pr.Number != 5 || pr.URL != "https://github.com/o/gem/pull/5" {
		t.Fatalf("column %s, pr %+v", tk.Column, tk.PR)
	}
	if got := details(tk, "move"); len(got) != 1 || got[0] != "ready_for_pr -> pr_open: draft PR https://github.com/o/gem/pull/5" {
		t.Errorf("moves = %q", got)
	}
	if got := details(tk, "review requested"); len(got) != 1 || got[0] != "maintainer-login" {
		t.Errorf("review requested = %q", got)
	}
	if mv, ok := tk.History[0].Data.(ticket.Moved); !ok || mv.Reason != model.ReasonPROpened {
		t.Errorf("move payload = %#v", tk.History[0].Data)
	}
	// the PR keys are Python's {**old, number, url, draft}
	if got := string(jsonx.EncodePython(tk.Tree().Get("pr"), -1)); got != `{"number": 5, "url": "https://github.com/o/gem/pull/5", "draft": true}` {
		t.Errorf("pr = %s", got)
	}
}

// port: TF GithubOutTest.test_never_pushes_a_moved_branch_or_a_non_bot_branch
func TestNeverPushesAMovedBranchOrANonBotBranch(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	p := project(t, cfg, "gem")
	tk := readyTicket(t, true, sha('0'))
	h.setHead("bot/x", sha('a'))
	_, err := h.flow.OpenPR(context.Background(), cfg, p, tk)
	var r *Refusal
	if !errors.As(err, &r) || err.Error() != "the branch moved after Gate 1 checked it; send it through the gate again" {
		t.Fatalf("err = %v", err)
	}
	tk = readyTicket(t, true, sha('a'))
	tk.Branch = ticket.Some("main")
	h.setHead("main", sha('a'))
	_, err = h.flow.OpenPR(context.Background(), cfg, p, tk)
	if !errors.As(err, &r) || err.Error() != "refusing to push main: the factory only pushes bot/* branches" {
		t.Fatalf("err = %v", err)
	}
	if len(h.git.pushes) != 0 || len(h.gh.calls()) != 0 || h.token.n != 0 {
		t.Fatalf("pushed %v, asked GitHub %v, minted %d tokens", h.git.pushes, h.gh.calls(), h.token.n)
	}
	// and a ticket that is not through the gate has no PR to open
	tk = readyTicket(t, true, sha('a'))
	tk.Column = "gate"
	if _, err := h.flow.OpenPR(context.Background(), cfg, p, tk); err == nil || err.Error() != "T0042 is not ready for a PR" {
		t.Fatalf("err = %v", err)
	}
}

// fakeRepoPR answers like GithubOutTest.fake_github: one PR that a PATCH
// updates, {} for review requests and GraphQL.
func fakeRepoPR(pr map[string]any) []route {
	return []route{
		{Method: "POST", Prefix: "/graphql", Reply: func(jsonx.Value) reply { return ok(map[string]any{"data": map[string]any{}}) }},
		{Method: "POST", Prefix: "/repos/o/gem/pulls/5/requested_reviewers", Reply: func(jsonx.Value) reply { return ok(map[string]any{}) }},
		{Method: "PATCH", Prefix: "/repos/o/gem/pulls/5", Reply: func(b jsonx.Value) reply {
			for k, v := range toAny(b).(map[string]any) {
				pr[k] = v
			}
			return ok(deepCopy(pr))
		}},
		{Method: "GET", Prefix: "/repos/o/gem/pulls/5", Reply: func(jsonx.Value) reply { return ok(deepCopy(pr)) }},
		{Method: "POST", Prefix: "/repos/o/gem/issues/5/comments", Reply: func(jsonx.Value) reply { return ok(map[string]any{}) }},
	}
}

func openPR5(draft bool) map[string]any {
	return map[string]any{"number": 5, "html_url": "https://github.com/o/gem/pull/5", "state": "open", "draft": draft,
		"head": map[string]any{"ref": "bot/x"}, "node_id": "PR_5"}
}

// port: TF GithubOutTest.test_a_rework_updates_the_open_pr_and_its_draft_state
func TestAReworkUpdatesTheOpenPRAndItsDraftState(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := readyTicket(t, true, sha('a'))
	tk.PR = ticket.Some(ticket.PR{Number: 5, URL: "u", Draft: ticket.Some(true)})
	h.setHead("bot/x", sha('a'))
	pr := openPR5(true)
	h.routes(fakeRepoPR(pr)...)
	if _, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
	if h.gh.did("POST", "/repos/o/gem/pulls") {
		t.Error("a rework opened a second PR")
	}
	if !strings.Contains(pr["body"].(string), "Clarify the install steps.") {
		t.Errorf("PR body not updated: %v", pr["body"])
	}
	g := h.gh.body(t, "POST", "/graphql")
	if q, _ := g.Get("query").Str(); !strings.Contains(q, "markPullRequestReadyForReview") {
		t.Errorf("graphql = %s", q)
	}
	if v := string(jsonx.EncodePython(g.Get("variables"), -1)); v != `{"id": "PR_5"}` {
		t.Errorf("variables = %s", v)
	}
	if tk.Column != "pr_open" || tk.PR.V.Draft.V {
		t.Fatalf("column %s, draft %v", tk.Column, tk.PR.V.Draft)
	}
	if mv := details(tk, "move"); len(mv) != 1 || !strings.HasSuffix(mv[0], "updated") || mv[0] != "ready_for_pr -> pr_open: PR https://github.com/o/gem/pull/5 updated" {
		t.Errorf("move = %q", mv)
	}
	if h.gh.did("POST", "/repos/o/gem/issues/5/comments") {
		t.Error("commented on a PR nobody reviewed")
	}
}

// The worker's summary answers the review: a rework after reviews posts
// "Updated after your review.", and a failed comment is only an
// event.
func TestAReworkAfterAReviewSaysWhatChanged(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newHarness(t)
		cfg := parseConfig(t, testConfig)
		tk := readyTicket(t, true, sha('a'))
		tk.PR = ticket.Some(ticket.PR{Number: 5, URL: "u", Draft: ticket.Some(false), ReviewsSeen: []int64{3}})
		h.setHead("bot/x", sha('a'))
		rs := fakeRepoPR(openPR5(false))
		if fail {
			rs = append([]route{at("POST", "/repos/o/gem/issues/5/comments", refuse(403, "Resource not accessible"))}, rs...)
		}
		h.routes(rs...)
		if _, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
			t.Fatal(err)
		}
		got, _ := h.gh.body(t, "POST", "/repos/o/gem/issues/5/comments").Get("body").Str()
		if got != "Updated after your review.\n\nClarify the install steps.\n\nOpened by a bot." {
			t.Errorf("comment = %q", got)
		}
		errs := details(tk, "error")
		if fail != (len(errs) == 1) || fail && errs[0] != "github: could not comment on PR #5 (403)" {
			t.Errorf("fail=%v: errors %q", fail, errs)
		}
		if tk.Column != "pr_open" || h.gh.any("PATCH") != true {
			t.Errorf("column %s", tk.Column)
		}
		if h.gh.did("POST", "/graphql") {
			t.Error("the draft state already matched; no GraphQL call is needed")
		}
	}
}

// A 422 on create means the branch already has an open PR (opened by hand,
// or by a run that crashed before saving): it is adopted and updated.
// A 422 with no such PR is the error.
func TestA422AdoptsTheOpenPROnTheBranch(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, testConfig)
	tk := readyTicket(t, true, sha('a'))
	h.setHead("bot/x", sha('a'))
	existing := map[string]any{"number": 9, "html_url": "https://github.com/o/gem/pull/9", "state": "open", "draft": false,
		"head": map[string]any{"ref": "bot/x"}, "node_id": "PR_9"}
	h.routes(
		at("POST", "/repos/o/gem/pulls/9/requested_reviewers", ok(map[string]any{})),
		at("POST", "/repos/o/gem/pulls", refuse(422, "A pull request already exists for o:bot/x.")),
		at("GET", "/repos/o/gem/pulls?state=open&head=o:bot/x", ok([]any{existing})),
		at("PATCH", "/repos/o/gem/pulls/9", ok(existing)),
	)
	pr, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 9 || tk.Column != "pr_open" || details(tk, "move")[0] != "ready_for_pr -> pr_open: PR https://github.com/o/gem/pull/9 updated" {
		t.Fatalf("pr %+v, moves %q", pr, details(tk, "move"))
	}

	h = newHarness(t)
	tk = readyTicket(t, true, sha('a'))
	h.setHead("bot/x", sha('a'))
	h.routes(
		at("POST", "/repos/o/gem/pulls", refuse(422, "Validation Failed")),
		at("GET", "/repos/o/gem/pulls?state=open&head=o:bot/x", ok([]any{})),
	)
	_, err = h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk)
	var he *github.HTTPError
	if !errors.As(err, &he) || he.Code != 422 || tk.Column != "ready_for_pr" {
		t.Fatalf("err = %v, column %s", err, tk.Column)
	}
}

// A PR that is gone (404), closed or on another branch is not reworked: a
// new one is opened.
func TestAGoneOrClosedPRIsReplaced(t *testing.T) {
	for _, old := range []reply{refuse(404, "Not Found"), ok(map[string]any{"number": 5, "state": "closed", "head": map[string]any{"ref": "bot/x"}}),
		ok(map[string]any{"number": 5, "state": "open", "head": map[string]any{"ref": "bot/other"}})} {
		h := newHarness(t)
		cfg := parseConfig(t, testConfig)
		tk := readyTicket(t, true, sha('a'))
		tk.PR = ticket.Some(ticket.PR{Number: 5, URL: "u", Draft: ticket.Some(false), ReviewsSeen: []int64{3},
			MergeBlocked: ticket.Some(ticket.MergeBlockedApproval)}) // a refusal of the old commits goes with them
		h.setHead("bot/x", sha('a'))
		h.routes(
			at("GET", "/repos/o/gem/pulls/5", old),
			at("POST", "/repos/o/gem/pulls/6/requested_reviewers", ok(map[string]any{})),
			at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 6, "html_url": "https://github.com/o/gem/pull/6", "draft": false})),
		)
		if _, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
			t.Fatal(err)
		}
		if tk.PR.V.Number != 6 || details(tk, "move")[0] != "ready_for_pr -> pr_open: PR https://github.com/o/gem/pull/6" {
			t.Errorf("pr %+v moves %q", tk.PR.V, details(tk, "move"))
		}
		if h.gh.did("POST", "/repos/o/gem/issues/6/comments") {
			t.Error("a new PR is not commented on")
		}
		// the old keys stay where they were
		if got := string(jsonx.EncodePython(tk.Tree().Get("pr"), -1)); got != `{"number": 6, "url": "https://github.com/o/gem/pull/6", "draft": false, "reviews_seen": [3]}` {
			t.Errorf("pr = %s", got)
		}
	}
}

// A failed review request leaves the PR open with an error event;
// with no reviewers configured nobody is asked.
func TestAFailedReviewRequestIsTolerated(t *testing.T) {
	for _, c := range []struct {
		name   string
		config string
		reply  reply
		events []string
		asked  bool
	}{
		{"refused", testConfig, refuse(422, "Reviews may only be requested from collaborators."), []string{"github: review request for maintainer-login failed (422)"}, true},
		{"unanswered", testConfig, reply{NetErr: errNoAnswer}, nil, true},
		{"nobody", strings.Replace(testConfig, `reviewers = ["maintainer-login"]`, "", 1), ok(map[string]any{}), nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := parseConfig(t, c.config)
			tk := readyTicket(t, true, sha('a'))
			h.setHead("bot/x", sha('a'))
			h.routes(
				at("POST", "/repos/o/gem/pulls/5/requested_reviewers", c.reply),
				at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 5, "html_url": "https://github.com/o/gem/pull/5", "draft": false})),
			)
			if _, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
				t.Fatal(err)
			}
			if tk.Column != "pr_open" || h.gh.did("POST", "/repos/o/gem/pulls/5/requested_reviewers") != c.asked {
				t.Fatalf("column %s, requests %v", tk.Column, h.gh.calls())
			}
			errs := details(tk, "error")
			if c.name == "unanswered" {
				if len(errs) != 1 || !strings.HasPrefix(errs[0], "github: review request for maintainer-login failed (") {
					t.Errorf("errors = %q", errs)
				}
				return
			}
			if strings.Join(errs, "|") != strings.Join(c.events, "|") {
				t.Errorf("errors = %q, want %q", errs, c.events)
			}
			if got := details(tk, "review requested"); len(got) != 0 {
				t.Errorf("review requested = %q", got)
			}
		})
	}
}

// port: TF GithubOutTest.test_through_the_gate_asks_to_open_the_pr
func TestThroughTheGateAsksToOpenThePR(t *testing.T) {
	tk := decode(t, `{"id": "T0001", "title": "x", "column": "gate", "history": [], "reports": {"work": {"summary": "Did it."}}}`)
	ticket.Move(tk, "ready_for_pr", "", "auto-merge eligible", "", "", startTime)
	a, ok := tk.Ask.Get()
	if !ok || a.Kind != "open" || a.Question != "Open a PR for this?" {
		t.Fatalf("ask = %+v", tk.Ask)
	}
}

// hygieneConfig is testConfig with gem a hygiene project and a deny list
// of made-up terms.
func hygieneConfig() string {
	return "[hygiene]\ndeny = [\"hostx\", \"192.0.2.7\"]\n" +
		strings.Replace(testConfig, `github = "o/gem"`, "github = \"o/gem\"\nhygiene = true", 1)
}

// On a hygiene project the PR title is the ticket's when clean, else the
// first sentence of the work summary, clipped to 72 code points.
func TestPRTitleAvoidsDeniedTerms(t *testing.T) {
	cfg := parseConfig(t, hygieneConfig())
	gem, local := project(t, cfg, "gem"), project(t, cfg, "local")
	tk := readyTicket(t, false, sha('a'))
	if got := PRTitle(cfg, gem, tk); got != "Tidy README" {
		t.Errorf("clean title = %q", got)
	}
	tk.Title = "Tidy README on hostx"
	if got := PRTitle(cfg, gem, tk); got != "Clarify the install steps." {
		t.Errorf("hit = %q", got)
	}
	if got := PRTitle(cfg, local, tk); got != tk.Title {
		t.Errorf("ordinary project = %q", got)
	}
	tk = decode(t, `{"id": "T1", "title": "Tidy README on hostx", "reports": {"work": {"summary": "`+strings.Repeat("word ", 30)+`and more. Second."}}}`)
	if got := PRTitle(cfg, gem, tk); []rune(got)[len([]rune(got))-1] != '…' || len([]rune(got)) != 72 {
		t.Errorf("long summary = %q", got)
	}
}

// OpenPR on a hygiene project scans the exact title, branch and body it
// would publish, and refuses on a hit before anything is pushed, naming
// only the count.
func TestOpenPRRefusesDeniedText(t *testing.T) {
	h := newHarness(t)
	cfg := parseConfig(t, strings.Replace(hygieneConfig(), `pr_footer = "Opened by a bot."`, `pr_footer = "Opened by a bot on 192.0.2.7."`, 1))
	tk := readyTicket(t, false, sha('a'))
	passPublic(cfg, project(t, cfg, "gem"), tk, sha('a'))
	h.setHead("bot/x", sha('a'))
	_, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk)
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(err.Error(), "1 finding(s)") || strings.Contains(err.Error(), "192.0.2.7") {
		t.Fatalf("err = %v", err)
	}
	if len(h.git.pushes) != 0 || len(h.gh.calls()) != 0 || h.token.n != 0 || tk.Column != "ready_for_pr" {
		t.Fatalf("pushed %v, asked GitHub %v, minted %d tokens, column %s", h.git.pushes, h.gh.calls(), h.token.n, tk.Column)
	}
	// the same text on an ordinary project opens
	h.routes(at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 5, "html_url": "https://github.com/o/gem/pull/5", "draft": true})))
	cfg = parseConfig(t, strings.Replace(testConfig, `pr_footer = "Opened by a bot."`, `pr_footer = "Opened by a bot on 192.0.2.7."`, 1))
	if _, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil {
		t.Fatal(err)
	}
}

// On a hygiene project OpenPR pushes only a commit public review passed:
// a ticket that reached ready_for_pr with no pass, or with a pass of
// another head, is refused before anything is pushed, and goes back to the
// gate, which has public review read it first.
func TestOpenPRRefusesAHygieneCommitPublicReviewDidNotPass(t *testing.T) {
	cfg := parseConfig(t, hygieneConfig())
	for _, passed := range []string{"", sha('b')} {
		h := newHarness(t)
		tk := readyTicket(t, false, sha('a'))
		if passed != "" {
			passPublic(cfg, project(t, cfg, "gem"), tk, passed)
		}
		h.setHead("bot/x", sha('a'))
		_, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk)
		var r *Refusal
		if !errors.As(err, &r) || !errors.Is(err, ErrUnread) || !strings.Contains(err.Error(), "public review") {
			t.Fatalf("passed %q: err = %v", passed, err)
		}
		if len(h.git.pushes) != 0 || len(h.gh.calls()) != 0 {
			t.Fatalf("passed %q: pushed %v, asked GitHub %v", passed, h.git.pushes, h.gh.calls())
		}
		if m, _ := lastEntry(tk).Data.(ticket.Moved); tk.Column != "gate" || m.Reason != model.ReasonPublicUnread {
			t.Fatalf("passed %q: %s %+v", passed, tk.Column, lastEntry(tk))
		}
	}
	h := newHarness(t)
	h.routes(at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 5, "html_url": "https://github.com/o/gem/pull/5", "draft": true})))
	tk := readyTicket(t, false, sha('a'))
	passPublic(cfg, project(t, cfg, "gem"), tk, sha('a'))
	h.setHead("bot/x", sha('a'))
	if _, err := h.flow.OpenPR(context.Background(), cfg, project(t, cfg, "gem"), tk); err != nil || tk.Column != "pr_open" {
		t.Fatalf("a passed commit: %v %s", err, tk.Column)
	}
	// a project without hygiene has no public review: no pass is needed
	plain := parseConfig(t, testConfig)
	h = newHarness(t)
	h.routes(at("POST", "/repos/o/gem/pulls", ok(map[string]any{"number": 5, "html_url": "https://github.com/o/gem/pull/5", "draft": true})))
	tk = readyTicket(t, false, sha('a'))
	h.setHead("bot/x", sha('a'))
	if tk.PublicHead.Set || project(t, plain, "gem").Hygiene {
		t.Fatal("the fixture has a pass or hygiene")
	}
	if _, err := h.flow.OpenPR(context.Background(), plain, project(t, plain, "gem"), tk); err != nil || tk.Column != "pr_open" || len(h.git.pushes) != 1 {
		t.Fatalf("a project without hygiene: %v %s %v", err, tk.Column, h.git.pushes)
	}
}

// A hygiene ticket in ready_for_pr that public review never passed (moved
// there by hand, or there before public review existed) is not refused
// tick after tick: the GitHub pass sends it back to the gate once, with no
// error event, and pushes nothing.
func TestTheGitHubPassSendsAnUnreadHygieneTicketToTheGate(t *testing.T) {
	cfgText := strings.Replace(hygieneConfig(), "[github]\n", "[github]\nopen_prs = \"auto\"\n", 1)
	if !strings.Contains(cfgText, `open_prs = "auto"`) {
		t.Fatal("the config has no [github] table")
	}
	h := newHarness(t)
	tk := readyTicket(t, false, sha('a'))
	h.board.put(t, tk)
	h.setHead("bot/x", sha('a'))
	h.pass(context.Background(), cfgText)
	got := h.board.load(t, tk.ID)
	if m, _ := lastEntry(got).Data.(ticket.Moved); got.Column != "gate" || m.Reason != model.ReasonPublicUnread {
		t.Fatalf("%s %+v", got.Column, lastEntry(got))
	}
	for _, e := range got.History {
		if e.Event == "error" {
			t.Errorf("an error event: %+v", e)
		}
	}
	if len(h.git.pushes) != 0 || len(h.gh.calls()) != 0 {
		t.Errorf("pushed %v, asked GitHub %v", h.git.pushes, h.gh.calls())
	}
	// the next pass leaves it alone: it is in the gate now
	saves := len(h.board.saves)
	h.pass(context.Background(), cfgText)
	if len(h.board.saves) != saves {
		t.Error("the next pass saved it again")
	}
}

// passPublic records public review's pass of head, with the texts the PR
// would carry now.
func passPublic(cfg *config.Config, p config.Project, tk *ticket.Ticket, head string) {
	tk.PublicHead = ticket.Some(head)
	tk.TextsBeforePublic = ticket.Some(PublicDigest(PublicTexts(cfg, p, tk)))
}

// A pass is of the commit and of the texts public review read: a text
// that changed with no new commit (a rerun's summary) is not pushed.
func TestOpenPRRefusesAPassWhoseTextsChanged(t *testing.T) {
	cfg := parseConfig(t, hygieneConfig())
	gem := project(t, cfg, "gem")
	h := newHarness(t)
	tk := readyTicket(t, false, sha('a'))
	passPublic(cfg, gem, tk, sha('a'))
	if !PublicPassed(cfg, gem, tk, sha('a')) || PublicPassed(cfg, gem, tk, sha('b')) {
		t.Fatal("a pass of this head and these texts")
	}
	tk.Reports.Set("work", jsonx.ObjectOf(jsonx.Member{Key: "summary", Value: jsonx.String("Clarify the install steps again.")}))
	if PublicPassed(cfg, gem, tk, sha('a')) {
		t.Fatal("a changed summary still passes")
	}
	h.setHead("bot/x", sha('a'))
	_, err := h.flow.OpenPR(context.Background(), cfg, gem, tk)
	if !errors.Is(err, ErrUnread) || len(h.git.pushes) != 0 || tk.Column != "gate" {
		t.Fatalf("err %v, pushed %v, column %s", err, h.git.pushes, tk.Column)
	}
}

// The texts public review reads are what OpenPR sends; the PR title is
// fixed (a finding there goes to a person) only when it is the ticket's
// own, not when it is made from the worker's summary.
func TestPublicTextsAndWhichTheWorkerCanChange(t *testing.T) {
	cfg := parseConfig(t, hygieneConfig())
	gem := project(t, cfg, "gem")
	tk := readyTicket(t, false, sha('a'))
	fixed := map[string]bool{}
	for _, x := range PublicTexts(cfg, gem, tk) {
		fixed[x.Name+": "+x.Text] = x.Fixed
	}
	want := map[string]bool{"branch: bot/x": true, "pr-title: Tidy README": true, "ticket: Fixes #7": true,
		"summary: Clarify the install steps.": false, "review: verdict: pass\nchanges runtime behaviour for users: no": false,
		"footer: Opened by a bot.": true}
	if len(fixed) != len(want) {
		t.Errorf("texts %v", fixed)
	}
	for k, v := range want {
		if got, ok := fixed[k]; !ok || got != v {
			t.Errorf("%q: fixed %v, %v; want %v", k, got, ok, v)
		}
	}
	tk.Title = "Tidy README on hostx"
	for _, x := range PublicTexts(cfg, gem, tk) {
		if x.Name == TextTitle && (x.Text != "Clarify the install steps." || x.Fixed) {
			t.Errorf("a title from the summary: %+v", x)
		}
	}
}
