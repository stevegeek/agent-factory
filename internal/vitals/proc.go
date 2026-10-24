package vitals

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

var errNoValue = errors.New("no value")

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

func firstLine(root, rel string) string {
	b, err := readSmall(root, rel)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

// parseMeminfo reads the kB lines of /proc/meminfo it needs.
func parseMeminfo(b []byte) (Memory, error) {
	kb := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseUint(f[0], 10, 64); err == nil {
			kb[k] = n * 1024
		}
	}
	total, ok := kb["MemTotal"]
	if !ok || total == 0 {
		return Memory{}, fmt.Errorf("no MemTotal")
	}
	avail, ok := kb["MemAvailable"]
	if !ok {
		avail = kb["MemFree"] + kb["Buffers"] + kb["Cached"]
	}
	avail = min(avail, total)
	m := Memory{Total: total, Available: avail, Used: total - avail, SwapTotal: kb["SwapTotal"]}
	if free := kb["SwapFree"]; free <= m.SwapTotal {
		m.SwapUsed = m.SwapTotal - free
	}
	return m, nil
}

// parseLoad is /proc/loadavg's three averages.
func parseLoad(b []byte) ([]float64, error) {
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return nil, fmt.Errorf("%q is not a load average", strings.TrimSpace(string(b)))
	}
	out := make([]float64, 3)
	for i := range out {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a load average", f[i])
		}
		out[i] = v
	}
	return out, nil
}

// parseUptime is /proc/uptime's first field, in whole seconds.
func parseUptime(b []byte) (int64, error) {
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errNoValue
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(v), nil
}

// parseCPUList counts a kernel CPU list ("0-3,6"); 0 when it does not read.
func parseCPUList(s string) int {
	n := 0
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return 0
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return 0
			}
		}
		n += b - a + 1
	}
	return n
}

func cpus(root string) int {
	if b, err := readSmall(root, "sys/devices/system/cpu/online"); err == nil {
		if n := parseCPUList(string(b)); n > 0 {
			return n
		}
	}
	return runtime.NumCPU()
}

// thermalPrefer is the zone types that are the CPU, best first.
func thermalPrefer() []string {
	return []string{"x86_pkg_temp", "cpu-thermal", "cpu_thermal", "coretemp", "k10temp", "soc_thermal", "soc-thermal", "acpitz"}
}

// readTemp is the CPU's zone, else the first zone that reads; nil when
// /sys/class/thermal has none.
func readTemp(root string) *Temp {
	zones, _ := filepath.Glob(filepath.Join(root, "sys/class/thermal/thermal_zone*"))
	var best *Temp
	prefer := thermalPrefer()
	rank := len(prefer)
	for _, z := range zones {
		b, err := readSmall(z, "temp")
		if err != nil {
			continue
		}
		milli, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || milli <= 0 || milli > 150000 {
			continue
		}
		typ := firstLine(z, "type")
		r := slices.Index(prefer, typ)
		if r < 0 {
			r = len(prefer)
		}
		if best == nil || r < rank {
			best, rank = &Temp{Zone: typ, Celsius: float64(milli) / 1000}, r
		}
	}
	return best
}

// Statfs is statfs(2) of path.
func Statfs(path string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	bs := uint64(st.Bsize)
	d := Disk{Path: path, Total: st.Blocks * bs, Free: st.Bavail * bs, fsid: st.Fsid.X__val}
	if free := st.Bfree * bs; free <= d.Total {
		d.Used = d.Total - free
	}
	return d, nil
}

// Disks statfs's each path once per file system ("" is skipped); statfs
// nil is Statfs, and problem, when not nil, hears of each that fails.
func Disks(statfs func(string) (Disk, error), list []string, problem func(string, error)) []Disk {
	if statfs == nil {
		statfs = Statfs
	}
	var out []Disk
	seen := map[[2]int32]bool{}
	for _, p := range list {
		if p == "" {
			continue
		}
		d, err := statfs(p)
		if err != nil {
			if problem != nil {
				problem("disk "+p, err)
			}
			continue
		}
		if d.fsid != ([2]int32{}) {
			if seen[d.fsid] {
				continue
			}
			seen[d.fsid] = true
		}
		out = append(out, d)
	}
	return out
}
