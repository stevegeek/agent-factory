package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// fixture is MemoryTest's setUp: a factory root whose memory/ and run/
// are its own, a config with project p, and the memory service on them.
type fixture struct {
	t   *testing.T
	env *testkit.Env
	p   paths.Paths
	cfg *config.Config
	svc *Service
}

const testConfig = `
[projects.p]
path = "/nonexistent/p"
github = "example/p"
base = "main"
`

func newFixture(t *testing.T, extra string) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	if err := os.MkdirAll(p.Run, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(testConfig + extra))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, env: env, p: p, cfg: cfg,
		svc: &Service{Store: &memstore.Store{Paths: p, Env: env.Environ()}, Paths: p, Clock: testkit.Fixed{T: testkit.Stamp()}}}
}

// ticket is RemoteStageTest.ticket: a change on project p in a column.
func (f *fixture) ticket(id, column, door string) *ticket.Ticket {
	f.t.Helper()
	tk, problems, err := ticket.Decode([]byte(`{"id": "` + id + `", "project": "p", "title": "Parser drops the last line",
"body": "b", "door": "` + door + `", "by": "factory", "column": "` + column + `", "branch": "bot/parser",
"agent": null, "reports": {}, "created": "2026-01-02T03:04:05+00:00", "history": []}`))
	if err != nil || len(problems) > 0 {
		f.t.Fatalf("ticket: %v %v", err, problems)
	}
	return tk
}

// start is start_stage's memory part: Prepare, then the agent record that
// carries the memory it started from.
func (f *fixture) start(tk *ticket.Ticket, stage model.Stage, local bool) *ticket.MemoryRef {
	f.t.Helper()
	info, _ := model.StageInfoOf(stage)
	name := string(lowerID(tk.ID)) + "-" + string(info.Suffix) + "1"
	ref, err := f.svc.Prepare(f.t.Context(), f.cfg, tk, stage, name, local)
	if err != nil {
		f.t.Fatal(err)
	}
	host := "local"
	if !local {
		host = "helper"
	}
	a := ticket.Agent{Name: name, Stage: string(stage), Started: "2026-01-02T03:04:05+00:00", Host: host}
	if ref != nil {
		a.Memory = ticket.Some(*ref)
	}
	tk.Agent = ticket.Some(a)
	return ref
}

func lowerID(id ticket.ID) string {
	b := []byte(id)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// finish is finish_local: the run's copy settled.
func (f *fixture) finish(tk *ticket.Ticket) {
	f.t.Helper()
	f.svc.Settle(f.t.Context(), f.cfg, tk, nil)
}

func (f *fixture) copyDir(tk *ticket.Ticket) string {
	f.t.Helper()
	a, _ := tk.Agent.Get()
	m, ok := a.Memory.Get()
	if !ok {
		f.t.Fatal("the run has no memory")
	}
	return m.Dir
}

func (f *fixture) write(tk *ticket.Ticket, rel, data string) {
	f.t.Helper()
	path := filepath.Join(f.copyDir(tk), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) headFiles(role string) map[string]string {
	f.t.Helper()
	r, err := f.svc.Store.Open("p", role)
	if err != nil {
		f.t.Fatal(err)
	}
	head, err := r.Head(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	files, err := r.Snapshot(f.t.Context(), head)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for p, d := range files {
		out[p] = string(d)
	}
	return out
}

func last(tk *ticket.Ticket) ticket.Entry {
	if len(tk.History) == 0 {
		return ticket.Entry{}
	}
	return tk.History[len(tk.History)-1]
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
