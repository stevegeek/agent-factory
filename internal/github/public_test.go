package github

import (
	"context"
	"testing"
)

func TestOpenIssuesIsUnauthenticatedAndDropsPullRequests(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/issues?state=open&per_page=50", 200,
		`[{"number": 7, "title": "Crash", "body": null, "html_url": "https://github.com/o/r/issues/7"},
		  {"number": 8, "title": "A PR", "pull_request": {}},
		  {"number": 9, "title": "Docs", "body": "Please.", "html_url": "u9", "pull_request": null}]`)
	c := f.client()
	is, err := c.OpenIssues(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	// "pull_request" in i: present at all, even null, means a PR (Python's test).
	if len(is) != 1 || is[0].Number != 7 || is[0].Title != "Crash" || is[0].Body != "" || is[0].HTMLURL != "https://github.com/o/r/issues/7" {
		t.Errorf("issues = %+v", is)
	}
	r := f.last()
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-Github-Api-Version") != "" {
		t.Errorf("sweep sent auth %q / version %q; Python sends neither", r.Header.Get("Authorization"), r.Header.Get("X-Github-Api-Version"))
	}
	if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "agent-factory" {
		t.Errorf("headers = %v", r.Header)
	}
}

// A verifier checks findings against the open issues and PRs both.
func TestOpenItemsKeepsPullRequests(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/issues?state=open&per_page=50", 200,
		`[{"number": 7, "title": "Crash", "body": null, "html_url": "u7"},
		  {"number": 8, "title": "A PR", "pull_request": {}, "html_url": "u8"}]`)
	is, err := f.client().OpenItems(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(is) != 2 || is[0].PR || !is[1].PR || is[1].Number != 8 || is[1].Title != "A PR" {
		t.Errorf("items = %+v", is)
	}
	if r := f.last(); r.Header.Get("Authorization") != "" {
		t.Error("the open items are read without credentials")
	}
}

func TestOpenIssuesNeedsNoAuth(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/issues?state=open&per_page=50", 200, `[]`)
	c := f.client()
	c.Auth = nil
	if _, err := c.OpenIssues(context.Background(), "o/r"); err != nil {
		t.Errorf("OpenIssues without Auth: %v", err)
	}
}

func TestLatestReleaseStripsLeadingVs(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/crystal-lang/crystal/releases/latest", 200, `{"tag_name": "v1.21.0", "name": "x"}`)
	f.on("GET", "/repos/o/vv/releases/latest", 200, `{"tag_name": "vv2.0"}`)
	f.on("GET", "/repos/o/none/releases/latest", 200, `[]`)
	c := f.client()
	c.Auth = nil
	if v, err := c.LatestRelease(context.Background(), "crystal-lang/crystal"); err != nil || v != "1.21.0" {
		t.Errorf("LatestRelease = %q, %v", v, err)
	}
	r := f.last()
	if r.Header.Get("Authorization") != "" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "agent-factory" {
		t.Errorf("headers = %v", r.Header)
	}
	if v, _ := c.LatestRelease(context.Background(), "o/vv"); v != "2.0" {
		t.Errorf("lstrip('v') of vv2.0 = %q, want 2.0", v)
	}
	if _, err := c.LatestRelease(context.Background(), "o/none"); err == nil {
		t.Error("no tag_name was accepted")
	}
}

func TestNPMLatestAsksTheRegistry(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/npm/@anthropic-ai/claude-code/latest", 200, `{"name": "@anthropic-ai/claude-code", "version": "2.1.0"}`)
	f.on("GET", "/npm/odd/latest", 200, `{"version": 2}`)
	c := f.client()
	c.Auth = nil
	if v, err := c.NPMLatest(context.Background(), "@anthropic-ai/claude-code"); err != nil || v != "2.1.0" {
		t.Errorf("NPMLatest = %q, %v", v, err)
	}
	r := f.last()
	if r.Header.Get("User-Agent") != "agent-factory" || r.Header.Get("Accept") != "" || r.Header.Get("Authorization") != "" {
		t.Errorf("npm headers = %v; Python sends only the User-Agent", r.Header)
	}
	if _, err := c.NPMLatest(context.Background(), "odd"); err == nil {
		t.Error("a numeric version was accepted")
	}
}

// The seam tests of S0p (tests/test_seams.py GitHubSeamTest, NpmSeamTest):
// every call goes to the base it names, and the defaults are the real ones.
func TestEveryCallGoesToTheBaseItNames(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/gh/repos/o/r/issues?state=open&per_page=50", 200, `[]`)
	f.on("GET", "/gh/repos/o/r/releases/latest", 200, `{"tag_name": "v1.2.3"}`)
	f.on("POST", "/gh/app/installations/1/access_tokens", 201, `{"token": "t", "expires_at": "2026-10-18T13:00:00Z"}`)
	f.on("GET", "/reg/@scope/pkg/latest", 200, `{"version": "2.1.0"}`)
	c := &Client{BaseURL: f.srv.URL + "/gh/", NPMURL: f.srv.URL + "/reg/", HTTP: f.srv.Client()}
	ctx := context.Background()
	if _, err := c.OpenIssues(ctx, "o/r"); err != nil {
		t.Fatal(err)
	}
	if v, err := c.LatestRelease(ctx, "o/r"); err != nil || v != "1.2.3" {
		t.Fatalf("LatestRelease = %q, %v", v, err)
	}
	app := &App{ClientID: "1", InstallationID: "1", KeyPath: writeKey(t, testKey(), "RSA PRIVATE KEY"), Clock: fixedNow(), API: c}
	if _, err := app.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := c.NPMLatest(ctx, "@scope/pkg"); err != nil || v != "2.1.0" {
		t.Fatalf("NPMLatest = %q, %v", v, err)
	}
	var uris []string
	for _, r := range f.requests() {
		uris = append(uris, r.URI)
	}
	want := []string{"/gh/repos/o/r/issues?state=open&per_page=50", "/gh/repos/o/r/releases/latest",
		"/gh/app/installations/1/access_tokens", "/reg/@scope/pkg/latest"}
	if len(uris) != len(want) {
		t.Fatalf("uris = %v", uris)
	}
	for i := range want {
		if uris[i] != want[i] {
			t.Errorf("request %d = %s, want %s", i, uris[i], want[i])
		}
	}
}
