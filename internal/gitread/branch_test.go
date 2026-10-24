package gitread

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
)

func TestBranchReads(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	f.repo.BranchWith("bot/b", map[string]string{"README.md": "one\ntwo\n", "lib/new.rb": "x = 1\n"})

	for _, tc := range []struct {
		branch string
		want   bool
	}{{"bot/b", true}, {"main", true}, {"bot/none", false}, {"--all", false}} {
		got, err := f.r.BranchExists(ctx, tc.branch)
		if err != nil || got != tc.want {
			t.Errorf("BranchExists(%q) = %v, %v; want %v", tc.branch, got, err, tc.want)
		}
	}
	head, err := f.r.Head(ctx, "bot/b")
	if err != nil || head != f.repo.Git("rev-parse", "refs/heads/bot/b") {
		t.Errorf("Head = %q, %v", head, err)
	}
	if _, err := f.r.Head(ctx, "bot/none"); err == nil || !strings.Contains(err.Error(), "git -C "+f.repo.Dir+" rev-parse failed (128)") {
		t.Errorf("Head of a missing branch: %v", err)
	}
	if n, err := f.r.CommitsAhead(ctx, "bot/b"); err != nil || n != 1 {
		t.Errorf("CommitsAhead = %d, %v", n, err)
	}
	if n, err := f.r.CommitsAhead(ctx, "main"); err != nil || n != 0 {
		t.Errorf("CommitsAhead(main) = %d, %v", n, err)
	}
	files, err := f.r.ChangedFiles(ctx, "bot/b")
	if err != nil || !slices.Equal(files, []string{"README.md", "lib/new.rb"}) {
		t.Errorf("ChangedFiles = %q, %v", files, err)
	}
	none, err := f.r.ChangedFiles(ctx, "main")
	if err != nil || len(none) != 0 {
		t.Errorf("ChangedFiles(main) = %q, %v", none, err)
	}
	added, err := f.r.AddedLines(ctx, "bot/b")
	if err != nil || !slices.Equal(added, []string{"one", "two", "x = 1"}) {
		t.Errorf("AddedLines = %q, %v", added, err)
	}
	cur, err := f.r.CurrentBranch(ctx)
	if err != nil || cur != "main" {
		t.Errorf("CurrentBranch = %q, %v", cur, err)
	}
}

// port: TF BranchTest.test_diff_size_counts_added_and_removed
func TestDiffSizeCountsAddedAndRemoved(t *testing.T) {
	f := newFixture(t)
	f.repo.BranchWith("b", map[string]string{"README.md": "one\ntwo\n"})
	n, err := f.r.DiffSize(ctxT(t), "b")
	if err != nil || n != 3 {
		t.Fatalf("DiffSize = %d, %v; want 3", n, err)
	}
}

func TestDiffSizeCountsBinaryAsZero(t *testing.T) {
	f := newFixture(t)
	f.repo.BranchWith("b", map[string]string{"blob.bin": "\x00\x01\x02binary\x00", "README.md": "hello\nmore\n"})
	stat, err := f.r.Numstat(ctxT(t), "b")
	if err != nil {
		t.Fatal(err)
	}
	want := []NumstatEntry{{Added: 1, Deleted: 0, Path: "README.md"}, {Binary: true, Path: "blob.bin"}}
	if !reflect.DeepEqual(stat, want) {
		t.Errorf("Numstat = %+v; want %+v", stat, want)
	}
	if n, _ := f.r.DiffSize(ctxT(t), "b"); n != 1 {
		t.Errorf("DiffSize = %d; want 1 (binary counts 0)", n)
	}
}

func TestCatFileExistsReadsTheBaseNotTheBranch(t *testing.T) {
	f := newFixture(t)
	ctx := ctxT(t)
	f.repo.BranchWith("bot/x", map[string]string{"REVIEW.md": "planted by the agent\n"})
	if ok, err := f.r.CatFileExists(ctx, "main", "REVIEW.md"); err != nil || ok {
		t.Errorf("REVIEW.md on main = %v, %v; want false", ok, err)
	}
	if ok, err := f.r.CatFileExists(ctx, "bot/x", "REVIEW.md"); err != nil || !ok {
		t.Errorf("REVIEW.md on bot/x = %v, %v; want true", ok, err)
	}
	last := f.rec.argvs()[len(f.rec.argvs())-1]
	if want := []string{"git", "-C", f.repo.Dir, "cat-file", "-e", "bot/x:REVIEW.md"}; !slices.Equal(last, want) {
		t.Errorf("argv %q; want %q", last, want)
	}
}

// The argv of every read, as Python runs it.
func TestReadArgvMatchPython(t *testing.T) {
	var got [][]string
	var envs [][]string
	fake := execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
		got = append(got, c.Argv)
		envs = append(envs, c.Env)
		out := ""
		switch {
		case slices.Contains(c.Argv, "--count"):
			out = "2\n"
		case slices.Contains(c.Argv, "--numstat"):
			out = "1\t2\ta\n-\t-\tb\n"
		}
		return execx.Result{Argv: c.Argv, Stdout: []byte(out)}, nil
	})
	r := &Repo{Path: "/p", Base: "main", Exec: fake, Env: []string{"PATH=/bin"}, Guard: nopGuard{}}
	ctx := context.Background()
	r.BranchExists(ctx, "bot/x")
	r.Head(ctx, "bot/x")
	r.CommitsAhead(ctx, "bot/x")
	r.ChangedFiles(ctx, "bot/x")
	r.DiffSize(ctx, "bot/x")
	r.AddedLines(ctx, "bot/x")
	r.CatFileExists(ctx, "main", "REVIEW.md")
	r.CurrentBranch(ctx)
	want := [][]string{
		{"git", "-C", "/p", "rev-parse", "--verify", "--quiet", "refs/heads/bot/x"},
		{"git", "-C", "/p", "rev-parse", "refs/heads/bot/x"},
		{"git", "-C", "/p", "rev-list", "--count", "main..bot/x"},
		{"git", "-C", "/p", "diff", "--name-only", "main...bot/x"},
		{"git", "-C", "/p", "diff", "--numstat", "main...bot/x"},
		{"git", "-C", "/p", "diff", "-U0", "main...bot/x"},
		{"git", "-C", "/p", "cat-file", "-e", "main:REVIEW.md"},
		{"git", "-C", "/p", "branch", "--show-current"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv:\n%q\nwant\n%q", got, want)
	}
	for i, e := range envs {
		if !slices.Equal(e, SafeEnv([]string{"PATH=/bin"})) {
			t.Errorf("call %d env %q", i, e)
		}
	}
}

type nopGuard struct{}

func (nopGuard) Check(context.Context, string, string) error { return nil }

type refuseGuard struct{}

func (refuseGuard) Check(context.Context, string, string) error { return errors.New("refused") }

// Every read runs the guard first, and a guard refusal stops the git.
func TestEveryReadIsGuarded(t *testing.T) {
	ran := 0
	fake := execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
		ran++
		return execx.Result{Argv: c.Argv}, nil
	})
	r := &Repo{Path: "/p", Base: "main", Exec: fake, Guard: refuseGuard{}}
	ctx := context.Background()
	calls := map[string]func() error{
		"BranchExists":   func() error { _, err := r.BranchExists(ctx, "b"); return err },
		"Head":           func() error { _, err := r.Head(ctx, "b"); return err },
		"CommitsAhead":   func() error { _, err := r.CommitsAhead(ctx, "b"); return err },
		"ChangedFiles":   func() error { _, err := r.ChangedFiles(ctx, "b"); return err },
		"Numstat":        func() error { _, err := r.Numstat(ctx, "b"); return err },
		"DiffSize":       func() error { _, err := r.DiffSize(ctx, "b"); return err },
		"AddedLines":     func() error { _, err := r.AddedLines(ctx, "b"); return err },
		"KeptFiles":      func() error { _, err := r.KeptFiles(ctx, "b"); return err },
		"TouchedFiles":   func() error { _, err := r.TouchedFiles(ctx, "b"); return err },
		"PublishedLines": func() error { _, err := r.PublishedLines(ctx, "b"); return err },
		"Messages":       func() error { _, err := r.Messages(ctx, "b"); return err },
		"CatFileExists":  func() error { _, err := r.CatFileExists(ctx, "main", "f"); return err },
		"CurrentBranch":  func() error { _, err := r.CurrentBranch(ctx); return err },
		"Checked":        func() error { return r.Checked(ctx) },
	}
	for name, call := range calls {
		if err := call(); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s with a refusing guard: %v", name, err)
		}
	}
	if ran != 0 {
		t.Errorf("%d git commands ran past a refusing guard", ran)
	}
	// every exported method is in the table above (a new one must be added, and guarded)
	typ := reflect.TypeOf(r)
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if _, ok := calls[name]; !ok && name != "BaseEnv" {
			t.Errorf("method %s is not covered by the guard test", name)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		out  string
		ok   bool
		want string
	}{
		{"git version 2.48.0\n", true, "2.48.0"},
		{"git version 2.56.0\n", true, "2.56.0"},
		{"git version 3.0.1.windows.1\n", true, "3.0.1.windows.1"},
		{"git version 2.47.9\n", false, ""},
		{"git version 2.9\n", false, ""},
		{"git version 1.99\n", false, ""},
		{"nonsense\n", false, ""},
	} {
		fake := execx.Func(func(_ context.Context, c execx.Cmd) (execx.Result, error) {
			if !slices.Equal(c.Argv, []string{"git", "--version"}) {
				t.Errorf("argv %q", c.Argv)
			}
			return execx.Result{Argv: c.Argv, Stdout: []byte(tc.out)}, nil
		})
		v, err := CheckVersion(context.Background(), fake, nil)
		if tc.ok && (err != nil || v != tc.want) {
			t.Errorf("%q: %q, %v", tc.out, v, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "2.48")) {
			t.Errorf("%q: want a refusal naming 2.48, got %q, %v", tc.out, v, err)
		}
	}
	// the real git on this machine is new enough for the tests to mean anything
	if _, err := CheckVersion(context.Background(), execx.OS{}, nil); err != nil {
		t.Skipf("local git: %v", err)
	}
}
