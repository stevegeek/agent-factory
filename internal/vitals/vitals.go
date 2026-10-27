// Package vitals is one machine's vital signs: hostname, kernel, uptime,
// load, CPUs, memory, disks, temperature and, when asked, its running
// containers and the image versions it built. The dispatcher's tick reads its own for sysinfo; a helper
// host answers the `vitals` verb with its own (factory-remote); a pull host
// posts its own to its mailbox (factory-pull).
//
// Everything is read from files under an injectable root, so the parsers
// run against fixture trees in tests; a part that cannot be read is a line
// in Problems, never a failure. What a worker sends is untrusted: Parse
// takes it only within strict bounds.
package vitals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/buildinfo"
)

// Vitals is one machine at Taken. Sizes are bytes.
type Vitals struct {
	Taken time.Time `json:"taken"`
	// Reported is the machine's own clock when it took them, for vitals the
	// dispatcher received (Received): Taken is then the dispatcher's.
	Reported time.Time `json:"reported,omitzero"`
	Version  string    `json:"version"` // the binary that took them (buildinfo)
	Hostname string    `json:"hostname,omitempty"`
	Kernel   string    `json:"kernel,omitempty"`
	Uptime   int64     `json:"uptime_seconds,omitempty"`
	Load     []float64 `json:"load,omitempty"` // 1, 5 and 15 minutes
	CPUs     int       `json:"cpus,omitempty"`
	Memory   *Memory   `json:"memory,omitempty"`
	Disks    []Disk    `json:"disks,omitempty"`
	Temp     *Temp     `json:"temp,omitempty"`
	// Containers are podman's running containers (nil: not asked, or podman
	// did not answer).
	Containers []string `json:"containers"`
	// Images are the versions in the host's images.json, by kind (nil: not
	// asked, or none recorded). A helper's vitals verb fills them in.
	Images map[string]Image `json:"images,omitempty"`
	// Workers is a pull host's own account of the workers it hands jobs to
	// (nil: not a pull host, or a factory-pull from before it was sent).
	Workers  *Workers `json:"workers,omitempty"`
	Problems []string `json:"problems,omitempty"` // what could not be read
}

// Workers is what a pull host can take on now: the dispatcher sends it no
// more jobs than Running, and none at all when Up is false or Running is 0.
type Workers struct {
	Up      bool   `json:"up"`             // the worker system answered (Lever's broker)
	Running int    `json:"running"`        // workers running
	Free    int    `json:"free"`           // running, idle, and with no job in hand
	Note    string `json:"note,omitempty"` // why none can take a job, or what is wrong
}

// maxWorkers bounds a posted worker count.
const maxWorkers = 1000

// Memory is /proc/meminfo's headline: Used is Total less Available.
type Memory struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
	Used      uint64 `json:"used"`
	SwapTotal uint64 `json:"swap_total"`
	SwapUsed  uint64 `json:"swap_used"`
}

// Disk is one file system as statfs(2) sees it: Free is what an
// unprivileged user may still use, Used is Total less what is free to root.
type Disk struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
	Used  uint64 `json:"used"`
	fsid  [2]int32
}

// Image is one kind's entry in a host's images.json: the version its
// image has, since when, the one :previous holds and one held back.
type Image struct {
	Version  string `json:"version"`
	Since    string `json:"since,omitempty"`
	Previous string `json:"previous,omitempty"`
	Held     string `json:"held,omitempty"`
}

// Temp is the CPU's temperature, from /sys/class/thermal.
type Temp struct {
	Zone    string  `json:"zone"`
	Celsius float64 `json:"celsius"`
}

// Deps is what Collect reads besides the files. Any func may be nil.
type Deps struct {
	Root string           // where /proc and /sys are: "" is "/" (tests: a fixture tree)
	Now  func() time.Time // nil: the wall clock, UTC
	// DiskPaths are statfs'd, each file system once.
	DiskPaths []string
	Statfs    func(path string) (Disk, error) // nil: Statfs
	// Containers is `podman ps` (names), given ContainersTimeout; nil
	// leaves Containers nil.
	Containers func(ctx context.Context) ([]string, error)
}

const (
	// ContainersTimeout bounds podman ps.
	ContainersTimeout = 2 * time.Second
	// MaxBytes caps what Parse takes.
	MaxBytes = 128 << 10
)

// Collect gathers this machine's Vitals. It never fails: what cannot be
// read is left out and named in Problems.
func Collect(ctx context.Context, d Deps) Vitals {
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now()
	}
	v := Vitals{Taken: now, Version: buildinfo.String()}
	problem := func(what string, err error) { v.Problems = append(v.Problems, what+": "+err.Error()) }
	root := d.Root
	if root == "" {
		root = "/"
	}
	v.Hostname = firstLine(root, "proc/sys/kernel/hostname")
	v.Kernel = firstLine(root, "proc/sys/kernel/osrelease")
	if b, err := readSmall(root, "proc/uptime"); err == nil {
		v.Uptime, _ = parseUptime(b)
	}
	if b, err := readSmall(root, "proc/loadavg"); err != nil {
		problem("loadavg", err)
	} else if l, err := parseLoad(b); err != nil {
		problem("loadavg", err)
	} else {
		v.Load = l
	}
	v.CPUs = cpus(root)
	if b, err := readSmall(root, "proc/meminfo"); err != nil {
		problem("meminfo", err)
	} else if m, err := parseMeminfo(b); err != nil {
		problem("meminfo", err)
	} else {
		v.Memory = &m
	}
	v.Disks = Disks(d.Statfs, d.DiskPaths, problem)
	v.Temp = readTemp(root)
	if d.Containers != nil {
		cctx, cancel := context.WithTimeout(ctx, ContainersTimeout)
		names, err := d.Containers(cctx)
		cancel()
		if err != nil {
			problem("podman ps", err)
		} else {
			v.Containers = append([]string{}, names...)
		}
	}
	return v
}

// Received is v as the dispatcher took it in at now: the machine's own
// stamp moves to Reported, so a worker's clock never decides an age.
func (v Vitals) Received(now time.Time) Vitals {
	v.Reported, v.Taken = v.Taken, now
	return v
}

// Bounds of what Parse takes.
const (
	maxLoad       = 3
	maxDisks      = 16
	maxContainers = 64
	maxImages     = 16
	maxProblems   = 32
	maxString     = 200
	maxCPUs       = 1 << 14
)

// Parse reads vitals a worker sent: JSON within MaxBytes, at most 3 load
// averages, 16 disks, 64 containers, 16 images and 32 problems, numbers within
// reason, and strings with no control characters, trimmed and cut to 200
// bytes. Anything else refuses the whole.
func Parse(b []byte) (Vitals, error) {
	if len(b) > MaxBytes {
		return Vitals{}, fmt.Errorf("vitals: over %d bytes", MaxBytes)
	}
	var v Vitals
	if err := json.Unmarshal(b, &v); err != nil {
		return Vitals{}, fmt.Errorf("vitals: %w", err)
	}
	if err := v.check(); err != nil {
		return Vitals{}, fmt.Errorf("vitals: %w", err)
	}
	return v, nil
}

func (v *Vitals) check() error {
	var errs []error
	str := func(what string, s *string) {
		t, err := clean(*s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
		*s = t
	}
	str("version", &v.Version)
	str("hostname", &v.Hostname)
	str("kernel", &v.Kernel)
	if v.Uptime < 0 {
		errs = append(errs, errors.New("uptime: negative"))
	}
	if len(v.Load) > maxLoad {
		errs = append(errs, fmt.Errorf("load: over %d values", maxLoad))
	}
	for _, l := range v.Load {
		if math.IsNaN(l) || l < 0 || l > 1e6 {
			errs = append(errs, fmt.Errorf("load: %v is not a load average", l))
		}
	}
	if v.CPUs < 0 || v.CPUs > maxCPUs {
		errs = append(errs, fmt.Errorf("cpus: %d", v.CPUs))
	}
	if m := v.Memory; m != nil && (m.Available > m.Total || m.Used > m.Total || m.SwapUsed > m.SwapTotal) {
		errs = append(errs, errors.New("memory: more used or available than there is"))
	}
	if len(v.Disks) > maxDisks {
		errs = append(errs, fmt.Errorf("disks: over %d", maxDisks))
	}
	for i := range v.Disks {
		d := &v.Disks[i]
		str("disk path", &d.Path)
		if d.Used > d.Total || d.Free > d.Total {
			errs = append(errs, fmt.Errorf("disk %s: more used or free than there is", d.Path))
		}
	}
	if w := v.Workers; w != nil {
		str("workers note", &w.Note)
		if w.Running < 0 || w.Running > maxWorkers || w.Free < 0 || w.Free > w.Running {
			errs = append(errs, fmt.Errorf("workers: %d running, %d free", w.Running, w.Free))
		}
	}
	if t := v.Temp; t != nil {
		str("temp zone", &t.Zone)
		if math.IsNaN(t.Celsius) || t.Celsius < -100 || t.Celsius > 200 {
			errs = append(errs, fmt.Errorf("temp: %v °C", t.Celsius))
		}
	}
	list := func(what string, l []string, max int) {
		if len(l) > max {
			errs = append(errs, fmt.Errorf("%s: over %d", what, max))
		}
		for i := range l {
			str(what, &l[i])
		}
	}
	list("containers", v.Containers, maxContainers)
	list("problems", v.Problems, maxProblems)
	if len(v.Images) > maxImages {
		errs = append(errs, fmt.Errorf("images: over %d", maxImages))
	}
	if v.Images != nil {
		images := make(map[string]Image, len(v.Images))
		for kind, im := range v.Images {
			str("image kind", &kind)
			str("image version", &im.Version)
			str("image since", &im.Since)
			str("image previous", &im.Previous)
			str("image held", &im.Held)
			images[kind] = im
		}
		v.Images = images
	}
	return errors.Join(errs...)
}

// clean is s trimmed and cut to maxString bytes (at a rune); a control
// character refuses it.
func clean(s string) (string, error) {
	if strings.ContainsFunc(s, unicode.IsControl) {
		return "", errors.New("a control character")
	}
	s = strings.TrimSpace(s)
	if len(s) <= maxString {
		return s, nil
	}
	cut := maxString
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], nil
}

// Encode is v as JSON, one line, HTML left unescaped (it is shown through
// templates, which escape it there).
func (v Vitals) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("vitals: %w", err)
	}
	return buf.Bytes(), nil
}
