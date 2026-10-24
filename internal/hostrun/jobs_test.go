package hostrun

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/jobtail"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/sshjobs"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/usage"
)

// remoteFixture is RemoteStageTest's setUp: a project with a pinned
// Gemfile.lock, role templates, and a helper reached through a fake ssh.
type remoteFixture struct {
	*fixture
	repo    *testkit.Repo
	project *config.Project
	jobs    *Jobs
}

func newRemote(t *testing.T) *remoteFixture {
	t.Helper()
	f := newFixture(t)
	repo := testkit.NewRepo(t)
	repo.Write("Gemfile.lock", "GEM\n") // untracked
	for _, name := range []string{"triage", "reviewer", "worker", "task"} {
		dir := filepath.Join(f.paths.ScionTemplates, name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "agents.md"), []byte("# "+name+" role\n"), 0o644)
	}
	host := HostOf(f.paths, config.HostCfg{Name: "pi", SSH: "factory@pi", Slots: 1})
	return &remoteFixture{
		fixture: f, repo: repo,
		project: &config.Project{Key: "p", Path: repo.Dir, Base: "main", PinnedUntracked: []string{"Gemfile.lock"}},
		jobs: &Jobs{Remote: host, Paths: f.paths, Via: sshjobs.Client{Exec: f.exec}, Read: jobtail.Reader{Exec: f.exec},
			Clock: clock.Fixed{T: now}},
	}
}

func (r *remoteFixture) ticket(t *testing.T, column, branch string) *ticket.Ticket {
	js := `{"id": "T0003", "project": "p", "title": "t", "body": "b", "door": "human", "column": "` + column + `", "branch": `
	if branch == "" {
		js += "null"
	} else {
		js += `"` + branch + `"`
	}
	return ticketOf(t, js+`, "reports": {}, "history": [], "agent": null}`)
}

// start runs a job's start and returns the tar the helper got.
func (r *remoteFixture) start(t *testing.T, j Job) map[string][]byte {
	t.Helper()
	r.exec.calls, r.exec.stdins = nil, nil
	if j.Project == nil && j.Stage != model.StageTask {
		j.Project = r.project
	}
	if j.Project != nil {
		j.Repo = gitRepo{j.Project.Path}
	}
	if j.Template == "" {
		j.Template = map[model.Stage]string{model.StageTriage: "triage", model.StageReview: "reviewer",
			model.StageWork: "worker", model.StageTask: "task"}[j.Stage]
	}
	if j.TimeoutMinutes == 0 {
		j.TimeoutMinutes = 30
	}
	if err := r.jobs.Start(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if len(r.exec.calls) != 1 || r.exec.calls[0].Argv[10] != "start" || r.exec.calls[0].Argv[11] != j.Name {
		t.Fatalf("calls %q", r.exec.args())
	}
	files, err := tarMembers(r.exec.stdins[0])
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// tarMembers reads every member, nested names included (the helper's view).
func tarMembers(b []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[h.Name] = data
	}
}

func meta(t *testing.T, files map[string][]byte) jobproto.Meta {
	t.Helper()
	m, err := jobproto.ParseMeta(files["meta.json"])
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// port: TF RemoteStageTest.test_remote_triage_ships_base_role_prompt_and_pinned_files
func TestRemoteTriageShipsBaseRolePromptAndPinnedFiles(t *testing.T) {
	r := newRemote(t)
	files := r.start(t, Job{Ticket: r.ticket(t, "triage", ""), Stage: model.StageTriage, Name: "t0003-t1",
		Prompt: "Triage this incoming report"})
	m := meta(t, files)
	if m.Branch != "triage" || m.Start != "main" || m.Bundle || m.TimeoutMinutes != 30 {
		t.Errorf("meta %+v", m)
	}
	if string(files["meta.json"]) != `{"branch": "triage", "start": "main", "model": "", "bundle": false, "timeout_minutes": 30}` {
		t.Errorf("meta.json %s", files["meta.json"])
	}
	if heads := bundleHeads(t, files["repo.bundle"]); !reflect.DeepEqual(heads, map[string]bool{"refs/heads/main": true}) {
		t.Errorf("bundle %v", heads)
	}
	if string(files["role.md"]) != "# triage role\n" || !strings.Contains(string(files["prompt.md"]), "Triage this incoming report") {
		t.Errorf("role %q prompt %q", files["role.md"], files["prompt.md"])
	}
	if string(files["untracked/Gemfile.lock"]) != "GEM\n" {
		t.Errorf("pinned %q", files["untracked/Gemfile.lock"])
	}
	var names []string
	for _, c := range []string{"prompt.md", "role.md", "meta.json", "repo.bundle", "untracked/Gemfile.lock"} {
		if _, ok := files[c]; ok {
			names = append(names, c)
		}
	}
	if len(names) != len(files) {
		t.Errorf("members %v", files)
	}
	if r.jobs.Host() != "pi" {
		t.Error(r.jobs.Host())
	}
}

// port: TF RemoteStageTest.test_remote_review_starts_from_the_branch
func TestRemoteReviewStartsFromTheBranch(t *testing.T) {
	r := newRemote(t)
	r.repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
	files := r.start(t, Job{Ticket: r.ticket(t, "review", "bot/x"), Stage: model.StageReview, Name: "t0003-r1", Prompt: "review"})
	if m := meta(t, files); m.Branch != "bot/x" || m.Start != "bot/x" || m.Bundle {
		t.Errorf("meta %+v", m)
	}
	if heads := bundleHeads(t, files["repo.bundle"]); !reflect.DeepEqual(heads, map[string]bool{"refs/heads/main": true, "refs/heads/bot/x": true}) {
		t.Errorf("bundle %v", heads)
	}
	// a branch that does not exist yet starts from the base; work sends its branch back
	files = r.start(t, Job{Ticket: r.ticket(t, "ready", "bot/new"), Stage: model.StageWork, Name: "t0003-w1", Prompt: "work", Model: "m"})
	if m := meta(t, files); m.Branch != "bot/new" || m.Start != "main" || !m.Bundle || m.Model != "m" {
		t.Errorf("work meta %+v", m)
	}
}

// port: TF TaskTicketTest.test_task_without_project_ships_no_repository
func TestTaskWithoutProjectShipsNoRepository(t *testing.T) {
	r := newRemote(t)
	tk := ticketOf(t, `{"id": "T0003", "project": "factory", "kind": "task", "column": "ready", "timeout_minutes": 60, "title": "Summarise https://example.com/doc"}`)
	files := r.start(t, Job{Ticket: tk, Stage: model.StageTask, Name: "t0003-k1", Prompt: "no repository here", TimeoutMinutes: 60})
	if _, ok := files["repo.bundle"]; ok {
		t.Error("a bundle")
	}
	if m := meta(t, files); m.Branch != "task" || m.Start != "none" || m.Bundle || m.TimeoutMinutes != 60 {
		t.Errorf("meta %+v", m)
	}
	if string(files["role.md"]) != "# task role\n" {
		t.Errorf("role %q", files["role.md"])
	}
	for name := range files {
		if strings.HasPrefix(name, "untracked/") {
			t.Errorf("shipped %s", name)
		}
	}
}

// port: TF TaskTicketTest.test_task_with_project_ships_base_only
func TestTaskWithProjectShipsBaseOnly(t *testing.T) {
	r := newRemote(t)
	files := r.start(t, Job{Ticket: r.ticket(t, "ready", ""), Stage: model.StageTask, Name: "t0003-k1", Prompt: "p", Project: r.project})
	if heads := bundleHeads(t, files["repo.bundle"]); !reflect.DeepEqual(heads, map[string]bool{"refs/heads/main": true}) {
		t.Errorf("bundle %v", heads)
	}
	// a task asked on a ticket's branch starts from it when it exists
	r.repo.BranchWith("bot/x", map[string]string{"README.md": "x\n"})
	tk := ticketOf(t, `{"id": "T0004", "project": "p", "kind": "task", "column": "ready", "ref": "bot/x"}`)
	files = r.start(t, Job{Ticket: tk, Stage: model.StageTask, Name: "t0004-k1", Prompt: "p", Project: r.project})
	if m := meta(t, files); m.Branch != "task" || m.Start != "bot/x" {
		t.Errorf("meta %+v", m)
	}
	if heads := bundleHeads(t, files["repo.bundle"]); len(heads) != 2 {
		t.Errorf("bundle %v", heads)
	}
	tk = ticketOf(t, `{"id": "T0005", "project": "p", "kind": "task", "column": "ready", "ref": "bot/gone"}`)
	if m := meta(t, r.start(t, Job{Ticket: tk, Stage: model.StageTask, Name: "t0005-k1", Prompt: "p", Project: r.project})); m.Start != "main" {
		t.Errorf("missing ref: %+v", m)
	}
}

// The client half of the memory test: the snapshot rides in the tar under
// memory/, in name order.
// port: TF MemoryTest.test_a_helper_host_gets_a_snapshot_and_its_tar_is_read_in_memory
func TestAHelperGetsItsMemoryInTheTar(t *testing.T) {
	r := newRemote(t)
	files := r.start(t, Job{Ticket: r.ticket(t, "triage", ""), Stage: model.StageTriage, Name: "t0003-t1", Prompt: "p",
		Memory: map[string][]byte{"memory/README.md": []byte("# Memory\n"), "memory/context/p/notes.md": []byte("n")}})
	if string(files["memory/README.md"]) != "# Memory\n" || string(files["memory/context/p/notes.md"]) != "n" {
		t.Errorf("memory %q", files)
	}
	err := r.jobs.Start(context.Background(), Job{Ticket: r.ticket(t, "triage", ""), Stage: model.StageTriage, Name: "t0003-t2",
		Template: "triage", Project: r.project, Repo: gitRepo{r.project.Path}, Memory: map[string][]byte{"../x": nil}})
	if err == nil {
		t.Error("a memory file outside memory/")
	}
}

// A pinned file reached through a link is not shipped (Python's isfile
// would follow it out of the checkout).
func TestPinnedFilesAreRegularFilesOnly(t *testing.T) {
	r := newRemote(t)
	secret := filepath.Join(r.env.Home, "secret")
	os.WriteFile(secret, []byte("token"), 0o600)
	os.Remove(filepath.Join(r.repo.Dir, "Gemfile.lock"))
	os.Symlink(secret, filepath.Join(r.repo.Dir, "Gemfile.lock"))
	files := r.start(t, Job{Ticket: r.ticket(t, "triage", ""), Stage: model.StageTriage, Name: "t0003-t1", Prompt: "p"})
	if _, ok := files["untracked/Gemfile.lock"]; ok {
		t.Error("a link was shipped")
	}
}

// A refused start removes whatever may have landed, keeps the helper's
// text, and is ErrRemote (the tick tries another host). A cancelled start
// is not a helper error and sends nothing more.
func TestAFailedStartRemovesTheJob(t *testing.T) {
	r := newRemote(t)
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Code: 2, Stderr: []byte("draining for the reboot window\n")}, nil
	}
	j := Job{Ticket: r.ticket(t, "triage", ""), Stage: model.StageTriage, Name: "t0003-t1", Template: "triage",
		Project: r.project, Repo: gitRepo{r.project.Path}, TimeoutMinutes: 30}
	err := r.jobs.Start(context.Background(), j)
	if !errors.Is(err, ErrRemote) || err.Error() != "factory@pi start failed (2): draining for the reboot window" {
		t.Errorf("%v", err)
	}
	var verbs []string
	for _, c := range r.exec.calls {
		verbs = append(verbs, c.Argv[10])
	}
	if !reflect.DeepEqual(verbs, []string{"start", "remove"}) {
		t.Errorf("verbs %q", verbs)
	}
	r.exec.calls = nil
	ctx, cancel := context.WithCancel(context.Background())
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) { cancel(); return execx.Result{}, context.Canceled }
	if err := r.jobs.Start(ctx, j); err == nil || errors.Is(err, ErrRemote) || len(r.exec.calls) != 1 {
		t.Errorf("cancelled: %v, %d calls", err, len(r.exec.calls))
	}
}

func (r *remoteFixture) collected(t *testing.T, files []jobproto.File) *Result {
	t.Helper()
	tar, err := jobproto.TarFlat(files, now)
	if err != nil {
		t.Fatal(err)
	}
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		switch c.Argv[10] {
		case "status":
			return execx.Result{Stdout: []byte(`{"state": "done"}`)}, nil
		case "collect":
			return execx.Result{Stdout: tar}, nil
		}
		return execx.Result{Stdout: []byte("{}")}, nil
	}
	if st, err := r.jobs.Poll(context.Background(), "t0003-x1"); err != nil || st != jobproto.StateDone {
		t.Fatalf("poll %q %v", st, err)
	}
	res, err := r.jobs.Collect(context.Background(), "T0003", "t0003-x1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.jobs.Remove(context.Background(), "T0003", "t0003-x1"); err != nil {
		t.Fatal(err)
	}
	if c := r.exec.calls[len(r.exec.calls)-1]; c.Argv[10] != "remove" || c.Timeout != jobproto.Timeout("remove") {
		t.Errorf("remove %q", c.Argv)
	}
	if c := r.exec.calls[len(r.exec.calls)-2]; c.Timeout != jobproto.Timeout("collect") {
		t.Errorf("collect timeout %v", c.Timeout)
	}
	return res
}

// port: TF RemoteStageTest.test_remote_worker_commits_come_back_through_the_bundle
func TestRemoteWorkerCommitsComeBackThroughTheBundle(t *testing.T) {
	r := newRemote(t)
	res := r.collected(t, []jobproto.File{{Name: "report.json", Data: []byte(`{"status": "done"}`)}, {Name: "out.bundle", Data: []byte("BUNDLE")},
		{Name: "agent.log", Data: []byte("log")}})
	if b, ok := res.OutBundle(); !ok || string(b) != "BUNDLE" {
		t.Errorf("bundle %q", b)
	}
	if b, ok := res.Report(); !ok || string(b) != `{"status": "done"}` {
		t.Errorf("report %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(r.paths.Logs, "T0003", "t0003-x1", "agent.log")); err != nil || string(b) != "log" {
		t.Errorf("archived log %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(r.paths.Logs, "T0003", "t0003-x1", "out.bundle")); err == nil {
		t.Error("archived the bundle")
	}
}

// port: TF RemoteStageTest.test_remote_reviewer_must_leave_the_head_where_it_was
func TestTheJobsHeadOrUnknown(t *testing.T) {
	r := newRemote(t)
	head := strings.Repeat("a", 40)
	for _, c := range []struct {
		files []jobproto.File
		want  string
	}{
		{[]jobproto.File{{Name: "report.json", Data: []byte(`{"verdict": "pass"}`)}, {Name: "head", Data: []byte(head + "\n")}}, head},
		{[]jobproto.File{{Name: "report.json", Data: []byte(`{"verdict": "pass"}`)}, {Name: "head", Data: []byte(" \n")}}, "unknown"},
		{[]jobproto.File{{Name: "report.json", Data: []byte(`{"verdict": "pass"}`)}}, "unknown"},
	} {
		if got := r.collected(t, c.files).Head(); got != c.want {
			t.Errorf("head %q, want %q", got, c.want)
		}
	}
}

// port: TF RemoteStageTest.test_remote_job_without_report_needs_human
func TestARemoteJobWithoutReport(t *testing.T) {
	r := newRemote(t)
	res := r.collected(t, []jobproto.File{{Name: "agent.log", Data: []byte("crashed")}})
	if _, ok := res.Report(); ok {
		t.Error("a report")
	}
	if _, ok := res.MemoryTar(); ok {
		t.Error("memory")
	}
	// what the limit check reads: the end of the log, decoded
	res = r.collected(t, []jobproto.File{{Name: "agent.log", Data: []byte(strings.Repeat("é", 30_000) + "\npi: 400 Your credit balance is too low\xff\npi exited 1\n")}})
	text := res.LimitText(20_000)
	if len([]rune(text)) != 20_000 || !strings.HasSuffix(text, "too low�\npi exited 1\n") {
		t.Errorf("limit text ends %q", text[len(text)-40:])
	}
}

// Collect keeps flat regular files only, and archives at most the last
// MaxArchivedBytes of each kept file; a status that is not done is passed
// on for the tick to decide; a broken collect is an error.
func TestCollectAndPollEdges(t *testing.T) {
	r := newRemote(t)
	big := strings.Repeat("x", MaxArchivedBytes+10) + "END"
	res := r.collected(t, []jobproto.File{{Name: "agent.log", Data: []byte(big)}, {Name: "../report.json", Data: []byte("{}")}, {Name: "sub/head", Data: []byte("h")}})
	if _, ok := res.Report(); ok || res.Head() != "unknown" {
		t.Error("a nested member was taken")
	}
	b, _ := os.ReadFile(filepath.Join(r.paths.Logs, "T0003", "t0003-x1", "agent.log"))
	if len(b) != MaxArchivedBytes || !strings.HasSuffix(string(b), "END") {
		t.Errorf("archived %d bytes", len(b))
	}
	for out, want := range map[string]string{`{"state": "running"}`: "running", `{"state": "missing"}`: "missing"} {
		r.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Stdout: []byte(out)}, nil }
		if st, err := r.jobs.Poll(context.Background(), "t0003-x1"); err != nil || st != want {
			t.Errorf("%s: %q %v", out, st, err)
		}
	}
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Code: 255, Stderr: []byte("down")}, nil }
	if _, err := r.jobs.Poll(context.Background(), "t0003-x1"); !errors.Is(err, ErrRemote) {
		t.Errorf("poll: %v", err)
	}
	if _, err := r.jobs.Collect(context.Background(), "T0003", "t0003-x1"); !errors.Is(err, ErrRemote) {
		t.Errorf("collect: %v", err)
	}
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{}, nil }
	if _, err := r.jobs.Collect(context.Background(), "T0003", "t0003-x1"); err == nil {
		t.Error("an empty collect")
	}
}

// port: TF LookTest.test_look_reads_the_local_screen_or_the_helper_hosts_tail
func TestLookReadsTheHelpersTail(t *testing.T) {
	r := newRemote(t)
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Stdout: []byte("pi output")}, nil }
	got, err := r.jobs.Tail(context.Background(), "t0001-w1", 120)
	if err != nil || got != "pi output" {
		t.Errorf("%q %v", got, err)
	}
	if a := r.exec.args(); a[0][len(a[0])-len("tail t0001-w1"):] != "tail t0001-w1" || r.exec.calls[0].Timeout != jobproto.Timeout("tail") {
		t.Errorf("argv %q", a)
	}
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Code: 255, Stderr: []byte("down")}, nil }
	got, err = r.jobs.Tail(context.Background(), "t0001-w1", 120)
	if err != nil || got != "(could not read the job on pi: factory@pi tail failed (255): down)" {
		t.Errorf("%q %v", got, err)
	}
}

func TestHostOfExpandsTheKey(t *testing.T) {
	f := newFixture(t)
	for key, want := range map[string]string{
		"":              f.paths.RemoteKey,
		"~/.ssh/helper": filepath.Join(f.env.Home, ".ssh/helper"),
		"/etc/k":        "/etc/k",
		"~":             f.env.Home,
	} {
		if got := HostOf(f.paths, config.HostCfg{Name: "h", SSH: "f@h", Key: key}); got.Key != want || got.SSH != "f@h" || got.Name != "h" {
			t.Errorf("%q: %+v", key, got)
		}
	}
}

// A job's usage.json comes back with the collect and is archived beside
// its log, cleaned; one that does not read is left out.
func TestCollectArchivesTheJobsUsage(t *testing.T) {
	r := newRemote(t)
	r.collected(t, []jobproto.File{{Name: "report.json", Data: []byte(`{"status": "done"}`)},
		{Name: "usage.json", Data: []byte(`{"models": {"claude-opus": {"input": 5, "output": -1, "messages": 1}}}`)}})
	b, err := os.ReadFile(filepath.Join(r.paths.Logs, "T0003", "t0003-x1", "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if u, err := usage.Parse(b); err != nil || u.Models["claude-opus"] != (usage.Tokens{Input: 5, Messages: 1}) {
		t.Errorf("usage %s %v", b, err)
	}
	os.Remove(filepath.Join(r.paths.Logs, "T0003", "t0003-x1", "usage.json"))
	r.collected(t, []jobproto.File{{Name: "usage.json", Data: []byte(`not json`)}})
	if _, err := os.Lstat(filepath.Join(r.paths.Logs, "T0003", "t0003-x1", "usage.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a broken usage.json was archived: %v", err)
	}
}

// The usage a removal brings back (a job removed before it was done) is
// archived as the run's usage.json, cleaned; an answer without one, or
// with one that does not read, archives none and is no error.
func TestRemoveArchivesTheUsageItBringsBack(t *testing.T) {
	r := newRemote(t)
	dest := filepath.Join(r.paths.Logs, "T0003", "t0003-x1", "usage.json")
	answer := func(out string) {
		r.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Stdout: []byte(out)}, nil }
	}
	answer(`{"removed": "t0003-x1", "usage": {"models": {"claude-opus": {"input": 5, "output": -1, "messages": 1}}}}` + "\n")
	if err := r.jobs.Remove(context.Background(), "T0003", "t0003-x1"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := usage.Parse(b); err != nil || u.Models["claude-opus"] != (usage.Tokens{Input: 5, Messages: 1}) {
		t.Errorf("usage %s %v", b, err)
	}
	for _, out := range []string{`{"removed": "t0003-x1"}`, `{"removed": "t0003-x1", "usage": "nope"}`, `not json`} {
		os.Remove(dest)
		answer(out + "\n")
		if err := r.jobs.Remove(context.Background(), "T0003", "t0003-x1"); err != nil {
			t.Errorf("%s: %v", out, err)
		}
		if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: usage.json archived: %v", out, err)
		}
	}
}
