package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf(m map[string]string) []string { return sortedKeys(m) }

func sh(t *testing.T, dir string, argv ...string) {
	t.Helper()
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
}

// port: TF MemoryTest.test_nothing_the_agent_plants_runs_or_leaves_the_copy
func TestNothingTheAgentPlantsRunsOrLeavesTheCopy(t *testing.T) {
	f := newFixture(t, "")
	marker := filepath.Join(f.env.Root, "pwned")
	secret := filepath.Join(f.env.Root, "secret")
	if err := os.WriteFile(secret, []byte("token"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, true)
	copy := f.copyDir(tk)
	// a nested repository whose config would run a command when git looks at it
	nested := filepath.Join(copy, "knowledge", "x")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, nested, "git", "init", "-q")
	sh(t, nested, "git", "config", "core.fsmonitor", "touch "+marker+"; false")
	f.write(tk, "knowledge/x/.gitattributes", "* filter=evil\n")
	sh(t, nested, "git", "config", "filter.evil.clean", "touch "+marker)
	f.write(tk, "knowledge/x/note.md", "ok\n")
	if err := os.MkdirAll(filepath.Join(copy, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(tk, ".GIT", "x")
	if err := os.Symlink(secret, filepath.Join(copy, "knowledge", "stolen.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.env.Root, filepath.Join(copy, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(copy, "locked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(copy, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	f.finish(tk)
	if exists(marker) {
		t.Fatal("something the agent planted ran")
	}
	files := f.headFiles("worker")
	if files["knowledge/x/note.md"] != "ok\n" {
		t.Errorf("note %q", files["knowledge/x/note.md"])
	}
	if files["knowledge/x/.gitattributes"] != "* filter=evil\n" { // only text, never applied
		t.Errorf(".gitattributes %q", files["knowledge/x/.gitattributes"])
	}
	for p, d := range files {
		low := strings.ToLower(p)
		if strings.Contains(low+"/", ".git/") || strings.HasSuffix(low, ".git") {
			t.Errorf("store holds %s", p)
		}
		if strings.Contains(d, "token") {
			t.Errorf("%s holds the secret", p)
		}
	}
	if _, ok := files["knowledge/stolen.md"]; ok {
		t.Error("a link was followed")
	}
	if r := last(tk).Rule; !strings.Contains(r, "left out") {
		t.Errorf("rule %q", r)
	}
	if exists(copy) {
		t.Error("the copy is left")
	}
}

// port: TF MemoryTest.test_two_runs_of_one_role_merge_instead_of_overwriting
func TestTwoRunsOfOneRoleMergeInsteadOfOverwriting(t *testing.T) {
	f := newFixture(t, "")
	a, b := f.ticket("T0003", "ready", "human"), f.ticket("T0004", "ready", "human")
	f.start(a, model.StageWork, true)
	f.start(b, model.StageWork, true)
	f.write(a, "tickets/T0003.md", "from A")
	f.write(b, "tickets/T0004.md", "from B")
	f.write(a, "knowledge/INDEX.md", "A's index\n")
	f.write(b, "knowledge/INDEX.md", "B's index\n")
	f.finish(a)
	f.finish(b)
	files := f.headFiles("worker")
	if files["tickets/T0003.md"] != "from A" || files["tickets/T0004.md"] != "from B" {
		t.Errorf("a run was overwritten: %q", files)
	}
	if !strings.Contains(files["knowledge/INDEX.md"], "A's index") || !strings.Contains(files["knowledge/INDEX.md"], "B's index") {
		t.Errorf("index %q", files["knowledge/INDEX.md"])
	}
	if r := last(b).Rule; r != "another run changed the same notes; both versions are kept in the file" {
		t.Errorf("rule %q", r)
	}
}

// port: TF MemoryTest.test_lasting_changes_from_public_tickets_are_flagged
func TestLastingChangesFromPublicTicketsAreFlagged(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "github")
	f.start(tk, model.StageWork, true)
	f.write(tk, "tickets/T0003.md", "mine\n")
	f.finish(tk)
	if r := last(tk).Rule; r != "" { // its own handover is fine
		t.Errorf("rule %q", r)
	}
	f.start(tk, model.StageWork, true)
	f.write(tk, "README.md", "always do X\n")
	f.finish(tk)
	if r := last(tk).Rule; r != "memory beyond this ticket's notes changed on a ticket from GitHub issues: check it" {
		t.Errorf("rule %q", r)
	}
	for _, door := range []string{"peer", "watch", "agent"} {
		tk := f.ticket("T0009", "ready", door)
		f.start(tk, model.StageWork, true)
		f.write(tk, "knowledge/"+door+".md", "x\n")
		f.finish(tk)
		info, _ := model.DoorOf(door)
		if r := last(tk).Rule; r != "memory beyond this ticket's notes changed on a ticket from "+info.Label+": check it" {
			t.Errorf("%s: rule %q", door, r)
		}
	}
	for _, door := range []string{"human", "clock"} {
		tk := f.ticket("T0010", "ready", door)
		f.start(tk, model.StageWork, true)
		f.write(tk, "knowledge/"+door+".md", "x\n")
		f.finish(tk)
		if r := last(tk).Rule; r != "" {
			t.Errorf("%s: rule %q", door, r)
		}
	}
}

// port: TF MemoryTest.test_a_memory_failure_never_holds_up_the_ticket
func TestAMemoryFailureNeverHoldsUpTheTicket(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, true)
	f.write(tk, "tickets/T0003.md", "x\n")
	// the store breaks under the run
	r, _ := f.svc.Store.Open("p", "worker")
	if err := os.RemoveAll(r.GitDir); err != nil {
		t.Fatal(err)
	}
	f.finish(tk)
	e := last(tk)
	if e.Event != "error" || !strings.HasPrefix(e.Detail, "memory: ") || len([]rune(e.Detail)) > len("memory: ")+250 {
		t.Errorf("event %+v", e)
	}
	if exists(f.copyDir(tk)) {
		t.Error("the copy is left")
	}
}

func TestSettleIsOnceAndAnUnchangedCopyRecordsNothing(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, true)
	f.finish(tk)
	if len(tk.History) != 0 {
		t.Errorf("an unchanged copy recorded %+v", tk.History)
	}
	if exists(f.copyDir(tk)) {
		t.Error("the copy is left")
	}
	f.finish(tk) // settled: nothing, no error for the missing copy
	f.svc.Lost(tk, "helper")
	if len(tk.History) != 0 {
		t.Errorf("a settled run recorded %+v", tk.History)
	}
	// no agent, or an agent with no memory: nothing
	none := f.ticket("T0004", "ready", "human")
	f.finish(none)
	f.svc.Lost(none, "helper")
	none.Agent = ticket.Some(ticket.Agent{Name: "t0004-w1", Stage: "work", Host: "local"})
	f.finish(none)
	if len(none.History) != 0 {
		t.Errorf("no memory recorded %+v", none.History)
	}
}

func TestAMissingLocalCopyIsAnErrorNotAnEmptyStore(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, true)
	if err := os.RemoveAll(f.copyDir(tk)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.env.Root, f.copyDir(tk)); err != nil {
		t.Fatal(err)
	}
	f.finish(tk)
	if e := last(tk); e.Event != "error" {
		t.Errorf("event %+v", e)
	}
	if files := f.headFiles("worker"); files["README.md"] == "" {
		t.Error("the store was emptied")
	}
	if !exists(f.env.Root) {
		t.Error("the link's target was removed")
	}
}

func TestSettleNamesWhatItLeftOutAndAStoreOverItsLimit(t *testing.T) {
	f := newFixture(t, "[memory]\nmax_kb = 1\n")
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, true)
	for i := range 8 {
		f.write(tk, "knowledge/"+string(rune('a'+i))+".md", strings.Repeat("x", 300))
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(f.copyDir(tk), "it's.md")); err != nil {
		t.Fatal(err)
	}
	f.write(tk, "a\x01b.md", "x")
	f.finish(tk)
	if len(tk.History) != 2 {
		t.Fatalf("history %+v", tk.History)
	}
	first, second := tk.History[0], tk.History[1]
	m := first.Data.(ticket.Memory)
	if want := "worker memory " + m.Commit + ": knowledge/a.md, knowledge/b.md, knowledge/c.md, knowledge/d.md, knowledge/e.md, knowledge/f.md …"; first.Detail != want {
		t.Errorf("detail %q, want %q", first.Detail, want)
	}
	if len(m.Files) != 8 {
		t.Errorf("files %v", m.Files)
	}
	wantRule := "left out 2 entries (not plain files, odd names, or over the 3 MB limit): "
	if !strings.HasPrefix(first.Rule, wantRule) || !strings.Contains(first.Rule, `"it's.md"`) || !strings.Contains(first.Rule, `'a\x01b.md'`) {
		t.Errorf("rule %q", first.Rule)
	}
	size := 8*300 + len(f.headFiles("worker")["README.md"]) + len("# Knowledge index\n\n(nothing yet)\n")
	if second.Event != "memory" || second.Detail != "worker memory is "+itoa(size/1024)+" KB, over its limit: tidy it up" {
		t.Errorf("second event %+v", second)
	}
	if o := second.Data.(ticket.Memory); o.Role != "worker" || o.OverLimitKB != size/1024 || o.Commit != "" {
		t.Errorf("over-limit payload %+v", o)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestALostRunSaysSoOnce(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, false)
	f.svc.Lost(tk, "helper")
	f.svc.Lost(tk, "helper")
	f.finish(tk)
	if len(tk.History) != 1 {
		t.Fatalf("history %+v", tk.History)
	}
	e := tk.History[0]
	if e.Event != "memory" || e.Detail != "no memory came back from helper; this run's notes are lost" {
		t.Errorf("event %+v", e)
	}
	if m := e.Data.(ticket.Memory); m.Role != "worker" || m.LostOn != "helper" || m.Commit != "" || m.Files != nil {
		t.Errorf("payload %+v", m)
	}
}

func TestATarFromAHelperIsSettledWithWhatItLeftOut(t *testing.T) {
	f := newFixture(t, "")
	tk := f.ticket("T0003", "ready", "human")
	f.start(tk, model.StageWork, false)
	f.svc.Settle(t.Context(), f.cfg, tk, &Copy{Files: memstore.Files{"tickets/T0003.md": []byte("x")}, Skipped: []string{"a", "b", "c", "d"}})
	if r := last(tk).Rule; r != "left out 4 entries (not plain files, odd names, or over the 3 MB limit): 'a', 'b', 'c'" {
		t.Errorf("rule %q", r)
	}
	// a remote copy replaces the store's files: what it lacks is deleted
	if files := f.headFiles("worker"); len(files) != 1 {
		t.Errorf("store %v", sortedKeys(files))
	}
}
