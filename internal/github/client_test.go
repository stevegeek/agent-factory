package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// checkAPIHeaders asserts the fixed headers of github_request.
func checkAPIHeaders(t *testing.T, r request) {
	t.Helper()
	for k, want := range map[string]string{
		"Accept":               "application/vnd.github+json",
		"User-Agent":           "agent-factory",
		"X-Github-Api-Version": "2022-11-28",
	} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s %s: header %s = %q, want %q", r.Method, r.URI, k, got, want)
		}
	}
}

// esc turns "~u" and "~U" into backslash escapes, so the expected JSON and
// Python text keep their escapes visible in the source.
func esc(s string) string {
	return strings.NewReplacer("~u", `\`+"u", "~U", `\`+"U").Replace(s)
}

func obj(ms ...jsonx.Member) jsonx.Value { return jsonx.ObjectOf(ms...) }
func m(k string, v jsonx.Value) jsonx.Member {
	return jsonx.Member{Key: k, Value: v}
}

func TestDoSendsTheFixedHeadersTheTokenAndAPythonJSONBody(t *testing.T) {
	f := newFake(t)
	f.on("PATCH", "/repos/o/r/pulls/3", 200, `{"number": 3}`)
	body := obj(m("title", jsonx.String("Café \U0001F600 <b>&")), m("body", jsonx.String("x")), m("draft", jsonx.Bool(false)))
	resp, err := f.client().Do(context.Background(), "PATCH", "/repos/o/r/pulls/3", body)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := resp.Value.Get("number").Int64(); n != 3 || resp.Code != 200 {
		t.Errorf("response = %d %v", resp.Code, resp.Value)
	}
	r := f.last()
	checkAPIHeaders(t, r)
	if got := r.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	// json.dumps(body): ", " and ": ", ensure_ascii with surrogate pairs, no HTML escaping.
	want := esc(`{"title": "Caf~u00e9 ~ud83d~ude00 <b>&", "body": "x", "draft": false}`)
	if r.Body != want {
		t.Errorf("body = %s\nwant   %s", r.Body, want)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestDoWithoutABodySendsNone(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/repos/o/r/pulls/3", 200, `{}`)
	if _, err := f.client().Do(context.Background(), "GET", "/repos/o/r/pulls/3", jsonx.Null()); err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Body != "" || r.Header.Get("Content-Type") != "" {
		t.Errorf("GET sent body %q, Content-Type %q", r.Body, r.Header.Get("Content-Type"))
	}
}

func TestAnEmptyAnswerIsAnEmptyObject(t *testing.T) {
	f := newFake(t)
	f.on("DELETE", "/repos/o/r/git/refs/heads/bot/x", 204, "")
	resp, err := f.client().Do(context.Background(), "DELETE", "/repos/o/r/git/refs/heads/bot/x", jsonx.Null())
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := resp.Value.Object(); !ok || len(o) != 0 {
		t.Errorf("empty answer = %v, want {}", resp.Value)
	}
}

func TestTheBaseURLLosesItsTrailingSlashes(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/gh/repos/o/r", 200, `{}`)
	c := f.client()
	c.BaseURL = f.srv.URL + "/gh//"
	if _, err := c.Do(context.Background(), "GET", "/repos/o/r", jsonx.Null()); err != nil {
		t.Fatal(err)
	}
	if got := f.last().URI; got != "/gh/repos/o/r" {
		t.Errorf("URI = %q", got)
	}
}

func TestDefaultsAreGitHubAndNPM(t *testing.T) {
	var c Client
	if got := c.base(); got != "https://api.github.com" {
		t.Errorf("base = %q", got)
	}
	if got := c.npm(); got != "https://registry.npmjs.org" {
		t.Errorf("npm = %q", got)
	}
}

func TestDoNeedsAnAuth(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:9"}
	if _, err := c.Do(context.Background(), "GET", "/x", jsonx.Null()); err == nil {
		t.Error("Do without Auth sent a request")
	}
}

func TestBadPathsAreRefusedBeforeAnyRequest(t *testing.T) {
	f := newFake(t)
	for _, p := range []string{"repos/o/r", "//evil.example/x", "/x\r\nHost: y", "/x y", ""} {
		if _, err := f.client().Do(context.Background(), "GET", p, jsonx.Null()); err == nil {
			t.Errorf("path %q was accepted", p)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

func TestHTTPErrorsCarryTheCodeAndGitHubsWords(t *testing.T) {
	f := newFake(t)
	f.on("PUT", "/repos/o/r/pulls/3/merge", 405, `{"message": "Required status check \"ci\" is expected.", "documentation_url": "x"}`)
	f.on("GET", "/repos/o/r/pulls/4", 502, `<html>bad gateway</html>`)
	f.on("GET", "/repos/o/r/pulls/5", 422, `{"message": ""}`)
	c := f.client()
	ctx := context.Background()

	_, err := c.Do(ctx, "PUT", "/repos/o/r/pulls/3/merge", jsonx.Null())
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *HTTPError", err)
	}
	if he.Code != 405 || he.Message != `Required status check "ci" is expected.` {
		t.Errorf("HTTPError = %+v", he)
	}
	if got := he.Error(); got != "HTTP Error 405: Method Not Allowed" {
		t.Errorf("Error() = %q, want urllib's str(e)", got)
	}

	// Not JSON, or no message: github_error falls back to str(e).
	_, err = c.Do(ctx, "GET", "/repos/o/r/pulls/4", jsonx.Null())
	if !errors.As(err, &he) || he.Message != "HTTP Error 502: Bad Gateway" || string(he.Body) != "<html>bad gateway</html>" {
		t.Errorf("502: %+v", he)
	}
	_, err = c.Do(ctx, "GET", "/repos/o/r/pulls/5", jsonx.Null())
	if !errors.As(err, &he) || he.Message != "HTTP Error 422: Unprocessable Entity" {
		t.Errorf("422 with an empty message: %+v", he)
	}
}

func TestTransientErrorsAreThoseWorthRetrying(t *testing.T) {
	cases := []struct {
		name      string
		rep       reply
		transient bool
	}{
		{"500", reply{Code: 500, Body: `{}`}, true},
		{"502", reply{Code: 502}, true},
		{"503", reply{Code: 503}, true},
		{"504", reply{Code: 504}, true},
		{"429", reply{Code: 429}, true},
		{"403 rate limited", reply{Code: 403, Body: `{"message": "API rate limit exceeded"}`, Header: map[string]string{"X-RateLimit-Remaining": "0"}}, true},
		{"403 secondary limit", reply{Code: 403, Header: map[string]string{"Retry-After": "60"}}, true},
		{"403 forbidden", reply{Code: 403, Body: `{"message": "Resource not accessible by integration"}`}, false},
		{"401", reply{Code: 401}, false},
		{"404", reply{Code: 404}, false},
		{"405 not mergeable", reply{Code: 405}, false},
		{"409", reply{Code: 409}, false},
		{"422", reply{Code: 422}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.onReply("GET", "/x", tc.rep)
			_, err := f.client().Do(context.Background(), "GET", "/x", jsonx.Null())
			if err == nil {
				t.Fatal("no error")
			}
			if got := IsTransient(err); got != tc.transient {
				t.Errorf("IsTransient(%v) = %v, want %v", err, got, tc.transient)
			}
			if got := errors.Is(err, ErrTransient); got != tc.transient {
				t.Errorf("errors.Is(ErrTransient) = %v", got)
			}
			var he *HTTPError
			if !errors.As(err, &he) || he.Code != tc.rep.Code || err.Error() != he.Error() {
				t.Errorf("err = %v, want the bare *HTTPError (its text is Python's str(e))", err)
			}
		})
	}
}

func TestNetworkFailuresAndTimeoutsAreTransient(t *testing.T) {
	ctx := context.Background()

	// Connection refused: the fake answers only for its own host.
	c := &Client{BaseURL: "http://elsewhere.test", HTTP: newFake(t).srv.Client(), Auth: staticToken("tok")}
	_, err := c.Do(ctx, "GET", "/x", jsonx.Null())
	if err == nil || !IsTransient(err) {
		t.Errorf("refused: err = %v, transient %v", err, IsTransient(err))
	}
	if he := (*HTTPError)(nil); errors.As(err, &he) {
		t.Errorf("a network failure is not an HTTPError: %v", err)
	}

	// The per-request timeout.
	f := newFake(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return true
	}
	c = f.client()
	c.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err = c.Do(ctx, "GET", "/slow", jsonx.Null())
	if err == nil || !IsTransient(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the timeout took %v", d)
	}

	// The caller's own deadline or cancel: kept for the next tick, so transient too.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = f.client().Do(cctx, "GET", "/slow", jsonx.Null())
	if err == nil || !IsTransient(err) || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
}

func TestATooLargeOrBrokenAnswerIsAnError(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/big", 200, `"`+strings.Repeat("a", MaxResponse)+`"`)
	f.on("GET", "/broken", 200, `{"number": `)
	c := f.client()
	if _, err := c.Do(context.Background(), "GET", "/big", jsonx.Null()); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("big: err = %v", err)
	}
	_, err := c.Do(context.Background(), "GET", "/broken", jsonx.Null())
	if err == nil || IsTransient(err) {
		t.Errorf("broken: err = %v, transient %v", err, IsTransient(err))
	}
}

func TestErrorsNameTheCall(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/broken", 200, `{`)
	_, err := f.client().Do(context.Background(), "GET", "/broken", jsonx.Null())
	if err == nil || !strings.Contains(err.Error(), "GET /broken") {
		t.Errorf("err = %v", err)
	}
}
