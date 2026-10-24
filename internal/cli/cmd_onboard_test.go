package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/onboard"
)

const kindsConfig = `[kinds.ext-shard]
from = "crystal-shard"
gate_memory = "1800m"

[kinds.ruby-gem]
lint = "true"

[projects.gem]
path = "/p/gem"
github = "o/gem"
base = "main"
install = "bundle install"
test = "rake"
lint = "standardrb"
pinned_untracked = ["Gemfile.lock"]

[projects.ext]
kind = "ext-shard"
path = "/p/ext"
github = "o/ext"
base = "main"

[projects.desk]
mode = "board"
`

func TestKindsListsSourceAndInferred(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, kindsConfig)
	code, out, errs := f.run("", "kinds")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	for _, want := range []string{
		"ruby-gem       config  ruby",
		"crystal-shard  core    crystal",
		"ext-shard      config  crystal  from crystal-shard",
		"gem  ruby-gem (inferred)  ruby",
		"ext  ext-shard  crystal",
		"    install      shards check || shards install",
		"    gate_memory  1800m",
		"    pinned       lib shard.lock",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("no line %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "desk") {
		t.Errorf("a board project is listed:\n%s", out)
	}
}

func TestKindsJSONStable(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, kindsConfig)
	code, out, errs := f.run("", "kinds", "--json")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	want := `{"kinds": [` +
		`{"name": "ruby-gem", "source": "config", "toolchain": "ruby", "from": ""}, ` +
		`{"name": "rails-gem", "source": "core", "toolchain": "ruby", "from": ""}, ` +
		`{"name": "crystal-shard", "source": "core", "toolchain": "crystal", "from": ""}, ` +
		`{"name": "ext-shard", "source": "config", "toolchain": "crystal", "from": "crystal-shard"}], ` +
		`"projects": [` +
		`{"project": "gem", "kind": "ruby-gem", "inferred": true, "toolchain": "ruby", "language": "Ruby gem", ` +
		`"install": "bundle install", "test": "rake", "lint": "standardrb", "gate_memory": "900m", "pinned_untracked": ["Gemfile.lock"]}, ` +
		`{"project": "ext", "kind": "ext-shard", "inferred": false, "toolchain": "crystal", "language": "Crystal shard", ` +
		`"install": "shards check || shards install", "test": "crystal spec", "lint": "crystal tool format --check src spec && ameba src spec", ` +
		`"gate_memory": "1800m", "pinned_untracked": ["lib", "shard.lock"]}]}` + "\n"
	if out != want {
		t.Errorf("kinds --json\n got %s\nwant %s", out, want)
	}
}

// onboardRun runs argv through the router with factory onboard wired by o.
func onboardRun(t *testing.T, ctx context.Context, f *fixture, o onboardCLI, argv ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	e := &Env{Stdout: &out, Stderr: &errb, Environ: f.env.Vars, Exec: &fakeExec{}}
	e.init()
	cmds := commands()
	for i := range cmds {
		if cmds[i].Name == "onboard" {
			cmds[i].Run = o.onboard
		}
	}
	code := dispatch(ctx, e, cmds, argv)
	return code, out.String(), errb.String()
}

// fakeOnboard is Deps with every check answered: the checkout's head,
// no App, and checks that call during (when set) and then answer green.
// Checks is set whether or not --checks is given: dropping it without the
// flag is onboard's job.
func fakeOnboard(during func()) onboardCLI {
	return onboardCLI{Deps: func(e *Env, cfg *config.Config, checks bool) onboard.Deps {
		return onboard.Deps{Config: cfg, Paths: e.Paths(),
			Head: func(context.Context, config.Project) (string, error) { return "0123456789abcdef", nil },
			Checks: func(context.Context, config.Project, string) (bool, string, error) {
				if during != nil {
					during()
				}
				return true, "", nil
			}}
	}}
}

func TestOnboardPrintsAndExits(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, kindsConfig)
	f.env.WriteFile(t, "context/ext/INDEX.md", "# ext\n")
	code, out, errs := onboardRun(t, t.Context(), f, fakeOnboard(nil), "onboard", "ext")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	for _, want := range []string{
		"ok    kind        ext-shard\n",
		"ok    checkout    main at 0123456789ab\n",
		"skip  github      no [github] App in config\n",
		"skip  main        not run (--checks runs it)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	code, out, _ = onboardRun(t, t.Context(), f, fakeOnboard(nil), "onboard", "ext", "--checks", "--json")
	if code != 0 || !strings.HasPrefix(out, `{"project": "ext", "kind": "ext-shard", "inferred": false, "ok": true, "items": [`) ||
		!strings.Contains(out, `{"check": "main", "status": "ok", "detail": "the checks pass at 0123456789ab"}`) || !strings.HasSuffix(out, "]}\n") {
		t.Errorf("--json: %d %s", code, out)
	}
	// gem's kind is inferred (a warning); with no INDEX.md that is a
	// warning too, so it still exits 0. A missing command fails it.
	f.env.WriteConfig(t, kindsConfig+"\n[projects.bare]\npath = \"/p/bare\"\nbase = \"main\"\n")
	code, out, _ = onboardRun(t, t.Context(), f, fakeOnboard(nil), "onboard", "gem")
	if code != 0 || !strings.Contains(out, "warn  kind        inferred from language; set kind = \"ruby-gem\" "+
		"(note: [kinds.ruby-gem] in config overrides the built-in the project uses now)\n") {
		t.Errorf("gem: %d %s", code, out)
	}
	code, out, _ = onboardRun(t, t.Context(), f, fakeOnboard(nil), "onboard", "bare")
	if code != 1 || !strings.Contains(out, "fail  commands    missing: install, test, lint\n") {
		t.Errorf("bare: %d %s", code, out)
	}
	code, _, errs = onboardRun(t, t.Context(), f, fakeOnboard(nil), "onboard", "desk")
	if code != 1 || !strings.Contains(errs, "board project") {
		t.Errorf("desk: %d %q", code, errs)
	}
}

func TestOnboardChecksTakesTickLock(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, kindsConfig)
	lockPath := f.env.Path(".tick.lock")
	held := false
	during := func() {
		l, err := fsx.Lock(context.Background(), lockPath, fsx.NoWait)
		if err == nil {
			l.Unlock()
		}
		held = err != nil
	}
	if code, out, errs := onboardRun(t, t.Context(), f, fakeOnboard(during), "onboard", "ext", "--checks"); code != 0 {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if !held {
		t.Error("the tick lock was free while the checks ran")
	}
	l, err := fsx.Lock(context.Background(), lockPath, fsx.NoWait)
	if err != nil {
		t.Fatalf("the tick lock is still held after onboard: %v", err)
	}
	l.Unlock()
}

func TestOnboardWithoutChecksTakesNoLock(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, kindsConfig)
	l, err := fsx.Lock(context.Background(), f.env.Path(".tick.lock"), fsx.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ran := false
	code, out, errs := onboardRun(t, ctx, f, fakeOnboard(func() { ran = true }), "onboard", "ext")
	if code != 0 || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Errorf("onboard waited for a running tick: %d %q %q", code, out, errs)
	}
	if ran || !strings.Contains(out, "skip  main        not run (--checks runs it)\n") {
		t.Errorf("the checks ran without --checks: %q", out)
	}
}

// The real Deps read the base's head through the gate's git, ask no App
// without [github], and run nothing without --checks.
func TestOnboardRealDepsReadTheHead(t *testing.T) {
	f := newFixture(t)
	f.env.WriteConfig(t, kindsConfig)
	ex := &fakeExec{answer: func(argv []string) (int, string) {
		if slices.Contains(argv, "rev-parse") {
			return 0, "0123456789abcdef0123456789abcdef01234567\n"
		}
		return 0, ""
	}}
	code, out, errs := opsRun(t, f, opsCLI{}, ex, "onboard", "ext")
	if code != 0 || !strings.Contains(out, "ok    checkout    main at 0123456789ab\n") {
		t.Errorf("%d %q %q (ran %q)", code, out, errs, ex.argv)
	}
	if ex.ran("podman") {
		t.Errorf("podman ran without --checks: %q", ex.argv)
	}
}

// With --checks the real Deps run the gate's checks on the head, detached,
// in a fresh clone under gate/onboard-<key> and a container of that name:
// a gate ID no ticket can have (a ticket's checks never share it).
func TestOnboardRealDepsChecksTheHead(t *testing.T) {
	f := newFixture(t)
	src := t.TempDir() // the pinned lib and shard.lock are copied from it when present
	f.env.WriteConfig(t, "image = \"gate:1\"\n"+strings.Replace(kindsConfig, `path = "/p/ext"`, `path = "`+src+`"`, 1))
	const sha = "0123456789abcdef0123456789abcdef01234567"
	ex := &fakeExec{answer: func(argv []string) (int, string) {
		switch {
		case slices.Contains(argv, "rev-parse"):
			return 0, sha + "\n"
		case slices.Contains(argv, "clone"):
			os.MkdirAll(argv[len(argv)-1], 0o755) // what the clone leaves
		}
		return 0, ""
	}}
	code, out, errs := opsRun(t, f, opsCLI{}, ex, "onboard", "ext", "--checks")
	if code != 0 || !strings.Contains(out, "ok    main        the checks pass at 0123456789ab\n") {
		t.Errorf("%d %q %q (ran %q)", code, out, errs, ex.argv)
	}
	wt := filepath.Join(f.env.Path("gate"), "onboard-ext")
	var cloned, detached, named bool
	for _, a := range ex.argv {
		switch {
		case slices.Contains(a, "clone"):
			cloned = a[len(a)-2] == src && a[len(a)-1] == wt
		case slices.Contains(a, "checkout"):
			detached = slices.Equal(a, []string{"git", "-C", wt, "checkout", "-q", "--detach", sha})
		case a[0] == "podman" && slices.Contains(a, "run"):
			i := slices.Index(a, "--name")
			named = i > 0 && a[i+1] == "factory-gate-onboard-ext" && slices.Contains(a, "gate:1") // the config's image
		}
	}
	if !cloned || !detached || !named {
		t.Errorf("clone %v, detached %v, container named %v in %q", cloned, detached, named, ex.argv)
	}
}

// onboard never writes the checkout, and no tick cleans a project it is
// not working on, so a refused .git/config says how to clean it by hand.
func TestOnboardRefusedConfigSaysWhatToRemove(t *testing.T) {
	f := newFixture(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.env.WriteConfig(t, kindsConfig+"\n[projects.raw]\nkind = \"ruby-gem\"\npath = \""+repo+"\"\ngithub = \"o/raw\"\nbase = \"main\"\n")
	ex := &fakeExec{answer: func(argv []string) (int, string) {
		if slices.Contains(argv, "--list") {
			return 0, "core.bare\nfalse\x00diff.external\ntouch x\x00remote.origin.url\ngit@github.com:o/raw.git\x00"
		}
		return 0, ""
	}}
	code, out, errs := opsRun(t, f, opsCLI{}, ex, "onboard", "raw")
	want := "fail  checkout    main in " + repo + ": " + repo + ": .git/config has settings a plain clone does not " +
		"(diff.external, remote.origin.url); onboard changes nothing, and a tick cleans it only once it works on the project, so run:\n" +
		strings.Repeat(" ", 18) + "git -C " + repo + " config --unset-all diff.external\n" +
		strings.Repeat(" ", 18) + "git -C " + repo + " remote set-url origin https://github.com/o/raw.git\n"
	if code != 1 || !strings.Contains(out, want) {
		t.Errorf("%d %q %q\nwant %q", code, out, errs, want)
	}
	if strings.Contains(out, "next tick") {
		t.Errorf("onboard promises a tick: %q", out)
	}
}
