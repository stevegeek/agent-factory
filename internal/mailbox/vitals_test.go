package mailbox

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jobproto"
)

const posted = `{"taken": "2020-01-01T00:00:00Z", "version": "v1", "hostname": "desktop", "load": [0.5, 0.4, 0.3], "cpus": 8,
"memory": {"total": 100, "available": 60, "used": 40, "swap_total": 0, "swap_used": 0}, "containers": null}`

func (r *rig) serve(command string, stdin []byte) (int, string, string) {
	var out, errb bytes.Buffer
	code := r.Serve("desktop", command, bytes.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// vitals HOST stores what the host sent, stamped with the mailbox's clock,
// and records that it called in.
func TestServeVitals(t *testing.T) {
	r := newRig(t)
	if _, err := r.Vitals("desktop"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no post yet: %v", err)
	}
	if code, out, errs := r.serve("vitals desktop", []byte(posted)); code != 0 || out != "ok\n" {
		t.Fatalf("vitals: %d %q %q", code, out, errs)
	}
	v, err := r.Vitals("desktop")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Taken.Equal(t0) || !v.Reported.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) || v.Hostname != "desktop" ||
		v.CPUs != 8 || v.Memory.Used != 40 || v.Containers != nil {
		t.Errorf("stored %+v", v)
	}
	if r.Away("desktop") {
		t.Error("a vitals post is not a call in")
	}
	fi, err := os.Lstat(filepath.Join(r.Dir, "desktop", jobproto.MailVitalsFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("vitals.json %v %v", fi, err)
	}
	// it is no job
	if names, _ := r.Jobs("desktop"); len(names) != 0 {
		t.Errorf("jobs %v", names)
	}
	// Do does not answer vitals for a pull host: the tick reads the post
	if _, err := r.Do(context.Background(), jobproto.Host{Name: "desktop"}, jobproto.VerbVitals, "", nil, 0); err == nil {
		t.Error("Do answered vitals")
	}
}

// Another host's name, a job, bad JSON or too much refuse the post and
// keep the last one.
func TestServeVitalsRefuses(t *testing.T) {
	r := newRig(t)
	if code, _, _ := r.serve("vitals desktop", []byte(posted)); code != 0 {
		t.Fatal("first post")
	}
	big := `{"hostname": "` + strings.Repeat("x", jobproto.MaxVitals) + `"}`
	for _, c := range []struct{ command, stdin string }{
		{"vitals laptop", posted},
		{"vitals desktop t0001-w1", posted},
		{"vitals", posted},
		{"vitals desktop", "{"},
		{"vitals desktop", `{"load": [1, 2, 3, 4]}`},
		{"vitals desktop", `{"hostname": "a\u001bb"}`},
		{"vitals desktop", big},
	} {
		if code, out, errs := r.serve(c.command, []byte(c.stdin)); code != 2 || out != "" || errs == "" {
			t.Errorf("%q %.20q: %d %q %q", c.command, c.stdin, code, out, errs)
		}
	}
	if v, err := r.Vitals("desktop"); err != nil || v.Hostname != "desktop" {
		t.Errorf("the last post is gone: %+v %v", v, err)
	}
	// a planted link is not read
	p := filepath.Join(r.Dir, "desktop", jobproto.MailVitalsFile)
	os.Remove(p)
	other := filepath.Join(t.TempDir(), "v.json")
	os.WriteFile(other, []byte(posted), 0o600)
	os.Symlink(other, p)
	if _, err := r.Vitals("desktop"); err == nil {
		t.Error("read through a link")
	}
}
