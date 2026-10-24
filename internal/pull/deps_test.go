package pull

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// Before the checks, gate.sh installs a checkout's missing dependencies into
// the worker's own caches (a Lever worker starts with none); when that fails,
// the job reports an error, so the gate runs again, rather than a failed gate.
func TestAGateJobInstallsMissingDependenciesFirst(t *testing.T) {
	for _, c := range []struct {
		install string // the fake bundle install's exit
		status  string
	}{{"0", "done"}, {"1", "error"}} {
		w := newWorld(t)
		w.repo = testkit.NewRepo(t)
		w.repo.Write("Gemfile", "source 'https://rubygems.org'\n")
		w.repo.Commit("gemfile")
		w.queueGate("t0001-g1", "set -e; test -f installed; echo tests ok")
		w.step()
		jd := w.jobDir("fw1", "t0001-g1")
		bin := t.TempDir()
		fake := "#!/bin/sh\ncase \"$1\" in check) exit 1 ;; install) echo installing; touch installed; exit " + c.install + " ;; esac\n"
		if err := os.WriteFile(filepath.Join(bin, "bundle"), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
		w.sh(jd, "sh", "-c", "PATH="+bin+":$PATH sh gate.sh; true")
		rep, _ := os.ReadFile(filepath.Join(jd, "report.json"))
		if !strings.Contains(string(rep), `"status": "`+c.status+`"`) {
			t.Errorf("install exit %s: report %s", c.install, rep)
		}
		if c.status == "done" && !strings.Contains(string(rep), `"gate_exit": 0`) {
			t.Errorf("the checks did not see the installed dependencies: %s", rep)
		}
		if log, _ := os.ReadFile(filepath.Join(jd, "deps.log")); !strings.Contains(string(log), "installing") {
			t.Errorf("deps.log %q", log)
		}
	}
}

// job.sh prepare installs the dependencies too, and a failure only warns:
// the agent still gets its checkout, and says what it found.
func TestJobScriptInstallsDependenciesAndOnlyWarns(t *testing.T) {
	s := string(script("t0001-w1", jobproto.Meta{Branch: "bot/x", Start: "main", TimeoutMinutes: 30}, "", ""))
	if !strings.Contains(s, "deps() {") || !strings.Contains(s, `deps || echo "job.sh: the dependencies did not install`) {
		t.Errorf("job.sh lacks its deps step:\n%s", s)
	}
}
