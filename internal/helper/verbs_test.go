package helper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/execx"
)

// port: TF FactoryRemoteTest.test_ssh_cannot_call_run_or_unknown_verbs
func TestSSHCannotCallRunOrUnknownVerbs(t *testing.T) {
	r := newRig(t)
	r.write(t, "jobs/t0001-t1/meta.json", goodMeta)
	for _, command := range []string{
		"run t0001-t1", "rm -rf /", "status ../../etc", // the Python test's three
		"rebuild", "version", "run", "", "   ", "start", "start a b",
		"status 'bad name'", "status ''", `status "unclosed`, "STATUS t0001-t1",
		"status t0001-t1; rm -rf ~", "collect -x", "tail ../t0001-t1",
	} {
		r.reset()
		if code := r.ssh(t, command); code != 2 {
			t.Errorf("%q: exit %d, want 2", command, code)
		}
		if r.stderr.Len() == 0 {
			t.Errorf("%q: no message on stderr", command)
		}
		if r.stdout.Len() != 0 {
			t.Errorf("%q: stdout %q, want nothing", command, r.stdout)
		}
		if got := r.exec.argvs(); len(got) != 0 {
			t.Errorf("%q: ran %v", command, got)
		}
		if onDisk(filepath.Join(r.jobDir("t0001-t1"), "exit")) {
			t.Fatalf("%q: a job ran", command)
		}
	}
}

// The usage is the Python docstring, printed for an unknown verb.
func TestUsageIsTheDocstring(t *testing.T) {
	r := newRig(t)
	r.ssh(t, "help")
	if !strings.HasPrefix(r.stderr.String(), "Runs dispatcher stages as Pi or Claude Code jobs on a helper host.") ||
		!strings.Contains(r.stderr.String(), "list            job names, one per line") {
		t.Errorf("usage:\n%s", r.stderr)
	}
}

// With SSH_ORIGINAL_COMMAND set, the verb comes from it alone: arguments
// cannot smuggle in run or rebuild, and the argv-only verbs never arrive
// through it.
func TestArgvOnlyVerbsNeverComeOverSSH(t *testing.T) {
	r := newRig(t)
	r.write(t, "jobs/t0001-t1/meta.json", goodMeta)
	r.Environ = append(r.Environ, "SSH_ORIGINAL_COMMAND=status t0001-t1")
	for _, args := range [][]string{{"run", "t0001-t1"}, {"rebuild"}, {"version"}, {"status", "t0001-t1"}} {
		r.reset()
		if code := r.Serve(t.Context(), append([]string{r.Self}, args...)); code != 2 {
			t.Errorf("%v with SSH_ORIGINAL_COMMAND: exit %d, want 2", args, code)
		}
		if len(r.exec.argvs()) != 0 || onDisk(filepath.Join(r.jobDir("t0001-t1"), "exit")) {
			t.Errorf("%v with SSH_ORIGINAL_COMMAND ran something: %v", args, r.exec.argvs())
		}
	}
}

// Invoked as `factory` (the helper's bin/factory symlink, which the root
// rebuild unit runs), only rebuild and version are taken.
func TestInvokedAsFactoryTakesOnlyRebuildAndVersion(t *testing.T) {
	r := newRig(t)
	factory := filepath.Join(filepath.Dir(r.Self), "factory")
	for _, args := range [][]string{{}, {"tick"}, {"status", "t0001-t1"}, {"run", "t0001-t1"}, {"start", "t0001-t1"},
		{"list"}, {"rebuild", "--now"}, {"version", "x"}, {"sweep"}} {
		r.reset()
		if code := r.Serve(t.Context(), append([]string{factory}, args...)); code != 2 {
			t.Errorf("factory %v: exit %d, want 2", args, code)
		}
		if len(r.exec.argvs()) != 0 {
			t.Errorf("factory %v ran %v", args, r.exec.argvs())
		}
		if !strings.Contains(r.stderr.String(), "rebuild") {
			t.Errorf("factory %v: stderr %q does not say what it takes", args, r.stderr)
		}
	}
	r.reset()
	if code := r.Serve(t.Context(), []string{factory, "version"}); code != 0 || r.stdout.String() != buildinfo.String()+"\n" {
		t.Errorf("factory version: %d %q", code, r.stdout)
	}
	r.reset()
	if code := r.Serve(t.Context(), []string{"factory", "rebuild"}); code != 0 {
		t.Errorf("factory rebuild: exit %d: %s", code, r.stderr)
	}
	if !slices.ContainsFunc(r.exec.argvs(), func(a []string) bool { return len(a) > 2 && a[1] == "image" && a[2] == "exists" }) {
		t.Errorf("factory rebuild did not look for its images: %v", r.exec.argvs())
	}
	// through SSH, factory takes nothing at all
	r.reset()
	r.Environ = append(r.Environ, "SSH_ORIGINAL_COMMAND=rebuild")
	if code := r.Serve(t.Context(), []string{factory, "rebuild"}); code != 2 || len(r.exec.argvs()) != 0 {
		t.Errorf("factory rebuild over ssh: exit %d, ran %v", code, r.exec.argvs())
	}
}

func TestVersion(t *testing.T) {
	r := newRig(t)
	if code := r.argv(t, "version"); code != 0 || r.stdout.String() != buildinfo.String()+"\n" {
		t.Errorf("version: %d %q", code, r.stdout)
	}
}

// A bad job name is refused before anything touches the disk, with
// Python's repr of the name.
func TestBadJobNames(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct{ command, msg string }{
		{"status ../../etc", "bad job name: '../../etc'\n"},
		{"collect T0001", "bad job name: 'T0001'\n"},
		{"tail -rf", "bad job name: '-rf'\n"},
		{`remove "it's"`, `bad job name: "it's"` + "\n"},
		{"start " + strings.Repeat("a", 42), "bad job name: '" + strings.Repeat("a", 42) + "'\n"},
	} {
		r.reset()
		if code := r.ssh(t, tc.command); code != 2 || r.stderr.String() != tc.msg {
			t.Errorf("%q: exit %d, stderr %q, want %q", tc.command, code, r.stderr, tc.msg)
		}
	}
	r.reset()
	if code := r.argv(t, "run", "../x"); code != 2 || r.stderr.String() != "bad job name: '../x'\n" {
		t.Errorf("run ../x: %d %q", code, r.stderr)
	}
	if onDisk(filepath.Join(r.Root, "x", "exit")) {
		t.Error("run ../x wrote an exit file")
	}
}

// build is a verb of its own: on a host that does not build it is refused,
// and never falls through to remove a job of that name.
func TestBuildIsNotRemove(t *testing.T) {
	r := newRig(t)
	r.write(t, "jobs/35956371f2ff/meta.json", goodMeta)
	if code := r.ssh(t, "build 35956371f2ff"); code != 2 || r.stderr.String() != "no builds on this host\n" {
		t.Errorf("build: %d %q", code, r.stderr)
	}
	if !onDisk(r.jobDir("35956371f2ff")) {
		t.Error("build removed a job")
	}
}

func TestVerbsNeedOneJob(t *testing.T) {
	r := newRig(t)
	for _, verb := range []string{"start", "status", "collect", "tail", "remove"} {
		r.reset()
		if code := r.ssh(t, verb+" a b"); code != 2 || r.stderr.String() != "usage: "+verb+" <job>\n" {
			t.Errorf("%s a b: %d %q", verb, code, r.stderr)
		}
	}
}

func TestStatus(t *testing.T) {
	r := newRig(t)
	active := 3
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Code: active}, nil }
	state := func(job string) string {
		t.Helper()
		r.reset()
		if code := r.ssh(t, "status "+job); code != 0 {
			t.Fatalf("status %s: exit %d: %s", job, code, r.stderr)
		}
		var v map[string]string
		if err := json.Unmarshal(r.stdout.Bytes(), &v); err != nil || !strings.HasSuffix(r.stdout.String(), "\"}\n") {
			t.Fatalf("status %s: %q", job, r.stdout)
		}
		return v["state"]
	}
	if got := state("t0001-w1"); got != "missing" || len(r.exec.argvs()) != 0 {
		t.Errorf("no dir: %s, ran %v", got, r.exec.argvs())
	}
	r.write(t, "jobs/t0001-w1/meta.json", goodMeta)
	if got := state("t0001-w1"); got != "missing" {
		t.Errorf("dir, unit inactive: %s", got)
	}
	want := []string{"systemctl", "--user", "is-active", "--quiet", "factory-job-t0001-w1"}
	if got := r.exec.argvs(); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("unit check %v, want %v", got, want)
	}
	active = 0
	if got := state("t0001-w1"); got != "running" {
		t.Errorf("dir, unit active: %s", got)
	}
	r.write(t, "jobs/t0001-w1/exit", "0")
	if got := state("t0001-w1"); got != "done" || len(r.exec.argvs()) != 0 {
		t.Errorf("exit file: %s, ran %v", got, r.exec.argvs())
	}
	if r.stdout.String() != `{"state": "done"}`+"\n" {
		t.Errorf("status prints %q", r.stdout)
	}
}

func TestRemove(t *testing.T) {
	r := newRig(t)
	r.write(t, "jobs/t0001-w1/work/agent.log", "x")
	locked := filepath.Join(r.jobDir("t0001-w1"), "work", "locked")
	os.MkdirAll(filepath.Join(locked, "deep"), 0o755)
	os.Chmod(locked, 0) // an agent can leave a directory nobody may enter
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) { return execx.Result{Code: 5}, nil }
	if code := r.ssh(t, "remove t0001-w1"); code != 0 {
		t.Fatalf("remove: exit %d: %s", code, r.stderr)
	}
	want := [][]string{
		{"systemctl", "--user", "stop", "factory-job-t0001-w1"},
		{"podman", "rm", "-f", "factory-job-t0001-w1"},
	}
	if got := r.exec.argvs(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("remove ran %v, want %v", got, want)
	}
	if onDisk(r.jobDir("t0001-w1")) {
		t.Error("job dir is still there")
	}
	if r.stdout.String() != `{"removed": "t0001-w1"}`+"\n" {
		t.Errorf("remove prints %q", r.stdout)
	}
	// a job removed before it was done (timed out): its usage goes back
	// with the answer, read from its home before the home is deleted
	r.reset()
	r.write(t, "jobs/t0003-w1/home/.claude/projects/-job/s.jsonl",
		`{"type":"assistant","message":{"id":"m1","model":"claude-opus","usage":{"input_tokens":3,"output_tokens":4}}}`+"\n")
	if code := r.ssh(t, "remove t0003-w1"); code != 0 {
		t.Fatalf("remove: exit %d: %s", code, r.stderr)
	}
	want3 := `{"removed": "t0003-w1", "usage": {"models":{"claude-opus":{"input":3,"output":4,"cache_write":0,"cache_read":0,"messages":1}}}}` + "\n"
	if r.stdout.String() != want3 || onDisk(r.jobDir("t0003-w1")) {
		t.Errorf("remove prints %q", r.stdout)
	}
	// a job that is already gone is removed all the same
	r.reset()
	if code := r.ssh(t, "remove t0002-w1"); code != 0 || r.stdout.String() != `{"removed": "t0002-w1"}`+"\n" {
		t.Errorf("remove of a missing job: %d %q %s", code, r.stdout, r.stderr)
	}
}

func TestList(t *testing.T) {
	r := newRig(t)
	if code := r.ssh(t, "list"); code != 0 || r.stdout.String() != "" {
		t.Errorf("empty list: %d %q", code, r.stdout)
	}
	for _, j := range []string{"t0002-w1", "t0001-r1", "t0010-w1"} {
		r.write(t, "jobs/"+j+"/meta.json", "{}")
	}
	r.write(t, "jobs/.stray", "")
	r.reset()
	if code := r.ssh(t, "list"); code != 0 || r.stdout.String() != ".stray\nt0001-r1\nt0002-w1\nt0010-w1\n" {
		t.Errorf("list: %d %q", code, r.stdout)
	}
	// list works from argv too, as in Python
	r.reset()
	if code := r.argv(t, "list"); code != 0 || !strings.Contains(r.stdout.String(), "t0001-r1") {
		t.Errorf("argv list: %d %q", code, r.stdout)
	}
}

// The jobs directory is made on every call, as Python's main does.
func TestJobsDirIsMade(t *testing.T) {
	r := newRig(t)
	r.ssh(t, "nonsense")
	if fi, err := os.Stat(filepath.Join(r.Root, "jobs")); err != nil || !fi.IsDir() {
		t.Errorf("jobs dir: %v", err)
	}
}
