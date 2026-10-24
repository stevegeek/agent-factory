package pull

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/mailbox"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

var t0 = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

// world is the desktop and the dispatcher in one test: ssh is the
// dispatcher's forced command (factory-remote mailbox desktop, served by
// mailbox.Box.Serve) and lever records the notes it is asked to send.
type world struct {
	t        *testing.T
	clk      *clock.Step
	factory  mailbox.Box // the dispatcher's side, with its windows
	p        *Puller
	mu       sync.Mutex
	notes    [][]string // lever argv
	ssh      [][]string
	failNote int // the next n notes fail, as a 409 does
	woken    []string
	repo     *testkit.Repo
	home     string // git's home for the fake worker
	outside  string // a directory no worker may reach
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base := t.TempDir()
	clk := clock.NewStep(t0, 0)
	w := &world{t: t, clk: clk, home: filepath.Join(base, "githome"), outside: filepath.Join(base, "outside")}
	w.factory = mailbox.Box{Dir: filepath.Join(base, "hub", "mailbox"), Clock: clk, ClaimWindow: 30 * time.Minute, BeatWindow: 10 * time.Minute}
	instance := filepath.Join(base, "lever", "factory-worker")
	cfg := Config{SSH: "hub", Host: "desktop", Instance: instance, Tree: filepath.Join(instance, "tree"),
		State: filepath.Join(base, "state"), Workers: []string{"fw1", "fw2"}, Lever: "lever", SSHBin: "ssh",
		Poll: 30 * time.Second, Beat: time.Minute, GitName: "Factory Bot", GitEmail: "bot@example.com"}
	for _, d := range []string{cfg.WorkerDir("fw1"), cfg.WorkerDir("fw2"), w.home, w.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.p = &Puller{Cfg: cfg, Exec: execx.Func(w.run), Clock: clk, Log: t.Logf,
		Wake: func(_ context.Context, worker string) error { w.woken = append(w.woken, worker); return nil }}
	return w
}

func (w *world) run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch c.Argv[0] {
	case "ssh":
		w.ssh = append(w.ssh, c.Argv)
		i := slices.Index(c.Argv, "hub")
		var out, errb bytes.Buffer
		code := mailbox.Box{Dir: w.factory.Dir, Clock: w.clk}.Serve("desktop", strings.Join(c.Argv[i+1:], " "), c.Stdin, &out, &errb)
		return execx.Result{Argv: c.Argv, Code: code, Stdout: out.Bytes(), Stderr: errb.Bytes()}, nil
	case "lever":
		if c.Dir != w.p.Cfg.Instance {
			w.t.Errorf("lever ran in %s", c.Dir)
		}
		w.notes = append(w.notes, c.Argv)
		if w.failNote > 0 {
			w.failNote--
			return execx.Result{Argv: c.Argv, Code: 1, Stderr: []byte("msg: 409 Conflict: worker is suspended")}, nil
		}
		return execx.Result{Argv: c.Argv}, nil
	}
	w.t.Errorf("ran %q", c.Argv)
	return execx.Result{Code: 127}, nil
}

func (w *world) step() {
	w.t.Helper()
	if err := w.p.Step(context.Background()); err != nil {
		w.t.Fatalf("step: %v", err)
	}
}

// queue starts a job on the dispatcher: a real bundle of the test repo's
// main, memory and a pinned untracked file.
func (w *world) queue(id string, extra ...jobproto.File) {
	w.t.Helper()
	if w.repo == nil {
		w.repo = testkit.NewRepo(w.t)
	}
	bundle := filepath.Join(w.t.TempDir(), "repo.bundle")
	w.repo.Git("bundle", "create", "-q", bundle, "refs/heads/main")
	b, err := os.ReadFile(bundle)
	if err != nil {
		w.t.Fatal(err)
	}
	meta := jobproto.Meta{Branch: "bot/t0001", Start: "main", Bundle: true, TimeoutMinutes: 30}
	files := append([]jobproto.File{
		{Name: "prompt.md", Data: []byte("fix the thing")}, {Name: "role.md", Data: []byte("you are a worker")},
		{Name: "meta.json", Data: meta.Encode()}, {Name: "repo.bundle", Data: b},
		{Name: "memory/notes.md", Data: []byte("old notes\n")}, {Name: "untracked/Gemfile.lock", Data: []byte("lock\n")},
	}, extra...)
	tar, err := jobproto.TarFlat(files, t0)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.factory.Start("desktop", id, tar); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) jobDir(worker, id string) string {
	return filepath.Join(w.p.Cfg.WorkerDir(worker), "jobs", id)
}

// sh runs a command as the worker would in its container, in dir.
func (w *world) sh(dir string, argv ...string) string {
	w.t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+w.home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("%q: %v\n%s", argv, err, out)
	}
	return strings.TrimSpace(string(out))
}

// work is the worker doing job id as its instructions say: job.sh
// prepare, a commit, progress, job.sh finish, its memory, and
// report.json last through a temp file.
func (w *world) work(worker, id string) (head string) {
	w.t.Helper()
	jd := w.jobDir(worker, id)
	w.sh(jd, "sh", "job.sh", "prepare")
	repo := filepath.Join(jd, "repo")
	if b, err := os.ReadFile(filepath.Join(repo, "Gemfile.lock")); err != nil || string(b) != "lock\n" {
		w.t.Errorf("untracked file not copied: %q %v", b, err)
	}
	if got := w.sh(repo, "git", "config", "user.name"); got != "Factory Bot" {
		w.t.Errorf("identity %q", got)
	}
	os.WriteFile(filepath.Join(repo, "fix.rb"), []byte("fixed\n"), 0o644)
	w.sh(repo, "git", "add", "-A")
	w.sh(repo, "git", "commit", "-q", "-m", "fix: the thing")
	os.WriteFile(filepath.Join(jd, "progress.log"), []byte("committed the fix\n"), 0o644)
	w.sh(jd, "sh", "job.sh", "finish")
	os.WriteFile(filepath.Join(jd, "memory", "notes.md"), []byte("new notes\n"), 0o644)
	os.WriteFile(filepath.Join(jd, "report.json.tmp"), []byte(`{"status": "done", "summary": "fixed"}`), 0o644)
	os.Rename(filepath.Join(jd, "report.json.tmp"), filepath.Join(jd, "report.json"))
	return w.sh(repo, "git", "rev-parse", "HEAD")
}

func (w *world) collected(id string) map[string][]byte {
	w.t.Helper()
	out, err := w.factory.Collect("desktop", id)
	if err != nil {
		w.t.Fatal(err)
	}
	files, err := jobproto.UntarFlat(out)
	if err != nil {
		w.t.Fatal(err)
	}
	return files
}

func (w *world) state(id string) string {
	w.t.Helper()
	st, err := w.factory.State("desktop", id)
	if err != nil {
		w.t.Fatal(err)
	}
	return st
}

func noteText(argv []string) string { return argv[len(argv)-1] }

func TestAJobRunsEndToEnd(t *testing.T) {
	w := newWorld(t)
	w.queue("t0001-w1")
	w.step()
	if st := w.state("t0001-w1"); st != jobproto.StateRunning {
		t.Fatalf("after the claim: %s", st)
	}
	if len(w.notes) != 1 || !slices.Equal(w.notes[0], []string{"lever", "msg", "send", "--to", "fw1", "job t0001-w1 ready at jobs/t0001-w1"}) {
		t.Fatalf("notes %q", w.notes)
	}
	jd := w.jobDir("fw1", "t0001-w1")
	for _, f := range []string{"prompt.md", "role.md", "meta.json", "repo.bundle", "memory/notes.md", "untracked/Gemfile.lock", "job.sh"} {
		if fi, err := os.Lstat(filepath.Join(jd, f)); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s: %v", f, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(jd)); len(entries) != 1 {
		t.Errorf("jobs/ holds %v", entries)
	}

	// running: a beat a minute, with the progress so far
	os.WriteFile(filepath.Join(jd, "progress.log"), []byte("read the code\n"), 0o644)
	w.clk.Advance(61 * time.Second)
	w.step()
	if s, _ := w.factory.Tail(context.Background(), jobproto.Host{Name: "desktop"}, "t0001-w1"); s != "read the code\n" {
		t.Errorf("tail on the dispatcher %q", s)
	}
	if w.factory.Away("desktop") {
		t.Error("a beating host is away")
	}

	head := w.work("fw1", "t0001-w1")
	w.step()
	if st := w.state("t0001-w1"); st != jobproto.StateDone {
		t.Fatalf("after the report: %s", st)
	}
	files := w.collected("t0001-w1")
	if string(files["report.json"]) != `{"status": "done", "summary": "fixed"}` || string(files["head"]) != head+"\n" ||
		string(files["agent.log"]) != "committed the fix\n" || string(files["prompt.md"]) != "fix the thing" {
		t.Errorf("collected %q", files)
	}
	bundle := filepath.Join(t.TempDir(), "out.bundle")
	os.WriteFile(bundle, files["out.bundle"], 0o644)
	if heads := w.sh(t.TempDir(), "git", "bundle", "list-heads", bundle); heads != head+" refs/heads/bot/t0001" {
		t.Errorf("out.bundle heads %q", heads)
	}
	mem := untar(t, files["memory.tar"])
	if string(mem["./notes.md"]) != "new notes\n" || len(mem) != 1 {
		t.Errorf("memory.tar %q", mem)
	}
	if _, err := os.Lstat(jd); err == nil {
		t.Error("the job's directory was left in the worker's")
	}
	if entries, _ := os.ReadDir(w.p.Cfg.State); len(entries) != 0 {
		t.Errorf("state left: %v", entries)
	}
	if len(w.notes) != 1 {
		t.Errorf("more notes: %q", w.notes)
	}
}

func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name], _ = io.ReadAll(tr)
	}
}

func TestATimedOutJobIsStoppedAndReportedAsAnError(t *testing.T) {
	w := newWorld(t)
	w.queue("t0001-w1")
	w.step()
	jd := w.jobDir("fw1", "t0001-w1")
	os.WriteFile(filepath.Join(jd, "progress.log"), []byte("still going\n"), 0o644)
	os.WriteFile(filepath.Join(jd, "head"), []byte("not to be trusted\n"), 0o644)
	for range 31 {
		w.clk.Advance(time.Minute)
		w.step()
	}
	last := w.notes[len(w.notes)-1]
	if !slices.Contains(last, "--interrupt") || !strings.HasPrefix(noteText(last), "stop job t0001-w1: it timed out") {
		t.Errorf("last note %q", last)
	}
	files := w.collected("t0001-w1")
	var rep map[string]string
	json.Unmarshal(files["report.json"], &rep)
	if rep["status"] != "error" || !strings.Contains(rep["summary"], "timed out on desktop") {
		t.Errorf("report %q", files["report.json"])
	}
	if _, ok := files["head"]; ok || string(files["agent.log"]) != "still going\n" {
		t.Errorf("collected %q", files)
	}
	if _, err := os.Lstat(jd); err == nil {
		t.Error("the job's directory was left")
	}
}

// The factory removing the job (its own timeout, a person) reaches the
// desktop as the beat's cancel: the worker is stopped and the job let go.
func TestACancelledJobIsStoppedAndLetGo(t *testing.T) {
	w := newWorld(t)
	w.queue("t0001-w1")
	w.step()
	if err := w.factory.Remove("desktop", "t0001-w1"); err != nil {
		t.Fatal(err)
	}
	w.clk.Advance(time.Minute)
	w.step()
	last := w.notes[len(w.notes)-1]
	if !slices.Contains(last, "--interrupt") || !strings.Contains(noteText(last), "the factory cancelled it") {
		t.Errorf("notes %q", w.notes)
	}
	if _, err := os.Lstat(w.jobDir("fw1", "t0001-w1")); err == nil {
		t.Error("the job's directory was left")
	}
	if len(w.p.jobs) != 0 {
		t.Errorf("still in hand: %v", w.p.jobs)
	}
	verbs := []string{}
	for _, a := range w.ssh {
		verbs = append(verbs, a[slices.Index(a, "hub")+1])
	}
	// then fw2 is idle, so it asks for another job
	if !slices.Equal(verbs, []string{"claim", "beat", "release", "claim"}) {
		t.Errorf("ssh verbs %q", verbs)
	}
}

func TestAFailedNoteWakesTheWorkerAndTriesOnceMore(t *testing.T) {
	w := newWorld(t)
	w.queue("t0001-w1")
	w.failNote = 1
	w.step()
	if !slices.Equal(w.woken, []string{"fw1"}) || len(w.notes) != 2 {
		t.Fatalf("woken %q, notes %q", w.woken, w.notes)
	}
	if st := w.state("t0001-w1"); st != jobproto.StateRunning {
		t.Errorf("handed after the wake: %s", st)
	}

	w2 := newWorld(t)
	w2.queue("t0002-w1")
	w2.failNote = 2
	w2.step()
	if st := w2.state("t0002-w1"); st != jobproto.StateQueued {
		t.Errorf("a job no worker took is not back in the queue: %s", st)
	}
	if _, err := os.Lstat(w2.jobDir("fw1", "t0002-w1")); err == nil {
		t.Error("the job's directory was left")
	}
}

// A job whose tar does not unpack (here a link) is never handed to a
// worker: it is posted at once with an error report.
func TestABadJobIsReportedNotRun(t *testing.T) {
	w := newWorld(t)
	w.repo = testkit.NewRepo(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []tar.Header{
		{Typeflag: tar.TypeReg, Name: "meta.json", Size: int64(len(jobproto.Meta{Branch: "task", Start: "none"}.Encode())), Mode: 0o644},
		{Typeflag: tar.TypeSymlink, Name: "prompt.md", Linkname: "/home/someone/.ssh/id_ed25519"},
	} {
		tw.WriteHeader(&h)
		if h.Typeflag == tar.TypeReg {
			tw.Write(jobproto.Meta{Branch: "task", Start: "none"}.Encode())
		}
	}
	tw.Close()
	if err := w.factory.Start("desktop", "t0001-k1", buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	w.step()
	w.step()
	if len(w.notes) != 0 {
		t.Errorf("notes %q", w.notes)
	}
	files := w.collected("t0001-k1")
	if !strings.Contains(string(files["report.json"]), "could not lay out the job: bad job input: prompt.md is a link") {
		t.Errorf("report %q", files["report.json"])
	}
	if entries, _ := os.ReadDir(filepath.Join(w.p.Cfg.WorkerDir("fw1"), "jobs")); len(entries) != 0 {
		t.Errorf("jobs/ holds %v", entries)
	}
}

func TestBusyWorkersAreSkipped(t *testing.T) {
	w := newWorld(t)
	stale := filepath.Join(w.p.Cfg.WorkerDir("fw1"), "jobs", "t0099-w1")
	os.MkdirAll(stale, 0o755)
	w.queue("t0001-w1")
	w.step()
	if len(w.notes) != 1 || w.notes[0][4] != "fw2" {
		t.Fatalf("notes %q", w.notes)
	}
	w.queue("t0002-w1")
	w.clk.Advance(time.Minute)
	n := len(w.ssh)
	w.step() // both busy: no claim (a beat for t0001 only)
	for _, a := range w.ssh[n:] {
		if slices.Contains(a, "claim") {
			t.Errorf("claimed with no idle worker: %q", a)
		}
	}
	// fw1's old job has a report now: it is idle
	os.WriteFile(filepath.Join(stale, "report.json"), []byte("{}"), 0o644)
	w.clk.Advance(time.Minute)
	w.step()
	if st := w.state("t0002-w1"); st != jobproto.StateRunning {
		t.Errorf("t0002 on the freed worker: %s", st)
	}
}

// What a worker plants in its own directory never takes factory-pull
// outside it: a jobs/ link makes the worker unusable, and a link for a
// result is left out of what is posted.
func TestLinksPlantedByAWorkerAreNotFollowed(t *testing.T) {
	w := newWorld(t)
	secret := filepath.Join(w.outside, "id_ed25519")
	os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600)
	if err := os.Symlink(w.outside, filepath.Join(w.p.Cfg.WorkerDir("fw1"), "jobs")); err != nil {
		t.Fatal(err)
	}
	w.queue("t0001-w1")
	w.step()
	if len(w.notes) != 1 || w.notes[0][4] != "fw2" {
		t.Fatalf("notes %q", w.notes)
	}
	if entries, _ := os.ReadDir(w.outside); len(entries) != 1 {
		t.Errorf("written outside the worker: %v", entries)
	}
	jd := w.jobDir("fw2", "t0001-w1")
	os.Symlink(secret, filepath.Join(jd, "out.bundle"))
	os.Symlink(secret, filepath.Join(jd, "progress.log"))
	os.Remove(filepath.Join(jd, "memory", "notes.md"))
	os.Symlink(secret, filepath.Join(jd, "memory", "notes.md"))
	os.Symlink(secret, filepath.Join(jd, "report.json"))
	w.clk.Advance(time.Minute)
	w.step()
	if st := w.state("t0001-w1"); st != jobproto.StateRunning {
		t.Errorf("a linked report.json counted as a report: %s", st)
	}
	os.Remove(filepath.Join(jd, "report.json"))
	os.WriteFile(filepath.Join(jd, "report.json"), []byte(`{"status": "done"}`), 0o644)
	w.step()
	files := w.collected("t0001-w1")
	for name, data := range files {
		if bytes.Contains(data, []byte("PRIVATE KEY")) {
			t.Errorf("%s carries the secret", name)
		}
	}
	if _, ok := files["out.bundle"]; ok {
		t.Error("a linked out.bundle was posted")
	}
}

// A restart carries on with the jobs in hand.
func TestARestartCarriesOn(t *testing.T) {
	w := newWorld(t)
	w.queue("t0001-w1")
	w.step()
	w.work("fw1", "t0001-w1")
	restarted := &Puller{Cfg: w.p.Cfg, Exec: w.p.Exec, Clock: w.clk, Log: t.Logf}
	w.p = restarted
	w.step()
	if st := w.state("t0001-w1"); st != jobproto.StateDone {
		t.Errorf("after a restart: %s", st)
	}
}

func TestJobScriptValues(t *testing.T) {
	s := string(script("t0001-w1", jobproto.Meta{Branch: "bot/t0001", Start: "main", Bundle: true}, "Bot", "b@e"))
	for _, want := range []string{"BRANCH='bot/t0001'\n", "START='main'\n", "BUNDLE='1'\n", "GIT_NAME='Bot'\n", "GIT_EMAIL='b@e'\n", "job t0001-w1 "} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(s, "@") && strings.Contains(s, "@BRANCH@") {
		t.Error("a placeholder is left")
	}
}

func TestOperatorWakePostsToTheSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "operator.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}
	got := make(chan string, 2)
	status := http.StatusOK
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- fmt.Sprintf("%s %s %s", r.Method, r.URL.Path, body)
		rw.WriteHeader(status)
		io.WriteString(rw, "not asleep\n")
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if err := OperatorWake(context.Background(), sock, "fw1"); err != nil {
		t.Fatal(err)
	}
	if s := <-got; s != `POST /operator/wake {"login":"factory-pull","tier":"operator","worker":"fw1"}` {
		t.Errorf("request %s", s)
	}
	status = http.StatusConflict
	if err := OperatorWake(context.Background(), sock, "fw1"); err == nil || !strings.Contains(err.Error(), "409 Conflict: not asleep") {
		t.Errorf("409: %v", err)
	}
}
