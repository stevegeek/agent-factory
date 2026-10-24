package github

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// ErrTransient matches (errors.Is) every failure that retrying later can
// fix: a network failure, a timeout or cancelled context, a 5xx, a 429, and
// GitHub's rate limits (a 403 with Retry-After or no requests remaining).
// A refusal GitHub will repeat (401, 403, 404, 405, 409, 422) does not match.
var ErrTransient = errors.New("github: transient failure")

// IsTransient reports whether err is worth retrying on a later tick.
func IsTransient(err error) bool { return errors.Is(err, ErrTransient) }

// HTTPError is a non-2xx answer: urllib's HTTPError.
type HTTPError struct {
	Method, Path string
	Code         int
	// Reason is the status line's phrase ("Unprocessable Entity").
	Reason string
	// Message is github_error(e) (F:2473): GitHub's own "message" when the
	// body is JSON with one, else Python's str(e).
	Message string
	// Body is the answer's first 64 KiB.
	Body []byte
	// RetryAfter and RateRemaining are GitHub's rate-limit headers, as sent.
	RetryAfter, RateRemaining string
}

// Error is Python's str(e) for an HTTPError: "HTTP Error 422: Unprocessable
// Entity". The ticket's error events quote it, so it carries no path.
func (e *HTTPError) Error() string {
	return "HTTP Error " + strconv.Itoa(e.Code) + ": " + e.Reason
}

// Is makes a transient HTTPError match ErrTransient.
func (e *HTTPError) Is(target error) bool {
	return target == ErrTransient && e.transient()
}

func (e *HTTPError) transient() bool {
	switch {
	case e.Code >= 500, e.Code == http.StatusTooManyRequests:
		return true
	case e.Code == http.StatusForbidden:
		return e.RetryAfter != "" || e.RateRemaining == "0"
	}
	return false
}

func newHTTPError(method, path string, resp *http.Response, body []byte) *HTTPError {
	e := &HTTPError{
		Method: method, Path: path, Code: resp.StatusCode, Body: body,
		RetryAfter:    resp.Header.Get("Retry-After"),
		RateRemaining: resp.Header.Get("X-RateLimit-Remaining"),
	}
	e.Reason = strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")
	if e.Reason == "" || e.Reason == resp.Status {
		e.Reason = http.StatusText(resp.StatusCode)
	}
	e.Message = e.Error()
	if v, err := jsonx.Parse(body); err == nil {
		if msg, ok := v.Get("message").Str(); ok && msg != "" {
			e.Message = msg
		}
	}
	return e
}

// NetError is a request that got no answer: the connection failed, the
// answer was cut off, or the request ran out of time. It is always transient.
type NetError struct {
	Method, Path string
	Err          error
}

func (e *NetError) Error() string { return fmt.Sprintf("github: %s %s: %v", e.Method, e.Path, e.Err) }

// Unwrap gives the cause, so errors.Is(err, context.DeadlineExceeded) works.
func (e *NetError) Unwrap() error { return e.Err }

// Is makes every NetError match ErrTransient.
func (e *NetError) Is(target error) bool { return target == ErrTransient }

// GraphQLError is a GraphQL answer with a non-empty "errors" (GitHub answers
// 200). It is a refusal, not transient.
type GraphQLError struct {
	// Op names the mutation for the message ("convertPullRequestToDraft").
	Op string
	// Errors is the answer's "errors" value.
	Errors jsonx.Value
}

// Error is set_pr_draft's RuntimeError (F:2588): "github: <op>:
// <str(errors)[:200]>", Python's str of the parsed errors cut to 200 code
// points.
func (e *GraphQLError) Error() string {
	op := e.Op
	if op == "" {
		op = "graphql"
	}
	return "github: " + op + ": " + clipRunes(jsonx.PyStr(e.Errors), 200)
}

func clipRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}
