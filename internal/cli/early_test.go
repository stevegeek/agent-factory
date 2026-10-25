package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// earlyExec answers podman's image inspect and run, and runs git for real.
type earlyExec struct {
	tickExec
}

func (x *earlyExec) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if c.Argv[0] == "podman" {
		x.mu.Lock()
		x.argv = append(x.argv, c.Argv)
		x.mu.Unlock()
		if slices.Contains(c.Argv, "inspect") {
			return execx.Result{Argv: c.Argv, Stdout: []byte(strings.Repeat("a", 64) + "\n")}, nil
		}
		return execx.Result{Argv: c.Argv, Stdout: []byte("checks ok\n")}, nil
	}
	return x.tickExec.Run(ctx, c)
}

func (x *earlyExec) podmanRuns() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, a := range x.argv {
		if a[0] == "podman" && len(a) > 1 && a[1] == "run" {
			n++
		}
	}
	return n
}

// earlyEnv is a factory with a gateable project and a ticket in review on
// bot/x; head is the branch's head.
func earlyEnv(t *testing.T, extra string) (e *Env, ex *earlyExec, id ticket.ID, head string) {
	t.Helper()
	root := testkit.Root(t)
	repo := testkit.NewRepo(t)
	repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
	head = repo.Git("rev-parse", "bot/x")
	// a checkout the tick's guard has cleaned: the early checks only verify
	for _, k := range []string{"user.email", "user.name", "commit.gpgsign"} {
		repo.Git("config", "--unset", k)
	}
	root.WriteConfig(t, "image = \"localhost/gate:test\"\n\n[projects.p]\npath = \""+repo.Dir+"\"\nbase = \"main\"\n"+
		"install = \"true\"\ntest = \"true\"\nlint = \"true\"\n"+extra)
	vars := map[string]string{}
	for k, v := range root.Vars {
		vars[k] = v
	}
	ex = &earlyExec{}
	var stdout, stderr bytes.Buffer
	e = &Env{Stdout: &stdout, Stderr: &stderr, Environ: vars, Exec: ex, Clock: testkit.Fixed{T: testkit.Stamp()}}
	e.init()
	tk, err := e.Board().FileTicket(t.Context(), ticket.NewIn{Project: "p", Title: "A change", Body: "b", Door: "human", Column: "review"})
	if err != nil {
		t.Fatal(err)
	}
	err = e.Board().Changing(t.Context(), string(tk.ID), store.NoWait, func(t *ticket.Ticket) error {
		t.Branch = ticket.Some("bot/x")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, ex, tk.ID, head
}

// `factory early-checks T HEAD` runs Gate 1's checks of the head in the
// gate image and keeps the result for the gate; it never writes the
// ticket.
func TestEarlyChecksCommand(t *testing.T) {
	e, ex, id, head := earlyEnv(t, "")
	before, err := os.ReadFile(e.Paths().TicketFile(string(id)))
	if err != nil {
		t.Fatal(err)
	}
	if err := runEarlyChecks(t.Context(), e, Args{pos: map[string]string{"ticket": string(id), "head": head}}); err != nil {
		t.Fatal(err)
	}
	if ex.podmanRuns() != 1 {
		t.Fatalf("ran %q", ex.argv)
	}
	kept, err := os.ReadFile(filepath.Join(e.Paths().EarlyChecks(), string(id)+".json"))
	if err != nil || !strings.Contains(string(kept), head) || !strings.Contains(string(kept), "checks ok") {
		t.Fatalf("kept %s %v", kept, err)
	}
	if after, _ := os.ReadFile(e.Paths().TicketFile(string(id))); string(after) != string(before) {
		t.Error("the early checks wrote the ticket")
	}
	cfg, err := e.Config()
	if err != nil {
		t.Fatal(err)
	}
	if d := tickDeps(e, cfg); d.Early == nil {
		t.Error("the tick starts no early checks")
	}
}

// A paused project's ticket, or a board project's, is not checked early.
func TestEarlyChecksCommandSkipsPausedProjects(t *testing.T) {
	for _, extra := range []string{"enabled = false\n", "mode = \"board\"\n"} {
		e, ex, id, head := earlyEnv(t, extra)
		if err := runEarlyChecks(t.Context(), e, Args{pos: map[string]string{"ticket": string(id), "head": head}}); err != nil {
			t.Fatalf("%s: %v", extra, err)
		}
		if ex.podmanRuns() != 0 || len(mustDir(t, e.Paths().EarlyChecks())) != 0 {
			t.Errorf("%s: checked early", extra)
		}
	}
}

func mustDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() != ".lock" {
			out = append(out, e.Name())
		}
	}
	return out
}
