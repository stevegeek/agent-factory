package helper

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

func (r *rig) start(t *testing.T, job string, input []byte) int {
	t.Helper()
	r.reset()
	r.Stdin = bytes.NewReader(input)
	return r.ssh(t, "start "+job)
}

func TestStartExtractsTheJobAndLaunchesItsUnit(t *testing.T) {
	r := newRig(t)
	in := jobInput(t, goodMeta,
		file("repo.bundle", "bundle"),
		file("memory/README.md", "notes"),
		file("memory/context/brief.md", "brief"),
		file("untracked/Gemfile.lock", "lock"))
	if code := r.start(t, "t0001-w1", in); code != 0 {
		t.Fatalf("start: exit %d: %s", code, r.stderr)
	}
	if r.stdout.String() != `{"started": "t0001-w1"}`+"\n" {
		t.Errorf("start prints %q", r.stdout)
	}
	d := r.jobDir("t0001-w1")
	for rel, want := range map[string]string{
		"work/prompt.md": "do the work\n", "work/role.md": "you are a worker\n", "work/repo.bundle": "bundle",
		"work/memory/README.md": "notes", "work/memory/context/brief.md": "brief",
		"work/untracked/Gemfile.lock": "lock", "meta.json": goodMeta,
	} {
		if got, err := os.ReadFile(filepath.Join(d, rel)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", rel, got, err, want)
		}
	}
	// meta.json is moved out of the agent's reach
	if onDisk(filepath.Join(d, "work", "meta.json")) {
		t.Error("meta.json is still in work/")
	}
	if fi, err := os.Stat(filepath.Join(d, "home")); err != nil || !fi.IsDir() {
		t.Errorf("home/: %v", err)
	}
	want := []string{"systemd-run", "--user", "--quiet", "--collect", "--unit=factory-job-t0001-w1",
		"--property=RuntimeMaxSec=2100", "/opt/factory/bin/factory-remote", "run", "t0001-w1"}
	got := r.exec.argvs()
	if len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("start ran %v, want %v", got, want)
	}
	// the unit's argv keeps `factory-remote run`, which host/updates.sh waits on
	if !strings.Contains(strings.Join(got[0], " "), "factory-remote run t0001-w1") {
		t.Error("the process-name contract is broken")
	}
	// the environment handed to systemd-run is the minimal one
	if env := r.exec.calls[0].Env; slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "SECRET_TOKEN=") }) {
		t.Errorf("systemd-run got %v", env)
	}
}

func TestStartTimeoutIsClampedForTheUnit(t *testing.T) {
	r := newRig(t)
	if code := r.start(t, "t0001-w1", jobInput(t, `{"branch": "x", "start": "main", "timeout_minutes": 9999}`)); code != 0 {
		t.Fatalf("start: %d %s", code, r.stderr)
	}
	if got := r.exec.argvs()[0][5]; got != "--property=RuntimeMaxSec=7500" {
		t.Errorf("unit limit %s, want 120*60+300", got)
	}
}

// Every refusal of start, in Python's order, leaves no job dir and runs
// nothing.
func TestStartRefusals(t *testing.T) {
	r := newRig(t)
	in := jobInput(t, goodMeta)
	check := func(name, job string, input []byte, msg string) {
		t.Helper()
		code := r.start(t, job, input)
		if code != 2 || r.stderr.String() != msg+"\n" {
			t.Errorf("%s: exit %d, stderr %q, want %q", name, code, r.stderr, msg)
		}
		if r.exec.ran("systemd-run") {
			t.Errorf("%s: launched a unit", name)
		}
		if job != "t0001-w1" && onDisk(r.jobDir(job)) {
			t.Errorf("%s: left %s", name, r.jobDir(job))
		}
	}
	r.write(t, "jobs/t0001-w1/meta.json", "{}")
	check("existing job", "t0001-w1", in, "t0001-w1 already exists")

	os.WriteFile(r.Draining, nil, 0o644)
	check("draining", "t0002-w1", in, "host is draining for a reboot")
	os.Remove(r.Draining)

	for _, j := range []string{"t0003-w1", "t0004-w1", "t0005-w1"} {
		os.MkdirAll(r.jobDir(j), 0o755)
	}
	check("max_jobs (4 by default)", "t0006-w1", in, "too many jobs on this host")
	os.RemoveAll(r.jobDir("t0005-w1"))
	r.write(t, "config.toml", "[remote]\nmax_jobs = 3\n")
	check("max_jobs from config", "t0006-w1", in, "too many jobs on this host")
	r.write(t, "config.toml", "[remote]\nmax_jobs = 5\n")

	r.FreeDisk = func(string) (uint64, error) { return 2<<30 - 1, nil }
	check("free disk", "t0006-w1", in, "not enough free disk on this host")
}

// The 200 MiB cap counts every byte of stdin, also after the archive ends.
func TestStartInputCap(t *testing.T) {
	r := newRig(t)
	in := jobInput(t, goodMeta)
	big := io.MultiReader(bytes.NewReader(in), io.LimitReader(zeros{}, jobproto.MaxInputBytes+1-int64(len(in))))
	r.reset()
	r.Stdin = big
	if code := r.ssh(t, "start t0001-w1"); code != 2 || r.stderr.String() != "job input too large\n" {
		t.Errorf("over the cap: %d %q", code, r.stderr)
	}
	if onDisk(r.jobDir("t0001-w1")) || r.exec.ran("systemd-run") {
		t.Error("an oversized start left a job")
	}
	// exactly at the cap is fine
	r.reset()
	r.Stdin = io.MultiReader(bytes.NewReader(in), io.LimitReader(zeros{}, jobproto.MaxInputBytes-int64(len(in))))
	if code := r.ssh(t, "start t0001-w1"); code != 0 {
		t.Errorf("at the cap: %d %q", code, r.stderr)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// The tar is extracted as tarfile's "data" filter does, or tighter: no
// member leaves work/, links and special files are refused, leading
// slashes are stripped, and modes lose their high and group/other write
// bits.
func TestStartExtractionIsContained(t *testing.T) {
	refused := []struct {
		name string
		m    member
	}{
		{"dot-dot", file("../escape", "x")},
		{"dot-dot inside", file("memory/../../escape", "x")},
		{"dot-dot only", file("..", "x")},
		{"symlink out", member{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc/passwd"}}},
		{"symlink in", member{hdr: tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "prompt.md"}}},
		{"hard link", member{hdr: tar.Header{Typeflag: tar.TypeLink, Name: "hard", Linkname: "prompt.md"}}},
		{"char device", member{hdr: tar.Header{Typeflag: tar.TypeChar, Name: "dev", Devmajor: 1, Devminor: 3}}},
		{"block device", member{hdr: tar.Header{Typeflag: tar.TypeBlock, Name: "blk", Devmajor: 8}}},
		{"fifo", member{hdr: tar.Header{Typeflag: tar.TypeFifo, Name: "fifo"}}},
		{"meta.json a directory", member{hdr: tar.Header{Typeflag: tar.TypeDir, Name: "meta.json/", Mode: 0o755}}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			var in []byte
			if tc.name == "meta.json a directory" {
				in = mktar(t, file("prompt.md", "p"), tc.m)
			} else {
				in = jobInput(t, goodMeta, tc.m)
			}
			code := r.start(t, "t0001-w1", in)
			if code != 2 || !strings.HasPrefix(r.stderr.String(), "bad job input: ") {
				t.Errorf("exit %d, stderr %q", code, r.stderr)
			}
			if onDisk(r.jobDir("t0001-w1")) || r.exec.ran("systemd-run") {
				t.Error("a refused input left a job")
			}
			for _, p := range []string{filepath.Join(r.Root, "jobs", "escape"), filepath.Join(r.Root, "escape")} {
				if onDisk(p) {
					t.Errorf("%s was written", p)
				}
			}
		})
	}

	r := newRig(t)
	in := jobInput(t, goodMeta,
		file("/abs/rooted.md", "abs"),
		member{hdr: tar.Header{Typeflag: tar.TypeReg, Name: "setuid", Mode: 0o4777}, data: "s"},
		member{hdr: tar.Header{Typeflag: tar.TypeReg, Name: "noexec", Mode: 0o646}, data: "n"},
		member{hdr: tar.Header{Typeflag: tar.TypeReg, Name: "unreadable", Mode: 0}, data: "u"},
		member{hdr: tar.Header{Typeflag: tar.TypeReg, Name: "groupexec", Mode: 0o070}, data: "g"},
		member{hdr: tar.Header{Typeflag: tar.TypeDir, Name: "dir/", Mode: 0o7777}},
		member{hdr: tar.Header{Typeflag: tar.TypeReg, Name: "dir/owned", Mode: 0o644, Uid: 0, Gid: 0, Uname: "root"}, data: "o"},
		member{hdr: tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o700}},
	)
	if code := r.start(t, "t0001-w1", in); code != 0 {
		t.Fatalf("start: %d %s", code, r.stderr)
	}
	work := filepath.Join(r.jobDir("t0001-w1"), "work")
	if got, _ := os.ReadFile(filepath.Join(work, "abs", "rooted.md")); string(got) != "abs" {
		t.Errorf("a leading / is stripped into work/: %q", got)
	}
	for name, want := range map[string]os.FileMode{"setuid": 0o755, "noexec": 0o644, "unreadable": 0o600, "groupexec": 0o640, "prompt.md": 0o644} {
		fi, err := os.Lstat(filepath.Join(work, name))
		if err != nil || fi.Mode() != want {
			t.Errorf("%s: mode %v, %v; want %v", name, fi.Mode(), err, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(work, "dir")); err != nil || fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !fi.IsDir() {
		t.Errorf("dir: %v %v", fi.Mode(), err)
	}
}

func TestStartRefusesBadMeta(t *testing.T) {
	for _, tc := range []struct{ name, meta, msg string }{
		{"not an object", `["not", "a", "dict"]`, "bad job input: meta.json is not an object"},
		{"bad branch", `{"branch": "../x", "start": "main"}`, "bad job input: bad branch in meta.json"},
		{"bad start", `{"branch": "x", "start": "-main"}`, "bad job input: bad start in meta.json"},
		{"bad model", `{"branch": "x", "start": "main", "model": "--help"}`, "bad job input: bad model in meta.json"},
		{"unknown harness", `{"branch": "x", "start": "main", "harness": "codex"}`, "bad job input: no harness \"codex\" on this host"},
		{"gate job", `{"branch": "gate", "start": "` + strings.Repeat("a", 40) + `", "kind": "gate"}`, "bad job input: no gate jobs on this host"},
		{"not json", `{`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			code := r.start(t, "t0001-w1", jobInput(t, tc.meta))
			if code != 2 || !strings.HasPrefix(r.stderr.String(), "bad job input: ") ||
				tc.msg != "" && r.stderr.String() != tc.msg+"\n" {
				t.Errorf("exit %d, stderr %q, want %q", code, r.stderr, tc.msg)
			}
			if onDisk(r.jobDir("t0001-w1")) || r.exec.ran("systemd-run") {
				t.Error("bad meta left a job")
			}
		})
	}
	r := newRig(t)
	if code := r.start(t, "t0001-w1", mktar(t, file("prompt.md", "p"))); code != 2 || !strings.HasPrefix(r.stderr.String(), "bad job input: ") {
		t.Errorf("no meta.json: %d %q", code, r.stderr)
	}
	if code := r.start(t, "t0001-w1", []byte("not a tar at all, just some bytes that go on")); code != 2 || !strings.HasPrefix(r.stderr.String(), "bad job input: ") {
		t.Errorf("not a tar: %d %q", code, r.stderr)
	}
	if onDisk(r.jobDir("t0001-w1")) {
		t.Error("left a job dir")
	}
}

func TestStartCleansUpWhenSystemdRunFails(t *testing.T) {
	r := newRig(t)
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Code: 1, Stderr: []byte("Failed to start transient service unit: boom\n")}, nil
	}
	if code := r.start(t, "t0001-w1", jobInput(t, goodMeta)); code != 2 ||
		r.stderr.String() != "systemd-run failed: Failed to start transient service unit: boom\n" {
		t.Errorf("exit %d, stderr %q", code, r.stderr)
	}
	if onDisk(r.jobDir("t0001-w1")) {
		t.Error("the job dir is left behind")
	}
}

func TestStartRefusesABadConfig(t *testing.T) {
	r := newRig(t)
	r.write(t, "config.toml", "[remote]\nmax_jobs = [1]\n")
	if code := r.start(t, "t0001-w1", jobInput(t, goodMeta)); code == 0 || r.exec.ran("systemd-run") {
		t.Errorf("a bad max_jobs started a job: %d", code)
	}
	r.write(t, "config.toml", "[remote\n")
	if code := r.start(t, "t0001-w1", jobInput(t, goodMeta)); code == 0 || r.exec.ran("systemd-run") {
		t.Errorf("an unparsable config started a job: %d", code)
	}
	if onDisk(r.jobDir("t0001-w1")) {
		t.Error("left a job dir")
	}
}
