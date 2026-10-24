package sshjobs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

var pi = Host{Name: "pi", SSH: "factory@pi", Key: "/k/factory-remote"}

func TestDoSendsStdinAndReturnsStdout(t *testing.T) {
	var got execx.Cmd
	var stdin []byte
	c := Client{Exec: execx.Func(func(ctx context.Context, cmd execx.Cmd) (execx.Result, error) {
		got = cmd
		if cmd.Stdin != nil {
			stdin, _ = io.ReadAll(cmd.Stdin)
		}
		return execx.Result{Argv: cmd.Argv, Stdout: []byte(`{"started": "t0003-t1"}`)}, nil
	})}
	out, err := c.Do(context.Background(), pi, jobproto.VerbStart, "t0003-t1", []byte("tar"), 0)
	if err != nil || string(out) != `{"started": "t0003-t1"}` {
		t.Fatalf("%q %v", out, err)
	}
	want := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "IdentitiesOnly=yes",
		"-i", "/k/factory-remote", "factory@pi", "start", "t0003-t1"}
	if !reflect.DeepEqual(got.Argv, want) || string(stdin) != "tar" {
		t.Errorf("argv %q stdin %q", got.Argv, stdin)
	}
	if got.Timeout != 120*time.Second || got.MaxOut != 1<<20 {
		t.Errorf("timeout %v cap %d", got.Timeout, got.MaxOut)
	}
	c.Do(context.Background(), pi, jobproto.VerbCollect, "t0003-t1", nil, 0)
	if got.Timeout != 300*time.Second || got.Stdin != nil || got.MaxOut < 100<<20 {
		t.Errorf("collect: %+v", got)
	}
	c.Do(context.Background(), pi, jobproto.VerbStatus, "t0003-t1", nil, 7*time.Second)
	if got.Timeout != 7*time.Second {
		t.Errorf("explicit timeout %v", got.Timeout)
	}
}

func TestFailuresAreRemoteErrorsOnlyWhenTheHelperSaidSo(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name   string
		res    execx.Result
		err    error
		ctx    context.Context
		remote string // the RemoteError's text; "" for an error that is not one
	}{
		{"exit", execx.Result{Code: 2, Stderr: []byte("t0003-t1 already exists\n")}, nil, context.Background(),
			"factory@pi start failed (2): t0003-t1 already exists"},
		{"timeout", execx.Result{}, errors.Join(execx.ErrTimeout), context.Background(), "factory@pi start timed out"},
		{"too much", execx.Result{Truncated: true}, nil, context.Background(), "factory@pi start sent more than 1048576 bytes"},
		{"no ssh", execx.Result{}, os.ErrNotExist, context.Background(), ""},
		{"cancelled", execx.Result{}, context.Canceled, cancelled, ""},
	} {
		cl := Client{Exec: execx.Func(func(ctx context.Context, cmd execx.Cmd) (execx.Result, error) { return c.res, c.err })}
		_, err := cl.Do(c.ctx, pi, jobproto.VerbStart, "t0003-t1", []byte("x"), 0)
		var re *RemoteError
		switch {
		case err == nil:
			t.Errorf("%s: no error", c.name)
		case c.remote == "" && errors.As(err, &re):
			t.Errorf("%s: a RemoteError: %v", c.name, err)
		case c.remote != "" && (!errors.As(err, &re) || re.Error() != c.remote):
			t.Errorf("%s: %v, want %q", c.name, err, c.remote)
		}
	}
}

func TestBadVerbsAndNamesNeverRun(t *testing.T) {
	cl := Client{Exec: execx.Func(func(ctx context.Context, cmd execx.Cmd) (execx.Result, error) {
		t.Errorf("ran %q", cmd.Argv)
		return execx.Result{}, nil
	})}
	for _, c := range [][2]string{{"run", "t0001-t1"}, {"rm", "-rf"}, {"status", "../../etc"}, {"status", "a b"}, {"status", ""}} {
		if _, err := cl.Do(context.Background(), pi, c[0], c[1], nil, 0); err == nil {
			t.Errorf("%q accepted", c)
		}
	}
}

// The real execer and a fake ssh on PATH: a helper that never answers is a
// RemoteError after the timeout, and its process group is gone.
func TestATimeoutOnARealProcessIsARemoteError(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" > \"$(dirname \"$0\")/argv\"\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cl := Client{Exec: execx.OS{WaitDelay: 100 * time.Millisecond}}
	start := time.Now()
	_, err := cl.Do(context.Background(), pi, jobproto.VerbStatus, "t0003-t1", nil, 300*time.Millisecond)
	var re *RemoteError
	if !errors.As(err, &re) || re.Error() != "factory@pi status timed out" {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if string(argv) != "-o BatchMode=yes -o ConnectTimeout=10 -o IdentitiesOnly=yes -i /k/factory-remote factory@pi status t0003-t1\n" {
		t.Errorf("argv %q", argv)
	}
}
