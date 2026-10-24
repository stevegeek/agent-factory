// Package github is the factory's GitHub client: the GitHub App (its JWT and
// installation token), REST and GraphQL calls with GitHub's fixed headers,
// the unauthenticated reads (open issues, latest releases) and the npm
// registry's latest version.
//
// It ports github_request, github_app_jwt, installation_token,
// fetch_open_issues, latest_github_release, latest_npm_version and the calls
// the PR flow makes (F:2330-2600, F:2836-2848, F:4720). It makes no decision:
// prflow, doors, upkeep and the CLI decide what a refusal means. Errors say
// whether retrying can help (IsTransient), which the intent drain (§6.3)
// needs and Python did not have.
//
// Requests and their bodies are what Python sends, byte for byte: the same
// paths (query strings unescaped, as Python built them), and bodies encoded as
// json.dumps(body) does (", " and ": ", ensure_ascii). The one deliberate
// difference is the Content-Type of a body: application/json, where urllib
// sent its form-encoded default (GitHub reads both).
package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

const (
	// DefaultBaseURL is GitHub's API. FACTORY_GITHUB_API points Python
	// elsewhere for the differential tests; the Go wiring sets BaseURL.
	DefaultBaseURL = "https://api.github.com"
	// DefaultNPMURL is the npm registry (FACTORY_NPM_REGISTRY in Python).
	DefaultNPMURL = "https://registry.npmjs.org"
	// UserAgent is sent on every request.
	UserAgent = "agent-factory"
	// APIVersion is GitHub's REST API version header on authenticated calls.
	APIVersion = "2022-11-28"
	// Accept is GitHub's JSON media type.
	Accept = "application/vnd.github+json"
	// Timeout is Python's urlopen timeout, per request.
	Timeout = 30 * time.Second
	// MaxResponse caps the bytes read from one answer. Python read without a
	// limit; the largest answer the factory asks for (100 reviews or
	// comments) is far below this.
	MaxResponse = 16 << 20
	// maxErrorBody caps the bytes of a refusal kept on an HTTPError.
	maxErrorBody = 64 << 10
)

// Doer sends one HTTP request; *http.Client is one.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// TokenSource gives the installation token for authenticated calls; *App
// is one.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Client calls GitHub's API and the npm registry.
type Client struct {
	// BaseURL is the API root; "" is DefaultBaseURL. Trailing slashes are
	// dropped, as Python's github_base does.
	BaseURL string
	// NPMURL is the registry root; "" is DefaultNPMURL.
	NPMURL string
	// HTTP sends the requests; nil is http.DefaultClient.
	HTTP Doer
	// Auth gives the installation token for Do, GraphQL and the typed PR
	// calls. The unauthenticated reads (OpenIssues, LatestRelease,
	// NPMLatest) never use it.
	Auth TokenSource
	// Timeout bounds each request; 0 is Timeout (30 s).
	Timeout time.Duration
}

// Response is an answer GitHub gave with a 2xx status.
type Response struct {
	Code   int
	Header http.Header
	// Value is the parsed JSON body; an empty body is {} (as in Python).
	Value jsonx.Value
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) npm() string {
	if c.NPMURL == "" {
		return DefaultNPMURL
	}
	return strings.TrimRight(c.NPMURL, "/")
}

func (c *Client) doer() Doer {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}

func (c *Client) timeout() time.Duration {
	if c.Timeout <= 0 {
		return Timeout
	}
	return c.Timeout
}

// Do is github_api (F:2388): an authenticated call to path (which starts with
// "/" and may carry a query) with body as JSON, or no body when body is null.
// A non-2xx answer is a bare *HTTPError, whose text is Python's str(e).
func (c *Client) Do(ctx context.Context, method, path string, body jsonx.Value) (*Response, error) {
	if c.Auth == nil {
		return nil, fmt.Errorf("github: %s %s: no installation token source", method, path)
	}
	if err := checkPath(path); err != nil {
		return nil, fmt.Errorf("github: %s %q: %w", method, path, err)
	}
	tok, err := c.Auth.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: installation token: %w", err)
	}
	return c.send(ctx, method, path, "Bearer "+tok, body)
}

// send is github_request (F:2366) with the Authorization header given whole.
func (c *Client) send(ctx context.Context, method, path, authorization string, body jsonx.Value) (*Response, error) {
	if err := checkPath(path); err != nil {
		return nil, fmt.Errorf("github: %s %q: %w", method, path, err)
	}
	h := http.Header{}
	h.Set("Accept", Accept)
	h.Set("User-Agent", UserAgent)
	h.Set("X-GitHub-Api-Version", APIVersion)
	h.Set("Authorization", authorization)
	return c.call(ctx, method, c.base(), path, h, body)
}

// get is an unauthenticated GET with exactly the headers given.
func (c *Client) get(ctx context.Context, root, path string, h http.Header) (*Response, error) {
	if err := checkPath(path); err != nil {
		return nil, fmt.Errorf("github: GET %q: %w", path, err)
	}
	return c.call(ctx, http.MethodGet, root, path, h, jsonx.Null())
}

func (c *Client) call(ctx context.Context, method, root, path string, h http.Header, body jsonx.Value) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	var rd io.Reader
	if !body.IsNull() {
		rd = bytes.NewReader(jsonx.EncodePython(body, -1))
		h.Set("Content-Type", "application/json")
	}
	req, err := http.NewRequestWithContext(ctx, method, root+path, rd)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	req.Header = h
	resp, err := c.doer().Do(req)
	if err != nil {
		return nil, &NetError{Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, newHTTPError(method, path, resp, raw)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return nil, &NetError{Method: method, Path: path, Err: err}
	}
	if len(raw) > MaxResponse {
		return nil, fmt.Errorf("github: %s %s: answer too large (over %d bytes)", method, path, MaxResponse)
	}
	out := &Response{Code: resp.StatusCode, Header: resp.Header}
	if len(raw) == 0 {
		out.Value = jsonx.ObjectOf()
		return out, nil
	}
	out.Value, err = jsonx.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	return out, nil
}

// checkPath refuses a path that is not one absolute path on the API's own
// host: it must start with a single "/" and hold no space or control byte.
// Repository and branch names come from config and ticket files.
func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("not an absolute API path")
	}
	for i := 0; i < len(path); i++ {
		if b := path[i]; b <= ' ' || b == 0x7f {
			return errors.New("space or control character in the path")
		}
	}
	return nil
}
