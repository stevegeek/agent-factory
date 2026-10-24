package hostrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Transport carries the helper verbs to a helper host's jobs:
// sshjobs.Client (factory-remote over ssh) or mailbox.Box (a pull host's
// mailbox here). Do has sshjobs.Client.Do's contract: a failure the host
// answered with is a *jobproto.RemoteError.
type Transport interface {
	Do(ctx context.Context, h jobproto.Host, verb, job string, stdin []byte, timeout time.Duration) ([]byte, error)
}

// Reader is the read-only half (tail and list): jobtail.Reader, or
// mailbox.Box for a pull host.
type Reader interface {
	Tail(ctx context.Context, h jobproto.Host, job string) (string, error)
	List(ctx context.Context, h jobproto.Host) ([]string, error)
}

// Jobs runs agents as jobs on one helper host, through its transport.
type Jobs struct {
	Remote jobproto.Host
	Paths  paths.Paths
	Via    Transport
	Read   Reader
	Clock  clock.Clock
}

// HostOf is how the dispatcher reaches [hosts.<name>]: its ssh destination
// and its key (`key`, else ~/.ssh/factory-remote), with a leading ~
// expanded as os.path.expanduser does.
func HostOf(p paths.Paths, h config.HostCfg) jobproto.Host {
	key := h.Key
	if key == "" {
		key = p.RemoteKey
	}
	if key == "~" {
		key = p.Home
	} else if rest, ok := strings.CutPrefix(key, "~/"); ok {
		key = filepath.Join(p.Home, rest)
	}
	return jobproto.Host{Name: h.Name, SSH: h.SSH, Key: key}
}

// Host is the helper's [hosts.<name>] key.
func (h *Jobs) Host() string { return h.Remote.Name }

// Start is start_remote. No-branch stages work on a branch named
// after the stage, starting from the base ("none" with no project; a task
// whose ref exists starts from the ref). Branch stages work on the
// ticket's branch, starting from it if it exists, otherwise from the base.
// The tar holds prompt.md, role.md (the template's agents.md), meta.json,
// repo.bundle of the base and start (with a project), the memory files,
// and untracked/<f> for each pinned_untracked regular file (a gate job's
// tar is gateFiles'). If the helper refuses, the job is removed (it may
// have landed; no orphan may hold a slot) and the start's error returned.
func (h *Jobs) Start(ctx context.Context, j Job) error {
	files, err := h.startFiles(ctx, j)
	if err != nil {
		return err
	}
	tar, err := jobproto.TarFlat(files, h.Clock.Now())
	if err != nil {
		return err
	}
	_, err = h.Via.Do(ctx, h.Remote, jobproto.VerbStart, j.Name, tar, 0)
	var re *jobproto.RemoteError
	if errors.As(err, &re) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobproto.Timeout(jobproto.VerbRemove))
		h.Via.Do(cleanup, h.Remote, jobproto.VerbRemove, j.Name, nil, 0) // best effort, as Python
		cancel()
	}
	return markRemote(err)
}

// StartMeta is the meta.json of j: its branch and start, model, bundle
// (work only) and timeout.
func (h *Jobs) StartMeta(ctx context.Context, j Job) (jobproto.Meta, error) {
	info, err := j.info()
	if err != nil {
		return jobproto.Meta{}, err
	}
	m := jobproto.Meta{Model: j.Model, Bundle: j.Stage == model.StageWork, TimeoutMinutes: j.TimeoutMinutes}
	exists := func(b string) (bool, error) {
		if j.Repo == nil {
			return false, fmt.Errorf("start %s: no repository", j.Name)
		}
		return j.Repo.BranchExists(ctx, b)
	}
	if !info.Branch {
		m.Branch, m.Start = string(j.Stage), "none"
		if j.Project != nil {
			m.Start = j.Project.Base
			if ref := j.Ticket.Ref.Or(""); j.Stage == model.StageTask && ref != "" {
				ok, err := exists(ref)
				if err != nil {
					return m, err
				}
				if ok {
					m.Start = ref
				}
			}
		}
		return m, nil
	}
	if j.Project == nil {
		return m, fmt.Errorf("start %s: a %s run needs its project", j.Name, j.Stage)
	}
	if m.Branch, err = j.branch(); err != nil {
		return m, err
	}
	ok, err := exists(m.Branch)
	if err != nil {
		return m, err
	}
	m.Start = j.Project.Base
	if ok {
		m.Start = m.Branch
	}
	return m, nil
}

// GateMinutes is a gate job's time on its host: the gate's own 30 minutes
// here (podman.GateTimeout), and five for the checkout and the report.
const GateMinutes = 35

// gateFiles is a gate job's start tar: meta.json (kind gate, the head as
// its start), checks.sh, repo.bundle of the base and the ticket's branch,
// and untracked/<f> for each pinned_untracked regular file.
func (h *Jobs) gateFiles(ctx context.Context, j Job) ([]jobproto.File, error) {
	if j.Project == nil || j.Repo == nil {
		return nil, fmt.Errorf("start %s: a gate job needs its project's repository", j.Name)
	}
	branch, err := j.branch()
	if err != nil {
		return nil, err
	}
	meta := jobproto.Meta{Branch: "gate", Start: j.Gate.Head, TimeoutMinutes: GateMinutes, Kind: jobproto.KindGate}
	refs := []string{j.Project.Base, branch}
	slices.Sort(refs)
	bundle, err := j.Repo.Bundle(ctx, slices.Compact(refs))
	if err != nil {
		return nil, fmt.Errorf("start %s: bundle: %w", j.Name, err)
	}
	files := []jobproto.File{
		{Name: jobproto.MemberMeta, Data: meta.Encode()},
		{Name: jobproto.MemberChecks, Data: []byte(j.Gate.Script)},
		{Name: jobproto.MemberBundle, Data: bundle},
	}
	return pinnedFiles(j, files)
}

// pinnedFiles adds untracked/<f> for each of the project's pinned_untracked
// regular files to files.
func pinnedFiles(j Job, files []jobproto.File) ([]jobproto.File, error) {
	if j.Project == nil {
		return files, nil
	}
	for _, f := range j.Project.PinnedUntracked {
		data, err := fsx.ReadNoFollow(filepath.Join(j.Project.Path, f), jobproto.MaxInputBytes)
		if err != nil {
			// os.path.isfile: only a regular file is shipped (here never
			// one reached through a link in its last component)
			if errors.Is(err, fsx.ErrNotRegular) || errors.Is(err, os.ErrNotExist) || errors.Is(err, fsx.ErrTooLarge) {
				continue
			}
			return nil, fmt.Errorf("start %s: %s: %w", j.Name, f, err)
		}
		files = append(files, jobproto.File{Name: jobproto.UntrackedDir + f, Data: data})
	}
	return files, nil
}

func (h *Jobs) startFiles(ctx context.Context, j Job) ([]jobproto.File, error) {
	if j.Gate != nil {
		return h.gateFiles(ctx, j)
	}
	meta, err := h.StartMeta(ctx, j)
	if err != nil {
		return nil, err
	}
	role, err := os.ReadFile(filepath.Join(h.Paths.ScionTemplates, j.Template, "agents.md"))
	if err != nil {
		return nil, fmt.Errorf("start %s: role: %w", j.Name, err)
	}
	files := []jobproto.File{
		{Name: jobproto.MemberPrompt, Data: []byte(j.Prompt)},
		{Name: jobproto.MemberRole, Data: role},
		{Name: jobproto.MemberMeta, Data: meta.Encode()},
	}
	if j.Project != nil {
		if j.Repo == nil {
			return nil, fmt.Errorf("start %s: no repository", j.Name)
		}
		refs := []string{j.Project.Base}
		if meta.Start != j.Project.Base {
			refs = append(refs, meta.Start)
		}
		slices.Sort(refs)
		bundle, err := j.Repo.Bundle(ctx, refs)
		if err != nil {
			return nil, fmt.Errorf("start %s: bundle: %w", j.Name, err)
		}
		files = append(files, jobproto.File{Name: jobproto.MemberBundle, Data: bundle})
	}
	names := make([]string, 0, len(j.Memory))
	for name := range j.Memory {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !strings.HasPrefix(name, jobproto.MemoryDir) {
			return nil, fmt.Errorf("start %s: memory file %q not under %s", j.Name, name, jobproto.MemoryDir)
		}
		files = append(files, jobproto.File{Name: name, Data: j.Memory[name]})
	}
	return pinnedFiles(j, files)
}

// Poll is a job's state, json.loads(status)["state"]: jobproto.StateRunning,
// StateDone, or anything else for a job that is gone.
func (h *Jobs) Poll(ctx context.Context, name string) (string, error) {
	out, err := h.Via.Do(ctx, h.Remote, jobproto.VerbStatus, name, nil, 0)
	if err != nil {
		return "", markRemote(err)
	}
	return jobproto.ParseStatus(out)
}

// Collect is the collect half of poll_remote: the job's files,
// flat and regular only, with report.json, agent.log and prompt.md
// archived to logs/<id>/<name>/ (the last MaxArchivedBytes of each).
func (h *Jobs) Collect(ctx context.Context, id ticket.ID, name string) (*Result, error) {
	out, err := h.Via.Do(ctx, h.Remote, jobproto.VerbCollect, name, nil, 0)
	if err != nil {
		return nil, markRemote(err)
	}
	files, err := jobproto.UntarFlat(out)
	if err != nil {
		return nil, fmt.Errorf("collect %s: %w", name, err)
	}
	dest, err := archiveDir(h.Paths.Logs, id, name)
	if err != nil {
		return nil, err
	}
	for _, f := range ArchivedFiles() {
		if data, ok := files[f]; ok {
			if err := writeNoFollow(filepath.Join(dest, f), lastBytes(data, MaxArchivedBytes)); err != nil {
				return nil, fmt.Errorf("archive %s: %w", f, err)
			}
		}
	}
	return &Result{Files: files}, nil
}

// Remove is `remove <job>`: the helper stops the job and deletes its files.
func (h *Jobs) Remove(ctx context.Context, name string) error {
	_, err := h.Via.Do(ctx, h.Remote, jobproto.VerbRemove, name, nil, 0)
	return markRemote(err)
}

// Tail is the helper half of look(): ScreenText of the job's
// output tail. A helper that cannot be read is shown inline, "(could not
// read the job on <host>: <error>)", as Python does; only an ssh that
// could not run is an error.
func (h *Jobs) Tail(ctx context.Context, name string, lines int) (string, error) {
	out, err := h.Read.Tail(ctx, h.Remote, name)
	var re *jobproto.RemoteError
	switch {
	case errors.As(err, &re):
		out = fmt.Sprintf("(could not read the job on %s: %s)", h.Remote.Name, re)
	case err != nil:
		return "", err
	}
	return ScreenText(out, lines), nil
}

// List is `list`: the jobs the helper holds.
func (h *Jobs) List(ctx context.Context) ([]string, error) {
	names, err := h.Read.List(ctx, h.Remote)
	return names, markRemote(err)
}

// Result is what a finished helper job sent back, by member name.
type Result struct {
	Files map[string][]byte
}

// Report is the job's report.json, and whether there is one.
func (r *Result) Report() ([]byte, bool) {
	b, ok := r.Files[jobproto.MemberReport]
	return b, ok
}

// Head is where the job left its branch: the head file, stripped, or
// "unknown" (which fails a reviewer's head check closed).
func (r *Result) Head() string {
	if h := textx.Strip(jobproto.DecodeText(r.Files[jobproto.MemberHead])); h != "" {
		return h
	}
	return "unknown"
}

// OutBundle is the work branch the job sent back (work only).
func (r *Result) OutBundle() ([]byte, bool) {
	b, ok := r.Files[jobproto.MemberOutBundle]
	return b, ok
}

// MemoryTar is the job's memory, to settle; false means it is lost.
func (r *Result) MemoryTar() ([]byte, bool) {
	b, ok := r.Files[jobproto.MemberMemoryTar]
	return b, ok
}

// LimitText is what a job with no report is searched for a usage limit
// (poll_remote): the last hosts.RemoteLimitChars characters of its
// decoded agent.log; the caller takes hosts.HitLimit(text, 40).
func (r *Result) LimitText(chars int) string {
	return lastRunes(jobproto.DecodeText(r.Files[jobproto.MemberLog]), chars)
}
