package doors

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The clock's built-in upkeep: its names in state.json and its cadence.
const (
	MemoryReportName  = "memory-report"
	MemoryReportEvery = 30 * 24 * time.Hour
	VersionsName      = "versions"
	VersionsEvery     = 7 * 24 * time.Hour
	// MemoryNoticeTitle is the monthly memory report's notice.
	MemoryNoticeTitle = "Memory stores: sizes and stale notes"
)

// Clock is clock(cfg), the clock door, run daily: each schedule
// files its ticket on each of its projects where it is due (every_days
// since it last filed there, from state.json), then the memory report
// (every 30 days) and the version check (every 7 days) run. state.json is
// written at the end, as Python's save_state; an error before that (a
// state file that does not parse, a ticket that cannot be written) leaves
// it as it was. The state lock is held from the read to the write, so the
// latest-toolchain trial's clock (written under it too) and the schedule
// runs never drop each other.
//
// A schedule's projects are its `projects`, or every factory project; a
// project with `clock = false` or paused is left out either way, and
// nothing is recorded for it, so one switched on gets its schedules on the
// next daily clock. A board project is logged and skipped, and a key
// config does not name (the factory's own project, or a mistake: quirk,
// the tick then fails it) is filed. A prospect or task goes to ready with
// its kind; anything else is a change in the schedule's column (triage by
// default). {github} in the body is the project's repository.
//
// Python kept one run per schedule (clock.<name>) and recorded it even
// when every project it named was paused; Go keeps the run per schedule
// and project (schedules.<name>.<project>), and reads the old one for a
// project that has none yet, so nothing fires twice after the change.
func (s Service) Clock(ctx context.Context, cfg *config.Config) error {
	// the state lock from the read to the write: the latest-toolchain
	// trial's clock is written under it too, and neither drops the other's
	l, err := s.Board.StateLock(ctx, fsx.Forever)
	if err != nil {
		return fmt.Errorf("clock: %w", err)
	}
	defer l.Unlock()
	st, err := s.Board.LoadState()
	if err != nil {
		return fmt.Errorf("clock: %w", err)
	}
	now := s.Board.Clock.Now()
	since := func(prev, where string, every time.Duration) (bool, error) {
		at, err := clock.Parse(prev)
		if err != nil {
			return false, fmt.Errorf("clock: state.json %s: %w", where, err)
		}
		return now.Sub(at) >= every, nil
	}
	due := func(name string, every time.Duration) (bool, error) {
		prev, ok := st.Clock(name)
		if !ok {
			return true, nil
		}
		return since(prev, "clock."+name, every)
	}
	// a schedule is due per project; a run from before runs were kept per
	// project (clock.<name>) counts for each project that has none since
	dueOn := func(job config.Schedule, key string) (bool, error) {
		if prev, ok := st.ScheduleRun(job.Name, key); ok {
			return since(prev, "schedules."+job.Name+"."+key, job.Every)
		}
		return due(job.Name, job.Every)
	}
	for _, job := range cfg.Schedules {
		for _, key := range scheduleProjects(cfg, job) {
			if mode, _ := cfg.ProjectMode(key); mode == config.ModeBoard {
				s.logf("clock: %s names %s, a board project; skipped", job.Name, key)
				continue
			}
			ok, err := dueOn(job, key)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := s.fileScheduled(ctx, cfg, job, key); err != nil {
				return fmt.Errorf("clock: %s: %w", job.Name, err)
			}
			st.SetScheduleRun(job.Name, key, clock.Stamp(s.Board.Clock.Now()))
		}
	}
	if ok, err := due(MemoryReportName, MemoryReportEvery); err != nil {
		return err
	} else if ok {
		if s.Memory != nil {
			if report, ok := s.Memory.Report(ctx, cfg, s.closedIDs()); ok && report != "" {
				if _, err := s.Notice(ctx, MemoryNoticeTitle, report); err != nil {
					return fmt.Errorf("clock: memory report: %w", err)
				}
			}
		}
		st.SetClock(MemoryReportName, clock.Stamp(s.Board.Clock.Now()))
	}
	if ok, err := due(VersionsName, VersionsEvery); err != nil {
		return err
	} else if ok {
		if s.Versions != nil {
			if err := s.Versions.Versions(ctx, cfg); err != nil {
				return fmt.Errorf("clock: versions: %w", err)
			}
		}
		st.SetClock(VersionsName, clock.Stamp(s.Board.Clock.Now()))
	}
	if err := s.Board.SaveState(st); err != nil {
		return fmt.Errorf("clock: %w", err)
	}
	return nil
}

// scheduleProjects is the clock's project list for one schedule: its own
// list, or every factory project; then without the paused ones and those
// that opted out with `clock = false` (a key config does not name stays).
func scheduleProjects(cfg *config.Config, job config.Schedule) []string {
	var keys []string
	if job.ProjectsSet {
		keys = job.Projects
	} else {
		for _, p := range cfg.FactoryProjects() {
			keys = append(keys, p.Key)
		}
	}
	var out []string
	for _, k := range keys {
		if p, named := cfg.Project(k); !named && cfg.Off == nil || p.Enabled && p.Clock {
			out = append(out, k)
		}
	}
	return out
}

func (s Service) fileScheduled(ctx context.Context, cfg *config.Config, job config.Schedule, key string) error {
	repo := ""
	if p, ok := cfg.Project(key); ok {
		repo = p.GitHub
	}
	in := ticket.NewIn{
		Project: key, Title: job.Title, Body: strings.ReplaceAll(job.Body, "{github}", repo),
		Door: string(model.DoorClock),
	}
	if job.Kind == string(model.KindProspect) || job.Kind == string(model.KindTask) {
		// a prospect proposes; a task reports back to you (a check, not a change)
		in.Column = string(model.ColReady)
		kind, findings := job.Kind, job.Findings
		_, err := s.file(ctx, in, func(t *ticket.Ticket) {
			t.Kind = ticket.Some(kind)
			if findings {
				// its agent reports structured findings, which a verifier checks
				t.FindingsAsked = ticket.Some(true)
			}
		})
		return err
	}
	in.Column = job.Column
	_, err := s.file(ctx, in, nil)
	return err
}

// closedIDs are the tickets in done or rejected.
func (s Service) closedIDs() map[string]bool {
	out := map[string]bool{}
	ts, _ := s.Board.Reader.List()
	for _, t := range ts {
		if closed(t.Column) {
			out[string(t.ID)] = true
		}
	}
	return out
}
