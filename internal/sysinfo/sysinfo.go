// Package sysinfo is the factory's vital signs for Foreman's Settings
// page: the dispatcher host's load, memory, the factory user's slice,
// disks, temperature and containers, each worker host's vitals, agents per
// host, pauses, image versions and the last rebuilds.
//
// Foreman runs sandboxed (host/foreman-system.sh): in its own user
// namespace, with ProtectProc=invisible and ProcSubset=pid, it cannot read
// /proc/loadavg, /proc/meminfo or any other process, nor reach a worker
// host. So the tick, which runs as the factory user outside that sandbox,
// collects a Snapshot at the end of every pass (Record) and writes it to
// Root/sysinfo.json (File), which Foreman reads (Read). What Foreman can
// see for itself (statfs, its own memory) it reads live (LiveOf).
//
// The machine-level readers are internal/vitals'; the slice, the factory's
// own state and Live are this package's. Everything is read from files
// under an injectable root, so the parsers run against fixture trees in
// tests; a part that cannot be read is a line in Problems, never a failure.
package sysinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// Snapshot is sysinfo.json: what the tick saw at Taken. Sizes are bytes.
type Snapshot struct {
	Taken    time.Time `json:"taken"`
	Version  string    `json:"version"` // the factory binary that took it (buildinfo)
	Hostname string    `json:"hostname,omitempty"`
	Kernel   string    `json:"kernel,omitempty"`
	Uptime   int64     `json:"uptime_seconds,omitempty"`
	Load     []float64 `json:"load,omitempty"` // 1, 5 and 15 minutes
	CPUs     int       `json:"cpus,omitempty"`
	Memory   *Memory   `json:"memory,omitempty"`
	Slice    *Cgroup   `json:"slice,omitempty"` // the factory user's slice: every agent shares it
	Disks    []Disk    `json:"disks,omitempty"` // / and the factory root
	Temp     *Temp     `json:"temp,omitempty"`
	// Containers are podman's running containers (nil: podman did not answer).
	Containers []string         `json:"containers"`
	Agents     map[string]int   `json:"agents"` // running agents by host ("local" is this one)
	Hosts      []Host           `json:"hosts,omitempty"`
	Pauses     []Pause          `json:"pauses,omitempty"`
	Images     map[string]Image `json:"images,omitempty"` // images.json, by kind
	Rebuilds   []Rebuild        `json:"rebuilds,omitempty"`
	// Switches are the maintainer's project switches (projects.json): each
	// overrides config.toml's enabled until it is back to follow.
	Switches map[string]config.Switch `json:"switches,omitempty"`
	// DeployHold is the maintainer's deploy hold; nil when deploys are not
	// held by the file (an open Deploy? ask holds them too).
	DeployHold *deployrec.Hold `json:"deploy_hold,omitempty"`
	Problems   []string        `json:"problems,omitempty"` // what could not be read
}

// Memory, Disk and Temp are the machine's, as vitals reads them; Image is
// an images.json entry, the dispatcher's own or a helper's.
type (
	Memory = vitals.Memory
	Disk   = vitals.Disk
	Temp   = vitals.Temp
	Image  = vitals.Image
)

// Cgroup is a cgroup v2 group's memory: Max and High are 0 for no limit.
type Cgroup struct {
	Path        string `json:"path"`
	Current     uint64 `json:"current"`
	Max         uint64 `json:"max,omitempty"`
	High        uint64 `json:"high,omitempty"`
	SwapCurrent uint64 `json:"swap_current,omitempty"`
}

// Host is one host's slots as config.toml gives them and its agents as the
// board shows them; "local" is the dispatcher itself. A worker host has
// its Vitals too: its answer this pass or, when it gave none
// (VitalsError), its last one, whose Taken shows its age.
type Host struct {
	Name        string         `json:"name"`
	Slots       int            `json:"slots"`
	Agents      int            `json:"agents"`
	PausedUntil *time.Time     `json:"paused_until,omitempty"`
	Pull        bool           `json:"pull,omitempty"` // transport = "pull": it posts its vitals
	Vitals      *vitals.Vitals `json:"vitals,omitempty"`
	VitalsError string         `json:"vitals_error,omitempty"`
}

// Pause is a host that gets no new agents until a time (pauses.json).
type Pause struct {
	Host  string    `json:"host"`
	Until time.Time `json:"until"`
}

// Rebuild is the last weekly rebuild of one image: When is its log's
// time, Verdict "ok" or "FAILED" from factory.log ("" when that line has
// rotated away and no earlier snapshot had it).
type Rebuild struct {
	Kind    string    `json:"kind"`
	When    time.Time `json:"when"`
	Verdict string    `json:"verdict,omitempty"`
	Summary string    `json:"summary,omitempty"`
}

// Deps is what Collect reads besides the files. Any func may be nil.
type Deps struct {
	Root   string // where /proc and /sys are: "" is "/" (tests: a fixture tree)
	Paths  paths.Paths
	Clock  clock.Clock
	UID    int            // the factory user, for its slice when the process is not in it
	Config *config.Config // the hosts and their slots; nil: none known
	// Tickets is the board, for the agents running per host.
	Tickets func() []*ticket.Ticket
	Pauses  func() []Pause
	// Switches is projects.json; Hold is the deploy hold (nil, nil when
	// there is none). An error is a problem.
	Switches func() (config.Switches, error)
	Hold     func() (*deployrec.Hold, error)
	// Containers is `podman ps` (names); it is given ContainersTimeout.
	Containers func(ctx context.Context) ([]string, error)
	Statfs     func(path string) (Disk, error) // nil: Statfs
	// HostVitals is one worker host's vitals: a helper's answer to the
	// vitals verb, a pull host's last post. Record asks every [hosts] host
	// at once, all within VitalsTimeout; nil asks none.
	HostVitals func(ctx context.Context, h config.HostCfg) (*vitals.Vitals, error)
	// Previous is the last snapshot, for a rebuild verdict factory.log no
	// longer has and the vitals of a host that gave none.
	Previous *Snapshot
}

const (
	// ContainersTimeout bounds podman ps.
	ContainersTimeout = vitals.ContainersTimeout
	// VitalsTimeout bounds asking the worker hosts, all at once, before
	// the collection: the tick waits at most it and RecordTimeout.
	VitalsTimeout = 7 * time.Second
	// RecordTimeout bounds the collection and the write.
	RecordTimeout = 3 * time.Second
	// maxFile caps a sysinfo.json or images.json read.
	maxFile = 1 << 20
)

// File is Root/sysinfo.json. The tick writes it; Foreman reads it (its
// sandbox lets it read the root, not write it).
func File(p paths.Paths) string { return filepath.Join(p.Root, "sysinfo.json") }

func (d Deps) now() time.Time {
	if d.Clock != nil {
		return d.Clock.Now()
	}
	return time.Now().UTC()
}

// Statfs is statfs(2) of path.
func Statfs(path string) (Disk, error) { return vitals.Statfs(path) }

// Collect gathers a Snapshot of this host and the factory. It never
// fails: what cannot be read is left out and named in Problems. It asks no
// worker host (Record does).
func Collect(ctx context.Context, d Deps) Snapshot {
	v := vitals.Collect(ctx, vitals.Deps{Root: d.Root, Now: d.now, DiskPaths: []string{"/", d.Paths.Root},
		Statfs: d.Statfs, Containers: d.Containers})
	s := Snapshot{Taken: v.Taken, Version: v.Version, Hostname: v.Hostname, Kernel: v.Kernel, Uptime: v.Uptime,
		Load: v.Load, CPUs: v.CPUs, Memory: v.Memory, Disks: v.Disks, Temp: v.Temp, Containers: v.Containers,
		Problems: v.Problems, Agents: map[string]int{}}
	problem := func(what string, err error) { s.Problems = append(s.Problems, what+": "+err.Error()) }
	root := d.Root
	if root == "" {
		root = "/"
	}
	self, _ := readSmall(root, "proc/self/cgroup")
	if cg, err := readCgroup(root, slicePath(string(self), d.UID)); err != nil {
		problem("slice", err)
	} else {
		s.Slice = cg
	}
	factory(&s, d, problem)
	return s
}

// Write saves s to path atomically (a temp file opened O_EXCL|O_NOFOLLOW,
// then a rename), so Foreman reads the old snapshot or the new one.
func Write(path string, s Snapshot) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("sysinfo: %w", err)
	}
	if err := fsx.WriteAtomic(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("sysinfo: %w", err)
	}
	return nil
}

// Read loads a snapshot, never through a link. A missing file matches
// fs.ErrNotExist.
func Read(path string) (Snapshot, error) {
	b, err := fsx.ReadNoFollow(path, maxFile)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Record is the tick's hook: ask the worker hosts for their vitals (all
// at once, within VitalsTimeout), then collect and write File(d.Paths)
// (within RecordTimeout). The error is for the log only; the tick goes on.
// A host or a collection that hangs is left behind (the tick's process
// ends soon after), and a panic in either is an error, not a crash.
func Record(ctx context.Context, d Deps) error {
	path := File(d.Paths)
	if d.Previous == nil {
		if prev, err := Read(path); err == nil {
			d.Previous = &prev
		}
	}
	answers := askHosts(ctx, d)
	ctx, cancel := context.WithTimeout(ctx, RecordTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("sysinfo: panic: %v\n%s", p, debug.Stack())
			}
		}()
		s := Collect(ctx, d)
		withVitals(&s, answers, d.Previous)
		done <- Write(path, s)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("sysinfo: took too long (%v); %s not written", context.Cause(ctx), path)
	}
}

// answer is one worker host's vitals, or why there are none.
type answer struct {
	v   *vitals.Vitals
	err error
}

// askHosts asks every configured host for its vitals at once, within
// VitalsTimeout; a host that has not answered by then is an error. nil
// when there is no HostVitals or no config.
func askHosts(ctx context.Context, d Deps) map[string]answer {
	if d.HostVitals == nil || d.Config == nil || len(d.Config.Hosts) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, VitalsTimeout)
	defer cancel()
	type reply struct {
		name string
		answer
	}
	replies := make(chan reply, len(d.Config.Hosts))
	for _, h := range d.Config.Hosts {
		go func() {
			defer func() {
				if p := recover(); p != nil {
					replies <- reply{h.Name, answer{err: fmt.Errorf("panic: %v", p)}}
				}
			}()
			v, err := d.HostVitals(ctx, h)
			if err == nil && v == nil {
				err = errors.New("no answer")
			}
			replies <- reply{h.Name, answer{v, err}}
		}()
	}
	out := map[string]answer{}
	for range d.Config.Hosts {
		select {
		case r := <-replies:
			out[r.name] = r.answer
		case <-ctx.Done():
			for _, h := range d.Config.Hosts {
				if _, ok := out[h.Name]; !ok {
					out[h.Name] = answer{err: fmt.Errorf("no answer in %v", VitalsTimeout)}
				}
			}
			return out
		}
	}
	return out
}

// withVitals puts each asked host's answer on its Host; a host that gave
// none keeps the vitals the previous snapshot had for it.
func withVitals(s *Snapshot, answers map[string]answer, prev *Snapshot) {
	for i := range s.Hosts {
		h := &s.Hosts[i]
		a, ok := answers[h.Name]
		if !ok {
			continue
		}
		if a.err == nil {
			h.Vitals = a.v
			continue
		}
		h.VitalsError = a.err.Error()
		if prev == nil {
			continue
		}
		for _, p := range prev.Hosts {
			if p.Name == h.Name && p.Vitals != nil {
				h.Vitals = p.Vitals
			}
		}
	}
}

// readSmall reads a kernel file: /proc and /sys report no size, so it is
// read to EOF, capped.
func readSmall(root, rel string) ([]byte, error) {
	f, err := os.Open(filepath.Join(root, rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}

var errNoValue = errors.New("no value")
