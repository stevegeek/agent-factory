package helper

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// port: TF FactoryRemoteTest.test_collect_skips_files_reached_through_symlinks
func TestCollectSkipsFilesReachedThroughSymlinks(t *testing.T) {
	r := newRig(t)
	jobs := filepath.Join(r.Root, "jobs")
	secret := r.write(t, "jobs/secret", "key")
	job := r.jobDir("t0001-r1")
	os.MkdirAll(filepath.Join(job, "work"), 0o755)
	os.Symlink(jobs, filepath.Join(job, "home"))
	r.write(t, "jobs/report.json", "{}")
	os.Symlink(secret, filepath.Join(job, "work", "head"))
	r.write(t, "jobs/t0001-r1/work/agent.log", "ok")
	if code := r.ssh(t, "collect t0001-r1"); code != 0 {
		t.Fatalf("collect: exit %d: %s", code, r.stderr)
	}
	got, err := jobproto.UntarFlat(r.stdout.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, []string{"agent.log"}) {
		t.Errorf("collected %v, want only agent.log", names)
	}
}

// A link anywhere on the path is never followed: a linked work dir, a
// link inside the job's own dirs, a fifo (which must not stall collect).
func TestCollectRefusesEveryLinkOnThePath(t *testing.T) {
	r := newRig(t)
	elsewhere := filepath.Join(r.Root, "elsewhere")
	os.MkdirAll(elsewhere, 0o755)
	r.write(t, "elsewhere/agent.log", "secret")
	r.write(t, "elsewhere/prompt.md", "secret")
	job := r.jobDir("t0001-w1")
	os.MkdirAll(filepath.Join(job, "home"), 0o755)
	os.Symlink(elsewhere, filepath.Join(job, "work"))
	// a link inside the job dir, to a file that is also inside it
	r.write(t, "jobs/t0001-w1/meta.json", goodMeta)
	os.Symlink("../meta.json", filepath.Join(job, "home", "report.json"))
	if code := r.ssh(t, "collect t0001-w1"); code != 0 {
		t.Fatalf("collect: %d %s", code, r.stderr)
	}
	if got, _ := jobproto.UntarFlat(r.stdout.Bytes()); len(got) != 0 {
		t.Errorf("collected %v through links", slices.Sorted(maps.Keys(got)))
	}

	os.Remove(filepath.Join(job, "work"))
	os.MkdirAll(filepath.Join(job, "work"), 0o755)
	if err := syscall.Mkfifo(filepath.Join(job, "work", "agent.log"), 0o644); err != nil {
		t.Skip("no fifos here:", err)
	}
	r.write(t, "jobs/t0001-w1/work/head", "abc\n")
	r.reset()
	done := make(chan int)
	go func() { done <- r.ssh(t, "collect t0001-w1") }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("collect: %d %s", code, r.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("collect stalled on a fifo")
	}
	got, _ := jobproto.UntarFlat(r.stdout.Bytes())
	if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, []string{"head"}) {
		t.Errorf("collected %v, want only head", names)
	}
}

// The caps: a member over its cap is left out, except agent.log, which is
// cut to its last 2 MB. Members come in MAX_COLLECTED_BYTES order, each a
// 0644 regular file, and the archive is padded to tarfile's 10240-byte
// record.
func TestCollectCapsAndOrder(t *testing.T) {
	r := newRig(t)
	log := strings.Repeat("x", 2_000_100) + "the end"
	r.write(t, "jobs/t0001-w1/home/report.json", `{"status": "done"}`)
	r.write(t, "jobs/t0001-w1/work/prompt.md", strings.Repeat("p", 1_000_001))
	r.write(t, "jobs/t0001-w1/work/head", strings.Repeat("h", 1000))
	r.write(t, "jobs/t0001-w1/work/agent.log", log)
	r.write(t, "jobs/t0001-w1/work/memory.tar", "mem")
	r.write(t, "jobs/t0001-w1/work/out.bundle", "bundle")
	r.write(t, "jobs/t0001-w1/work/report.json", `{"forged": true}`) // only home/ counts
	r.write(t, "jobs/t0001-w1/work/extra", "not collected")
	r.write(t, "jobs/t0001-w1/work/usage.json", `{"models": {}}`)
	if code := r.ssh(t, "collect t0001-w1"); code != 0 {
		t.Fatalf("collect: %d %s", code, r.stderr)
	}
	out := r.stdout.Bytes()
	if len(out)%10240 != 0 {
		t.Errorf("tar of %d bytes is not padded to a 10240-byte record", len(out))
	}
	tr := tar.NewReader(bytes.NewReader(out))
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		if h.Typeflag != tar.TypeReg || h.Mode != 0o644 || h.Uid != 0 || h.Gid != 0 || h.ModTime.Unix() != 0 || h.Uname != "" {
			t.Errorf("%s: header %+v", h.Name, h)
		}
		switch h.Name {
		case "report.json":
			if string(data) != `{"status": "done"}` {
				t.Errorf("report.json = %q", data)
			}
		case "agent.log":
			if len(data) != 2_000_000 || !strings.HasSuffix(string(data), "the end") {
				t.Errorf("agent.log: %d bytes, ends %q", len(data), data[len(data)-7:])
			}
		case "head":
			if len(data) != 1000 {
				t.Errorf("head: %d bytes", len(data))
			}
		}
	}
	want := []string{"report.json", "head", "agent.log", "out.bundle", "memory.tar", "usage.json"}
	if !slices.Equal(names, want) {
		t.Errorf("members %v, want %v (prompt.md is over its cap)", names, want)
	}
}

func TestCollectOfAMissingJob(t *testing.T) {
	r := newRig(t)
	if code := r.ssh(t, "collect t0009-w1"); code != 2 || r.stderr.String() != "t0009-w1 not found\n" || r.stdout.Len() != 0 {
		t.Errorf("collect of a missing job: %d %q %q", code, r.stderr, r.stdout)
	}
	// an empty job still gives a valid (empty) archive
	os.MkdirAll(r.jobDir("t0009-w1"), 0o755)
	r.reset()
	if code := r.ssh(t, "collect t0009-w1"); code != 0 || r.stdout.Len() != 10240 {
		t.Errorf("collect of an empty job: %d, %d bytes", code, r.stdout.Len())
	}
}

// port: TF FactoryRemoteTest.test_tail_reads_only_the_jobs_own_output
func TestTailReadsOnlyTheJobsOwnOutput(t *testing.T) {
	r := newRig(t)
	r.write(t, "jobs/t0001-w1/work/agent.log", strings.Repeat("x", 70_000)+"the end")
	if code := r.ssh(t, "tail t0001-w1"); code != 0 {
		t.Fatalf("tail: %d %s", code, r.stderr)
	}
	data := r.stdout.Bytes()
	if len(data) != jobproto.TailBytes || !bytes.HasSuffix(data, []byte("the end")) {
		t.Errorf("tail: %d bytes, want %d ending in the end", len(data), jobproto.TailBytes)
	}
	// a link out of the job is never followed
	log := filepath.Join(r.jobDir("t0001-w1"), "work", "agent.log")
	os.Remove(log)
	os.Symlink(r.write(t, "elsewhere", "secret"), log)
	r.reset()
	if code := r.ssh(t, "tail t0001-w1"); code != 0 || r.stdout.Len() != 0 {
		t.Errorf("tail through a link: %d, wrote %q", code, r.stdout)
	}
	// nor is a linked work dir
	os.RemoveAll(filepath.Join(r.jobDir("t0001-w1"), "work"))
	os.MkdirAll(filepath.Join(r.Root, "other"), 0o755)
	r.write(t, "other/agent.log", "secret")
	os.Symlink(filepath.Join(r.Root, "other"), filepath.Join(r.jobDir("t0001-w1"), "work"))
	r.reset()
	if code := r.ssh(t, "tail t0001-w1"); code != 0 || r.stdout.Len() != 0 {
		t.Errorf("tail through a linked work dir: %d, wrote %q", code, r.stdout)
	}
	// a short log comes whole; a job with no log gives nothing
	r.write(t, "jobs/t0002-w1/work/agent.log", "short")
	r.reset()
	if r.ssh(t, "tail t0002-w1"); r.stdout.String() != "short" {
		t.Errorf("short tail %q", r.stdout)
	}
	r.reset()
	if code := r.ssh(t, "tail t0003-w1"); code != 0 || r.stdout.Len() != 0 {
		t.Errorf("tail of no job: %d %q", code, r.stdout)
	}
}
