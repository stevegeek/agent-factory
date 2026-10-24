package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// tickExec answers scion with an empty list and a start that works, and
// runs everything else (git) for real.
type tickExec struct {
	mu   sync.Mutex
	argv [][]string
}

func (x *tickExec) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	x.mu.Lock()
	x.argv = append(x.argv, c.Argv)
	x.mu.Unlock()
	if strings.HasSuffix(c.Argv[0], "/scion") {
		r := execx.Result{Argv: c.Argv}
		if len(c.Argv) > 1 && c.Argv[1] == "list" {
			r.Stdout = []byte("[]")
		}
		return r, nil
	}
	if c.Argv[0] == "podman" && len(c.Argv) > 1 && c.Argv[1] == "ps" {
		return execx.Result{Argv: c.Argv, Stdout: []byte("t0001-t1\n")}, nil // the sysinfo snapshot's
	}
	return execx.OS{}.Run(ctx, c)
}

func (x *tickExec) scion(verb string) [][]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out [][]string
	for _, a := range x.argv {
		if strings.HasSuffix(a[0], "/scion") && len(a) > 1 && a[1] == verb {
			out = append(out, a)
		}
	}
	return out
}

func tickEnv(t *testing.T) (*Env, *testkit.Env, *tickExec, *bytes.Buffer, ticket.ID) {
	t.Helper()
	root := testkit.Root(t)
	repo := testkit.NewRepo(t)
	if err := os.MkdirAll(filepath.Join(repo.Dir, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Dir, ".scion", "project-id"), []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root.WriteConfig(t, "[projects.p]\npath = \""+repo.Dir+"\"\nbase = \"main\"\n")
	if err := os.WriteFile(filepath.Join(root.Home, ".config", "factory", "claude-oauth-token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for k, v := range root.Vars {
		vars[k] = v
	}
	ex := &tickExec{}
	var stdout, stderr bytes.Buffer
	e := &Env{Stdout: &stdout, Stderr: &stderr, Environ: vars, Exec: ex, Clock: testkit.Fixed{T: testkit.Stamp()}}
	e.init()
	tk, err := e.Board().FileTicket(context.Background(), ticket.NewIn{Project: "p", Title: "An issue", Body: "b", Door: "human", Column: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	return e, root, ex, &stdout, tk.ID
}

// `factory tick` starts what waits, through the real adapters: the triage
// agent is started here with Scion's argv, and nothing is printed.
func TestTickCommandStartsAnAgent(t *testing.T) {
	e, _, ex, stdout, id := tickEnv(t)
	if err := runTick(t.Context(), e, Args{}); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout)
	}
	starts := ex.scion("start")
	if len(starts) != 1 {
		t.Fatalf("scion starts %v (all: %v)", starts, ex.argv)
	}
	argv := starts[0]
	if !slices.Equal(argv[1:7], []string{"start", "t0001-t1", "-t", "triage", "--message-mode", "none"}) ||
		!strings.Contains(argv[len(argv)-1], "Triage this incoming report") {
		t.Fatalf("argv %q", argv)
	}
	got, err := e.Board().Reader.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Agent.V; a.Name != "t0001-t1" || a.Host != "local" || a.Starting.Set {
		t.Fatalf("agent %+v", a)
	}
}

// A tick while another holds the lock only asks it to go round once more.
func TestTickCommandWhileATickRuns(t *testing.T) {
	e, root, ex, _, _ := tickEnv(t)
	held, err := e.Board().TickLock(t.Context(), store.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	if err := runTick(t.Context(), e, Args{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root.Path(".tick.again")); err != nil {
		t.Fatal(".tick.again not left")
	}
	// Only the git version check ran, and the sysinfo snapshot's
	// podman ps: the factory is alive while a long tick holds the lock.
	if len(ex.argv) != 2 || !slices.Equal(ex.argv[0], []string{"git", "--version"}) ||
		!slices.Equal(ex.argv[1], []string{"podman", "ps", "--format", "{{.Names}}"}) {
		t.Fatalf("ran %v", ex.argv)
	}
}

// Every tick ends by writing the host's vital signs for Foreman.
func TestTickRecordsSysinfo(t *testing.T) {
	e, _, _, _, _ := tickEnv(t)
	if err := runTick(t.Context(), e, Args{}); err != nil {
		t.Fatal(err)
	}
	s, err := sysinfo.Read(sysinfo.File(e.Paths()))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Taken.Equal(testkit.Stamp()) || !slices.Equal(s.Containers, []string{"t0001-t1"}) || s.Agents["local"] != 1 ||
		len(s.Hosts) != 1 || s.Hosts[0].Slots != 2 || s.Memory == nil {
		t.Fatalf("snapshot %+v", s)
	}
}

// A snapshot that cannot be written is logged; the tick still succeeds.
func TestTickSurvivesASysinfoFailure(t *testing.T) {
	e, root, ex, _, id := tickEnv(t)
	path := sysinfo.File(e.Paths())
	if err := os.MkdirAll(filepath.Join(path, "in-the-way"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runTick(t.Context(), e, Args{}); err != nil {
		t.Fatalf("the tick failed: %v", err)
	}
	if len(ex.scion("start")) != 1 {
		t.Fatalf("the tick did not run: %v", ex.argv)
	}
	got, _ := e.Board().Reader.Load(id)
	if a, ok := got.Agent.Get(); !ok || a.Name != "t0001-t1" {
		t.Fatalf("agent %+v", got.Agent)
	}
	log, _ := os.ReadFile(root.Path("logs", "factory.log"))
	if !strings.Contains(string(log), "sysinfo: replace "+path) {
		t.Fatalf("not logged:\n%s", log)
	}
}

// oldGit answers git --version with a git older than 2.48 and records the
// rest.
type oldGit struct{ tickExec }

func (x *oldGit) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if slices.Equal(c.Argv, []string{"git", "--version"}) {
		x.mu.Lock()
		x.argv = append(x.argv, c.Argv)
		x.mu.Unlock()
		return execx.Result{Argv: c.Argv, Stdout: []byte("git version 2.37.1\n")}, nil
	}
	return x.tickExec.Run(ctx, c)
}

// A git older than 2.48 stops the tick before it runs anything, and
// `factory check` lists it as a problem.
func TestTickRefusesAnOldGit(t *testing.T) {
	e, _, _, stdout, id := tickEnv(t)
	ex := &oldGit{}
	e.Exec = ex
	err := runTick(t.Context(), e, Args{})
	if err == nil || !strings.Contains(err.Error(), "git version 2.37.1: the factory needs git 2.48 or newer") {
		t.Fatalf("err = %v", err)
	}
	if len(ex.argv) != 1 {
		t.Fatalf("ran more than git --version: %v", ex.argv)
	}
	got, err := e.Board().Reader.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Column != "triage" || got.Agent.Set && !got.Agent.Null {
		t.Fatalf("the tick ran: column %s, agent %+v", got.Column, got.Agent)
	}
	// The check is once per invocation: a second call does not run git again.
	if err := e.Git(t.Context()); err == nil || len(ex.argv) != 1 {
		t.Fatalf("second check: %v, argv %v", err, ex.argv)
	}
	e2, _, _, out2, _ := tickEnv(t)
	e2.Exec = &oldGit{}
	if err := runCheck(t.Context(), e2, Args{}); err == nil {
		t.Fatal("check passed with git 2.37")
	}
	if !strings.Contains(out2.String(), "the factory needs git 2.48 or newer") {
		t.Fatalf("check printed %q", out2)
	}
	_ = stdout
}
