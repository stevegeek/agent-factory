package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The ticket API: /api/v1, for scripts and agents with a token from `factory tokens`. Foreman checks who is
// calling (Guard) and hands the request to `factory ticket VERB --as NAME --json`; the CLI decides the rest.

const apiPrefix = "/api/v1/"

var apiTicketID = regexp.MustCompile(`^T\d{4,6}$`)

// apiStatus: the HTTP status for each refusal code the CLI answers with.
var apiStatus = map[string]int{
	"bad_request":           http.StatusBadRequest,
	"unauthorized":          http.StatusUnauthorized,
	"forbidden":             http.StatusForbidden,
	"factory_ticket":        http.StatusForbidden,
	"not_on_factory_ticket": http.StatusForbidden,
	"not_found":             http.StatusNotFound,
	"conflict":              http.StatusConflict,
	"idempotency_mismatch":  http.StatusUnprocessableEntity,
	"too_many":              http.StatusTooManyRequests,
	"busy":                  http.StatusServiceUnavailable,
	"storage_full":          http.StatusInsufficientStorage,
}

type queryKind int

const (
	qString queryKind = iota
	qInt
	qBool
	qTime
)

// apiRoute: one endpoint. The verb is fixed here, never taken from the request.
type apiRoute struct {
	verb    string
	status  int                  // on success
	query   map[string]queryKind // the query parameters it takes (GET only)
	rename  map[string]string    // query name -> request key
	body    bool                 // takes a JSON body
	created bool                 // 201 and Location only when the CLI says it filed one
}

var (
	listQuery = map[string]queryKind{"project": qString, "column": qString, "assignee": qString, "label": qString,
		"kind": qString, "q": qString, "updated_since": qTime, "include_closed": qBool, "limit": qInt, "cursor": qString,
		"format": qString}
	apiRoutes = map[string]map[string]apiRoute{
		"me":       {"GET": {verb: "whoami", status: 200}},
		"projects": {"GET": {verb: "projects", status: 200}},
		"work":     {"GET": {verb: "mine", status: 200, query: map[string]queryKind{"project": qString, "limit": qInt, "format": qString}}},
		"tickets": {"GET": {verb: "list", status: 200, query: listQuery, rename: map[string]string{"q": "text"}},
			"POST": {verb: "create", status: 200, body: true, created: true}},
		"tickets/{id}": {"GET": {verb: "get", status: 200, query: map[string]queryKind{"format": qString}},
			"PATCH": {verb: "update", status: 200, body: true}},
		"tickets/{id}/notes":  {"POST": {verb: "note", status: 201, body: true}},
		"tickets/{id}/move":   {"POST": {verb: "move", status: 200, body: true}},
		"tickets/{id}/claim":  {"POST": {verb: "claim", status: 200, body: true}},
		"tickets/{id}/ask":    {"POST": {verb: "ask", status: 201, body: true}},
		"tickets/{id}/answer": {"POST": {verb: "answer", status: 200, body: true}},
		"tickets/{id}/split":  {"POST": {verb: "split", status: 201, body: true}},
		"tickets/{id}/send":   {"POST": {verb: "send", status: 201, body: true}},
	}
)

type API struct {
	guard  *Guard
	runner *APIRunner
}

func NewAPI(guard *Guard, runner *APIRunner) *API { return &API{guard: guard, runner: runner} }

// apiMatch: the route pattern for a path under /api/v1/ and the ticket id in it ("" when none).
func apiMatch(path string) (pattern, id string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, apiPrefix), "/")
	switch {
	case len(parts) == 1:
		_, ok = apiRoutes[parts[0]]
		return parts[0], "", ok
	case parts[0] == "tickets" && (len(parts) == 2 || len(parts) == 3):
		pattern = "tickets/{id}"
		if len(parts) == 3 {
			pattern += "/" + parts[2]
		}
		_, ok = apiRoutes[pattern]
		return pattern, parts[1], ok
	}
	return "", "", false
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := a.guard.Admit(w, r, r.Method != http.MethodGet)
	if name == "" {
		return
	}
	pattern, id, ok := apiMatch(r.URL.Path)
	if !ok {
		apiError(w, http.StatusNotFound, "not_found", "no such endpoint", "")
		return
	}
	methods := apiRoutes[pattern]
	route, ok := methods[r.Method]
	if !ok {
		var allow []string
		for m := range methods {
			allow = append(allow, m)
		}
		sort.Strings(allow)
		w.Header().Set("Allow", strings.Join(allow, ", "))
		apiError(w, http.StatusMethodNotAllowed, "bad_request", "that method is not allowed here", "")
		return
	}
	if strings.Contains(pattern, "{id}") && !apiTicketID.MatchString(id) {
		apiError(w, http.StatusNotFound, "not_found", "no ticket "+id, "id")
		return
	}
	req, status, msg, field := apiRequest(r, route, id)
	if status != 0 {
		code := "bad_request"
		if status == http.StatusRequestEntityTooLarge {
			code = "too_large"
		}
		apiError(w, status, code, msg, field)
		return
	}
	a.answer(w, route, name, req)
}

// apiRequest: the verb's input, from the path id, the query and the body. The body may not name the id.
func apiRequest(r *http.Request, route apiRoute, id string) (in map[string]json.RawMessage, status int, msg, field string) {
	in = map[string]json.RawMessage{}
	if route.body {
		data, err := readAll(r)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is at most %d KiB", apiBody>>10), ""
		}
		if err != nil {
			return nil, http.StatusBadRequest, "the body could not be read", ""
		}
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &in); err != nil || in == nil {
				return nil, http.StatusBadRequest, "the body is one JSON object", ""
			}
		}
		if _, ok := in["id"]; ok && id != "" {
			return nil, http.StatusBadRequest, "the ticket is named by the path, not the body", "id"
		}
	}
	q := r.URL.Query()
	for k, vs := range q {
		kind, ok := route.query[k]
		if !ok {
			return nil, http.StatusBadRequest, "unknown query parameter " + strconv.Quote(k), k
		}
		if len(vs) != 1 {
			return nil, http.StatusBadRequest, k + " is given once", k
		}
		v := vs[0]
		if v == "" {
			continue
		}
		key := k
		if n, ok := route.rename[k]; ok {
			key = n
		}
		switch kind {
		case qInt:
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, http.StatusBadRequest, k + " must be an integer", k
			}
			in[key], _ = json.Marshal(n)
		case qBool:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, http.StatusBadRequest, k + " must be true or false", k
			}
			in[key], _ = json.Marshal(b)
		case qTime:
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return nil, http.StatusBadRequest, k + " must be an RFC 3339 time", k
			}
			in[key], _ = json.Marshal(v)
		default:
			in[key], _ = json.Marshal(v)
		}
	}
	if route.verb == "create" {
		if keys := r.Header.Values("Idempotency-Key"); len(keys) > 1 {
			return nil, http.StatusBadRequest, "Idempotency-Key is given once", "idempotency_key"
		} else if len(keys) == 1 {
			in["idempotency_key"], _ = json.Marshal(keys[0])
		}
	}
	if id != "" {
		in["id"], _ = json.Marshal(id)
	}
	return in, 0, "", ""
}

func readAll(r *http.Request) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.Bytes(), err
}

// answer runs the verb and relays what the CLI says.
func (a *API) answer(w http.ResponseWriter, route apiRoute, name string, in map[string]json.RawMessage) {
	input, _ := json.Marshal(in)
	out, refusal, err := a.runner.Call(route.verb, name, input)
	switch {
	case errors.Is(err, errAPIBusy):
		w.Header().Set("Retry-After", "5")
		apiError(w, http.StatusServiceUnavailable, "busy", "the factory is busy; try again in a few seconds", "")
	case err != nil:
		apiInternal(w)
	case refusal != nil:
		switch refusal.Code {
		case "busy":
			w.Header().Set("Retry-After", "30")
		case "unauthorized":
			// revoked or expired since Foreman last read the store
			w.Header().Set("WWW-Authenticate", `Bearer realm="factory"`)
		}
		apiError(w, apiStatus[refusal.Code], refusal.Code, refusal.Message, refusal.Field)
	default:
		status := route.status
		if route.created && string(out["created"]) == "true" {
			status = http.StatusCreated
			var t struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(out["ticket"], &t) == nil && apiTicketID.MatchString(t.ID) {
				w.Header().Set("Location", apiPrefix+"tickets/"+t.ID)
			}
		}
		apiJSON(w, status, out)
	}
}

func apiJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"the answer could not be written"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(data, '\n'))
}

func apiError(w http.ResponseWriter, status int, code, message, field string) {
	e := map[string]string{"code": code}
	if message != "" {
		e["message"] = message
	}
	if field != "" {
		e["field"] = field
	}
	apiJSON(w, status, map[string]any{"error": e})
}

func apiInternal(w http.ResponseWriter) {
	apiError(w, http.StatusInternalServerError, "internal", "the factory could not do that; it is logged", "")
}

// ---------------------------------------------------------------- the CLI, for the API

var (
	errAPIBusy = errors.New("no free slot for the factory CLI")
	errCLI     = errors.New("the factory CLI failed; it is logged")
)

// cliRefusal: a verb's refusal, as the CLI words it. Its code is one of apiStatus's.
type cliRefusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field"`
}

// Call runs a verb and sorts its answer: what it said (without "ok"), or its refusal. An error means there
// is nothing to relay: errAPIBusy, or errCLI for a failure of the factory's own, logged here. A refusal
// code this Foreman does not know is such a failure, so drift between the two shows.
func (ar *APIRunner) Call(verb, name string, input []byte) (map[string]json.RawMessage, *cliRefusal, error) {
	res, err := ar.Run(verb, name, input)
	if errors.Is(err, errAPIBusy) {
		return nil, nil, err
	}
	if err != nil {
		log.Printf("api %s as %s: %v", verb, name, err)
		return nil, nil, errCLI
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(res.stdout, &out) != nil || out == nil {
		log.Printf("api %s as %s: exit %d with no JSON answer: %s", verb, name, res.exit, tail(string(res.stderr), 500))
		return nil, nil, errCLI
	}
	ok := string(out["ok"])
	switch {
	case res.exit == 0 && ok == "true":
		delete(out, "ok")
		return out, nil, nil
	case res.exit == 1 && ok == "false":
		var e cliRefusal
		if json.Unmarshal(out["error"], &e) == nil {
			if _, known := apiStatus[e.Code]; known {
				return nil, &e, nil
			}
		}
		log.Printf("api %s as %s: a refusal this Foreman does not know: %s", verb, name, tail(string(out["error"]), 300))
	default:
		log.Printf("api %s as %s: exit %d: %s", verb, name, res.exit, tail(string(res.stderr), 500))
	}
	return nil, nil, errCLI
}

// APIRunner runs `factory ticket` for the API: a few at a time, apart from Runner, whose one lock can be
// held by a ten-minute `pr`.
type APIRunner struct {
	Factory, Root, Tokens string
	slots                 chan struct{}
	slotWait, timeout     time.Duration
	maxOut                int
}

func NewAPIRunner(factory, root, tokens string, concurrency int) *APIRunner {
	return &APIRunner{Factory: factory, Root: root, Tokens: tokens, slots: make(chan struct{}, max(concurrency, 1)),
		slotWait: 10 * time.Second, timeout: 30 * time.Second, maxOut: 1 << 20}
}

type cliResult struct {
	exit           int
	stdout, stderr []byte
}

// capped keeps the first n bytes and notes that there were more.
type capped struct {
	buf  bytes.Buffer
	n    int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.n - c.buf.Len(); room < len(p) {
		c.over = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// Run: the verb's exit code and output; an error only when there is no answer to relay (no slot, a
// timeout, too much output, or the CLI would not start).
func (ar *APIRunner) Run(verb, name string, input []byte) (cliResult, error) {
	timer := time.NewTimer(ar.slotWait)
	select {
	case ar.slots <- struct{}{}:
		timer.Stop()
	case <-timer.C:
		return cliResult{}, errAPIBusy
	}
	defer func() { <-ar.slots }()
	// not the request's context: a client that hangs up must not cut a write short
	ctx, cancel := context.WithTimeout(context.Background(), ar.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ar.Factory, "ticket", verb, "--as", name, "--wait", "10", "--json")
	cmd.Env = append(apiEnv(), "FACTORY_ROOT="+ar.Root, "FACTORY_API_TOKENS="+ar.Tokens)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = bytes.NewReader(input)
	stdout, stderr := &capped{n: ar.maxOut}, &capped{n: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	res := cliResult{stdout: stdout.buf.Bytes(), stderr: stderr.buf.Bytes()}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return res, fmt.Errorf("timed out after %s: %s", ar.timeout, tail(string(res.stderr), 500))
	case stdout.over:
		return res, fmt.Errorf("answered more than %d bytes", ar.maxOut)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.exit = exit.ExitCode()
	} else if err != nil {
		return res, err
	}
	return res, nil
}

// apiEnv: Foreman's own environment without the two settings the runner sets itself.
func apiEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FACTORY_ROOT=") && !strings.HasPrefix(kv, "FACTORY_API_TOKENS=") {
			env = append(env, kv)
		}
	}
	return env
}
