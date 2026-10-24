package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
)

// request is one call the fake GitHub received.
type request struct {
	Method string
	URI    string // path and query, as sent
	Header http.Header
	Body   string
}

// reply is the fake's scripted answer to one method and URI.
type reply struct {
	Code   int
	Body   string
	Header map[string]string
}

// fakeGitHub is a GitHub that answers from a script keyed by "METHOD URI"
// and records every request with its headers. It serves in-process: it is
// the Doer itself (through httptest.ResponseRecorder), so the tests open no
// socket and run where loopback connections are not allowed.
type fakeGitHub struct {
	t   testing.TB
	srv inproc

	mu      sync.Mutex
	script  map[string][]reply // repeated keys answer in order; the last one sticks
	got     []request
	handler func(w http.ResponseWriter, r *http.Request) bool // optional override
}

func newFake(t testing.TB) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, script: map[string][]reply{}}
	f.srv = inproc{URL: fmt.Sprintf("http://gh-%p.test", f), f: f}
	return f
}

// inproc stands where an httptest.Server would.
type inproc struct {
	URL string
	f   *fakeGitHub
}

// Client is the Doer that reaches this fake.
func (s inproc) Client() Doer { return s.f }

// Do serves req in-process, as an http.Client would: a request to another
// host fails as a connection would, and a done context ends the call with a
// *url.Error wrapping the context's error.
func (f *fakeGitHub) Do(req *http.Request) (*http.Response, error) {
	fail := func(err error) (*http.Response, error) {
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: err}
	}
	if "http://"+req.URL.Host != f.srv.URL {
		return fail(errors.New("dial tcp: connection refused"))
	}
	if err := req.Context().Err(); err != nil {
		return fail(err)
	}
	if req.Body == nil {
		req.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	f.serve(rec, req)
	if err := req.Context().Err(); err != nil {
		return fail(err)
	}
	return rec.Result(), nil
}

func (f *fakeGitHub) on(method, uri string, code int, body string) *fakeGitHub {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := method + " " + uri
	f.script[key] = append(f.script[key], reply{Code: code, Body: body})
	return f
}

func (f *fakeGitHub) onReply(method, uri string, r reply) *fakeGitHub {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := method + " " + uri
	f.script[key] = append(f.script[key], r)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, request{Method: r.Method, URI: r.URL.RequestURI(), Header: r.Header.Clone(), Body: string(body)})
	h := f.handler
	f.mu.Unlock()
	if h != nil && h(w, r) {
		return
	}
	f.mu.Lock()
	key := r.Method + " " + r.URL.RequestURI()
	rs := f.script[key]
	var rep reply
	switch {
	case len(rs) == 0:
		rep = reply{Code: 404, Body: `{"message": "Not Found"}`}
	case len(rs) == 1:
		rep = rs[0]
	default:
		rep, f.script[key] = rs[0], rs[1:]
	}
	f.mu.Unlock()
	for k, v := range rep.Header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(rep.Code)
	io.WriteString(w, rep.Body)
}

func (f *fakeGitHub) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.got...)
}

func (f *fakeGitHub) last() request {
	f.t.Helper()
	rs := f.requests()
	if len(rs) == 0 {
		f.t.Fatal("the fake GitHub received no request")
	}
	return rs[len(rs)-1]
}

// staticToken is a TokenSource for tests of the authenticated calls.
type staticToken string

func (s staticToken) Token(ctx context.Context) (string, error) { return string(s), nil }

// client is a Client on the fake with a fixed installation token.
func (f *fakeGitHub) client() *Client {
	return &Client{BaseURL: f.srv.URL, NPMURL: f.srv.URL + "/npm", HTTP: f.srv.Client(), Auth: staticToken("tok")}
}

// testKey is a 2048-bit RSA key (the size the Python test uses), made once.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// writeKey writes key as PEM of the given block type ("RSA PRIVATE KEY" is
// PKCS#1, as openssl genrsa writes it; "PRIVATE KEY" is PKCS#8) and returns
// its path.
func writeKey(t testing.TB, key any, blockType string) string {
	t.Helper()
	var der []byte
	var err error
	switch blockType {
	case "RSA PRIVATE KEY":
		der = x509.MarshalPKCS1PrivateKey(key.(*rsa.PrivateKey))
	case "PRIVATE KEY":
		der, err = x509.MarshalPKCS8PrivateKey(key)
	default:
		t.Fatalf("unknown block type %q", blockType)
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testNow is the fixed time the App tests sign at.
var testNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// newApp is an App on the fake with the test key and a stepping clock.
func newApp(t testing.TB, f *fakeGitHub, clk clock.Clock) *App {
	t.Helper()
	return &App{
		ClientID:       "Iv23test",
		InstallationID: "42",
		KeyPath:        writeKey(t, testKey(), "RSA PRIVATE KEY"),
		Clock:          clk,
		API:            &Client{BaseURL: f.srv.URL, HTTP: f.srv.Client()},
	}
}
