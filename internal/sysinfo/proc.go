package sysinfo

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// parseLimit is a cgroup memory file: a number, or "max" (0, no limit).
func parseLimit(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "max" || s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// ownCgroup is the cgroup v2 path in /proc/self/cgroup ("0::/path").
func ownCgroup(selfCgroup string) string {
	for _, line := range strings.Split(selfCgroup, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// slicePath is the user slice the process is in (the tick Foreman's wake
// starts runs in the user manager, under it), else user-<uid>.slice (the
// timer's tick is a system unit, but the agents' containers are there).
func slicePath(selfCgroup string, uid int) string {
	for _, line := range strings.Split(selfCgroup, "\n") {
		_, p, ok := strings.Cut(line, "::")
		if !ok {
			continue
		}
		parts := strings.Split(strings.TrimSpace(p), "/")
		for i, seg := range parts {
			if strings.HasPrefix(seg, "user-") && strings.HasSuffix(seg, ".slice") {
				return strings.Join(parts[:i+1], "/")
			}
		}
	}
	return fmt.Sprintf("/user.slice/user-%d.slice", uid)
}

// readCgroup reads a group's memory under root's /sys/fs/cgroup.
func readCgroup(root, path string) (*Cgroup, error) {
	if path == "" {
		return nil, errNoValue
	}
	dir := filepath.Join(root, "sys/fs/cgroup", filepath.Clean("/"+path))
	b, err := readSmall(dir, "memory.current")
	if err != nil {
		return nil, err
	}
	cur, err := parseLimit(string(b))
	if err != nil {
		return nil, fmt.Errorf("memory.current: %w", err)
	}
	cg := &Cgroup{Path: path, Current: cur}
	for name, dst := range map[string]*uint64{"memory.max": &cg.Max, "memory.high": &cg.High, "memory.swap.current": &cg.SwapCurrent} {
		if b, err := readSmall(dir, name); err == nil {
			*dst, _ = parseLimit(string(b))
		}
	}
	return cg, nil
}

// Live is what Foreman reads for itself, inside its sandbox: statfs works
// there, and it may see its own process.
type Live struct {
	Taken   time.Time `json:"taken"`
	Disks   []Disk    `json:"disks,omitempty"`
	Process Process   `json:"process"`
}

// Process is a process's own memory: RSS from /proc/self/status, the Go
// runtime's, and its cgroup's (a unit's MemoryMax).
type Process struct {
	RSS    uint64  `json:"rss,omitempty"`
	GoSys  uint64  `json:"go_sys"`
	GoHeap uint64  `json:"go_heap"`
	Cgroup *Cgroup `json:"cgroup,omitempty"`
}

// LiveOf reads Live under root ("" is "/"); statfs nil is Statfs.
func LiveOf(root string, p paths.Paths, now time.Time, statfs func(string) (Disk, error)) Live {
	if root == "" {
		root = "/"
	}
	l := Live{Taken: now, Disks: vitals.Disks(statfs, []string{"/", p.Root}, nil)}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	l.Process.GoSys, l.Process.GoHeap = ms.Sys, ms.HeapAlloc
	if b, err := readSmall(root, "proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				if f := strings.Fields(v); len(f) > 0 {
					n, _ := strconv.ParseUint(f[0], 10, 64)
					l.Process.RSS = n * 1024
				}
			}
		}
	}
	if b, err := readSmall(root, "proc/self/cgroup"); err == nil {
		l.Process.Cgroup, _ = readCgroup(root, ownCgroup(string(b)))
	}
	return l
}
