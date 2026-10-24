package helper

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// vitalsRig is a rig whose /proc is a fixture, whose disks are fake and
// whose podman names two containers.
func vitalsRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.write(t, "proc/sys/kernel/hostname", "helpera\n")
	r.write(t, "proc/loadavg", "0.50 0.40 0.30 1/200 999\n")
	r.write(t, "proc/meminfo", "MemTotal: 8000000 kB\nMemAvailable: 6000000 kB\n")
	r.write(t, "images.json", `{"pi": {"version": "0.9.1", "since": "2026-10-01T03:00:00+00:00", "previous": "0.9.0"}}`)
	r.ProcRoot = r.Root
	r.Statfs = func(p string) (vitals.Disk, error) { return vitals.Disk{Path: p, Total: 100, Free: 40, Used: 60}, nil }
	r.exec.answer = func(c execx.Cmd) (execx.Result, error) {
		if slices.Equal(c.Argv, []string{"podman", "ps", "--format", "{{.Names}}"}) {
			return execx.Result{Stdout: []byte("factory-job-t0001-w1\nfactory-job-t0002-w1\n")}, nil
		}
		return execx.Result{Code: 1}, nil
	}
	return r
}

// vitals answers with this host's vital signs, its containers among them,
// and nothing of its environment.
func TestVitals(t *testing.T) {
	r := vitalsRig(t)
	if code := r.ssh(t, "vitals"); code != 0 {
		t.Fatalf("vitals: exit %d: %s", code, r.stderr)
	}
	out := r.stdout.String()
	v, err := vitals.Parse([]byte(out))
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if v.Hostname != "helpera" || len(v.Load) != 3 || v.Memory == nil || v.Memory.Total != 8000000*1024 ||
		!v.Taken.Equal(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("vitals %+v", v)
	}
	if len(v.Disks) != 2 || v.Disks[0].Path != "/" || v.Disks[1].Path != r.Root {
		t.Errorf("disks %+v", v.Disks)
	}
	if !slices.Equal(v.Containers, []string{"factory-job-t0001-w1", "factory-job-t0002-w1"}) {
		t.Errorf("containers %v", v.Containers)
	}
	if len(v.Images) != 1 || v.Images["pi"] != (vitals.Image{Version: "0.9.1", Since: "2026-10-01T03:00:00+00:00", Previous: "0.9.0"}) {
		t.Errorf("images %+v", v.Images)
	}
	for _, leak := range []string{"SECRET_TOKEN", "leak", "XDG_RUNTIME_DIR", "PATH"} {
		if strings.Contains(out, leak) {
			t.Errorf("vitals carry %q: %s", leak, out)
		}
	}
	// podman runs with the minimal environment, bounded
	for _, c := range r.exec.calls {
		if slices.Contains(c.Env, "SECRET_TOKEN=leak") {
			t.Errorf("podman got the session's environment: %v", c.Env)
		}
	}
}

// A podman that fails is a problem in the answer, not a failed verb; vitals
// takes no job.
func TestVitalsWithoutPodmanAndWithAJob(t *testing.T) {
	r := vitalsRig(t)
	r.exec.answer = func(execx.Cmd) (execx.Result, error) { return execx.Result{}, errors.New("podman: not found") }
	if code := r.ssh(t, "vitals"); code != 0 {
		t.Fatalf("vitals: exit %d: %s", code, r.stderr)
	}
	v, err := vitals.Parse(r.stdout.Bytes())
	if err != nil || v.Containers != nil || !strings.Contains(strings.Join(v.Problems, "\n"), "podman: not found") {
		t.Errorf("no podman: %+v %v", v, err)
	}
	// a broken images.json is a problem too; a missing one is no images
	r.reset()
	r.write(t, "images.json", "{")
	if code := r.ssh(t, "vitals"); code != 0 {
		t.Fatalf("vitals: exit %d: %s", code, r.stderr)
	}
	if v, err := vitals.Parse(r.stdout.Bytes()); err != nil || v.Images != nil || !strings.Contains(strings.Join(v.Problems, "\n"), "images.json") {
		t.Errorf("broken images.json: %+v %v", v, err)
	}
	r.reset()
	os.Remove(filepath.Join(r.Root, "images.json"))
	if code := r.ssh(t, "vitals"); code != 0 {
		t.Fatalf("vitals: exit %d: %s", code, r.stderr)
	}
	if v, err := vitals.Parse(r.stdout.Bytes()); err != nil || v.Images != nil || strings.Contains(strings.Join(v.Problems, "\n"), "images.json") {
		t.Errorf("no images.json: %+v %v", v, err)
	}
	r.reset()
	if code := r.ssh(t, "vitals t0001-w1"); code != 2 || r.stdout.Len() != 0 || len(r.exec.argvs()) != 0 {
		t.Errorf("vitals with a job: exit %d %q %v", code, r.stdout, r.exec.argvs())
	}
}
