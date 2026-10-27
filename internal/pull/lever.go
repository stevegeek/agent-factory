package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// leverTimeout bounds one `lever msg send`.
const leverTimeout = 60 * time.Second

// ReadyMessage is the operator note that hands a worker a job; the worker
// instructions (images/lever/factory-worker.md) take only this shape.
func ReadyMessage(id string) string { return "job " + id + " ready at jobs/" + id }

// StopMessage is the interrupting note that stops a job.
func StopMessage(id, why string) string {
	return "stop job " + id + ": " + why + ". Do not write to jobs/" + id + " again; wait for the next job."
}

// send is `lever msg send [--interrupt] --to <worker> <body>`, run in the
// instance directory (lever reads lever.yaml from there): an operator note,
// which the worker can verify with message_verify.
func (p *Puller) send(ctx context.Context, worker, body string, interrupt bool) error {
	argv := []string{p.Cfg.Lever, "msg", "send"}
	if interrupt {
		argv = append(argv, "--interrupt")
	}
	argv = append(argv, "--to", worker, body)
	res, err := p.Exec.Run(ctx, execx.Cmd{Argv: argv, Dir: p.Cfg.Instance, Timeout: leverTimeout, MaxOut: 64 << 10})
	if err != nil {
		return fmt.Errorf("lever msg send to %s: %w", worker, err)
	}
	if res.Code != 0 {
		return fmt.Errorf("lever msg send to %s failed (%d): %s", worker, res.Code, textx.Prefix(textx.Strip(string(res.Stderr)), 300))
	}
	return nil
}

// hand gives a worker its job: the ready note. The worker was running
// and idle when it was chosen; a note that fails anyway is the caller's to
// give back.
func (p *Puller) hand(ctx context.Context, worker, id string) error {
	return p.send(ctx, worker, ReadyMessage(id), false)
}

// Lever is what factory-pull asks the instance's broker: its workers'
// states, and to start one that is not running.
type Lever interface {
	Workers(ctx context.Context) ([]WorkerState, error)
	Start(ctx context.Context, worker string) error
}

// WorkerState is one worker as GET /operator/worker/list reports it.
type WorkerState struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`    // running, suspended, stopped, error, none, running-container-down
	Activity string `json:"activity"` // working, waiting_for_input, completed, stalled, offline, ...
	// Idle is idle_at_prompt: running and waiting at its prompt; nil when
	// Lever cannot tell (a stalled worker may be idle or stuck)
	Idle *bool `json:"idle_at_prompt"`
}

// Ready reports a worker that can take a job now: running, and not busy
// as far as Lever can tell. Lever often cannot (a worker waiting between
// jobs reports its last status, such as "blocked", with idle_at_prompt
// null), so only a definite busy counts against it; factory-pull's own
// jobs/ is what says a worker still has a job.
func (w WorkerState) Ready() bool { return w.Phase == "running" && !w.Busy() }

// Busy reports a worker Lever says is working: running and not idle at
// its prompt.
func (w WorkerState) Busy() bool { return w.Phase == "running" && w.Idle != nil && !*w.Idle }

// Startable reports a worker the broker can start: one that is not running
// and not broken.
func (w WorkerState) Startable() bool {
	return w.Phase == "stopped" || w.Phase == "suspended" || w.Phase == "none"
}

// Broker is Lever's operator socket (0600: the host user's own channel).
type Broker struct{ Sock string }

// brokerTimeout bounds one call to the broker; a start waits for the
// worker's container.
const (
	brokerTimeout = 10 * time.Second
	startTimeout  = 120 * time.Second
)

// Workers is GET /operator/worker/list.
func (b Broker) Workers(ctx context.Context) ([]WorkerState, error) {
	body, err := b.call(ctx, http.MethodGet, "/operator/worker/list", nil, brokerTimeout)
	if err != nil {
		return nil, err
	}
	var out struct {
		Workers []WorkerState `json:"workers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("worker list: %w", err)
	}
	return out.Workers, nil
}

// Start is POST /operator/worker/start: it resumes a stopped or suspended
// worker, or creates a missing one from its task file; a running one is
// left alone.
func (b Broker) Start(ctx context.Context, worker string) error {
	req, err := json.Marshal(map[string]string{"worker": worker})
	if err != nil {
		return err
	}
	_, err = b.call(ctx, http.MethodPost, "/operator/worker/start", req, startTimeout)
	return err
}

// call is one request on the socket; a non-2xx answer is an error with
// Lever's X-Lever-Reason when it gave one.
func (b Broker) call(ctx context.Context, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", b.Sock)
	}}
	defer tr.CloseIdleConnections()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://lever"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	msg, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		why := textx.Prefix(textx.Strip(string(msg)), 300)
		if r := resp.Header.Get("X-Lever-Reason"); r != "" {
			why = r + ": " + why
		}
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, why)
	}
	return msg, nil
}
