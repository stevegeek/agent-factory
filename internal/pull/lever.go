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

// wakeTimeout bounds one /operator/wake.
const wakeTimeout = 60 * time.Second

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

// hand gives a worker its job: the ready note; when that fails (a
// suspended or stopped worker answers 409), a wake and one more try.
func (p *Puller) hand(ctx context.Context, worker, id string) error {
	err := p.send(ctx, worker, ReadyMessage(id), false)
	if err == nil {
		return nil
	}
	p.logf("job %s: %v; waking %s", id, err, worker)
	if werr := p.wake(ctx, worker); werr != nil {
		p.logf("wake %s: %v", worker, werr)
	}
	return p.send(ctx, worker, ReadyMessage(id), false)
}

func (p *Puller) wake(ctx context.Context, worker string) error {
	if p.Wake != nil {
		return p.Wake(ctx, worker)
	}
	return OperatorWake(ctx, p.Cfg.OperatorSock(), worker)
}

// OperatorWake is POST /operator/wake on the Lever broker's operator
// socket (0600: the host user's own channel), which resumes a suspended or
// stopped worker; tier "operator" may wake a stopped one.
func OperatorWake(ctx context.Context, sock, worker string) error {
	ctx, cancel := context.WithTimeout(ctx, wakeTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"worker": worker, "login": "factory-pull", "tier": "operator"})
	if err != nil {
		return err
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://lever/operator/wake", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("wake %s: %s: %s", worker, resp.Status, textx.Strip(string(msg)))
	}
	return nil
}
