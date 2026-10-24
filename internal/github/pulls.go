package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// PR is a pull request as the PR flow reads it.
type PR struct {
	Number  int64
	HTMLURL string
	State   string
	// Draft is bool(pr.get("draft")); HasDraft whether the key is there at
	// all (open_pr compares pr.get("draft") with the wanted state, and a
	// missing key differs from both).
	Draft, HasDraft bool
	// Merged is bool(pr.get("merged")); MergedBy is merged_by.login.
	Merged         bool
	MergedBy       string
	MergeableState string
	NodeID         string
	HeadRef        string
	HeadSHA        string
	// Raw is the whole answer.
	Raw jsonx.Value
}

func decodePR(what string, v jsonx.Value) (*PR, error) {
	if err := object(what, v); err != nil {
		return nil, err
	}
	o, _ := v.Object()
	draft, has := o.Lookup("draft")
	head := v.Get("head")
	return &PR{
		Number:         num(v, "number"),
		HTMLURL:        str(v, "html_url"),
		State:          str(v, "state"),
		Draft:          jsonx.Truthy(draft),
		HasDraft:       has,
		Merged:         jsonx.Truthy(v.Get("merged")),
		MergedBy:       str(v.Get("merged_by"), "login"),
		MergeableState: str(v, "mergeable_state"),
		NodeID:         str(v, "node_id"),
		HeadRef:        str(head, "ref"),
		HeadSHA:        str(head, "sha"),
		Raw:            v,
	}, nil
}

func n(i int64) string { return strconv.FormatInt(i, 10) }

func (c *Client) prCall(ctx context.Context, method, path string, body jsonx.Value) (*PR, error) {
	resp, err := c.Do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return decodePR(method+" "+path, resp.Value)
}

// PR is GET /repos/<repo>/pulls/<n> (open_pr_of, check_pr, merge_pr,
// hold_pr).
func (c *Client) PR(ctx context.Context, repo string, number int64) (*PR, error) {
	return c.prCall(ctx, "GET", "/repos/"+repo+"/pulls/"+n(number), jsonx.Null())
}

// NewPR is the body of a new pull request.
type NewPR struct {
	Title, Body, Head, Base string
	Draft                   bool
}

// CreatePR is open_pr's POST /repos/<repo>/pulls with {title, body, head,
// base, draft} in that order. A 422 (the PR exists) comes back as the
// *HTTPError, for the caller to look up with FindOpenPR.
func (c *Client) CreatePR(ctx context.Context, repo string, in NewPR) (*PR, error) {
	body := jsonx.ObjectOf(
		jsonx.Member{Key: "title", Value: jsonx.String(in.Title)},
		jsonx.Member{Key: "body", Value: jsonx.String(in.Body)},
		jsonx.Member{Key: "head", Value: jsonx.String(in.Head)},
		jsonx.Member{Key: "base", Value: jsonx.String(in.Base)},
		jsonx.Member{Key: "draft", Value: jsonx.Bool(in.Draft)})
	return c.prCall(ctx, "POST", "/repos/"+repo+"/pulls", body)
}

// FindOpenPR is GET /repos/<repo>/pulls?state=open&head=<owner>:<branch>
// (the query unescaped, as Python sent it), giving the first PR or nil.
func (c *Client) FindOpenPR(ctx context.Context, repo, owner, branch string) (*PR, error) {
	path := "/repos/" + repo + "/pulls?state=open&head=" + owner + ":" + branch
	resp, err := c.Do(ctx, "GET", path, jsonx.Null())
	if err != nil {
		return nil, err
	}
	items, ok := resp.Value.Array()
	if !ok {
		return nil, fmt.Errorf("github: GET %s: expected a list, got %s", path, resp.Value.Kind())
	}
	if len(items) == 0 {
		return nil, nil
	}
	return decodePR("GET "+path, items[0])
}

// UpdatePR is a rework's PATCH /repos/<repo>/pulls/<n> with {title, body}.
func (c *Client) UpdatePR(ctx context.Context, repo string, number int64, title, body string) (*PR, error) {
	b := jsonx.ObjectOf(
		jsonx.Member{Key: "title", Value: jsonx.String(title)},
		jsonx.Member{Key: "body", Value: jsonx.String(body)})
	return c.prCall(ctx, "PATCH", "/repos/"+repo+"/pulls/"+n(number), b)
}

// RequestReviewers is request_review's POST
// /repos/<repo>/pulls/<n>/requested_reviewers with {"reviewers": [...]}.
func (c *Client) RequestReviewers(ctx context.Context, repo string, number int64, reviewers []string) error {
	vs := make([]jsonx.Value, len(reviewers))
	for i, r := range reviewers {
		vs[i] = jsonx.String(r)
	}
	_, err := c.Do(ctx, "POST", "/repos/"+repo+"/pulls/"+n(number)+"/requested_reviewers",
		jsonx.ObjectOf(jsonx.Member{Key: "reviewers", Value: jsonx.ArrayOf(vs...)}))
	return err
}

// Review is one PR review.
type Review struct {
	ID       int64
	State    string
	Login    string // user.login
	CommitID string
	Body     string
	Raw      jsonx.Value
}

// Reviews is GET /repos/<repo>/pulls/<n>/reviews?per_page=100: one page,
// as Python asks.
func (c *Client) Reviews(ctx context.Context, repo string, number int64) ([]Review, error) {
	path := "/repos/" + repo + "/pulls/" + n(number) + "/reviews?per_page=100"
	items, err := c.list(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]Review, len(items))
	for i, v := range items {
		out[i] = Review{ID: num(v, "id"), State: str(v, "state"), Login: str(v.Get("user"), "login"),
			CommitID: str(v, "commit_id"), Body: str(v, "body"), Raw: v}
	}
	return out, nil
}

// ReviewComment is one line comment on a PR.
type ReviewComment struct {
	ReviewID     int64 // pull_request_review_id
	Path         string
	Line         int64 // 0 when null
	OriginalLine int64
	Body         string
	Raw          jsonx.Value
}

// ReviewComments is GET /repos/<repo>/pulls/<n>/comments?per_page=100 (the
// list that carries file line numbers), one page.
func (c *Client) ReviewComments(ctx context.Context, repo string, number int64) ([]ReviewComment, error) {
	path := "/repos/" + repo + "/pulls/" + n(number) + "/comments?per_page=100"
	items, err := c.list(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]ReviewComment, len(items))
	for i, v := range items {
		out[i] = ReviewComment{ReviewID: num(v, "pull_request_review_id"), Path: str(v, "path"),
			Line: num(v, "line"), OriginalLine: num(v, "original_line"), Body: str(v, "body"), Raw: v}
	}
	return out, nil
}

func (c *Client) list(ctx context.Context, path string) ([]jsonx.Value, error) {
	resp, err := c.Do(ctx, "GET", path, jsonx.Null())
	if err != nil {
		return nil, err
	}
	return objects("GET "+path, resp.Value)
}

// MergeResult is the answer to a merge. SHA is the merge commit ("" when
// the answer names none).
type MergeResult struct {
	Merged  bool
	Message string
	SHA     string
	Raw     jsonx.Value
}

// Merge is land_pr's PUT /repos/<repo>/pulls/<n>/merge with {"sha": sha,
// "merge_method": "merge"}: GitHub merges exactly that commit or refuses
// (an *HTTPError: 405 not mergeable, 409 head moved). merged false is a
// result, not an error; land_pr says what it means.
func (c *Client) Merge(ctx context.Context, repo string, number int64, sha string) (*MergeResult, error) {
	path := "/repos/" + repo + "/pulls/" + n(number) + "/merge"
	resp, err := c.Do(ctx, "PUT", path, jsonx.ObjectOf(
		jsonx.Member{Key: "sha", Value: jsonx.String(sha)},
		jsonx.Member{Key: "merge_method", Value: jsonx.String("merge")}))
	if err != nil {
		return nil, err
	}
	if err := object("PUT "+path, resp.Value); err != nil {
		return nil, err
	}
	return &MergeResult{Merged: jsonx.Truthy(resp.Value.Get("merged")), Message: str(resp.Value, "message"), SHA: str(resp.Value, "sha"), Raw: resp.Value}, nil
}

// DeleteRef is DELETE /repos/<repo>/git/refs/heads/<branch>.
func (c *Client) DeleteRef(ctx context.Context, repo, branch string) error {
	_, err := c.Do(ctx, "DELETE", "/repos/"+repo+"/git/refs/heads/"+branch, jsonx.Null())
	return err
}

// BranchHead is GET /repos/<repo>/git/ref/heads/<branch> (the branch
// unescaped, as DeleteRef sends it): the commit GitHub has at the branch's
// head. Anything but a commit with a full object id is an error.
func (c *Client) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	path := "/repos/" + repo + "/git/ref/heads/" + branch
	resp, err := c.Do(ctx, "GET", path, jsonx.Null())
	if err != nil {
		return "", err
	}
	obj := resp.Value.Get("object")
	sha := str(obj, "sha")
	if str(obj, "type") != "commit" || !fullSHA(sha) {
		return "", fmt.Errorf("github: GET %s: no commit at the head", path)
	}
	return sha, nil
}

// fullSHA reports a whole SHA-1 or SHA-256 object id in lower-case hex.
func fullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Comment is POST /repos/<repo>/issues/<n>/comments with {"body": body}.
func (c *Client) Comment(ctx context.Context, repo string, number int64, body string) error {
	_, err := c.Do(ctx, "POST", "/repos/"+repo+"/issues/"+n(number)+"/comments",
		jsonx.ObjectOf(jsonx.Member{Key: "body", Value: jsonx.String(body)}))
	return err
}

// GraphQL POSTs /graphql with {"query": query, "variables": variables} and
// returns the answer. A non-empty "errors" is a *GraphQLError (returned with
// the answer).
func (c *Client) GraphQL(ctx context.Context, query string, variables jsonx.Value) (jsonx.Value, error) {
	resp, err := c.Do(ctx, "POST", "/graphql", jsonx.ObjectOf(
		jsonx.Member{Key: "query", Value: jsonx.String(query)},
		jsonx.Member{Key: "variables", Value: variables}))
	if err != nil {
		return jsonx.Null(), err
	}
	if err := object("POST /graphql", resp.Value); err != nil {
		return jsonx.Null(), err
	}
	if errs := resp.Value.Get("errors"); jsonx.Truthy(errs) {
		return resp.Value, &GraphQLError{Errors: errs}
	}
	return resp.Value, nil
}

// SetDraft is set_pr_draft: REST cannot change a PR's draft state,
// so GraphQL's convertPullRequestToDraft or markPullRequestReadyForReview,
// with the PR's node id. GraphQL errors fail with Python's message.
func (c *Client) SetDraft(ctx context.Context, nodeID string, draft bool) error {
	op := "markPullRequestReadyForReview"
	if draft {
		op = "convertPullRequestToDraft"
	}
	query := "mutation($id: ID!) { " + op + "(input: {pullRequestId: $id}) { pullRequest { isDraft } } }"
	_, err := c.GraphQL(ctx, query, jsonx.ObjectOf(jsonx.Member{Key: "id", Value: jsonx.String(nodeID)}))
	var ge *GraphQLError
	if errors.As(err, &ge) {
		ge.Op = op
	}
	return err
}

// Repo is one repository the installation reaches.
type Repo struct {
	FullName string
	Private  bool
	Raw      jsonx.Value
}

// Repos is cmd_github's GET /installation/repositories (one page, GitHub's
// default size, as Python asks).
func (c *Client) Repos(ctx context.Context) ([]Repo, error) {
	resp, err := c.Do(ctx, "GET", "/installation/repositories", jsonx.Null())
	if err != nil {
		return nil, err
	}
	if err := object("GET /installation/repositories", resp.Value); err != nil {
		return nil, err
	}
	rv := resp.Value.Get("repositories")
	if rv.IsNull() {
		return nil, nil
	}
	items, err := objects("GET /installation/repositories", rv)
	if err != nil {
		return nil, err
	}
	out := make([]Repo, len(items))
	for i, v := range items {
		out[i] = Repo{FullName: str(v, "full_name"), Private: jsonx.Truthy(v.Get("private")), Raw: v}
	}
	return out, nil
}

// CanReach is GET /repos/<repo> with the installation token: true on a
// 2xx, false on a 404 (the App is not granted the repository, or it does
// not exist), and any other failure as the error. It asks for the one
// repository rather than reading Repos, which sees only the first page.
func (c *Client) CanReach(ctx context.Context, repo string) (bool, error) {
	_, err := c.Do(ctx, "GET", "/repos/"+repo, jsonx.Null())
	var he *HTTPError
	if errors.As(err, &he) && he.Code == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// BranchRules is GET /repos/<repo>/rules/branches/<branch> (the branch
// unescaped, as DeleteRef sends it): the "type" of each rule that applies
// to the branch, in GitHub's order. A refusal (403, 404) comes back as the
// *HTTPError for the caller to judge. Only the first page is read (30
// rules): enough for onboard's pull_request check, as a branch has few.
func (c *Client) BranchRules(ctx context.Context, repo, branch string) ([]string, error) {
	items, err := c.list(ctx, "/repos/"+repo+"/rules/branches/"+branch)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(items))
	for i, v := range items {
		out[i] = str(v, "type")
	}
	return out, nil
}
