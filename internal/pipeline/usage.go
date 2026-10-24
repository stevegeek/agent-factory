package pipeline

import (
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/usage"
)

// maxUsageFile caps the read of a run's archived usage.json.
const maxUsageFile = 1 << 20

// recordUsage appends the run of agent a on t to usage.jsonl, from the
// usage.json that removing a local agent or collecting a job archived in
// logs/<id>/<run>/. A run with none (Pi, a crash before the first message)
// records nothing. What it spent is only a record: a failure here is a log
// line, never the run's.
func (d Deps) recordUsage(t *ticket.Ticket, a ticket.Agent) {
	path := filepath.Join(d.Paths.TicketLogs(string(t.ID)), a.Name, jobproto.MemberUsage)
	b, err := fsx.ReadNoFollow(path, maxUsageFile)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var u usage.Usage
	if err == nil {
		u, err = usage.Parse(b)
	}
	if err == nil && u.Empty() {
		return
	}
	if err == nil {
		err = usage.Append(d.Paths.UsageLog(), usage.Record{At: clock.Stamp(d.now()), Ticket: string(t.ID), Project: t.Project,
			Stage: a.Stage, Run: a.Name, Host: hostOf(a), Models: u.Models, Partial: u.Partial})
	}
	if err != nil && d.Log != nil {
		d.Log.Printf("%s: usage of %s not recorded: %v", t.ID, a.Name, err)
	}
}
