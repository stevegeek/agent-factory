package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/mailbox"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/sshjobs"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// recordSysinfo is the tick's last step: the host's vital signs and each
// worker host's, written to ~/factory/sysinfo.json for Foreman's Settings
// page (Foreman's sandbox cannot see /proc or reach a worker). It is
// bounded by sysinfo.VitalsTimeout plus sysinfo.RecordTimeout, and a
// failure is logged, never the tick's.
func recordSysinfo(ctx context.Context, e *Env, cfg *config.Config) {
	p := e.Paths()
	board := e.Board()
	pm := podman.Client{Exec: e.Exec, Dir: p.Home, Env: podman.PassEnv(doorsEnviron(e.Environ))}
	d := sysinfo.Deps{
		Paths:  p,
		Clock:  e.Clock,
		UID:    os.Getuid(),
		Config: cfg,
		Tickets: func() []*ticket.Ticket {
			ts, _ := board.Reader.List()
			return ts
		},
		Pauses: func() []sysinfo.Pause {
			var out []sysinfo.Pause
			for _, x := range board.LoadPauses() {
				out = append(out, sysinfo.Pause{Host: x.Host, Until: x.Until})
			}
			return out
		},
		Containers: func(ctx context.Context) ([]string, error) { return pm.Running(ctx, "") },
		HostVitals: hostVitals(e),
	}
	d.Switches, d.Hold = sysinfoState(p)
	if err := sysinfo.Record(ctx, d); err != nil {
		e.Log().Printf("%v", err)
	}
}

// hostVitals is how the tick gets a worker host's vitals: a helper is
// asked over ssh (the vitals verb, within sysinfo.VitalsTimeout) and its
// answer stamped with this clock; a pull host's last post is read from
// its mailbox, already stamped when it arrived.
func hostVitals(e *Env) func(ctx context.Context, h config.HostCfg) (*vitals.Vitals, error) {
	p := e.Paths()
	ssh := sshjobs.Client{Exec: e.Exec}
	box := mailbox.Box{Dir: p.Mailbox(), Clock: e.Clock}
	return func(ctx context.Context, h config.HostCfg) (*vitals.Vitals, error) {
		if h.Pull() {
			v, err := box.Vitals(h.Name)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errors.New("no report yet")
			}
			if err != nil {
				return nil, err
			}
			return &v, nil
		}
		out, err := ssh.Do(ctx, hostrun.HostOf(p, h), jobproto.VerbVitals, "", nil, sysinfo.VitalsTimeout)
		if err != nil {
			return nil, err
		}
		v, err := vitals.Parse(out)
		if err != nil {
			return nil, err
		}
		v = v.Received(e.Clock.Now())
		return &v, nil
	}
}

// sysinfoState reads what System status shows of the maintainer's own
// switches: the project switches (projects.json) and the deploy hold.
func sysinfoState(p paths.Paths) (func() (config.Switches, error), func() (*deployrec.Hold, error)) {
	switches := func() (config.Switches, error) { return config.ReadSwitches(p.ProjectSwitches()) }
	hold := func() (*deployrec.Hold, error) {
		h, held, err := deployrec.ReadHold(p.DeployHold())
		if err != nil || !held {
			return nil, err
		}
		return &h, nil
	}
	return switches, hold
}
