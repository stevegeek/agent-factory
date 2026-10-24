package scion

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/scionread"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// projectCase is ScionProjectTest's setUp: a git repository with a
// committed .gitignore, and a scion whose init makes the marker and appends
// to .gitignore as the real one does. Git runs for real.
type projectCase struct {
	dir    string
	calls  [][]string
	guards []string
	client Client
}

func newProjectCase(t *testing.T, initMakesProject bool) *projectCase {
	t.Helper()
	repo := testkit.NewRepo(t)
	repo.Write(".gitignore", "/tmp/\n")
	repo.Commit("i")
	pc := &projectCase{dir: repo.Dir}
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_GLOBAL=" + gitconfig, "GIT_CONFIG_NOSYSTEM=1"}
	token := filepath.Join(home, "token")
	if err := os.WriteFile(token, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real := execx.OS{}
	ex := execx.Func(func(ctx context.Context, c execx.Cmd) (execx.Result, error) {
		if c.Argv[0] != "/s/scion" {
			return real.Run(ctx, c) // git
		}
		pc.calls = append(pc.calls, c.Argv[1:])
		if c.Argv[1] == "init" && initMakesProject {
			if err := os.MkdirAll(filepath.Join(c.Dir, ".scion"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c.Dir, ".scion", "project-id"), []byte("id"), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(c.Dir, ".gitignore"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString(".scion/agents/\n")
			f.Close()
		}
		return execx.Result{Argv: c.Argv}, nil
	})
	pc.client = Client{
		Reader:    scionread.Reader{Bin: "/s/scion", Exec: ex, Env: base},
		TokenFile: token,
		Guard: func(_ context.Context, dir string) error {
			pc.guards = append(pc.guards, dir)
			return nil
		},
	}
	return pc
}

func (pc *projectCase) status(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", pc.dir, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return string(out)
}

// port: TF ScionProjectTest.test_a_new_checkout_gets_its_own_scion_project_and_stays_clean
func TestNewCheckoutGetsItsOwnScionProjectAndStaysClean(t *testing.T) {
	pc := newProjectCase(t, true)
	ctx := context.Background()
	if err := pc.client.EnsureProject(ctx, pc.dir); err != nil {
		t.Fatal(err)
	}
	if err := pc.client.EnsureProject(ctx, pc.dir); err != nil { // already there: nothing to do
		t.Fatal(err)
	}
	if want := [][]string{{"init"}}; !reflect.DeepEqual(pc.calls, want) {
		t.Fatalf("scion calls %q, want %q", pc.calls, want)
	}
	if s := pc.status(t); s != "" {
		t.Fatalf("checkout not clean:\n%s", s)
	}
	b, err := os.ReadFile(filepath.Join(pc.dir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l == ".scion/" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf(".scion/ appears %d times in exclude:\n%s", n, b)
	}
	if len(pc.guards) == 0 {
		t.Fatal("the repository was not guarded before scion and git ran in it")
	}
}

// port: TF ScionProjectTest.test_a_repository_without_a_gitignore_stays_clean_too
func TestRepositoryWithoutAGitignoreStaysCleanToo(t *testing.T) {
	pc := newProjectCase(t, true)
	for _, args := range [][]string{{"rm", "-q", ".gitignore"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "r"}} {
		if out, err := exec.Command("git", append([]string{"-C", pc.dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if err := pc.client.EnsureProject(context.Background(), pc.dir); err != nil {
		t.Fatal(err)
	}
	if s := pc.status(t); s != "" {
		t.Fatalf("checkout not clean:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(pc.dir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("the .gitignore scion made is still there: %v", err)
	}
}

// port: TF ScionProjectTest.test_no_agent_starts_when_init_makes_no_project
func TestNoAgentStartsWhenInitMakesNoProject(t *testing.T) {
	pc := newProjectCase(t, false)
	err := pc.client.EnsureProject(context.Background(), pc.dir)
	if err == nil || !strings.Contains(err.Error(), "scion init made no project in "+pc.dir) {
		t.Fatalf("err %v", err)
	}
}

// A failing init is an error too (scion(project, "init") checks).
func TestFailingInitIsAnError(t *testing.T) {
	pc := newProjectCase(t, true)
	pc.client.Exec = execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
		return execx.Result{Argv: c.Argv, Code: 1, Stderr: []byte("boom")}, nil
	})
	if err := pc.client.EnsureProject(context.Background(), pc.dir); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err %v", err)
	}
}

// (security) An agent can write the project's .git: a planted symlink at
// .git/info/exclude must not make the factory append to a file elsewhere.
func TestExcludeSymlinkIsNotFollowed(t *testing.T) {
	pc := newProjectCase(t, true)
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(pc.dir, ".git", "info")
	os.MkdirAll(info, 0o755)
	os.Remove(filepath.Join(info, "exclude"))
	if err := os.Symlink(outside, filepath.Join(info, "exclude")); err != nil {
		t.Fatal(err)
	}
	if err := pc.client.EnsureProject(context.Background(), pc.dir); err == nil {
		t.Fatal("a symlinked exclude was accepted")
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep\n" {
		t.Fatalf("the file outside was written: %q", b)
	}
}

func TestExcludeDirSymlinkIsNotFollowed(t *testing.T) {
	pc := newProjectCase(t, true)
	outside := t.TempDir()
	info := filepath.Join(pc.dir, ".git", "info")
	if err := os.RemoveAll(info); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, info); err != nil {
		t.Fatal(err)
	}
	if err := pc.client.EnsureProject(context.Background(), pc.dir); err == nil {
		t.Fatal("a symlinked .git/info was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "exclude")); !os.IsNotExist(err) {
		t.Fatalf("exclude written outside the repository: %v", err)
	}
}

func TestGuardFailureStopsEverything(t *testing.T) {
	pc := newProjectCase(t, true)
	pc.client.Guard = func(context.Context, string) error { return os.ErrPermission }
	if err := pc.client.EnsureProject(context.Background(), pc.dir); err == nil {
		t.Fatal("no error")
	}
	if len(pc.calls) != 0 {
		t.Fatalf("scion ran after a failed guard: %q", pc.calls)
	}
}
