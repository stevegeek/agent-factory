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
	"github.com/stevegeek/agent-factory/internal/vitals"
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

// Away are the pull hosts that have not called in lately, or whose last
// vitals say their workers cannot take a job (the worker system is down,
// or no worker runs): they get no new jobs, as if paused, until that
// changes.
func (j JobHosts) Away(cfg *config.Config) []string {
	var away []string
	for _, h := range cfg.Hosts {
		if !h.Pull() {
			continue
		}
		if j.box(h).Away(h.Name) {
			away = append(away, h.Name)
		} else if w, ok := j.workers(h); ok && (!w.Up || w.Running == 0) {
			away = append(away, h.Name)
		}
	}
	return away
}

// Capacity is each pull host whose last vitals report fewer running
// workers than its slots, with that count: it is sent no more jobs than it
// has workers to run them.
func (j JobHosts) Capacity(cfg *config.Config) map[string]int {
	out := map[string]int{}
	for _, h := range cfg.Hosts {
		if w, ok := j.workers(h); h.Pull() && ok && w.Running < h.Slots {
			out[h.Name] = w.Running
		}
	}
	return out
}

// GivenBack is why the pull host gave job back, false when it did not.
func (j JobHosts) GivenBack(cfg *config.Config, host, job string) (string, bool) {
	h, ok := cfg.HostNamed(host)
	if !ok || !h.Pull() {
		return "", false
	}
	return j.box(*h).GivenBack(host, job)
}

// workers is the pull host's own account of its workers from vitals no
// older than its beat window; false when there is none (an older
// factory-pull, or no fresh post), so the host is judged by its calls alone.
func (j JobHosts) workers(h config.HostCfg) (vitals.Workers, bool) {
	box := j.box(h)
	v, err := box.Vitals(h.Name)
	if err != nil || v.Workers == nil || j.Clock.Now().Sub(v.Taken) > box.BeatWindow {
		return vitals.Workers{}, false
	}
	return *v.Workers, true
}

func (j JobHosts) box(h config.HostCfg) mailbox.Box {
	return mailbox.Box{Dir: j.Paths.Mailbox(), Clock: j.Clock,
		ClaimWindow: time.Duration(h.ClaimMinutes) * time.Minute, BeatWindow: time.Duration(h.BeatMinutes) * time.Minute}
}
