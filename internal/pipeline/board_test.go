package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/stage"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// port: TF TickRaceTest.test_a_send_back_made_while_the_tick_runs_is_kept
func TestASendBackMadeWhileTheTickRunsIsKept(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "Fix", "gate", func(t *ticket.Ticket) { t.Branch = ticket.Some("bot/x") })
	h.local.onList = func(n int) {
		if n == 2 { // the tick has read the board by now
			h.change(id, func(t *ticket.Ticket) {
				ticket.AddNote(t, "handle the empty case", "maintainer", nil, h.clock.Now())
				ticket.Move(t, "ready", model.ReasonByHuman, "by human", "", "maintainer", h.clock.Now())
			})
		}
	}
	h.tick()
	if len(h.gate.ran) != 0 {
		t.Fatal("the gate ran on a ticket a person had sent back")
	}
	if len(h.local.started) != 1 || h.local.started[0].Stage != model.StageWork {
		t.Fatalf("started %+v", h.local.started)
	}
	got := h.load(id)
	if len(got.Notes) != 1 || got.Notes[0].Text != "handle the empty case" {
		t.Fatalf("notes %+v", got.Notes)
	}
	if !strings.Contains(h.local.started[0].Prompt, "handle the empty case") {
		t.Fatal("the worker was not told the note")
	}
}

// the tick waits for a ticket a person is changing, and then works on
// what they saved.
func TestTheTickWaitsForATicketsLock(t *testing.T) {
	h := newHarness(t)
	id := h.file("p", "Fix", "ready", nil)
	unlock := h.lockHeld(id)
	done := make(chan error, 1)
	go func() { done <- Tick(context.Background(), h.deps()) }()
	select {
	case err := <-done:
		t.Fatalf("the tick did not wait for the lock: %v", err)
	default:
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if h.load(id).Agent.V.Name != "t0001-w1" {
		t.Fatal("the ticket was not started once the lock was free")
	}
}

const boardConfig = `max_agents = 2

[projects.p]
path = "{repo}"
base = "main"

[projects.sandbox]
mode = "board"
`

// port: TT ModeTest.test_a_tick_leaves_every_board_ticket_as_it_was
func TestATickLeavesEveryBoardTicketAsItWas(t *testing.T) {
	h := newHarness(t)
	h.config(boardConfig)
	before := map[ticket.ID]string{}
	for _, c := range model.BoardColumns() {
		tk, err := h.board.FileTicket(context.Background(), ticket.NewIn{Project: "sandbox", Title: "in " + string(c.Name), Body: "b",
			Door: "human", Column: string(c.Name), Mode: model.ModeBoard})
		if err != nil {
			t.Fatal(err)
		}
		before[tk.ID] = h.raw(tk.ID)
	}
	h.tick()
	if len(h.local.started) != 0 || len(h.local.removed) != 0 || len(h.gate.ran) != 0 {
		t.Fatal("the tick touched a board project's agents")
	}
	for id, raw := range before {
		if h.raw(id) != raw {
			t.Errorf("%s changed", id)
		}
	}
}

// port: TL BoardApartTest.test_a_tick_starts_nothing_on_a_board_project_whatever_the_file_says
func TestATickStartsNothingOnABoardProjectWhateverTheFileSays(t *testing.T) {
	h := newHarness(t)
	h.config(boardConfig)
	id := h.file("sandbox", "planted", "ready", func(t *ticket.Ticket) { t.Kind = ticket.Some("prospect") })
	h.file("sandbox", "planted gate", "gate", nil)
	before := h.raw(id)
	h.tick()
	if len(h.local.started) != 0 || len(h.gate.ran) != 0 {
		t.Fatal("a board project's ticket started")
	}
	if h.raw(id) != before {
		t.Fatal("a board project's ticket was saved")
	}
}

// port: TT ModeTest.test_no_board_column_has_a_stage
func TestNoBoardColumnHasAStage(t *testing.T) {
	kind := jsonx.Object{{Key: "kind", Value: jsonx.String("item")}}
	for _, c := range model.BoardColumns() {
		tk := ticket.New("T0001", ticket.NewIn{Project: "sandbox", Title: "x", Column: string(c.Name), Mode: model.ModeBoard, Fields: kind}, h0())
		if s, ok := stage.ForTicket(tk); ok {
			t.Errorf("%s has stage %s", c.Name, s)
		}
	}
	// belt and braces: even a factory column name on a board ticket starts nothing
	tk := ticket.New("T0001", ticket.NewIn{Project: "sandbox", Title: "x", Column: "ready", Mode: model.ModeBoard,
		Fields: jsonx.Object{{Key: "kind", Value: jsonx.Null()}}}, h0())
	if s, ok := stage.ForTicket(tk); ok {
		t.Errorf("ready on a board ticket has stage %s", s)
	}
}

// port: TF DoorsTest.test_a_paused_project_gets_no_tickets_and_no_agents
func TestAPausedProjectGetsNoAgents(t *testing.T) {
	h := newHarness(t)
	h.config(strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\nenabled = false\n", 1))
	id := h.file("p", "Fix it", "ready", nil)
	h.tick()
	if len(h.local.started) != 0 {
		t.Fatal("a paused project's ticket started")
	}
	h.config(defaultConfig) // switched back on: it starts
	h.tick()
	if len(h.local.started) != 1 || h.local.started[0].Ticket.ID != id {
		t.Fatalf("started %+v", h.local.started)
	}
}

// A paused project's running agents still finish.
func TestAPausedProjectsAgentsStillFinish(t *testing.T) {
	h := newHarness(t)
	h.config(strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\nenabled = false\n", 1))
	id := localAgent(h, "triage", "triage", nil)
	h.local.running["t0001-t1"] = true
	h.local.reports["t0001-t1"] = []byte(`{"decision": "reject", "reason": "dup"}`)
	h.tick()
	if got := h.load(id); got.Column != "rejected" {
		t.Fatalf("column %s", got.Column)
	}
}

// The maintainer's switch in foreman-state/projects.json overrides config's
// `enabled` both ways, and a broken file leaves config in charge.
func TestAProjectSwitchedFromForemanOverridesConfig(t *testing.T) {
	h := newHarness(t)
	sw := h.paths.ProjectSwitches()
	if err := os.MkdirAll(filepath.Dir(sw), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(sw+".new", []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(sw+".new", sw); err != nil {
			t.Fatal(err)
		}
	}
	id := h.file("p", "Fix it", "ready", nil)
	write(`{"p": {"enabled": false, "by": "alice", "at": "2026-01-01T00:00:00+00:00"}}`)
	h.tick()
	if len(h.local.started) != 0 {
		t.Fatal("a project paused from Foreman started an agent")
	}
	// paused in config, switched on here: it starts
	h.config(strings.Replace(defaultConfig, "lint = \"true\"\n", "lint = \"true\"\nenabled = false\n", 1))
	write(`{"p": {"enabled": true, "by": "alice", "at": "2026-01-01T00:00:00+00:00"}}`)
	h.tick()
	if len(h.local.started) != 1 || h.local.started[0].Ticket.ID != id {
		t.Fatalf("started %+v", h.local.started)
	}
	// a broken file: config's enabled = false holds, the tick goes on
	h.file("p", "Another", "ready", nil)
	write(`{"p": {"enabled": tru`)
	h.tick()
	if len(h.local.started) != 1 {
		t.Fatalf("started %+v under a broken switch file", h.local.started)
	}
	if !h.log.has("following config.toml") {
		t.Error("the broken file was not logged")
	}
	write(`{"p": {"enabled": true}}`) // mended: the second starts too
	h.tick()
	if len(h.local.started) != 2 {
		t.Fatalf("started %+v once the file was mended", h.local.started)
	}
}

// the manager's workspace is made on first use: a repository
// with a README commit, as the factory, with gpg signing off.
func TestTheManagerWorkspaceIsMadeOnFirstUse(t *testing.T) {
	h := newHarness(t)
	h.git(t, "config", "--global", "commit.gpgsign", "true") // a signing default must not stop it
	id := h.file("factory", "A morning check", "ready", func(t *ticket.Ticket) {
		t.Kind = ticket.Some("manage")
		t.Host = ticket.Some("local")
	})
	h.tick()
	if len(h.local.started) != 1 {
		t.Fatalf("started %+v", h.local.started)
	}
	job := h.local.started[0]
	if job.Project == nil || job.Project.Path != h.paths.Manager || job.Project.Base != "main" || job.Stage != model.StageManage {
		t.Fatalf("job %+v", job)
	}
	readme, err := os.ReadFile(filepath.Join(h.paths.Manager, "README.md"))
	if err != nil || !strings.Contains(string(readme), "manager's scratch workspace") {
		t.Fatalf("README %q %v", readme, err)
	}
	if who := h.gitIn(t, h.paths.Manager, "log", "-1", "--format=%an <%ae> %s"); who != "factory <factory@localhost> workspace" {
		t.Fatalf("commit %q", who)
	}
	if got := h.load(id); got.Agent.V.Name != "t0001-m1" || got.Column != "working" {
		t.Fatalf("agent %+v column %s", got.Agent.V, got.Column)
	}
}

// The manager reads the deploy state, the live context and System status
// through read-only mounts: their sources exist before it starts.
func TestManagerWorkspaceMakesDeploysDir(t *testing.T) {
	h := newHarness(t)
	h.file("factory", "What deployed?", "ready", func(t *ticket.Ticket) {
		t.Kind = ticket.Some("manage")
		t.Host = ticket.Some("local")
	})
	h.tick()
	if len(h.local.started) != 1 {
		t.Fatalf("started %+v", h.local.started)
	}
	for _, dir := range []string{h.paths.Deploys(), h.paths.Context} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.paths.Root, "sysinfo.json")); err != nil {
		t.Errorf("sysinfo.json: %v", err)
	}
}
