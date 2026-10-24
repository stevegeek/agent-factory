package pipeline

import (
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobtail"
	"github.com/stevegeek/agent-factory/internal/mailbox"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/sshjobs"
)

// JobHosts are the helper hosts of the config, each reached through its
// transport: factory-remote over ssh (hostrun.Jobs over sshjobs), or, for
// a pull host, its mailbox here (hostrun.Jobs over mailbox.Box).
type JobHosts struct {
	Paths paths.Paths
	SSH   sshjobs.Client
	Read  jobtail.Reader
	Clock clock.Clock
}

// Helper is the [hosts.<name>] helper, if the config has it.
func (j JobHosts) Helper(cfg *config.Config, name string) (Helper, bool) {
	h, ok := cfg.HostNamed(name)
	if !ok {
		return nil, false
	}
	jobs := &hostrun.Jobs{Remote: hostrun.HostOf(j.Paths, *h), Paths: j.Paths, Via: j.SSH, Read: j.Read, Clock: j.Clock}
	if h.Pull() {
		box := j.box(*h)
		jobs.Via, jobs.Read = box, box
	}
	return jobs, true
}

// Away are the pull hosts that have not called in lately: they get no new
// jobs, as if paused, until they do.
func (j JobHosts) Away(cfg *config.Config) []string {
	var away []string
	for _, h := range cfg.Hosts {
		if h.Pull() && j.box(h).Away(h.Name) {
			away = append(away, h.Name)
		}
	}
	return away
}

func (j JobHosts) box(h config.HostCfg) mailbox.Box {
	return mailbox.Box{Dir: j.Paths.Mailbox(), Clock: j.Clock,
		ClaimWindow: time.Duration(h.ClaimMinutes) * time.Minute, BeatWindow: time.Duration(h.BeatMinutes) * time.Minute}
}
