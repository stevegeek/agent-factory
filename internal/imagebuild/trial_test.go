package imagebuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
)

// trialSpec is the gate image's spec with a newer Crystal and ameba, as a
// trial: three projects, one passing, one broken by the new toolchain (it
// passes in the live image), one failing in both.
func trialSpec(images *[]string) Spec {
	s := claudeSpec(map[string]string{"claude": "2.1.296", "crystal": "1.21.0", "ameba": "1.8.0"})
	s.Trial = true
	run := func(ok, liveOK bool, out string) func(context.Context, string) (bool, string, error) {
		return func(_ context.Context, image string) (bool, string, error) {
			if images != nil {
				*images = append(*images, image)
			}
			if image == s.Tag {
				return liveOK, "live: " + out, nil
			}
			return ok, out, nil
		}
	}
	s.Projects = []ProjectCheck{
		{Key: "broke", Base: "main", Run: run(false, true, "1 failure")},
		{Key: "fine", Base: "main", Run: run(true, true, "2 examples, 0 failures")},
		{Key: "old", Base: "trunk", Run: run(false, false, "lint: 2 offences")},
	}
	return s
}

const trialSmoke = "ruby 3.3\nCrystal 1.21.0 [abc] (2026-10-01)\n1.8.0\n2.1.296 (Claude Code)\n"

func noTag(t *testing.T, f *fakePodman) {
	t.Helper()
	for _, c := range f.calls {
		if c[1] == "tag" {
			t.Fatalf("a trial tagged: %q", c)
		}
	}
}

func removedTrial(t *testing.T, f *fakePodman) {
	t.Helper()
	if !f.has("podman", "rmi", "localhost/factory-agent:trial") {
		t.Fatalf("the trial image was not removed: %q", f.calls)
	}
}

// A trial builds <repo>:trial, runs every project (a regression does not
// stop it), sorts the outcomes, keeps each project's tail, and never tags.
func TestTrialRunsEveryProjectAndNeverTags(t *testing.T) {
	f := &fakePodman{results: map[string]string{"smoke": trialSmoke}}
	r, _ := runner(t, f)
	var images []string
	res := r.Rebuild(context.Background(), trialSpec(&images))
	if !res.OK || res.Err != nil {
		t.Fatalf("%+v", res)
	}
	if !reflect.DeepEqual(res.Passed, []string{"fine"}) || !reflect.DeepEqual(res.Broken, []string{"broke"}) ||
		!reflect.DeepEqual(res.AlreadyFailing, []string{"old"}) {
		t.Fatalf("passed %q broken %q failing %q", res.Passed, res.Broken, res.AlreadyFailing)
	}
	if res.Summary != "trial: 1 passes, 1 breaks, 1 already failing" {
		t.Fatalf("summary %q", res.Summary)
	}
	want := map[string]string{"broke": "1 failure", "fine": "2 examples, 0 failures", "old": "lint: 2 offences"}
	if !reflect.DeepEqual(res.Tails, want) {
		t.Fatalf("tails %q", res.Tails)
	}
	trial, live := "localhost/factory-agent:trial", "localhost/factory-agent:scion"
	if !reflect.DeepEqual(images, []string{trial, live, trial, trial, live}) {
		t.Fatalf("ran in %q", images)
	}
	if f.calls[0][len(f.calls[0])-2] != trial {
		t.Fatalf("build %q", f.calls[0])
	}
	noTag(t, f)
	removedTrial(t, f)
	if f.has("podman", "rmi", "localhost/factory-agent:candidate") {
		t.Fatal("a trial touched the candidate tag")
	}
	log := strings.Join(res.Log, "\n")
	for _, line := range []string{"== 2026-10-18T03:00:00+00:00 trial claude -> localhost/factory-agent:trial",
		"-- broke on main in the live image: pass, so the new toolchain broke it",
		"-- old on trunk in the live image: FAIL too (already failing)"} {
		if !strings.Contains(log, line) {
			t.Errorf("log has no %q:\n%s", line, log)
		}
	}
}

// The trial's smoke test checks that the image has the toolchain versions
// it was built with, not only the harness's.
func TestTrialSmokeChecksTheToolchainVersions(t *testing.T) {
	f := &fakePodman{results: map[string]string{"smoke": "Crystal 1.20.1\n1.8.0\n2.1.296 (Claude Code)\n"}}
	r, _ := runner(t, f)
	res := r.Rebuild(context.Background(), trialSpec(nil))
	if res.OK || res.Summary != "smoke test failed (expected Crystal 1.21.0)" {
		t.Fatalf("%+v", res)
	}
	noTag(t, f)
	removedTrial(t, f)
}

// The trial image is removed and nothing is tagged on every path: a failed
// build, an error, a cancel.
func TestTrialIsAlwaysRemovedAndNeverTagged(t *testing.T) {
	t.Run("build fails", func(t *testing.T) {
		f := &fakePodman{build: 1}
		r, _ := runner(t, f)
		if res := r.Rebuild(context.Background(), trialSpec(nil)); res.OK || res.Summary != "build failed" {
			t.Fatalf("%+v", res)
		}
		noTag(t, f)
		removedTrial(t, f)
	})
	t.Run("a check errs", func(t *testing.T) {
		f := &fakePodman{results: map[string]string{"smoke": trialSmoke}}
		r, _ := runner(t, f)
		s := trialSpec(nil)
		s.Projects[1].Run = func(context.Context, string) (bool, string, error) { return false, "", errors.New("no base") }
		res := r.Rebuild(context.Background(), s)
		if res.OK || res.Err == nil || !strings.Contains(res.Summary, "fine checks: no base") {
			t.Fatalf("%+v", res)
		}
		noTag(t, f)
		removedTrial(t, f)
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &fakePodman{}
		var calls [][]string
		ex := execx.Func(func(c context.Context, cmd execx.Cmd) (execx.Result, error) {
			calls = append(calls, cmd.Argv)
			if cmd.Argv[1] == "build" {
				cancel()
				return execx.Result{Argv: cmd.Argv}, context.Canceled
			}
			return f.Run(c, cmd)
		})
		r, _ := runner(t, f)
		r.Podman.Exec = ex
		if res := r.Rebuild(ctx, trialSpec(nil)); !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("%+v", res)
		}
		noTag(t, f)
		removedTrial(t, f)
	})
	t.Run("a check panics", func(t *testing.T) {
		f := &fakePodman{results: map[string]string{"smoke": trialSmoke}}
		r, _ := runner(t, f)
		s := trialSpec(nil)
		s.Projects[1].Run = func(context.Context, string) (bool, string, error) { panic("boom") }
		func() {
			defer func() {
				if p := recover(); p != "boom" {
					t.Fatalf("recovered %v", p)
				}
			}()
			r.Rebuild(context.Background(), s)
			t.Fatal("no panic")
		}()
		noTag(t, f)
		removedTrial(t, f)
	})
}

// A trial refuses a live tag that is its own trial tag.
func TestTrialTagClash(t *testing.T) {
	f := &fakePodman{}
	r, _ := runner(t, f)
	s := SpecFor(Kinds()[0], "/f/images", "localhost/a:trial", nil)
	s.Trial = true
	if res := r.Rebuild(context.Background(), s); res.OK || res.Summary != "image tag localhost/a:trial clashes with the rebuild's own tags" || len(f.calls) != 0 {
		t.Fatalf("%+v %q", res, f.calls)
	}
}

// All writes a trial's log to trial-<kind>.log (the weekly rebuild's log
// stays) and logs it as a trial.
func TestAllLogsATrialApart(t *testing.T) {
	f := &fakePodman{results: map[string]string{"smoke": trialSmoke}, exists: map[string]bool{"localhost/factory-agent:scion": true}}
	r, logs := runner(t, f)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "rebuild-claude.log"), []byte("weekly"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.All(context.Background(), []Spec{trialSpec(nil)}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(logs, "rebuild-claude.log")); string(b) != "weekly" {
		t.Fatalf("the weekly log was overwritten: %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(logs, "trial-claude.log")); err != nil || !strings.Contains(string(b), "-- fine on main: pass") {
		t.Fatalf("trial log %q %v", b, err)
	}
	fl, _ := os.ReadFile(filepath.Join(logs, "factory.log"))
	if want := "2026-10-18T03:00:00+00:00 trial claude: ok: trial: 1 passes, 1 breaks, 1 already failing\n"; string(fl) != want {
		t.Fatalf("factory.log %q", fl)
	}
	noTag(t, f)
}
