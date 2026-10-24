package hostrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/scion"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Local runs agents on this host through Scion.
type Local struct {
	Paths paths.Paths
	Scion scion.Client
	Clock clock.Clock
}

// Host is "local".
func (l *Local) Host() string { return model.LocalHost }

// Running is running_agents: the names of the agents Scion runs
// in every factory project and, once it has a .scion, the manager's
// workspace. If any list fails the answer is ErrUnknown, never a partial or
// empty set (a failed list is not an empty one), so the tick counts this
// host as full and strands nothing. (Python also crashed the tick when a
// list could not run at all; that is ErrUnknown here too.) A project the
// repository guard refuses runs no agent of the factory's and is
// skipped, so one refused project does not stop starts in every other; its
// own tickets get the refusal as a dispatcher error when they start.
func (l *Local) Running(ctx context.Context, cfg *config.Config) (map[string]bool, error) {
	var dirs []string
	for _, p := range cfg.FactoryProjects() {
		dirs = append(dirs, p.Path)
	}
	if st, err := os.Stat(filepath.Join(l.Paths.Manager, ".scion")); err == nil && st.IsDir() {
		dirs = append(dirs, l.Paths.Manager)
	}
	names := map[string]bool{}
	var failed []error
	for _, dir := range dirs {
		got, err := l.Scion.List(ctx, dir)
		if errors.Is(err, scion.ErrRefused) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			failed = append(failed, err)
			continue
		}
		for _, n := range got {
			names[n] = true
		}
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("%w: %w", ErrUnknown, errors.Join(failed...))
	}
	return names, nil
}

// Start is the local launch of start_stage: the project's Scion
// bootstrap, then `scion start <name> -t <template> --message-mode none
// [--config run/<name>.yaml] [-b <branch>] [--model M] <prompt>`, with -b
// for branch stages only. A failed start leaves its run config removed
// (scion.Client) and the memory copy to the caller.
func (l *Local) Start(ctx context.Context, j Job) error {
	if j.Gate != nil {
		return fmt.Errorf("start %s: a gate here runs in the tick, not as a job", j.Name)
	}
	info, err := j.info()
	if err != nil {
		return err
	}
	if j.Project == nil || j.Project.Path == "" {
		return fmt.Errorf("start %s: no checkout on this host", j.Name)
	}
	args := scion.StartArgs{Name: j.Name, Template: j.Template, Config: j.RunConfig, Model: j.Model, Prompt: j.Prompt}
	if info.Branch {
		if args.Branch, err = j.branch(); err != nil {
			return err
		}
	}
	if err := l.Scion.EnsureProject(ctx, j.Project.Path); err != nil {
		return err
	}
	if len(j.Pinned) > 0 {
		if args.Config, err = l.pinnedConfig(j); err != nil {
			return err
		}
	}
	return l.Scion.Start(ctx, j.Project.Path, args)
}

// pinnedConfig adds the pinned mounts to the run's config: after the
// memory mount in run/<name>.yaml (whose volumes list ends the file), or
// in a new one. On a failure the file is removed, as a failed start does.
func (l *Local) pinnedConfig(j Job) (string, error) {
	pins, err := l.pins(j)
	if err != nil {
		l.dropPinned(j.Name)
		return "", fmt.Errorf("start %s: pinned mounts: %w", j.Name, err)
	}
	text, err := scion.PinnedVolumes(j.Name, pins)
	if err != nil {
		l.dropPinned(j.Name)
		return "", err
	}
	conf, flag := j.RunConfig, os.O_WRONLY|os.O_APPEND
	if conf == "" {
		conf, flag, text = filepath.Join(l.Paths.Run, j.Name+".yaml"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, "volumes:\n"+text
	}
	f, err := os.OpenFile(conf, flag, 0o666)
	if err == nil {
		_, err = f.WriteString(text)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.Remove(conf)
		l.dropPinned(j.Name)
		return "", fmt.Errorf("start %s: pinned mounts: %w", j.Name, err)
	}
	return conf, nil
}

// maxPinnedCopy caps a pinned file copied for a run (a lockfile is a few KiB).
const maxPinnedCopy = 16 << 20

// pinnedDir is where a run's copies of the pinned files live: run/pinned/<name>.
func (l *Local) pinnedDir(name string) string { return filepath.Join(l.Paths.Run, "pinned", name) }

// pins are the run's pinned mounts: a file is copied into the run's own
// directory and mounted from there, writable; a directory is mounted from
// the project, read-only. The copy is read without following a link.
func (l *Local) pins(j Job) ([]scion.Pinned, error) {
	dir := l.pinnedDir(j.Name)
	if err := fsx.RemoveTree(dir); err != nil {
		return nil, err
	}
	var out []scion.Pinned
	for _, rel := range j.Pinned {
		if rel == "" || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("pinned path %q is not inside the project", rel)
		}
		src := filepath.Join(j.Project.Path, rel)
		fi, err := os.Lstat(src)
		if err != nil {
			return nil, err
		}
		if fi.IsDir() {
			out = append(out, scion.Pinned{Rel: rel, Source: src, ReadOnly: true})
			continue
		}
		data, err := fsx.ReadNoFollow(src, maxPinnedCopy)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return nil, err
		}
		out = append(out, scion.Pinned{Rel: rel, Source: dst})
	}
	return out, nil
}

// dropPinned removes a run's copies of the pinned files, best effort.
func (l *Local) dropPinned(name string) { fsx.RemoveTree(l.pinnedDir(name)) }

// home is the agent's Scion home, "" when there is none.
func (l *Local) home(name string) string { return scion.AgentHome(l.Paths.AgentHomes, name) }

// MaxReportBytes bounds a local report.json; a bigger one reads as invalid.
const MaxReportBytes = 16 << 20

// Report is read_report: the agent's report.json, and whether it
// left one. The file is read without following a link (the home is the
// agent's to plant one in) and up to MaxReportBytes; a report that is not a
// regular file or is too big comes back empty, which parses as an invalid
// report and fails closed.
func (l *Local) Report(name string) ([]byte, bool, error) {
	home := l.home(name)
	if home == "" {
		return nil, false, nil
	}
	path := filepath.Join(home, jobproto.MemberReport)
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case !st.Mode().IsRegular() || st.Size() > MaxReportBytes:
		return []byte{}, true, nil
	}
	f, ok, err := openRegular(path)
	if err != nil || !ok {
		return []byte{}, true, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxReportBytes+1))
	if err != nil {
		return nil, true, err
	}
	if len(b) > MaxReportBytes {
		return []byte{}, true, nil
	}
	return b, true, nil
}

// Quiet is quiet(): the agent's agent.log exists and has not
// changed for quietFor (hosts.QuietFor, 4 minutes); only then is it looked at for a
// usage limit.
func (l *Local) Quiet(name string, quietFor time.Duration) bool {
	home := l.home(name)
	if home == "" {
		return false
	}
	st, err := os.Lstat(filepath.Join(home, jobproto.MemberLog))
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	return l.Clock.Now().Sub(st.ModTime()) > quietFor
}

// LookBytes is how much of a local agent.log a look falls back to.
const LookBytes = 64_000

// Look is the local half of look(): the agent's screen from
// `scion look --plain -n <lines>` in its checkout dir, or, when Scion shows
// nothing, the end of its agent.log; ScreenText of either, "" when there is
// neither. Only a Scion that could not run (or a refused repository) is an
// error.
func (l *Local) Look(ctx context.Context, dir, name string, lines int) (string, error) {
	if dir != "" {
		out, ok, err := l.screen(ctx, dir, name, lines)
		if err != nil {
			return "", err
		}
		if ok {
			return ScreenText(out, lines), nil
		}
	}
	home := l.home(name)
	if home == "" {
		return "", nil
	}
	b, ok, err := readTail(filepath.Join(home, jobproto.MemberLog), LookBytes)
	if err != nil || !ok {
		return "", err
	}
	return ScreenText(jobproto.DecodeText(b), lines), nil
}

// screen runs scion look; ok is false for a non-zero exit or a blank screen.
func (l *Local) screen(ctx context.Context, dir, name string, lines int) (string, bool, error) {
	out, err := l.Scion.Look(ctx, dir, name, lines)
	var exit *execx.ExitError
	switch {
	case errors.As(err, &exit):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	out = jobproto.DecodeText([]byte(out))
	if textx.Strip(out) == "" {
		return "", false, nil
	}
	return out, true, nil
}

// Remove is remove_agent: keep the agent's last screen and archive
// its report, log and prompt into logs/<id>/<name>/, drop its run config,
// then `scion stop` and `scion delete [-b] -y` (keepBranch gives -b, as
// remove_agent(keep_branch=True) does; the tick passes true for branch
// stages). Scion's exit codes are ignored, as Python's check=False; a Scion
// that cannot run, or a refused repository, stops the removal with its
// error, as in Python.
func (l *Local) Remove(ctx context.Context, dir string, id ticket.ID, name string, keepBranch bool) error {
	if err := l.keepScreen(ctx, dir, id, name); err != nil {
		return err
	}
	if err := l.archive(id, name); err != nil {
		return err
	}
	if err := l.DropRunConfig(name); err != nil {
		return err
	}
	if err := l.Scion.Stop(ctx, dir, name); err != nil {
		return err
	}
	return l.Scion.Delete(ctx, dir, name, keepBranch)
}

// keepScreen is keep_screen: the last 200 lines of the agent's
// screen in screen.txt, cut to MaxArchivedBytes characters.
func (l *Local) keepScreen(ctx context.Context, dir string, id ticket.ID, name string) error {
	out, ok, err := l.screen(ctx, dir, name, ScreenLines)
	if err != nil || !ok {
		return err
	}
	dest, err := archiveDir(l.Paths.Logs, id, name)
	if err != nil {
		return err
	}
	text := lastRunes(ScreenText(out, ScreenLines), MaxArchivedBytes)
	return writeNoFollow(filepath.Join(dest, "screen.txt"), []byte(text))
}

// archive is archive_agent: report.json, agent.log and prompt.md
// copied whole from the agent's home, with their mode and times. A link in
// the home is not followed (Python's copy2 would copy what it points at).
func (l *Local) archive(id ticket.ID, name string) error {
	home := l.home(name)
	if home == "" {
		return nil
	}
	dest, err := archiveDir(l.Paths.Logs, id, name)
	if err != nil {
		return err
	}
	for _, f := range ArchivedFiles() {
		if _, err := copyRegular(filepath.Join(home, f), filepath.Join(dest, f)); err != nil {
			return fmt.Errorf("archive %s: %w", f, err)
		}
	}
	return nil
}

// DropRunConfig removes run/<name>.yaml, the run's inline Scion config;
// one already gone is fine.
func (l *Local) DropRunConfig(name string) error {
	if !jobproto.JobNameOK(name) {
		return fmt.Errorf("run config: bad agent name %q", name)
	}
	l.dropPinned(name)
	err := os.Remove(filepath.Join(l.Paths.Run, name+".yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
