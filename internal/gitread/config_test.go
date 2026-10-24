package gitread

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
)

func TestSafeEnv(t *testing.T) {
	base := []string{"PATH=/bin", "GIT_CONFIG_COUNT=7", "GIT_CONFIG_KEY_0=core.pager", "HOME=/h", "GIT_CONFIG_NOSYSTEM=0"}
	got := SafeEnv(base)
	want := []string{"PATH=/bin", "HOME=/h",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=protocol.ext.allow", "GIT_CONFIG_VALUE_2=never"}
	if !slices.Equal(got, want) {
		t.Errorf("SafeEnv:\n%q\nwant\n%q", got, want)
	}
	if !slices.Equal(base, []string{"PATH=/bin", "GIT_CONFIG_COUNT=7", "GIT_CONFIG_KEY_0=core.pager", "HOME=/h", "GIT_CONFIG_NOSYSTEM=0"}) {
		t.Errorf("SafeEnv changed its argument: %q", base)
	}
	if !reflect.DeepEqual(SafeConfig(), [][2]string{{"core.hooksPath", "/dev/null"}, {"core.fsmonitor", "false"}, {"protocol.ext.allow", "never"}}) {
		t.Errorf("SafeConfig = %q", SafeConfig())
	}
}

// The REPO_CONFIG_OK port against Python's own answers (re.match).
func TestRepoConfigOKAgainstPython(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "textx", "testdata", "py", "regex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Patterns []struct {
			Name  string
			Cases []struct {
				In  string
				Out json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range golden.Patterns {
		if p.Name != "repo_config_ok" {
			continue
		}
		for _, c := range p.Cases {
			n++
			var out []*string
			if err := json.Unmarshal(c.Out, &out); err != nil {
				t.Fatalf("%q: %v", c.In, err)
			}
			loc := matchConfigKey(c.In)
			if (loc != nil) != (out != nil) {
				t.Errorf("%q: match %v; Python %v", c.In, loc != nil, out != nil)
				continue
			}
			if loc == nil {
				continue
			}
			for g, want := range out {
				var got *string
				if loc[2*g] >= 0 {
					s := c.In[loc[2*g]:loc[2*g+1]]
					got = &s
				}
				if (got == nil) != (want == nil) || got != nil && *got != *want {
					t.Errorf("%q group %d: %v; Python %v", c.In, g, deref(got), deref(want))
				}
			}
		}
	}
	if n < 20 {
		t.Fatalf("only %d golden cases for repo_config_ok", n)
	}
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func TestKeepEntry(t *testing.T) {
	for _, tc := range []struct {
		key, value, github string
		keep               bool
	}{
		{"core.bare", "false", "", true},
		{"Core.Bare", "false", "", true},
		{"core.hooksPath", "/x", "", false},
		{"remote.origin.url", "https://evil.example/o/r.git", "", true},
		{"remote.origin.url", "https://evil.example/o/r.git", "o/r", false},
		{"remote.origin.url", "https://github.com/o/r.git", "o/r", true},
		{"Remote.Origin.URL", "https://github.com/o/r", "o/r", false},
		{"branch.main.merge", "refs/heads/main", "o/r", true},
		{"branch.main.pushremote", "x", "", false},
		{"url.https://evil/.insteadof", "https://github.com/", "", false},
		{"include.path", "/tmp/x", "", false},
		{"core.fsmonitor", "true", "", false},
	} {
		if got := KeepEntry(tc.key, tc.value, tc.github); got != tc.keep {
			t.Errorf("KeepEntry(%q, %q, %q) = %v", tc.key, tc.value, tc.github, got)
		}
	}
}

func plant(t *testing.T, f *fixture, kv ...string) {
	t.Helper()
	for i := 0; i < len(kv); i += 2 {
		f.repo.Git("config", kv[i], kv[i+1])
	}
}

func TestInspect(t *testing.T) {
	ctx := ctxT(t)
	t.Run("clean", func(t *testing.T) {
		f := newFixture(t)
		cleanConfig(f)
		in, err := Inspect(ctx, f.rec, f.env, f.repo.Dir, "")
		if err != nil || !in.Present || !in.Plain || len(in.Removed) != 0 {
			t.Fatalf("clean: %+v, %v", in, err)
		}
		keys := []string{}
		for _, e := range in.Kept {
			keys = append(keys, e.Key)
		}
		if !slices.Contains(keys, "core.repositoryformatversion") {
			t.Errorf("kept %q", keys)
		}
		want := []string{"git", "config", "--file", filepath.Join(f.repo.Dir, ".git", "config"), "--no-includes", "--list", "-z"}
		if got := f.rec.argvs(); len(got) != 1 || !slices.Equal(got[0], want) {
			t.Errorf("argv %q; want %q", got, want)
		}
		if f.rec.cmds[0].Dir != "/" {
			t.Errorf("dir %q; want /", f.rec.cmds[0].Dir)
		}
	})
	t.Run("planted", func(t *testing.T) {
		f := newFixture(t)
		plant(t, f, "core.fsmonitor", "touch x", "include.path", "/tmp/elsewhere", "remote.origin.url", "https://evil.example/o/r.git")
		in, err := Inspect(ctx, f.rec, f.env, f.repo.Dir, "o/r")
		if err != nil {
			t.Fatal(err)
		}
		// the testkit repo sets user.* and commit.gpgsign in .git/config: a plain clone has none of them
		want := []string{"core.fsmonitor", "user.email", "user.name", "commit.gpgsign", "include.path", "remote.origin.url"}
		if !slices.Equal(in.Removed, want) {
			t.Errorf("removed %q; want %q", in.Removed, want)
		}
	})
	t.Run("symlinked config", func(t *testing.T) {
		f := newFixture(t)
		cfg := filepath.Join(f.repo.Dir, ".git", "config")
		elsewhere := filepath.Join(t.TempDir(), "evil")
		os.WriteFile(elsewhere, []byte("[core]\n\tfsmonitor = touch x\n"), 0o644)
		os.Remove(cfg)
		if err := os.Symlink(elsewhere, cfg); err != nil {
			t.Fatal(err)
		}
		in, err := Inspect(ctx, f.rec, f.env, f.repo.Dir, "")
		if err != nil || in.Plain || !slices.Equal(in.Removed, []string{".git/config was not a plain file"}) || len(in.Kept) != 0 {
			t.Errorf("symlink: %+v, %v", in, err)
		}
		if len(f.rec.argvs()) != 0 {
			t.Errorf("a symlinked config was read through: %q", f.rec.argvs())
		}
	})
	t.Run("fifo config", func(t *testing.T) {
		f := newFixture(t)
		cfg := filepath.Join(f.repo.Dir, ".git", "config")
		os.Remove(cfg)
		if err := syscall.Mkfifo(cfg, 0o644); err != nil {
			t.Skip(err)
		}
		in, err := Inspect(ctx, f.rec, f.env, f.repo.Dir, "")
		if err != nil || in.Plain || !slices.Equal(in.Removed, []string{".git/config was not a plain file"}) {
			t.Errorf("fifo: %+v, %v", in, err)
		}
	})
	t.Run("no config", func(t *testing.T) {
		f := newFixture(t)
		os.Remove(filepath.Join(f.repo.Dir, ".git", "config"))
		in, err := Inspect(ctx, f.rec, f.env, f.repo.Dir, "")
		if err != nil || in.Present || len(in.Removed) != 0 {
			t.Errorf("no config: %+v, %v", in, err)
		}
	})
	t.Run("no .git", func(t *testing.T) {
		dir := t.TempDir()
		in, err := Inspect(ctx, execx.OS{}, nil, dir, "")
		if err != nil || in.Present {
			t.Errorf("no .git: %+v, %v", in, err)
		}
	})
	// quirk 11 (§13.2): Python skipped a .git that is not a directory; the port refuses it
	t.Run(".git file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644)
		if _, err := Inspect(ctx, execx.OS{}, nil, dir, ""); err == nil || !strings.Contains(err.Error(), "unexpected .git file") {
			t.Errorf(".git file: %v", err)
		}
	})
	t.Run(".git symlink", func(t *testing.T) {
		f := newFixture(t)
		dir := t.TempDir()
		os.Symlink(filepath.Join(f.repo.Dir, ".git"), filepath.Join(dir, ".git"))
		if _, err := Inspect(ctx, execx.OS{}, nil, dir, ""); err == nil || !strings.Contains(err.Error(), "unexpected .git") {
			t.Errorf(".git symlink: %v", err)
		}
	})
}

func TestVerifierRefusesAPlantedConfigAndChangesNothing(t *testing.T) {
	ctx := ctxT(t)
	f := newFixture(t)
	cleanConfig(f)
	v := Verifier{Exec: f.rec, Env: f.env}
	if err := v.Check(ctx, f.repo.Dir, ""); err != nil {
		t.Fatalf("clean config refused: %v", err)
	}
	plant(t, f, "diff.external", "touch pwned")
	cfg := filepath.Join(f.repo.Dir, ".git", "config")
	before, _ := os.ReadFile(cfg)
	err := v.Check(ctx, f.repo.Dir, "")
	if err == nil || !strings.Contains(err.Error(), "diff.external") {
		t.Fatalf("planted config: %v", err)
	}
	var refusal *ConfigRefusal
	if !errors.As(err, &refusal) || refusal.Path != f.repo.Dir || !slices.Equal(refusal.Removed, []string{"diff.external"}) {
		t.Errorf("refusal %#v", refusal)
	}
	after, _ := os.ReadFile(cfg)
	if string(before) != string(after) {
		t.Errorf("the verifier changed .git/config")
	}
	// a Repo with no Guard verifies: a planted config stops a read
	r := &Repo{Path: f.repo.Dir, Base: "main", Exec: f.rec, Env: f.env}
	if _, err := r.Head(ctx, "main"); err == nil || !strings.Contains(err.Error(), "diff.external") {
		t.Errorf("unguarded read on a planted repo: %v", err)
	}
}

// §10.7 S7a: gitread has no method that runs a write verb. Every git argv
// its methods ran in these tests is one of the read-only forms.
func TestNoWriteVerb(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	f.repo.BranchWith("bot/b", map[string]string{"README.md": "x\n"})
	cleanConfig(f)
	r := &Repo{Path: f.repo.Dir, Base: "main", Exec: f.rec, Env: f.env}
	r.BranchExists(ctx, "bot/b")
	r.Head(ctx, "bot/b")
	r.CommitsAhead(ctx, "bot/b")
	r.ChangedFiles(ctx, "bot/b")
	r.Numstat(ctx, "bot/b")
	r.DiffSize(ctx, "bot/b")
	r.AddedLines(ctx, "bot/b")
	r.KeptFiles(ctx, "bot/b")
	r.TouchedFiles(ctx, "bot/b")
	r.PublishedLines(ctx, "bot/b")
	r.Messages(ctx, "bot/b")
	r.CatFileExists(ctx, "main", "README.md")
	r.CurrentBranch(ctx)
	r.Checked(ctx)
	CheckVersion(ctx, f.rec, f.env)
	Inspect(ctx, f.rec, f.env, f.repo.Dir, "")
	readOnly := [][]string{
		{"rev-parse"}, {"rev-list", "--count"}, {"diff", "--name-only"}, {"diff", "--numstat"}, {"diff", "-U0"},
		{"diff", "-U0", "--text"}, {"log", "--format=%B%x00"}, {"log", "--name-only"}, {"log", "-p"},
		{"cat-file", "-e"}, {"branch", "--show-current"}, {"--version"},
		{"config", "--file", "", "--no-includes", "--list", "-z"},
	}
	argvs := f.rec.argvs()
	if len(argvs) < 12 {
		t.Fatalf("only %d calls recorded", len(argvs))
	}
	for _, argv := range argvs {
		if argv[0] != "git" {
			t.Errorf("ran %q", argv)
			continue
		}
		rest := argv[1:]
		if len(rest) >= 2 && rest[0] == "-C" {
			rest = rest[2:]
		}
		ok := false
		for _, form := range readOnly {
			if len(rest) >= len(form) && matchForm(rest[:len(form)], form) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("gitread ran a git that is not a known read: %q", argv)
		}
	}
	// and the repository is as it was: no ref, config or index change
	if out := f.repo.Git("status", "--porcelain"); out != "" {
		t.Errorf("work tree changed: %q", out)
	}
}

func matchForm(got, form []string) bool {
	for i := range form {
		if form[i] != "" && got[i] != form[i] {
			return false
		}
	}
	return true
}

// A planted fsmonitor in the repo's config never runs under SafeEnv.
func TestSafeEnvOutranksRepoConfig(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	hook := filepath.Join(f.repo.Dir, ".git", "hooks", "post-checkout")
	os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	f.repo.Git("config", "core.fsmonitor", "touch "+marker+"; false")
	cmd := exec.Command("git", "-C", f.repo.Dir, "checkout", "-q", "-b", "bot/z")
	cmd.Env = SafeEnv(f.env)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	cmd = exec.Command("git", "-C", f.repo.Dir, "status")
	cmd.Env = SafeEnv(f.env)
	cmd.Run()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a planted hook or fsmonitor ran under SafeEnv")
	}
}

// cleanConfig drops what testkit's repository adds to .git/config beyond a
// plain clone's (its committer identity), once its commits are made.
func cleanConfig(f *fixture) {
	f.repo.Git("config", "--unset", "user.name")
	f.repo.Git("config", "--unset", "user.email")
	f.repo.Git("config", "--unset", "commit.gpgsign")
}
