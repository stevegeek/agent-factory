package cli

import (
	"context"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/scionread"
	"github.com/stevegeek/agent-factory/internal/screens"
)

// The screen relay (package screens): `factory screens`, run by
// factory-screens.service, which factory-screens.path starts when Foreman
// touches foreman-state/watching. The command table (commands.go) names
// its runner:
//
//	screens: runScreens

// runScreens is `factory screens`: while Foreman's Live tab is open (the
// marker's mtime), it writes each running local agent's screen to
// screens/ through scion look, as `factory look` reads it, then exits.
// The marker's mtime is the real time Foreman touched it, so the relay
// reads the real clock, whatever FACTORY_TEST_NOW says.
func runScreens(ctx context.Context, e *Env, _ Args) error {
	p := e.Paths()
	log := e.Log()
	board := e.Board()
	verbs := &ops.Service{Board: board, Config: e.ConfigSource(), Clock: e.Clock}
	snap := boardread.NewSnapshot(board.Reader)
	r := &screens.Relay{
		Dir:    p.Screens(),
		Marker: p.Watching(),
		Agents: func(context.Context) ([]screens.Agent, error) {
			v, err := snap.Load()
			if err != nil {
				return nil, err
			}
			var out []screens.Agent
			for _, a := range verbs.LocalAgents(v.Tickets) {
				out = append(out, screens.Agent(a))
			}
			return out, nil
		},
		Scion: scionread.Reader{Bin: p.Scion, Exec: e.Exec, Env: gitread.SafeEnv(environList(e.Environ))},
		Clock: clock.System{},
		Log:   log,
	}
	return r.Run(ctx)
}
