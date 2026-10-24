package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

type fakeScreens struct {
	calls []string
	out   string
	err   error
}

func (f *fakeScreens) Look(_ context.Context, dir, name string, lines int) (string, error) {
	f.calls = append(f.calls, strings.Join([]string{dir, "look", name, "--plain", "-n", itoa(lines)}, " "))
	return f.out, f.err
}

type fakeJobs struct {
	calls []string
	out   string
	err   error
}

func (f *fakeJobs) Tail(_ context.Context, h config.HostCfg, job string) (string, error) {
	f.calls = append(f.calls, h.SSH+" tail "+job)
	return f.out, f.err
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

const lookConfig = `[projects.gem]
path = "/srv/gem"
base = "main"

[hosts.helper]
ssh = "factory@helper.example"
`

// port: TF LookTest.test_screen_drops_scion_noise_and_keeps_the_end
func TestScreenDropsScionNoiseAndKeepsTheEnd(t *testing.T) {
	text := "2026/10/16 20:12:08 WARN settings file contains unrecognized keys\none\ntwo\nthree\n\n"
	if got := ops.ScreenText(text, 2); got != "two\nthree" {
		t.Errorf("%q", got)
	}
	if got := ops.ScreenText("a  \r\nb c\x0b\n\n \n", 10); got != "a\nb\nc" {
		t.Errorf("Python's line breaks and trailing blanks: %q", got)
	}
	if got := ops.ScreenText("", 10); got != "" {
		t.Errorf("empty: %q", got)
	}
}

// port: TF LookTest.test_look_reads_the_local_screen_or_the_helper_hosts_tail
func TestLookReadsTheLocalScreenOrTheHelperHostsTail(t *testing.T) {
	w := newWorld(t, lookConfig)
	ctx := context.Background()
	sc, jobs := &fakeScreens{out: "working…\n"}, &fakeJobs{out: "pi output"}
	w.svc.Scion, w.svc.Jobs = sc, jobs
	w.new(ticket.NewIn{Project: "gem", Title: "x", Body: "b", Door: "human", Column: "ready"}, nil)
	var noAgent *ops.ErrNoAgent
	if _, err := w.svc.Look(ctx, "T0001", 120); !errors.As(err, &noAgent) || err.Error() != "T0001 has no agent running" {
		t.Errorf("no agent: %v", err)
	}
	agent := ticket.Agent{Name: "t0001-w1", Stage: "work", Host: "local", Started: "2026-10-18T09:00:00+00:00"}
	setAgent := func(a ticket.Agent) {
		t.Helper()
		if err := w.board.Changing(ctx, "T0001", 0, func(t *ticket.Ticket) error { t.Agent = ticket.Some(a); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setAgent(agent)
	s, err := w.svc.Look(ctx, "t0001", 120)
	if err != nil || s.Text != "working…" {
		t.Fatalf("local: %+v %v", s, err)
	}
	if sc.calls[0] != "/srv/gem look t0001-w1 --plain -n 120" {
		t.Errorf("scion argv %q", sc.calls[0])
	}
	if s.String() != "t0001-w1 · work · started 2026-10-18T09:00:00+00:00\nworking…" {
		t.Errorf("header %q", s.String())
	}
	// lines are clamped 10..400
	w.svc.Look(ctx, "T0001", 3)
	w.svc.Look(ctx, "T0001", 9000)
	if !strings.HasSuffix(sc.calls[1], "-n 10") || !strings.HasSuffix(sc.calls[2], "-n 400") {
		t.Errorf("clamp: %q", sc.calls)
	}
	// Scion has nothing to show: the end of the agent's log here
	sc.out, sc.err = "", errors.New("exit 1")
	home := filepath.Join(w.paths.Home, ".scion", "project-configs", "p", ".scion", "agents", "t0001-w1", "home")
	os.MkdirAll(home, 0o755)
	os.WriteFile(filepath.Join(home, "agent.log"), []byte(strings.Repeat("x\n", 40000)+"last line\xe2\x82\n"), 0o644)
	if s, _ := w.svc.Look(ctx, "T0001", 10); !strings.HasSuffix(s.Text, "x\nlast line�") {
		t.Errorf("log tail: %q", s.Text[len(s.Text)-30:])
	}
	// a helper host: the job's tail through jobtail, and what went wrong if it cannot be read
	agent.Host = "helper"
	setAgent(agent)
	if s, _ := w.svc.Look(ctx, "T0001", 120); s.Text != "pi output" || jobs.calls[0] != "factory@helper.example tail t0001-w1" {
		t.Errorf("helper: %q %v", s.Text, jobs.calls)
	}
	if s, _ := w.svc.Look(ctx, "T0001", 120); !strings.HasPrefix(s.String(), "t0001-w1 · work @helper · started") {
		t.Errorf("helper header %q", s.String())
	}
	jobs.err = errors.New("down")
	if s, _ := w.svc.Look(ctx, "T0001", 120); s.Text != "(could not read the job on helper: down)" {
		t.Errorf("unreachable helper: %q", s.Text)
	}
	// a host config no longer names: nothing to read
	agent.Host = "gone"
	setAgent(agent)
	if s, err := w.svc.Look(ctx, "T0001", 120); err != nil || s.Text != "" {
		t.Errorf("unknown host: %q %v", s.Text, err)
	}
	if _, err := w.svc.Look(ctx, "../T0001", 120); err == nil || err.Error() != "no ticket ../T0001" {
		t.Errorf("a name that is not an id: %v", err)
	}
}

// fakeRelay is the relay's screens directory (screens.Reader).
type fakeRelay struct {
	text  map[string]string
	at    map[string]time.Time
	calls []string
}

func (f *fakeRelay) Read(name string) (string, time.Time, error) {
	f.calls = append(f.calls, name)
	text, ok := f.text[name]
	if !ok {
		return "", time.Time{}, os.ErrNotExist
	}
	return text, f.at[name], nil
}

// Foreman's look: a local agent's screen is the one the relay wrote, with
// its age; one missing or stale shows the agent's log, noted as starting.
// Scion never runs. A helper host's job is read as before.
func TestLookReadsTheRelayedScreen(t *testing.T) {
	w := newWorld(t, lookConfig)
	ctx := context.Background()
	sc, jobs := &fakeScreens{out: "scion"}, &fakeJobs{out: "pi output"}
	relay := &fakeRelay{text: map[string]string{"t0001-w1": "2026/10/16 20:12:08 WARN noise\n$ make test\nok\n\n"},
		at: map[string]time.Time{"t0001-w1": now.Add(-3 * time.Second)}}
	w.svc.Scion, w.svc.Jobs, w.svc.Relay = sc, jobs, relay
	agent := ticket.Agent{Name: "t0001-w1", Stage: "work", Host: "local", Started: "2026-10-18T09:00:00+00:00"}
	w.new(ticket.NewIn{Project: "gem", Title: "x", Body: "b", Door: "human", Column: "working"}, func(t *ticket.Ticket) {
		t.Agent = ticket.Some(agent)
	})
	s, err := w.svc.Look(ctx, "T0001", 120)
	if err != nil || s.String() != "t0001-w1 · work · started 2026-10-18T09:00:00+00:00 · screen 3 s old\n$ make test\nok" {
		t.Errorf("relayed: %q %v", s.String(), err)
	}
	if s, _ := w.svc.Look(ctx, "T0001", 10); s.Text != "$ make test\nok" {
		t.Errorf("lines: %q", s.Text)
	}
	// stale: the agent's log, until the relay catches up
	home := filepath.Join(w.paths.Home, ".scion", "project-configs", "p", ".scion", "agents", "t0001-w1", "home")
	os.MkdirAll(home, 0o755)
	os.WriteFile(filepath.Join(home, "agent.log"), []byte("from the log\n"), 0o644)
	relay.at["t0001-w1"] = now.Add(-ops.RelayStale)
	if s, _ := w.svc.Look(ctx, "T0001", 120); s.String() != "t0001-w1 · work · started 2026-10-18T09:00:00+00:00 · screen starting…\nfrom the log" {
		t.Errorf("stale: %q", s.String())
	}
	// missing: the same
	delete(relay.text, "t0001-w1")
	if s, _ := w.svc.Look(ctx, "T0001", 120); s.Note != "screen starting…" || s.Text != "from the log" {
		t.Errorf("missing: %+v", s)
	}
	if len(sc.calls) != 0 {
		t.Errorf("Foreman ran scion: %v", sc.calls)
	}
	// a helper host: its job's tail, no relay, no note
	agent.Host = "helper"
	w.board.Changing(ctx, "T0001", 0, func(t *ticket.Ticket) error { t.Agent = ticket.Some(agent); return nil })
	relay.calls = nil
	if s, _ := w.svc.Look(ctx, "T0001", 120); s.Text != "pi output" || s.Note != "" || len(relay.calls) != 0 {
		t.Errorf("helper: %+v relay %v", s, relay.calls)
	}
}

// LocalAgents is what the relay looks at: the running agents here, each
// with its workspace; another host's, or one with no workspace here, not.
func TestLocalAgents(t *testing.T) {
	w := newWorld(t, lookConfig)
	running := func(name, host string) func(t *ticket.Ticket) {
		return func(t *ticket.Ticket) {
			t.Agent = ticket.Some(ticket.Agent{Name: name, Stage: "work", Host: host, Started: "x"})
		}
	}
	w.new(ticket.NewIn{Project: "gem", Title: "a", Body: "b", Door: "human", Column: "working"}, running("t0001-w1", "local"))
	w.new(ticket.NewIn{Project: "gem", Title: "b", Body: "b", Door: "human", Column: "working"}, running("t0002-w1", "helper"))
	w.new(ticket.NewIn{Project: "gem", Title: "c", Body: "b", Door: "human", Column: "ready"}, nil)
	w.new(ticket.NewIn{Project: "factory", Title: "d", Body: "b", Door: "human", Column: "working"}, running("t0004-w1", ""))
	w.new(ticket.NewIn{Project: "factory", Title: "m", Body: "b", Door: "human", Column: "working"}, func(t *ticket.Ticket) {
		t.Kind = ticket.Some("manage")
		running("t0005-m1", "local")(t)
	})
	ts, _ := w.board.Reader.List()
	got := w.svc.LocalAgents(ts)
	want := []ops.LocalAgent{{Name: "t0001-w1", Dir: "/srv/gem"}, {Name: "t0005-m1", Dir: w.paths.Manager}}
	if !slices.Equal(got, want) {
		t.Errorf("%+v, want %+v", got, want)
	}
}

// The manager's agents run in its own workspace; look never creates it.
func TestLookAtTheManager(t *testing.T) {
	w := newWorld(t, lookConfig)
	sc := &fakeScreens{out: "managing\n"}
	w.svc.Scion = sc
	w.new(ticket.NewIn{Project: "factory", Title: "m", Body: "b", Door: "human", Column: "working"}, func(t *ticket.Ticket) {
		t.Kind = ticket.Some("manage")
		t.Agent = ticket.Some(ticket.Agent{Name: "t0001-m1", Stage: "manage", Host: "local", Started: "x"})
	})
	if s, err := w.svc.Look(context.Background(), "T0001", 120); err != nil || s.Text != "managing" {
		t.Fatalf("%+v %v", s, err)
	}
	if !strings.HasPrefix(sc.calls[0], w.paths.Manager+" look") {
		t.Errorf("workspace %q", sc.calls[0])
	}
	if _, err := os.Stat(w.paths.Manager); err == nil {
		t.Errorf("look created the manager's workspace")
	}
}

type regexCase struct {
	In  string          `json:"in"`
	Out json.RawMessage `json:"out"` // as the pattern's op returns it; null: no match
}

// regexCases reads one pattern's cases from the regex golden that
// the Python goldens tool wrote from Python.
func regexCases(t *testing.T, name string) []regexCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "textx", "testdata", "py", "regex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Patterns []struct {
			Name  string      `json:"name"`
			Cases []regexCase `json:"cases"`
		} `json:"patterns"`
	}
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	for _, p := range g.Patterns {
		if p.Name == name {
			return p.Cases
		}
	}
	t.Fatalf("no golden for %s", name)
	return nil
}
