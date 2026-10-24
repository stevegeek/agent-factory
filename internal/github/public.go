package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// Issue is one open issue, as the sweep files it.
type Issue struct {
	Number  int64
	Title   string
	Body    string // "" when null
	HTMLURL string
	PR      bool // a pull request (OpenItems only; OpenIssues leaves them out)
	Raw     jsonx.Value
}

// OpenIssues is fetch_open_issues (F:2335): an unauthenticated GET
// /repos/<repo>/issues?state=open&per_page=50 with only Accept and
// User-Agent, keeping the items with no "pull_request" key. One page of 50,
// as Python asks (§13.2 quirk 8).
func (c *Client) OpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	items, err := c.OpenItems(ctx, repo)
	var out []Issue
	for _, it := range items {
		if !it.PR {
			out = append(out, it)
		}
	}
	return out, err
}

// OpenItems is the same page with the pull requests kept (PR set): what a
// verifier checks a check's findings against, read without credentials.
func (c *Client) OpenItems(ctx context.Context, repo string) ([]Issue, error) {
	path := "/repos/" + repo + "/issues?state=open&per_page=50"
	h := http.Header{}
	h.Set("Accept", Accept)
	h.Set("User-Agent", UserAgent)
	resp, err := c.get(ctx, c.base(), path, h)
	if err != nil {
		return nil, err
	}
	items, err := objects("GET "+path, resp.Value)
	if err != nil {
		return nil, err
	}
	var out []Issue
	for _, v := range items {
		o, _ := v.Object()
		_, pr := o.Lookup("pull_request")
		out = append(out, Issue{Number: num(v, "number"), Title: str(v, "title"), Body: str(v, "body"),
			HTMLURL: str(v, "html_url"), PR: pr, Raw: v})
	}
	return out, nil
}

// LatestRelease is latest_github_release (F:2843): the tag_name of
// GET /repos/<repo>/releases/latest (unauthenticated, Accept and
// User-Agent), with its leading "v"s stripped (str.lstrip("v")).
func (c *Client) LatestRelease(ctx context.Context, repo string) (string, error) {
	path := "/repos/" + repo + "/releases/latest"
	h := http.Header{}
	h.Set("User-Agent", UserAgent)
	h.Set("Accept", Accept)
	resp, err := c.get(ctx, c.base(), path, h)
	if err != nil {
		return "", err
	}
	tag, ok := resp.Value.Get("tag_name").Str()
	if !ok {
		return "", fmt.Errorf("github: GET %s: no tag_name", path)
	}
	return strings.TrimLeft(tag, "v"), nil
}

// NPMLatest is latest_npm_version (F:2836): the "version" of
// GET <registry>/<package>/latest, sent with only the User-Agent.
func (c *Client) NPMLatest(ctx context.Context, pkg string) (string, error) {
	path := "/" + pkg + "/latest"
	h := http.Header{}
	h.Set("User-Agent", UserAgent)
	resp, err := c.get(ctx, c.npm(), path, h)
	if err != nil {
		return "", err
	}
	v, ok := resp.Value.Get("version").Str()
	if !ok {
		return "", fmt.Errorf("npm: GET %s: no version", path)
	}
	return v, nil
}
