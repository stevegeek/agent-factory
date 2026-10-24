package hostrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
)

// A local run's pinned paths join its memory mount in run/<name>.yaml, or
// get a run config of their own when it has no memory. A pinned file is the
// run's own writable copy (bundle check rewrites the lockfile); a pinned
// directory is the project's, read-only. Dropping the run config drops the
// copies.
func TestALocalStartMountsThePinnedPaths(t *testing.T) {
	f := newFixture(t)
	p := projectAt(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(p.Path, "Gemfile.lock"), []byte("GEM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.Path, "lib", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}
	tk := ticketOf(t, `{"id": "T0003"}`)
	if err := os.MkdirAll(f.paths.Run, 0o755); err != nil {
		t.Fatal(err)
	}
	memText := "volumes:\n  - type: local\n    source: \"/m\"\n    target: /home/scion/memory\n"
	mem := filepath.Join(f.paths.Run, "t0003-k1.yaml")
	if err := os.WriteFile(mem, []byte(memText), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		job  Job
		conf string
		head string
	}{
		{Job{Ticket: tk, Stage: model.StageTask, Name: "t0003-k1", Prompt: "p", Template: "task", Project: p,
			RunConfig: mem, Pinned: []string{"Gemfile.lock", "lib"}}, mem, memText},
		{Job{Ticket: tk, Stage: model.StageVerify, Name: "t0003-f1", Prompt: "p", Template: "verifier", Project: p,
			Pinned: []string{"Gemfile.lock", "lib"}}, filepath.Join(f.paths.Run, "t0003-f1.yaml"), "volumes:\n"},
	} {
		f.exec.calls = nil
		if err := f.local().Start(context.Background(), c.job); err != nil {
			t.Fatal(err)
		}
		if args := f.exec.args(); len(args) != 1 || !strings.Contains(args[0], "--config "+c.conf+" ") {
			t.Errorf("argv %q", args)
		}
		copied := filepath.Join(f.paths.Run, "pinned", c.job.Name, "Gemfile.lock")
		if b, err := os.ReadFile(copied); err != nil || string(b) != "GEM\n" {
			t.Errorf("the run's copy: %q, %v", b, err)
		}
		ws := "/repo-root/.scion/agents/" + c.job.Name + "/workspace/"
		b, _ := os.ReadFile(c.conf)
		want := c.head +
			"  - type: local\n    source: \"" + copied + "\"\n    target: \"" + ws + "Gemfile.lock\"\n" +
			"  - type: local\n    source: \"" + filepath.Join(p.Path, "lib") + "\"\n    target: \"" + ws + "lib\"\n    read_only: true\n"
		if string(b) != want {
			t.Errorf("run config\n%s\nwant\n%s", b, want)
		}
	}
	// the copy goes with the run config; the project's file stays
	if err := f.local().DropRunConfig("t0003-f1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.paths.Run, "pinned", "t0003-f1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("dropping the run config left its pinned copies")
	}
	if _, err := os.Stat(filepath.Join(p.Path, "Gemfile.lock")); err != nil {
		t.Errorf("the project's own lockfile: %v", err)
	}

	bad := Job{Ticket: tk, Stage: model.StageVerify, Name: "t0003-f2", Prompt: "p", Template: "verifier", Project: p, Pinned: []string{"../x"}}
	if err := f.local().Start(context.Background(), bad); err == nil {
		t.Error("a pinned path outside the project")
	}
	for _, left := range []string{"t0003-f2.yaml", "pinned/t0003-f2"} {
		if _, err := os.Stat(filepath.Join(f.paths.Run, left)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused start left %s", left)
		}
	}
}
