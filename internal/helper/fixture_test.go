package helper

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
)

// fakeExec records every command and answers through answer (exit 0 with
// no output when answer is nil). It never runs anything.
type fakeExec struct {
	mu     sync.Mutex
	calls  []execx.Cmd
	answer func(c execx.Cmd) (execx.Result, error)
}

func (f *fakeExec) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	answer := f.answer
	f.mu.Unlock()
	if answer == nil {
		return execx.Result{Argv: c.Argv}, nil
	}
	res, err := answer(c)
	res.Argv = c.Argv
	return res, err
}

func (f *fakeExec) argvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		out = append(out, c.Argv)
	}
	return out
}

func (f *fakeExec) ran(prog string) bool {
	for _, a := range f.argvs() {
		if len(a) > 0 && a[0] == prog {
			return true
		}
	}
	return false
}

// rig is a Server on a scratch root with a fake execer and a fixed clock.
type rig struct {
	*Server
	exec           *fakeExec
	stdout, stderr *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "factory")
	home := filepath.Join(base, "home")
	for _, d := range []string{root, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fx := &fakeExec{}
	r := &rig{exec: fx, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	r.Server = &Server{
		Root:     root,
		Home:     home,
		Self:     "/opt/factory/bin/factory-remote",
		Draining: filepath.Join(base, "draining"),
		Exec:     fx,
		Clock:    clock.Fixed{T: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)},
		Environ:  []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_RUNTIME_DIR=/run/user/1000", "SECRET_TOKEN=leak"},
		FreeDisk: func(string) (uint64, error) { return 10 << 30, nil },
		// no registry in the tests: the rebuild keeps each version
		Latest: func(context.Context, string) (string, error) { return "", nil },
		Stdin:  strings.NewReader(""),
		Stdout: r.stdout,
		Stderr: r.stderr,
	}
	return r
}

// ssh runs one forced-command call: the verb line in SSH_ORIGINAL_COMMAND
// and no arguments, as sshd runs the helper.
func (r *rig) ssh(t *testing.T, command string) int {
	t.Helper()
	r.Environ = append(slices.Clip(r.Environ), "SSH_ORIGINAL_COMMAND="+command)
	defer func() { r.Environ = r.Environ[:len(r.Environ)-1] }()
	return r.Serve(t.Context(), []string{r.Self})
}

// argv runs the helper with local arguments (systemd-run, the rebuild unit).
func (r *rig) argv(t *testing.T, args ...string) int {
	t.Helper()
	return r.Serve(t.Context(), append([]string{r.Self}, args...))
}

func (r *rig) reset() {
	r.stdout.Reset()
	r.stderr.Reset()
	r.exec.mu.Lock()
	r.exec.calls = nil
	r.exec.mu.Unlock()
}

func (r *rig) jobDir(job string) string { return filepath.Join(r.Root, "jobs", job) }

func (r *rig) write(t *testing.T, rel, data string) string {
	t.Helper()
	p := filepath.Join(r.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func onDisk(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// member is one raw tar member for a start input.
type member struct {
	hdr  tar.Header
	data string
}

func file(name, data string) member {
	return member{hdr: tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(data))}, data: data}
}

func mktar(t *testing.T, ms ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range ms {
		h := m.hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(m.data))
		}
		if h.ModTime.IsZero() {
			h.ModTime = time.Unix(1_700_000_000, 0)
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if m.data != "" {
			if _, err := tw.Write([]byte(m.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const goodMeta = `{"branch": "bot/t0001", "start": "main", "model": "claude-sonnet-5-5", "bundle": true, "timeout_minutes": 30}`

func jobInput(t *testing.T, meta string, extra ...member) []byte {
	t.Helper()
	ms := []member{file("prompt.md", "do the work\n"), file("role.md", "you are a worker\n"), file("meta.json", meta)}
	return mktar(t, append(ms, extra...)...)
}
