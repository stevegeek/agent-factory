package pull

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// queueGate starts a gate job on the dispatcher: a bundle of the test
// repo's main and a branch, the branch's commit as the gated head, the
// checks and a pinned untracked file.
func (w *world) queueGate(id, checks string) (head string) {
	w.t.Helper()
	if w.repo == nil {
		w.repo = testkit.NewRepo(w.t)
	}
	w.repo.BranchWith("bot/x", map[string]string{"fix.rb": "fixed\n"})
	head = w.repo.Git("rev-parse", "bot/x")
	bundle := filepath.Join(w.t.TempDir(), "repo.bundle")
	w.repo.Git("bundle", "create", "-q", bundle, "refs/heads/bot/x", "refs/heads/main")
	b, err := os.ReadFile(bundle)
	if err != nil {
		w.t.Fatal(err)
	}
	meta := jobproto.Meta{Branch: "gate", Start: head, TimeoutMinutes: 35, Kind: jobproto.KindGate}
	tar, err := jobproto.TarFlat([]jobproto.File{
		{Name: "meta.json", Data: meta.Encode()}, {Name: "checks.sh", Data: []byte(checks)},
		{Name: "repo.bundle", Data: b}, {Name: "untracked/Gemfile.lock", Data: []byte("lock\n")},
	}, t0)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.factory.Start("desktop", id, tar); err != nil {
		w.t.Fatal(err)
	}
	return head
}

func gateReport(t *testing.T, files map[string][]byte) map[string]any {
	t.Helper()
	var rep map[string]any
	if err := json.Unmarshal(files["report.json"], &rep); err != nil {
		t.Fatalf("report %q: %v", files["report.json"], err)
	}
	return rep
}

// A gate job is laid out with gate.sh (no job.sh, no prompt) and handed
// over with the usual note; the worker only runs gate.sh, which checks out
// the gated commit with the pinned files, runs the checks and reports
// their exit and output, which are posted as any job's are.
func TestAGateJobRunsTheChecksAndReportsTheirExit(t *testing.T) {
	for _, c := range []struct {
		checks string
		exit   float64
		log    string
	}{
		{"set -e; echo installing; test -f Gemfile.lock; test -f fix.rb; echo tests ok", 0, "installing\ntests ok\n"},
		{"set -e; echo installing; false; echo never", 1, "installing\n"},
	} {
		w := newWorld(t)
		head := w.queueGate("t0001-g1", c.checks)
		w.step()
		if len(w.notes) != 1 || noteText(w.notes[0]) != "job t0001-g1 ready at jobs/t0001-g1" {
			t.Fatalf("notes %q", w.notes)
		}
		jd := w.jobDir("fw1", "t0001-g1")
		if _, err := os.Lstat(filepath.Join(jd, ScriptName)); err == nil {
			t.Error("a gate job got job.sh")
		}
		script, err := os.ReadFile(filepath.Join(jd, GateScriptName))
		if err != nil || !strings.Contains(string(script), "HEAD='"+head+"'") || !strings.Contains(string(script), "LIMIT='1980'") {
			t.Fatalf("gate.sh %v:\n%s", err, script)
		}
		w.sh(jd, "sh", "gate.sh")
		w.step()
		files := w.collected("t0001-g1")
		rep := gateReport(t, files)
		if rep["status"] != "done" || rep["gate_exit"] != c.exit {
			t.Errorf("%s: report %v", c.checks, rep)
		}
		if string(files["agent.log"]) != c.log {
			t.Errorf("%s: agent.log %q", c.checks, files["agent.log"])
		}
		if checkout := filepath.Join(jd, "repo"); dirExists(checkout) {
			t.Error("the job's directory was left")
		}
	}
}

// A gate.sh that cannot check out the commit reports an error, not a
// failed check; a second run of it refuses.
func TestAGateJobThatCannotRunReportsAnError(t *testing.T) {
	w := newWorld(t)
	w.queueGate("t0001-g1", "true")
	w.step()
	jd := w.jobDir("fw1", "t0001-g1")
	os.WriteFile(filepath.Join(jd, "repo.bundle"), []byte("not a bundle"), 0o644)
	cmd := []string{"sh", "-c", "sh gate.sh; echo rc=$?"}
	if out := w.sh(jd, cmd...); !strings.HasSuffix(out, "rc=1") {
		t.Errorf("gate.sh: %s", out)
	}
	rep, _ := os.ReadFile(filepath.Join(jd, "report.json"))
	if !strings.Contains(string(rep), `"status": "error"`) || !strings.Contains(string(rep), "could not clone the bundle") {
		t.Errorf("report %s", rep)
	}
	if out := w.sh(jd, cmd...); !strings.Contains(out, "has run already") || !strings.HasSuffix(out, "rc=1") {
		t.Errorf("second run: %s", out)
	}
	w.clk.Advance(time.Minute)
	w.step()
	if rep := gateReport(t, w.collected("t0001-g1")); rep["status"] != "error" {
		t.Errorf("posted %v", rep)
	}
}

// A gate job with no checks.sh is reported as bad input, never handed to
// a worker.
func TestAGateJobWithoutChecksIsReportedNotRun(t *testing.T) {
	w := newWorld(t)
	w.repo = testkit.NewRepo(t)
	head := w.repo.Git("rev-parse", "main")
	meta := jobproto.Meta{Branch: "gate", Start: head, TimeoutMinutes: 35, Kind: jobproto.KindGate}
	tar, err := jobproto.TarFlat([]jobproto.File{{Name: "meta.json", Data: meta.Encode()}, {Name: "repo.bundle", Data: []byte("x")}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.factory.Start("desktop", "t0001-g1", tar); err != nil {
		t.Fatal(err)
	}
	w.step()
	w.step()
	if len(w.notes) != 0 {
		t.Errorf("notes %q", w.notes)
	}
	if rep := gateReport(t, w.collected("t0001-g1")); rep["status"] != "error" || !strings.Contains(rep["summary"].(string), "no checks.sh") {
		t.Errorf("report %v", rep)
	}
}

func TestGateScriptValues(t *testing.T) {
	head := strings.Repeat("ab", 20)
	s := string(gateScript("t0001-g1", jobproto.Meta{Branch: "gate", Start: head, TimeoutMinutes: 1, Kind: jobproto.KindGate}))
	for _, want := range []string{"HEAD='" + head + "'\n", "LIMIT='60'\n", "gate job t0001-g1 "} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if slices.ContainsFunc([]string{"@ID@", "@HEAD@", "@LIMIT@"}, func(p string) bool { return strings.Contains(s, p) }) {
		t.Error("a placeholder is left")
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
