package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/prflow"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The GitHub pass through the real PR flow, with GitHub behind a fake
// transport: the intents Foreman records are carried out first in the pass.

type ghRequest struct{ Method, Path, Body string }

type ghFake struct {
	c    *calls // nil counts nothing
	mu   sync.Mutex
	reqs []ghRequest
	pr   map[string]any
}

func (f *ghFake) Do(req *http.Request) (*http.Response, error) {
	if f.c != nil {
		f.c.note("gh." + req.Method)
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, ghRequest{req.Method, req.URL.Path, string(body)})
	f.mu.Unlock()
	var v any = map[string]any{}
	switch {
	case req.Method == "POST" && req.URL.Path == "/repos/o/gem/pulls":
		v = map[string]any{"number": 7, "html_url": "https://github.com/o/gem/pull/7", "state": "open", "draft": false,
			"node_id": "PR_7", "head": map[string]any{"ref": "bot/x", "sha": "abc"}}
	case req.Method == "GET" && isPRPath(req.URL.Path) && f.pr != nil:
		v = f.pr
	case req.Method == "PUT" && strings.HasSuffix(req.URL.Path, "/merge"):
		v = map[string]any{"merged": true, "sha": "abc"}
	case req.Method == "POST" && req.URL.Path == "/graphql":
		v = map[string]any{"data": map[string]any{}}
	case req.Method == "GET":
		v = []any{}
	}
	raw, _ := json.Marshal(v)
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw)), Request: req}, nil
}

// isPRPath is /repos/o/gem/pulls/<n>.
func isPRPath(p string) bool {
	n, ok := strings.CutPrefix(p, "/repos/o/gem/pulls/")
	if !ok || n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (f *ghFake) did(method, suffix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.Method == method && strings.HasSuffix(r.Path, suffix) {
			return true
		}
	}
	return false
}

func (f *ghFake) bodyOf(method, path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			return r.Body
		}
	}
	return ""
}

type token struct{}

func (token) Token(context.Context) (string, error) { return "tok", nil }

type prGit struct{ head string }

func (g prGit) BranchExists(context.Context, config.Project, string) (bool, error) { return true, nil }
func (g prGit) Head(context.Context, config.Project, string) (string, error)       { return g.head, nil }
func (g prGit) Push(context.Context, config.Project, string, string) error         { return nil }

type holds struct{}

func (holds) StillHoldsOnMain(context.Context, gate.HoldIn) (bool, string, error) {
	return true, "", nil
}

func withFlow(h *harness, gh *ghFake) Deps {
	d := h.deps()
	d.GitHub = &prflow.Flow{
		GitHub:  &github.Client{BaseURL: "https://api.github.test", HTTP: gh, Auth: token{}},
		Git:     prGit{head: "abc"},
		Checks:  holds{},
		Board:   h.board,
		LogsDir: h.paths.Logs,
		Clock:   h.clock,
		Log:     h.log,
	}
	return d
}

// a PR sent back from pr_open to ready is put back to draft by the
// next tick, whatever its column now.
func TestASendBackDraftsThePR(t *testing.T) {
	h := newHarness(t)
	h.config(withGitHub)
	id := h.file("p", "Fix", "ready", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		t.PR = ticket.Some(ticket.PR{Number: 5, URL: "u", Draft: ticket.Some(false), HoldRequested: ticket.Some(stamp(h.clock.Now()))})
	})
	gh := &ghFake{pr: map[string]any{"number": 5, "state": "open", "draft": false, "node_id": "PR_5", "head": map[string]any{"ref": "bot/x", "sha": "abc"}}}
	if err := Tick(context.Background(), withFlow(h, gh)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gh.bodyOf("POST", "/graphql"), "convertPullRequestToDraft") {
		t.Fatalf("no draft conversion: %+v", gh.reqs)
	}
	got := h.load(id)
	if !got.PR.V.Draft.V || got.PR.V.HoldRequested.Present() {
		t.Fatalf("pr %+v", got.PR.V)
	}
	if len(h.local.started) != 1 {
		t.Fatal("the rework did not start")
	}
}

func mergeable(h *harness) ticket.ID {
	return h.file("p", "Fix", "pr_open", func(t *ticket.Ticket) {
		t.Branch = ticket.Some("bot/x")
		t.Gate = ticket.Some(passedGate())
		t.PR = ticket.Some(ticket.PR{Number: 5, URL: "u", Draft: ticket.Some(false), MergeRequested: ticket.Some(stamp(h.clock.Now()))})
	})
}

func openPR() map[string]any {
	return map[string]any{"number": 5, "state": "open", "draft": false, "merged": false, "node_id": "PR_5",
		"mergeable_state": "clean", "head": map[string]any{"ref": "bot/x", "sha": "abc"}}
}

// a merge asked for with too little of the tick left waits for
// the next tick and keeps its intent; with time left it merges.
func TestADeferredMergeKeepsItsIntent(t *testing.T) {
	h := newHarness(t)
	h.config(withGitHub)
	id := mergeable(h)
	gh := &ghFake{pr: openPR()}
	d := withFlow(h, gh)
	d.Deadline = 10 * time.Minute
	if err := Tick(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if gh.did("PUT", "/merge") {
		t.Fatal("merged with too little time left")
	}
	if got := h.load(id); !got.PR.V.MergeRequested.Present() || got.Column != "pr_open" {
		t.Fatalf("pr %+v column %s", got.PR.V, got.Column)
	}
	if err := Tick(context.Background(), withFlow(h, gh)); err != nil {
		t.Fatal(err)
	}
	got := h.load(id)
	if !gh.did("PUT", "/merge") || got.Column != "done" || got.PR.V.MergeRequested.Present() {
		t.Fatalf("column %s pr %+v reqs %+v", got.Column, got.PR.V, gh.reqs)
	}
}
