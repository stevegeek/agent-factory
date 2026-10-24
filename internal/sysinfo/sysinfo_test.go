package sysinfo

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var now = time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)

// fixture writes files under a fake root: rel path -> content.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, data := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const meminfo = `MemTotal:       16303204 kB
MemFree:          812344 kB
MemAvailable:    9876544 kB
Buffers:          123456 kB
SwapTotal:       8388604 kB
SwapFree:        8000000 kB
`

// a host as /proc and /sys show it to the tick
func hostFiles() map[string]string {
	return map[string]string{
		"proc/sys/kernel/hostname":      "hub\n",
		"proc/sys/kernel/osrelease":     "6.12.1-arch1\n",
		"proc/uptime":                   "273722.31 1000000.00\n",
		"proc/loadavg":                  "0.42 0.30 1.25 2/345 6789\n",
		"proc/meminfo":                  meminfo,
		"sys/devices/system/cpu/online": "0-3\n",
		"proc/self/cgroup":              "0::/system.slice/factory@tick.service\n",
		"sys/fs/cgroup/user.slice/user-1001.slice/memory.current":      "524288000\n",
		"sys/fs/cgroup/user.slice/user-1001.slice/memory.max":          "1258291200\n",
		"sys/fs/cgroup/user.slice/user-1001.slice/memory.high":         "max\n",
		"sys/fs/cgroup/user.slice/user-1001.slice/memory.swap.current": "1048576\n",
		"sys/class/thermal/thermal_zone0/type":                         "acpitz\n",
		"sys/class/thermal/thermal_zone0/temp":                         "27800\n",
		"sys/class/thermal/thermal_zone1/type":                         "x86_pkg_temp\n",
		"sys/class/thermal/thermal_zone1/temp":                         "51000\n",
	}
}

func fakeDisk(path string) (Disk, error) {
	return Disk{Path: path, Total: 100 << 30, Free: 40 << 30, Used: 60 << 30}, nil
}

func TestParseLimit(t *testing.T) {
	for in, want := range map[string]uint64{"max\n": 0, "1234\n": 1234, "": 0} {
		if got, _ := parseLimit(in); got != want {
			t.Errorf("limit %q = %d", in, got)
		}
	}
}

// The user's slice is taken from /proc/self/cgroup when the process is in
// it, else it is user.slice/user-<uid>.slice.
func TestSliceDiscovery(t *testing.T) {
	cases := []struct {
		cgroup string
		uid    int
		want   string
	}{
		{"0::/user.slice/user-1001.slice/user@1001.service/app.slice/factory-wake.service\n", 1001, "/user.slice/user-1001.slice"},
		{"0::/system.slice/factory@tick.service\n", 1001, "/user.slice/user-1001.slice"},
		{"12:memory:/foo\n0::/user.slice/user-7.slice/session-3.scope\n", 1001, "/user.slice/user-7.slice"},
		{"", 42, "/user.slice/user-42.slice"},
	}
	for _, c := range cases {
		if got := slicePath(c.cgroup, c.uid); got != c.want {
			t.Errorf("slicePath(%q, %d) = %q, want %q", c.cgroup, c.uid, got, c.want)
		}
	}
	if got := ownCgroup("0::/system.slice/foreman.service\n"); got != "/system.slice/foreman.service" {
		t.Errorf("ownCgroup %q", got)
	}
	if got := ownCgroup("1:name=systemd:/x\n"); got != "" {
		t.Errorf("ownCgroup with no v2 line %q", got)
	}
	root := fixture(t, hostFiles())
	cg, err := readCgroup(root, "/user.slice/user-1001.slice")
	if err != nil {
		t.Fatal(err)
	}
	if cg.Current != 524288000 || cg.Max != 1258291200 || cg.High != 0 || cg.SwapCurrent != 1048576 {
		t.Errorf("cgroup %+v", cg)
	}
	if _, err := readCgroup(root, "/user.slice/user-9.slice"); err == nil {
		t.Error("a missing slice read")
	}
}

func collectDeps(t *testing.T) (Deps, paths.Paths) {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	env.WriteFile(t, "pauses.json", "")
	env.WriteFile(t, "images.json", `{"claude": {"version": "2.1.3", "since": "2026-10-12T03:00:00Z", "previous": "2.1.0"}, "pi": {"version": "0.9.1", "held": "0.9.2"}}`)
	if err := os.MkdirAll(p.Logs, 0o755); err != nil {
		t.Fatal(err)
	}
	env.WriteFile(t, "logs/rebuild-claude.log", "== 2026-10-12T03:00:00Z rebuild claude -> x\nok\n")
	env.WriteFile(t, "logs/factory.log", "[2026-10-12 03:20:00] rebuild claude: ok: 2.1.3 (Claude Code)\n[2026-10-12 03:21:00] tick\n")
	cfg, err := config.Parse([]byte("max_agents = 3\n[hosts.helpera]\nssh = \"factory@helpera\"\nslots = 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	tk := func(host string) *ticket.Ticket {
		x := &ticket.Ticket{}
		x.Agent.Set, x.Agent.V = true, ticket.Agent{Name: "a", Host: host}
		return x
	}
	return Deps{
		Root:   fixture(t, hostFiles()),
		Paths:  p,
		Clock:  testkit.Fixed{T: now},
		UID:    1001,
		Config: cfg,
		Tickets: func() []*ticket.Ticket {
			return []*ticket.Ticket{tk("local"), tk(""), tk("helpera"), {}}
		},
		Pauses:     func() []Pause { return []Pause{{Host: "helpera", Until: now.Add(time.Hour)}} },
		Containers: func(context.Context) ([]string, error) { return []string{"t0001-t1", "t0002-w1"}, nil },
		Statfs:     fakeDisk,
	}, p
}

func TestCollect(t *testing.T) {
	d, p := collectDeps(t)
	s := Collect(t.Context(), d)
	if !s.Taken.Equal(now) || s.Hostname != "hub" || s.Kernel != "6.12.1-arch1" || s.Uptime != 273722 || s.CPUs != 4 {
		t.Errorf("host %+v", s)
	}
	if !slices.Equal(s.Load, []float64{0.42, 0.30, 1.25}) {
		t.Errorf("load %v", s.Load)
	}
	if s.Memory == nil || s.Memory.Total != 16303204*1024 {
		t.Errorf("memory %+v", s.Memory)
	}
	if s.Slice == nil || s.Slice.Path != "/user.slice/user-1001.slice" || s.Slice.Max != 1258291200 {
		t.Errorf("slice %+v", s.Slice)
	}
	if len(s.Disks) != 2 || s.Disks[0].Path != "/" || s.Disks[1].Path != p.Root {
		t.Errorf("disks %+v", s.Disks)
	}
	if s.Temp == nil || s.Temp.Celsius != 51 {
		t.Errorf("temp %+v", s.Temp)
	}
	if !slices.Equal(s.Containers, []string{"t0001-t1", "t0002-w1"}) {
		t.Errorf("containers %v", s.Containers)
	}
	if s.Agents["local"] != 2 || s.Agents["helpera"] != 1 || len(s.Agents) != 2 {
		t.Errorf("agents %v", s.Agents)
	}
	if len(s.Hosts) != 2 || s.Hosts[0].Name != "local" || s.Hosts[0].Slots != 3 || s.Hosts[0].Agents != 2 ||
		s.Hosts[1].Name != "helpera" || s.Hosts[1].Slots != 2 || s.Hosts[1].Agents != 1 || s.Hosts[1].PausedUntil == nil {
		t.Errorf("hosts %+v", s.Hosts)
	}
	if len(s.Pauses) != 1 || s.Pauses[0].Host != "helpera" {
		t.Errorf("pauses %+v", s.Pauses)
	}
	if s.Images["claude"].Version != "2.1.3" || s.Images["pi"].Held != "0.9.2" {
		t.Errorf("images %+v", s.Images)
	}
	if len(s.Rebuilds) != 1 || s.Rebuilds[0].Kind != "claude" || s.Rebuilds[0].Verdict != "ok" || s.Rebuilds[0].Summary != "2.1.3 (Claude Code)" || s.Rebuilds[0].When.IsZero() {
		t.Errorf("rebuilds %+v", s.Rebuilds)
	}
	if s.Version == "" {
		t.Error("no version")
	}
	if len(s.Problems) != 0 {
		t.Errorf("problems %v", s.Problems)
	}
}

// What cannot be read is a problem in the snapshot, never a failure.
func TestCollectWithNothingReadable(t *testing.T) {
	d, _ := collectDeps(t)
	d.Root = t.TempDir()
	d.Containers = func(context.Context) ([]string, error) { return nil, errors.New("podman: not found") }
	d.Statfs = func(string) (Disk, error) { return Disk{}, errors.New("statfs: denied") }
	d.Tickets, d.Pauses, d.Config = nil, nil, nil
	s := Collect(t.Context(), d)
	if s.Memory != nil || s.Load != nil || s.Slice != nil || len(s.Disks) != 0 || s.Temp != nil {
		t.Errorf("read from nothing: %+v", s)
	}
	joined := strings.Join(s.Problems, "\n")
	for _, want := range []string{"podman: not found", "statfs: denied", "meminfo", "loadavg"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems miss %q: %v", want, s.Problems)
		}
	}
	if s.Containers != nil || s.Agents == nil {
		t.Errorf("containers %v agents %v", s.Containers, s.Agents)
	}
}

// podman ps gets a short deadline of its own.
func TestContainersAreBounded(t *testing.T) {
	d, _ := collectDeps(t)
	d.Containers = func(ctx context.Context) ([]string, error) {
		dl, ok := ctx.Deadline()
		if !ok || time.Until(dl) > ContainersTimeout {
			t.Errorf("deadline %v %v", dl, ok)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	s := Collect(t.Context(), d)
	if time.Since(start) > 2*ContainersTimeout || len(s.Problems) != 1 {
		t.Errorf("took %v, problems %v", time.Since(start), s.Problems)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	d, p := collectDeps(t)
	s := Collect(t.Context(), d)
	path := File(p)
	if err := Write(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Taken.Equal(s.Taken) || got.Hostname != s.Hostname || got.Slice.Max != s.Slice.Max || got.Agents["helpera"] != 1 ||
		got.Rebuilds[0].Summary != s.Rebuilds[0].Summary || got.Hosts[1].PausedUntil == nil {
		t.Errorf("round trip:\n%+v\n%+v", got, s)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"taken": "2026-10-18T09:30:00Z"`) {
		t.Errorf("no taken stamp in %s", raw)
	}
	if _, err := Read(filepath.Join(t.TempDir(), "none.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	// never read through a link
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link); err == nil {
		t.Error("read through a link")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{"), 0o644)
	if _, err := Read(bad); err == nil {
		t.Error("a broken file read")
	}
}

// Record writes the snapshot, keeps a rebuild verdict the log has rotated
// away, and turns a slow or failing collection into an error.
func TestRecord(t *testing.T) {
	d, p := collectDeps(t)
	if err := Record(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(File(p)); err != nil {
		t.Fatal(err)
	}
	// factory.log rotated: the verdict is carried over from the last snapshot
	os.WriteFile(p.FactoryLog, []byte("[x] tick\n"), 0o644)
	if err := Record(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	s, _ := Read(File(p))
	if len(s.Rebuilds) != 1 || s.Rebuilds[0].Verdict != "ok" {
		t.Errorf("verdict not carried over: %+v", s.Rebuilds)
	}

	// a path it cannot write
	os.Remove(File(p))
	os.Mkdir(File(p), 0o755)
	os.WriteFile(filepath.Join(File(p), "x"), nil, 0o644)
	if err := Record(t.Context(), d); err == nil {
		t.Error("wrote over a directory")
	}

	// a collection that hangs is cut off
	d.Containers = func(ctx context.Context) ([]string, error) { select {} }
	d.Paths.Root = t.TempDir()
	d.Paths.Logs = filepath.Join(d.Paths.Root, "logs")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := Record(ctx, d); err == nil || !strings.Contains(err.Error(), "took too long") {
		t.Errorf("hang: %v", err)
	}

	// a panic is an error, not a crash
	d.Containers = func(context.Context) ([]string, error) { panic("boom") }
	if err := Record(t.Context(), d); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("panic: %v", err)
	}
}

func TestLive(t *testing.T) {
	root := fixture(t, map[string]string{
		"proc/self/status": "Name:\tforeman\nVmRSS:\t   23456 kB\n",
		"proc/self/cgroup": "0::/system.slice/foreman.service\n",
		"sys/fs/cgroup/system.slice/foreman.service/memory.current": "31457280\n",
		"sys/fs/cgroup/system.slice/foreman.service/memory.max":     "134217728\n",
	})
	p := paths.FromEnv(map[string]string{"HOME": t.TempDir(), "FACTORY_ROOT": "/srv/factory"})
	l := LiveOf(root, p, now, fakeDisk)
	if !l.Taken.Equal(now) || len(l.Disks) != 2 || l.Disks[1].Path != "/srv/factory" {
		t.Errorf("live %+v", l)
	}
	if l.Process.RSS != 23456*1024 || l.Process.GoSys == 0 || l.Process.Cgroup == nil || l.Process.Cgroup.Max != 134217728 ||
		l.Process.Cgroup.Path != "/system.slice/foreman.service" {
		t.Errorf("process %+v %+v", l.Process, l.Process.Cgroup)
	}
	l = LiveOf(t.TempDir(), p, now, func(string) (Disk, error) { return Disk{}, errors.New("no") })
	if len(l.Disks) != 0 || l.Process.RSS != 0 || l.Process.Cgroup != nil || l.Process.GoSys == 0 {
		t.Errorf("live from nothing %+v", l)
	}
}

// System status carries the project switches and the deploy hold, for the
// operator (the manager reads sysinfo.json): a switch overrides config's
// enabled, and a hold turns deploys into Deploy? asks.
func TestSysinfoHasSwitchesAndHold(t *testing.T) {
	d, p := collectDeps(t)
	d.Switches = func() (config.Switches, error) {
		return config.Switches{"alpha": {Enabled: false, By: "maintainer", At: "2026-10-18T09:00:00+00:00"}}, nil
	}
	d.Hold = func() (*deployrec.Hold, error) {
		return &deployrec.Hold{By: "maintainer", At: "2026-10-18T08:00:00+00:00", Note: "travelling"}, nil
	}
	s := Collect(t.Context(), d)
	if sw, ok := s.Switches["alpha"]; !ok || sw.Enabled || sw.By != "maintainer" || len(s.Switches) != 1 {
		t.Errorf("switches %+v", s.Switches)
	}
	if s.DeployHold == nil || s.DeployHold.Note != "travelling" {
		t.Errorf("hold %+v", s.DeployHold)
	}
	if err := Write(File(p), s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(File(p))
	for _, want := range []string{`"switches": {`, `"enabled": false`, `"deploy_hold": {`, `"note": "travelling"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("sysinfo.json lacks %s", want)
		}
	}
	back, err := Read(File(p))
	if err != nil || back.DeployHold == nil || back.Switches["alpha"].By != "maintainer" {
		t.Errorf("read back %+v %v", back, err)
	}

	// none: both left out; what does not read is a problem
	d.Switches = func() (config.Switches, error) { return nil, nil }
	d.Hold = func() (*deployrec.Hold, error) { return nil, nil }
	s = Collect(t.Context(), d)
	if s.Switches != nil || s.DeployHold != nil {
		t.Errorf("none: %+v %+v", s.Switches, s.DeployHold)
	}
	Write(File(p), s)
	b, _ = os.ReadFile(File(p))
	if strings.Contains(string(b), "switches") || strings.Contains(string(b), "deploy_hold") {
		t.Errorf("empty fields written: %s", b)
	}
	d.Switches = func() (config.Switches, error) { return nil, errors.New("projects.json: bad") }
	d.Hold = func() (*deployrec.Hold, error) { return nil, errors.New("deploy-hold.json: bad") }
	s = Collect(t.Context(), d)
	joined := strings.Join(s.Problems, "\n")
	if !strings.Contains(joined, "projects.json: bad") || !strings.Contains(joined, "deploy hold: deploy-hold.json: bad (deploys are held)") {
		t.Errorf("problems %v", s.Problems)
	}
}
