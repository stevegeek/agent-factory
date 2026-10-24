package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const prJSON = `{"number": 12, "html_url": "https://github.com/o/r/pull/12", "state": "open", "draft": true,
 "merged": false, "merged_by": null, "mergeable_state": "dirty", "node_id": "PR_node",
 "head": {"ref": "bot/x", "sha": "abc1234def"}, "extra": [1]}`

func TestPRDecodesWhatTheFlowReads(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/pulls/12", 200, prJSON)
	pr, err := f.client().PR(context.Background(), "o/r", 12)
	if err != nil {
		t.Fatal(err)
	}
	want := PR{Number: 12, HTMLURL: "https://github.com/o/r/pull/12", State: "open", Draft: true, HasDraft: true,
		MergeableState: "dirty", NodeID: "PR_node", HeadRef: "bot/x", HeadSHA: "abc1234def"}
	got := *pr
	got.Raw = want.Raw
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PR = %+v\nwant %+v", got, want)
	}
	if pr.Raw.Get("extra").Kind().String() != "array" {
		t.Errorf("Raw lost the unknown key: %v", pr.Raw)
	}
	r := f.last()
	checkAPIHeaders(t, r)
	if r.Method != "GET" || r.URI != "/repos/o/r/pulls/12" {
		t.Errorf("request = %s %s", r.Method, r.URI)
	}
}

func TestPRDecodingIsLenient(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/pulls/1", 200, `{"merged": true, "merged_by": {"login": "someone"}, "head": "odd", "draft": "yes"}`)
	f.on("GET", "/repos/o/r/pulls/2", 200, `[]`)
	pr, err := f.client().PR(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.MergedBy != "someone" || pr.HeadSHA != "" || !pr.Draft || !pr.HasDraft {
		t.Errorf("PR = %+v", pr)
	}
	if _, err := f.client().PR(context.Background(), "o/r", 2); err == nil {
		t.Error("a list for a PR was accepted")
	}
}

func TestCreatePRPostsPythonsFieldsInPythonsOrder(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/repos/o/r/pulls", 201, prJSON)
	pr, err := f.client().CreatePR(context.Background(), "o/r", NewPR{Title: "Tidy README", Body: "Fixes #7\n", Head: "bot/x", Base: "main", Draft: true})
	if err != nil || pr.Number != 12 {
		t.Fatalf("CreatePR = %+v, %v", pr, err)
	}
	if got, want := f.last().Body, `{"title": "Tidy README", "body": "Fixes #7\n", "head": "bot/x", "base": "main", "draft": true}`; got != want {
		t.Errorf("body = %s\nwant   %s", got, want)
	}
}

func TestCreatePRKeepsThe422ForTheCaller(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/repos/o/r/pulls", 422, `{"message": "Validation Failed"}`)
	_, err := f.client().CreatePR(context.Background(), "o/r", NewPR{Title: "t", Head: "bot/x", Base: "main"})
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 422 {
		t.Errorf("err = %v, want the 422 (open_pr then looks for the open PR)", err)
	}
}

func TestFindOpenPRAsksByOwnerAndBranch(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/pulls?state=open&head=o:bot/x", 200, `[`+prJSON+`, {"number": 13}]`)
	f.on("GET", "/repos/o/r/pulls?state=open&head=o:bot/y", 200, `[]`)
	c := f.client()
	pr, err := c.FindOpenPR(context.Background(), "o/r", "o", "bot/x")
	if err != nil || pr == nil || pr.Number != 12 {
		t.Fatalf("FindOpenPR = %+v, %v", pr, err)
	}
	pr, err = c.FindOpenPR(context.Background(), "o/r", "o", "bot/y")
	if err != nil || pr != nil {
		t.Errorf("no PR: %+v, %v", pr, err)
	}
}

func TestUpdatePRPatchesTitleAndBody(t *testing.T) {
	f := newFake(t)
	f.on("PATCH", "/repos/o/r/pulls/12", 200, prJSON)
	if _, err := f.client().UpdatePR(context.Background(), "o/r", 12, "T", "B"); err != nil {
		t.Fatal(err)
	}
	if got := f.last().Body; got != `{"title": "T", "body": "B"}` {
		t.Errorf("body = %s", got)
	}
}

func TestRequestReviewersCommentDeleteRefAndMerge(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/repos/o/r/pulls/12/requested_reviewers", 201, `{}`)
	f.on("POST", "/repos/o/r/issues/12/comments", 201, `{"id": 1}`)
	f.on("DELETE", "/repos/o/r/git/refs/heads/bot/x", 204, ``)
	f.on("PUT", "/repos/o/r/pulls/12/merge", 200, `{"sha": "m", "merged": true, "message": "Pull Request successfully merged"}`)
	f.on("PUT", "/repos/o/r/pulls/13/merge", 200, `{"merged": false, "message": "nope"}`)
	c := f.client()
	ctx := context.Background()
	if err := c.RequestReviewers(ctx, "o/r", 12, []string{"alice", "bob"}); err != nil {
		t.Fatal(err)
	}
	if got := f.last().Body; got != `{"reviewers": ["alice", "bob"]}` {
		t.Errorf("reviewers body = %s", got)
	}
	if err := c.Comment(ctx, "o/r", 12, "Updated after your review.\n\nDone."); err != nil {
		t.Fatal(err)
	}
	if got := f.last().Body; got != `{"body": "Updated after your review.\n\nDone."}` {
		t.Errorf("comment body = %s", got)
	}
	if err := c.DeleteRef(ctx, "o/r", "bot/x"); err != nil {
		t.Fatal(err)
	}
	if r := f.last(); r.Method != "DELETE" || r.Body != "" {
		t.Errorf("delete = %+v", r)
	}
	res, err := c.Merge(ctx, "o/r", 12, "abc")
	if err != nil || !res.Merged || res.Message != "Pull Request successfully merged" {
		t.Errorf("Merge = %+v, %v", res, err)
	}
	if got := f.last().Body; got != `{"sha": "abc", "merge_method": "merge"}` {
		t.Errorf("merge body = %s", got)
	}
	res, err = c.Merge(ctx, "o/r", 13, "abc")
	if err != nil || res.Merged || res.Message != "nope" {
		t.Errorf("unmerged: %+v, %v (land_pr decides what merged false means)", res, err)
	}
	for _, r := range f.requests() {
		checkAPIHeaders(t, r)
	}
}

func TestReviewsAndCommentsAskForAHundred(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/pulls/12/reviews?per_page=100", 200,
		`[{"id": 5, "state": "CHANGES_REQUESTED", "user": {"login": "Alice"}, "commit_id": "abc", "body": "Fix it."},
		  {"id": 6, "state": "APPROVED", "user": null, "commit_id": null, "body": null}]`)
	f.on("GET", "/repos/o/r/pulls/12/comments?per_page=100", 200,
		`[{"pull_request_review_id": 5, "path": "lib/a.rb", "line": null, "original_line": 3, "body": " typo "},
		  {"pull_request_review_id": 9, "path": "b", "line": 4, "body": "x"}]`)
	c := f.client()
	rs, err := c.Reviews(context.Background(), "o/r", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].ID != 5 || rs[0].State != "CHANGES_REQUESTED" || rs[0].Login != "Alice" ||
		rs[0].CommitID != "abc" || rs[0].Body != "Fix it." || rs[1].Login != "" || rs[1].CommitID != "" {
		t.Errorf("reviews = %+v", rs)
	}
	cs, err := c.ReviewComments(context.Background(), "o/r", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].ReviewID != 5 || cs[0].Path != "lib/a.rb" || cs[0].Line != 0 || cs[0].OriginalLine != 3 ||
		cs[0].Body != " typo " || cs[1].Line != 4 {
		t.Errorf("comments = %+v", cs)
	}
	if !cs[0].Raw.Get("line").IsNull() {
		t.Error("Raw does not keep the null line")
	}
}

func TestListsMustBeLists(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/pulls/12/reviews?per_page=100", 200, `{"message": "odd"}`)
	f.on("GET", "/repos/o/r/pulls/12/comments?per_page=100", 200, `[1, 2]`)
	c := f.client()
	if _, err := c.Reviews(context.Background(), "o/r", 12); err == nil {
		t.Error("an object for reviews was accepted")
	}
	if _, err := c.ReviewComments(context.Background(), "o/r", 12); err == nil {
		t.Error("numbers for comments were accepted")
	}
}

func TestSetDraftUsesGraphQLWithTheNodeID(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/graphql", 200, `{"data": {"convertPullRequestToDraft": {"pullRequest": {"isDraft": true}}}}`)
	c := f.client()
	if err := c.SetDraft(context.Background(), "PR_node", true); err != nil {
		t.Fatal(err)
	}
	want := `{"query": "mutation($id: ID!) { convertPullRequestToDraft(input: {pullRequestId: $id}) { pullRequest { isDraft } } }", "variables": {"id": "PR_node"}}`
	if got := f.last().Body; got != want {
		t.Errorf("body = %s\nwant   %s", got, want)
	}
	checkAPIHeaders(t, f.last())
	if err := c.SetDraft(context.Background(), "PR_node", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.last().Body, `"mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }"`) {
		t.Errorf("ready body = %s", f.last().Body)
	}
}

func TestSetDraftFailsOnGraphQLErrorsAsPythonSaysIt(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/graphql", 200, `{"data": null, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a node with the global id of 'x'"}]}`)
	err := f.client().SetDraft(context.Background(), "x", true)
	want := `github: convertPullRequestToDraft: [{'type': 'NOT_FOUND', 'message': "Could not resolve to a node with the global id of 'x'"}]`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant  %s", err, want)
	}
	var ge *GraphQLError
	if !errors.As(err, &ge) || IsTransient(err) {
		t.Errorf("err %v is not a refusal *GraphQLError", err)
	}
}

func TestSetDraftClipsTheErrorsTo200(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/graphql", 200, `{"errors": [{"message": "`+strings.Repeat("é", 300)+`"}]}`)
	err := f.client().SetDraft(context.Background(), "x", false)
	if err == nil {
		t.Fatal("no error")
	}
	tail, _ := strings.CutPrefix(err.Error(), "github: markPullRequestReadyForReview: ")
	if n := len([]rune(tail)); n != 200 {
		t.Errorf("clipped to %d code points, want 200 (str(...)[:200])", n)
	}
}

func TestGraphQLWithEmptyErrorsIsFine(t *testing.T) {
	f := newFake(t)
	f.on("POST", "/graphql", 200, `{"data": {"x": 1}, "errors": []}`)
	v, err := f.client().GraphQL(context.Background(), "query { x }", obj())
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := v.Get("data").Get("x").Int64(); n != 1 {
		t.Errorf("GraphQL = %v", v)
	}
	if got := f.last().Body; got != `{"query": "query { x }", "variables": {}}` {
		t.Errorf("body = %s", got)
	}
}

func TestReposListsTheInstallationsRepositories(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/installation/repositories", 200,
		`{"total_count": 2, "repositories": [{"full_name": "o/a", "private": true}, {"full_name": "o/b", "private": false}]}`)
	rs, err := f.client().Repos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].FullName != "o/a" || !rs[0].Private || rs[1].FullName != "o/b" || rs[1].Private {
		t.Errorf("repos = %+v", rs)
	}
	checkAPIHeaders(t, f.last())
}

func TestCanReach(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r", 200, `{"full_name": "o/r"}`)
	f.on("GET", "/repos/o/gone", 404, `{"message": "Not Found"}`)
	f.on("GET", "/repos/o/down", 500, `{}`)
	c := f.client()
	if ok, err := c.CanReach(context.Background(), "o/r"); !ok || err != nil {
		t.Errorf("200: %v, %v", ok, err)
	}
	checkAPIHeaders(t, f.last())
	if ok, err := c.CanReach(context.Background(), "o/gone"); ok || err != nil {
		t.Errorf("404: %v, %v (want false and no error)", ok, err)
	}
	var he *HTTPError
	if ok, err := c.CanReach(context.Background(), "o/down"); ok || !errors.As(err, &he) || he.Code != 500 {
		t.Errorf("500: %v, %v (want the error)", ok, err)
	}
}

func TestBranchRules(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/rules/branches/main", 200,
		`[{"type": "deletion"}, {"type": "pull_request", "parameters": {}}, {"type": "non_fast_forward"}]`)
	f.on("GET", "/repos/o/r/rules/branches/none", 200, `[]`)
	f.on("GET", "/repos/o/r/rules/branches/locked", 403, `{"message": "Upgrade to GitHub Pro"}`)
	c := f.client()
	got, err := c.BranchRules(context.Background(), "o/r", "main")
	if err != nil || !reflect.DeepEqual(got, []string{"deletion", "pull_request", "non_fast_forward"}) {
		t.Errorf("BranchRules = %q, %v", got, err)
	}
	if r := f.last(); r.Method != "GET" || r.URI != "/repos/o/r/rules/branches/main" {
		t.Errorf("request = %s %s", r.Method, r.URI)
	}
	if got, err := c.BranchRules(context.Background(), "o/r", "none"); err != nil || len(got) != 0 {
		t.Errorf("no rules = %q, %v", got, err)
	}
	var he *HTTPError
	if _, err := c.BranchRules(context.Background(), "o/r", "locked"); !errors.As(err, &he) || he.Code != 403 {
		t.Errorf("403: %v, want the *HTTPError", err)
	}
}
