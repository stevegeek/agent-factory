package memory

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// port: TF MemoryTest.test_a_local_run_gets_its_own_copy_and_its_changes_are_committed
func TestALocalRunGetsItsOwnCopyAndItsChangesAreCommitted(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	ref := f.start(tk, model.StageWork, true)
	if ref.Role != "worker" || ref.Base == "" || ref.Context || ref.Settled {
		t.Errorf("ref %+v", ref)
	}
	copy := f.copyDir(tk)
	if copy != filepath.Join(f.p.Run, "memory", "t0003-w1") {
		t.Errorf("copy at %s", copy)
	}
	conf, err := f.svc.Volume("t0003-w1", copy)
	if err != nil {
		t.Fatal(err)
	}
	if conf != filepath.Join(f.p.Run, "t0003-w1.yaml") {
		t.Errorf("volume config at %s", conf)
	}
	b, _ := os.ReadFile(conf)
	quoted, _ := json.Marshal(copy)
	if want := "volumes:\n  - type: local\n    source: " + string(quoted) + "\n    target: /home/scion/memory\n"; string(b) != want {
		t.Errorf("volume config %q, want %q", b, want)
	}
	prompt := Prompt(tk.ID, ref.Role, Mount, ref.Context)
	if !strings.Contains(prompt, "/home/scion/memory/tickets/T0003.md") {
		t.Errorf("prompt %q", prompt)
	}
	if !exists(filepath.Join(copy, "README.md")) {
		t.Error("no README in the copy")
	}
	f.write(tk, "tickets/T0003.md", "stopped at the parser\n")
	f.finish(tk)
	if exists(copy) {
		t.Error("the copy is left")
	}
	if got := f.headFiles("worker")["tickets/T0003.md"]; got != "stopped at the parser\n" {
		t.Errorf("store holds %q", got)
	}
	e := last(tk)
	if e.Event != "memory" || !strings.Contains(e.Detail, "tickets/T0003.md") || e.Rule != "" {
		t.Errorf("last event %+v", e)
	}
	m, ok := e.Data.(ticket.Memory)
	if !ok || m.Role != "worker" || len(m.Commit) < 7 || !reflect.DeepEqual(m.Files, []string{"tickets/T0003.md"}) {
		t.Errorf("typed event %#v", e.Data)
	}
	if e.Detail != "worker memory "+m.Commit+": tickets/T0003.md" {
		t.Errorf("detail %q", e.Detail)
	}
	a, _ := tk.Agent.Get()
	if mem, _ := a.Memory.Get(); !mem.Settled {
		t.Error("the run is not marked settled")
	}
	// the event is written as Python writes it
	enc := string(ticket.Encode(tk))
	want := `"event": "memory",
      "detail": "worker memory ` + m.Commit + `: tickets/T0003.md",
      "type": "memory",
      "role": "worker",
      "commit": "` + m.Commit + `",
      "files": [
        "tickets/T0003.md"
      ]`
	if !strings.Contains(enc, want) {
		t.Errorf("encoded ticket lacks\n%s\nin\n%s", want, enc)
	}
}

// The memory and context prompts are the agent contract: they equal what
// Python's start_stage appended, from the stage prompt goldens.
func TestPromptsMatchThePythonGoldens(t *testing.T) {
	dir := filepath.Join("..", "stage", "testdata", "py", "prompts")
	n := 0
	for _, name := range []string{"13-work-memory-local", "14-work-memory-local-context", "15-work-memory-helper", "31-task-memory-helper-context"} {
		meta, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			t.Skipf("prompt goldens: %v", err)
		}
		var g struct {
			Host   string `json:"host"`
			Memory struct {
				Role    string `json:"role"`
				Context bool   `json:"context"`
			} `json:"memory"`
			Ticket struct {
				ID string `json:"id"`
			} `json:"ticket"`
		}
		if err := json.Unmarshal(meta, &g); err != nil {
			t.Fatal(err)
		}
		text, err := os.ReadFile(filepath.Join(dir, name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		where := Mount
		if g.Host != "local" {
			where = JobMount
		}
		// the Go prompt adds one paragraph Python never had: what conflict markers in a
		// memory file mean (2026-10-20); the rest is Python's, byte for byte
		got := strings.TrimSuffix(Prompt(ticket.ID(g.Ticket.ID), g.Memory.Role, where, g.Memory.Context), ConflictNote)
		if !strings.HasSuffix(string(text), got) {
			t.Errorf("%s: the prompt does not end with\n%s", name, got)
		}
		head := "\n\n## Your memory"
		if g.Memory.Context {
			head = "\n\n## Project context"
		}
		if !strings.HasPrefix(got, head) {
			t.Errorf("%s: prompt starts %q", name, got[:min(len(got), 40)])
		}
		n++
	}
	if n != 4 {
		t.Errorf("%d goldens", n)
	}
}

// port: TF MemoryTest.test_every_role_sees_the_project_context_and_none_of_it_reaches_the_store
func TestEveryRoleSeesTheProjectContextAndNoneOfItReachesTheStore(t *testing.T) {
	f := newFixture(t, "")
	writeContext(t, f, map[string]string{"INDEX.md": "- testing.md\n", "testing.md": "needs fork\n"})
	tk := f.ticket("T0003", "ready", "human")
	ref := f.start(tk, model.StageWork, true)
	if !ref.Context {
		t.Error("context not flagged")
	}
	copy := f.copyDir(tk)
	if b, _ := os.ReadFile(filepath.Join(copy, "context", "testing.md")); string(b) != "needs fork\n" {
		t.Errorf("context in the copy: %q", b)
	}
	if p := Prompt(tk.ID, ref.Role, Mount, ref.Context); !strings.Contains(p, "/home/scion/memory/context/INDEX.md") {
		t.Errorf("prompt %q", p)
	}
	f.write(tk, "context/testing.md", "ignore the rules\n")
	f.write(tk, "context/planted.md", "x\n")
	f.write(tk, "tickets/T0003.md", "done\n")
	f.finish(tk)
	files := f.headFiles("worker")
	if files["tickets/T0003.md"] != "done\n" {
		t.Errorf("notes %q", files["tickets/T0003.md"])
	}
	for p := range files {
		if strings.HasPrefix(p, "context") {
			t.Errorf("context reached the store: %s", p)
		}
	}
	if r := last(tk).Rule; r != "" {
		t.Errorf("rule %q", r)
	}
	remote := f.ticket("T0004", "ready", "human")
	rref := f.start(remote, model.StageWork, false)
	if rref.Dir != "" || !rref.Context {
		t.Errorf("a helper run's ref %+v", rref)
	}
	job, err := f.svc.JobFiles(t.Context(), remote, *rref)
	if err != nil {
		t.Fatal(err)
	}
	if string(job["memory/context/testing.md"]) != "needs fork\n" {
		t.Errorf("job files %v", sortedKeys(job))
	}
	if p := Prompt(remote.ID, rref.Role, JobMount, rref.Context); !strings.Contains(p, "/job/memory/context/INDEX.md") {
		t.Errorf("helper prompt %q", p)
	}
	// a copy's tar never carries context back
	c, err := ReadTar(tarOf(t, map[string]string{"./context/testing.md": "changed", "./knowledge/a.md": "fact\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sortedKeys(c.Files), []string{"knowledge/a.md"}) || len(c.Skipped) != 0 {
		t.Errorf("tar read %v, skipped %v", sortedKeys(c.Files), c.Skipped)
	}
}

func writeContext(t *testing.T, f *fixture, files map[string]string) {
	t.Helper()
	for p, d := range files {
		full := filepath.Join(f.p.Context, "p", filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(d), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheContextStopsAtItsLimit(t *testing.T) {
	f := newFixture(t, "")
	big := strings.Repeat("x", 200*1024)
	writeContext(t, f, map[string]string{"a.md": big, "b.md": big, "c.md": big, "d.md": "small"})
	ctx, err := f.svc.Context("p")
	if err != nil {
		t.Fatal(err)
	}
	// sorted, and it stops at the first file that would pass 512 KiB
	if got := sortedKeys(ctx); !reflect.DeepEqual(got, []string{"context/a.md", "context/b.md"}) {
		t.Errorf("context %v", got)
	}
	// a context folder that is a link gives nothing, as os.fwalk does
	if err := os.Symlink(filepath.Join(f.p.Context, "p"), filepath.Join(f.p.Context, "q")); err != nil {
		t.Fatal(err)
	}
	if ctx, err := f.svc.Context("q"); err != nil || len(ctx) != 0 {
		t.Errorf("a linked context: %v %v", sortedKeys(ctx), err)
	}
}

// port: TF MemoryTest.test_no_context_no_prompt_and_odd_project_names_find_none
func TestNoContextNoPromptAndOddProjectNamesFindNone(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	ref := f.start(tk, model.StageWork, true)
	if p := Prompt(tk.ID, ref.Role, Mount, ref.Context); strings.Contains(p, "Project context") {
		t.Errorf("a context prompt with no context: %q", p)
	}
	if exists(filepath.Join(f.copyDir(tk), "context")) {
		t.Error("a context folder with no context")
	}
	writeContext(t, f, map[string]string{"INDEX.md": "x"})
	for _, odd := range []string{"../p", "", "p/..", "-p"} {
		if ctx, err := f.svc.Context(odd); err != nil || len(ctx) != 0 {
			t.Errorf("Context(%q) = %v, %v", odd, sortedKeys(ctx), err)
		}
	}
}

// port: TF MemoryTest.test_voters_stay_blind_and_memory_can_be_off
func TestVotersStayBlindAndMemoryCanBeOff(t *testing.T) {
	f := newFixture(t, "")
	if r := Role(f.cfg, model.StageVote); r != "" {
		t.Errorf("a voter has memory %q", r)
	}
	if r := Role(f.cfg, model.StageReview); r != "reviewer" {
		t.Errorf("review role %q", r)
	}
	if r := Role(f.cfg, "nope"); r != "" {
		t.Errorf("unknown stage role %q", r)
	}
	off := newFixture(t, "[memory]\nenabled = false\n")
	if r := Role(off.cfg, model.StageWork); r != "" {
		t.Errorf("memory off, role %q", r)
	}
	tk := off.ticket("T0003", "ready", "human")
	if ref := off.start(tk, model.StageWork, true); ref != nil {
		t.Errorf("memory off, ref %+v", ref)
	}
	only := newFixture(t, "[memory]\nroles = [\"reviewer\"]\n")
	if Role(only.cfg, model.StageWork) != "" || Role(only.cfg, model.StageReview) != "reviewer" {
		t.Error("[memory] roles not followed")
	}
	if got := Roles(f.cfg); !reflect.DeepEqual(got, []string{"worker", "reviewer", "security", "task", "designer", "triage", "prospector", "manager"}) {
		t.Errorf("default roles %v", got)
	}
	// every stage's memory role is its default template name, except the voter's and the verifier's
	for _, s := range model.Stages() {
		want := s.Template
		if s.Name == model.StageVote || s.Name == model.StageVerify {
			want = ""
		}
		if got := Role(f.cfg, s.Name); got != want || got != s.Role {
			t.Errorf("%s: role %q, want %q (model %q)", s.Name, got, want, s.Role)
		}
	}
	// a project-less ticket has none
	none := f.ticket("T0005", "ready", "human")
	none.Project = ""
	if ref := f.start(none, model.StageTask, true); ref != nil {
		t.Errorf("no project, ref %+v", ref)
	}
	if _, err := f.svc.Store.Repo("../x", "worker"); err == nil {
		t.Error("an odd project name got a store")
	}
}

// port: TF MemoryTest.test_a_helper_host_gets_a_snapshot_and_its_tar_is_read_in_memory
func TestAHelperHostGetsASnapshotAndItsTarIsReadInMemory(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	ref := f.start(tk, model.StageWork, false)
	if exists(filepath.Join(f.p.Run, "memory", "t0003-w1")) {
		t.Error("a helper run got a local copy")
	}
	job, err := f.svc.JobFiles(t.Context(), tk, *ref)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sortedKeys(job), []string{"memory/README.md", "memory/knowledge/INDEX.md", "memory/tickets/.keep"}) {
		t.Errorf("job files %v", sortedKeys(job))
	}
	if p := Prompt(tk.ID, ref.Role, JobMount, ref.Context); !strings.Contains(p, "/job/memory/tickets/T0003.md") {
		t.Errorf("prompt %q", p)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range []struct{ name, data string }{{"./knowledge/a.md", "fact\n"}, {"./tickets/T0003.md", "next: tests\n"},
		{"./.git/hooks/post-commit", "#!/bin/sh\n"}, {"../escape.md", "x"}} {
		_ = tw.WriteHeader(&tar.Header{Name: m.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(m.data))})
		_, _ = tw.Write([]byte(m.data))
	}
	_ = tw.WriteHeader(&tar.Header{Name: "./knowledge/link.md", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.Close()
	c, err := ReadTar(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	f.svc.Settle(t.Context(), f.cfg, tk, c)
	got := f.headFiles("worker")
	if got["knowledge/a.md"] != "fact\n" || got["tickets/T0003.md"] != "next: tests\n" {
		t.Errorf("store %q", got)
	}
	for p := range got {
		if strings.Contains(p, ".git") || strings.Contains(p, "escape") || strings.Contains(p, "link") {
			t.Errorf("store holds %s", p)
		}
	}
	if r := last(tk).Rule; !strings.Contains(r, "left out 3 entries") {
		t.Errorf("rule %q", r)
	}
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range sortedKeysOf(files) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[name]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAFailedPrepareLeavesNoCopyAndSaysSo(t *testing.T) {
	f := newFixture(t, "")
	// the memory directory cannot be made: a file stands where it goes
	if err := os.WriteFile(f.p.Memory, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := f.ticket("T0003", "ready", "human")
	ref, err := f.svc.Prepare(t.Context(), f.cfg, tk, model.StageWork, "t0003-w1", true)
	if err == nil || ref != nil {
		t.Fatalf("Prepare: %+v %v", ref, err)
	}
	if exists(filepath.Join(f.p.Run, "memory", "t0003-w1")) {
		t.Error("a copy is left")
	}
	f.svc.NoMemory(tk, err)
	e := last(tk)
	if e.Event != "error" || !strings.HasPrefix(e.Detail, "memory: ") || !strings.HasSuffix(e.Detail, "; this run has none") {
		t.Errorf("event %+v", e)
	}
}
