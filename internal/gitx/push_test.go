package gitx

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gitread"
)

type captured struct{ cmds []execx.Cmd }

func (c *captured) Run(_ context.Context, cmd execx.Cmd) (execx.Result, error) {
	c.cmds = append(c.cmds, cmd)
	return execx.Result{Argv: cmd.Argv}, nil
}

func TestPushKeepsTheTokenOutOfArgvAndGoesToAnExplicitURL(t *testing.T) {
	cap := &captured{}
	base := []string{"PATH=/bin", "FACTORY_GH_TOKEN=stale", "GIT_TERMINAL_PROMPT=1"}
	w := Writer{Repo: &gitread.Repo{Path: "/p", Base: "main", GitHub: "o/r", Exec: cap, Env: base, Guard: noGuard{}}}
	if err := w.Push(context.Background(), "bot/x", "secret-token"); err != nil {
		t.Fatal(err)
	}
	if len(cap.cmds) != 1 {
		t.Fatalf("ran %d commands", len(cap.cmds))
	}
	c := cap.cmds[0]
	want := []string{"git", "-C", "/p", "-c", "credential.helper=", "-c",
		`credential.helper=!f() { echo username=x-access-token; echo "password=$FACTORY_GH_TOKEN"; }; f`,
		"push", "--force", "--quiet", "https://github.com/o/r.git", "refs/heads/bot/x:refs/heads/bot/x"}
	if !slices.Equal(c.Argv, want) {
		t.Errorf("argv\n%q\nwant\n%q", c.Argv, want)
	}
	if strings.Contains(strings.Join(c.Argv, " "), "secret-token") {
		t.Error("the token is in argv")
	}
	wantEnv := gitread.SafeEnv([]string{"PATH=/bin", "FACTORY_GH_TOKEN=secret-token", "GIT_TERMINAL_PROMPT=0"})
	if !slices.Equal(c.Env, wantEnv) {
		t.Errorf("env %q; want %q", c.Env, wantEnv)
	}
	if c.Timeout != 300*time.Second {
		t.Errorf("timeout %v", c.Timeout)
	}
}

func TestPushRefusesAnythingButBotBranches(t *testing.T) {
	for _, branch := range []string{"main", "bot", "Bot/x", "-bot/x", "feature/bot/x", ""} {
		cap := &captured{}
		w := Writer{Repo: &gitread.Repo{Path: "/p", Base: "main", GitHub: "o/r", Exec: cap, Guard: noGuard{}}}
		err := w.Push(context.Background(), branch, "tok")
		if err == nil || err.Error() != "refusing to push "+branch+": the factory only pushes bot/* branches" {
			t.Errorf("push %q: %v", branch, err)
		}
		if len(cap.cmds) != 0 {
			t.Errorf("push %q ran %q", branch, cap.cmds[0].Argv)
		}
	}
	cap := &captured{}
	w := Writer{Repo: &gitread.Repo{Path: "/p", Base: "main", Exec: cap, Guard: noGuard{}}}
	if err := w.Push(context.Background(), "bot/x", "tok"); err == nil || len(cap.cmds) != 0 {
		t.Errorf("push with no GitHub repository: %v", err)
	}
	refused := &gitread.Repo{Path: "/p", Base: "main", GitHub: "o/r", Exec: cap, Guard: refuseGuard{}}
	if err := (Writer{Repo: refused}).Push(context.Background(), "bot/x", "tok"); err == nil || len(cap.cmds) != 0 {
		t.Errorf("push past a refusing guard: %v", err)
	}
}

// The credential helper really hands git the token from the environment.
func TestPushCredentialHelperAnswersFromTheEnvironment(t *testing.T) {
	f := newFixture(t)
	cmd := exec.Command("git", "-c", "credential.helper=", "-c",
		`credential.helper=!f() { echo username=x-access-token; echo "password=$FACTORY_GH_TOKEN"; }; f`,
		"credential", "fill")
	cmd.Dir = f.repo.Dir
	cmd.Env = gitread.SafeEnv(append(f.env, "FACTORY_GH_TOKEN=tok-123", "GIT_TERMINAL_PROMPT=0"))
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=o/r.git\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("credential fill: %v", err)
	}
	if !strings.Contains(string(out), "username=x-access-token\n") || !strings.Contains(string(out), "password=tok-123\n") {
		t.Errorf("credential fill gave %q", out)
	}
}

func TestDeleteBranch(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	f.repo.BranchWith("bot/old", map[string]string{"x": "x\n"})
	if err := f.w.DeleteBranch(ctx, "main"); err == nil {
		t.Error("deleted main")
	}
	if err := f.w.DeleteBranch(ctx, "bot/old"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.r.BranchExists(ctx, "bot/old"); ok {
		t.Error("bot/old still exists")
	}
}

type refuseGuard struct{}

func (refuseGuard) Check(context.Context, string, string) error { return errRefused }
