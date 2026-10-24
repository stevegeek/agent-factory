package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// port: TF MemoryTest.test_the_monthly_report_names_sizes_and_notes_for_closed_tickets
func TestTheMonthlyReportNamesSizesAndNotesForClosedTickets(t *testing.T) {
	f := newFixture(t, "")
	if _, ok := f.svc.Report(t.Context(), f.cfg, nil); ok {
		t.Error("a report with no stores")
	}
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, true)
	f.write(tk, "tickets/T0003.md", "done\n")
	f.finish(tk)
	report, ok := f.svc.Report(t.Context(), f.cfg, map[string]bool{"T0003": true})
	if !ok {
		t.Fatal("no report")
	}
	if !strings.Contains(report, "**p/worker**") || !strings.Contains(report, "notes for closed tickets: T0003") {
		t.Errorf("report %q", report)
	}
	files := f.headFiles("worker")
	size := 0
	for _, d := range files {
		size += len(d)
	}
	want := "The agents' memory stores (`factory memory <project> <role>` shows each one's history):\n\n" +
		"- **p/worker**: 4 files, " + itoa(size/1024) + " KB; notes for closed tickets: T0003\n\n" +
		"1 ticket notes belong to closed tickets; they can go when nothing in them is still useful " +
		"(a `factory memory --revert` undoes any change). Nothing has been changed."
	if report != want {
		t.Errorf("report\n%q\nwant\n%q", report, want)
	}
	// an over-limit store, a hidden folder and a broken store
	small := newFixture(t, "[memory]\nmax_kb = 0\n")
	if _, err := small.svc.Store.Ensure(t.Context(), "p", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := small.svc.Store.Ensure(t.Context(), "p", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(small.p.Memory, "p", "broken.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(small.p.Memory, ".hidden", "worker.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, ok = small.svc.Report(t.Context(), small.cfg, nil)
	if !ok {
		t.Fatal("no report")
	}
	if want := "\n\n- **p/reviewer**: 3 files, 0 KB **over its limit**\n- **p/worker**: 3 files, 0 KB **over its limit**\n\nNothing has been changed."; !strings.HasSuffix(report, want) {
		t.Errorf("report %q", report)
	}
}

// port: TF MemoryTest.test_undo_a_memory_change
func TestUndoAMemoryChange(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, true)
	f.write(tk, "knowledge/bad.md", "wrong\n")
	f.finish(tk)
	bad := strings.TrimRight(strings.Fields(last(tk).Detail)[2], ":")
	out, err := f.svc.Revert(t.Context(), "p", "worker", bad)
	if err != nil {
		t.Fatal(err)
	}
	if out != "undid "+bad+" in p/worker" {
		t.Errorf("printed %q", out)
	}
	if _, ok := f.headFiles("worker")["knowledge/bad.md"]; ok {
		t.Error("the change is still there")
	}
	for _, c := range []struct{ project, role, want string }{
		{"", "worker", "--revert needs the project and the role"},
		{"p", "", "--revert needs the project and the role"},
		{"p", "manager", "no memory store for p/manager"},
		{"../p", "worker", "no memory for '../p'/'worker': names must be plain"},
	} {
		if _, err := f.svc.Revert(t.Context(), c.project, c.role, bad); err == nil || err.Error() != c.want {
			t.Errorf("Revert(%q, %q): %v, want %s", c.project, c.role, err, c.want)
		}
	}
	r, _ := f.svc.Store.Open("p", "worker")
	root := strings.TrimSpace(gitLog(t, r, "--format=%H", "--reverse"))
	root, _, _ = strings.Cut(root, "\n")
	if _, err := f.svc.Revert(t.Context(), "p", "worker", root); !errors.Is(err, memstore.ErrRootCommit) {
		t.Errorf("root: %v", err)
	}
}

func gitLog(t *testing.T, r *memstore.Repo, args ...string) string {
	t.Helper()
	c := append([]string{"git", "--git-dir=" + r.GitDir, "log"}, args...)
	c = append(c, "refs/heads/main")
	dir := t.TempDir()
	out, err := runIn(dir, c...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runIn(dir string, argv ...string) (string, error) {
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = dir
	c.Env = append(testkit.WithoutGitIdentity(os.Environ()), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := c.Output()
	return string(out), err
}

func TestClosedIsDoneAndRejected(t *testing.T) {
	f := newFixture(t, "")
	var ts []*ticket.Ticket
	for i, col := range []string{"done", "rejected", "dropped", "ready"} {
		ts = append(ts, f.ticket("T000"+itoa(i+1), col, "human"))
	}
	got := Closed(ts)
	if len(got) != 2 || !got["T0001"] || !got["T0002"] {
		t.Errorf("closed %v", got)
	}
}

func TestListingPrintsEachStoresHistory(t *testing.T) {
	f := newFixture(t, "")
	for _, pr := range [][2]string{{"p", "worker"}, {"p", "reviewer"}, {"q", "worker"}} {
		if _, err := f.svc.Store.Ensure(t.Context(), pr[0], pr[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(f.p.Memory, ".hidden", "worker.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := f.svc.Listing(t.Context(), &out, "", ""); err != nil {
		t.Fatal(err)
	}
	heads := []string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "== ") {
			heads = append(heads, line)
		}
	}
	if strings.Join(heads, "|") != "== p/reviewer|== p/worker|== q/worker" {
		t.Errorf("stores %q", heads)
	}
	if !strings.Contains(out.String(), " memory for worker on p\n") || !strings.Contains(out.String(), "README.md") {
		t.Errorf("listing %q", out.String())
	}
	for _, c := range []struct{ project, role, want string }{
		{"p", "", "== p/reviewer|== p/worker"},
		{"", "worker", "== p/worker|== q/worker"},
		{"q", "worker", "== q/worker"},
		{"[pq]", "w*", "== p/worker|== q/worker"},
		{"x", "", ""},
		{"..", "", ""},
		{"p/../q", "", ""},
	} {
		out.Reset()
		if err := f.svc.Listing(t.Context(), &out, c.project, c.role); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(line, "== ") {
				got = append(got, line)
			}
		}
		if strings.Join(got, "|") != c.want {
			t.Errorf("Listing(%q, %q): %q, want %q", c.project, c.role, got, c.want)
		}
	}
}
