package jobtail

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

var helper = jobproto.Host{Name: "helper", SSH: "h", Key: "/k"}

type recorder struct {
	argv    [][]string
	timeout []time.Duration
	answer  execx.Result
	err     error
}

func (r *recorder) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	r.argv = append(r.argv, c.Argv)
	r.timeout = append(r.timeout, c.Timeout)
	return r.answer, r.err
}

// Every verb this package sends is a read: tail, status or list.
func TestOnlyReadVerbsAreSent(t *testing.T) {
	rec := &recorder{answer: execx.Result{Stdout: []byte(`{"state": "running"}`)}}
	r := Reader{Exec: rec}
	ctx := context.Background()
	r.Tail(ctx, helper, "t0001-w1")
	if st, err := r.Status(ctx, helper, "t0001-w1"); err != nil || st != jobproto.StateRunning {
		t.Errorf("status %q %v", st, err)
	}
	rec.answer.Stdout = []byte("t0001-w1\n\nt0002-r1\n")
	if names, err := r.List(ctx, helper); err != nil || !reflect.DeepEqual(names, []string{"t0001-w1", "t0002-r1"}) {
		t.Errorf("list %q %v", names, err)
	}
	read := map[string]bool{"tail": true, "status": true, "list": true}
	for _, argv := range rec.argv {
		if argv[0] != "ssh" || argv[9] != "h" || !read[argv[10]] {
			t.Errorf("argv %q", argv)
		}
	}
	if !reflect.DeepEqual(rec.timeout, []time.Duration{20 * time.Second, 120 * time.Second, 120 * time.Second}) {
		t.Errorf("timeouts %v", rec.timeout)
	}
}

// port: TF LookTest.test_look_reads_the_local_screen_or_the_helper_hosts_tail
func TestTailDecodesAndFailsAsARemoteError(t *testing.T) {
	rec := &recorder{answer: execx.Result{Stdout: []byte("pi output \xe2\x82")}}
	r := Reader{Exec: rec}
	out, err := r.Tail(context.Background(), helper, "t0001-w1")
	if err != nil || out != "pi output �" {
		t.Errorf("%q %v", out, err)
	}
	if got := rec.argv[0][len(rec.argv[0])-2:]; !reflect.DeepEqual(got, []string{"tail", "t0001-w1"}) {
		t.Errorf("args %q", got)
	}
	rec.answer = execx.Result{Code: 255, Stderr: []byte("ssh: connect to host h: down\n")}
	_, err = r.Tail(context.Background(), helper, "t0001-w1")
	var re *jobproto.RemoteError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "h tail failed (255): ssh: connect to host h: down") {
		t.Errorf("%v", err)
	}
	rec.answer, rec.err = execx.Result{}, execx.ErrTimeout
	if _, err := r.Tail(context.Background(), helper, "t0001-w1"); err == nil || err.Error() != "h tail timed out" {
		t.Errorf("timeout: %v", err)
	}
	if _, err := r.Tail(context.Background(), helper, "../x"); err == nil {
		t.Error("bad job name sent")
	}
}
