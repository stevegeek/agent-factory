package vitals

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

func machineFiles() map[string]string {
	return map[string]string{
		"proc/sys/kernel/hostname":             "helpera\n",
		"proc/sys/kernel/osrelease":            "6.12.1-arch1\n",
		"proc/uptime":                          "273722.31 1000000.00\n",
		"proc/loadavg":                         "0.42 0.30 1.25 2/345 6789\n",
		"proc/meminfo":                         meminfo,
		"sys/devices/system/cpu/online":        "0-3\n",
		"sys/class/thermal/thermal_zone0/type": "acpitz\n",
		"sys/class/thermal/thermal_zone0/temp": "27800\n",
		"sys/class/thermal/thermal_zone1/type": "x86_pkg_temp\n",
		"sys/class/thermal/thermal_zone1/temp": "51000\n",
	}
}

func fakeDisk(path string) (Disk, error) {
	return Disk{Path: path, Total: 100 << 30, Free: 40 << 30, Used: 60 << 30}, nil
}

func TestParsers(t *testing.T) {
	m, err := parseMeminfo([]byte(meminfo))
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 16303204*1024 || m.Available != 9876544*1024 || m.Used != (16303204-9876544)*1024 ||
		m.SwapTotal != 8388604*1024 || m.SwapUsed != (8388604-8000000)*1024 {
		t.Errorf("meminfo %+v", m)
	}
	if _, err := parseMeminfo([]byte("MemFree: 1 kB\n")); err == nil {
		t.Error("a meminfo with no MemTotal read")
	}
	l, err := parseLoad([]byte("0.42 0.30 1.25 2/345 6789\n"))
	if err != nil || !slices.Equal(l, []float64{0.42, 0.30, 1.25}) {
		t.Errorf("load %v %v", l, err)
	}
	if _, err := parseLoad([]byte("x")); err == nil {
		t.Error("a broken loadavg read")
	}
	if u, err := parseUptime([]byte("273722.31 1000000.00\n")); err != nil || u != 273722 {
		t.Errorf("uptime %d %v", u, err)
	}
	for in, want := range map[string]int{"0-3\n": 4, "0\n": 1, "0-1,4-5,7\n": 5, "": 0, "x-y": 0} {
		if got := parseCPUList(in); got != want {
			t.Errorf("cpus %q = %d, want %d", in, got, want)
		}
	}
}

func TestTemperature(t *testing.T) {
	root := fixture(t, machineFiles())
	tp := readTemp(root)
	if tp == nil || tp.Zone != "x86_pkg_temp" || tp.Celsius != 51 {
		t.Errorf("temp %+v", tp)
	}
	if tp := readTemp(t.TempDir()); tp != nil {
		t.Errorf("no thermal zones, yet %+v", tp)
	}
	only := fixture(t, map[string]string{"sys/class/thermal/thermal_zone3/type": "acpitz\n", "sys/class/thermal/thermal_zone3/temp": "33500\n"})
	if tp := readTemp(only); tp == nil || tp.Zone != "acpitz" || tp.Celsius != 33.5 {
		t.Errorf("the only zone %+v", tp)
	}
}

// Statfs reads a real file system; Disks names each file system once.
func TestStatfsAndDisks(t *testing.T) {
	dir := t.TempDir()
	d, err := Statfs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Total == 0 || d.Used > d.Total || d.Free > d.Total {
		t.Errorf("statfs %+v", d)
	}
	if _, err := Statfs(filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Error("statfs of nothing")
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	var problems []string
	ds := Disks(nil, []string{dir, "", sub, filepath.Join(dir, "gone")}, func(what string, err error) { problems = append(problems, what) })
	if len(ds) != 1 || ds[0].Path != dir || len(problems) != 1 {
		t.Errorf("disks %+v problems %v", ds, problems)
	}
}

func TestCollect(t *testing.T) {
	d := Deps{Root: fixture(t, machineFiles()), Now: func() time.Time { return now }, DiskPaths: []string{"/", "/srv/factory"},
		Statfs: fakeDisk, Containers: func(context.Context) ([]string, error) { return []string{"factory-job-t0001-w1"}, nil }}
	v := Collect(t.Context(), d)
	if !v.Taken.Equal(now) || v.Hostname != "helpera" || v.Kernel != "6.12.1-arch1" || v.Uptime != 273722 || v.CPUs != 4 || v.Version == "" {
		t.Errorf("machine %+v", v)
	}
	if !slices.Equal(v.Load, []float64{0.42, 0.30, 1.25}) || v.Memory == nil || v.Memory.Total != 16303204*1024 ||
		v.Temp == nil || v.Temp.Celsius != 51 || len(v.Disks) != 2 || v.Disks[1].Path != "/srv/factory" {
		t.Errorf("vitals %+v", v)
	}
	if !slices.Equal(v.Containers, []string{"factory-job-t0001-w1"}) || len(v.Problems) != 0 {
		t.Errorf("containers %v problems %v", v.Containers, v.Problems)
	}
	// nothing to read, nothing asked
	v = Collect(t.Context(), Deps{Root: t.TempDir(), Statfs: func(string) (Disk, error) { return Disk{}, errors.New("denied") }, DiskPaths: []string{"/"}})
	if v.Memory != nil || v.Load != nil || v.Disks != nil || v.Containers != nil || len(v.Problems) != 3 {
		t.Errorf("from nothing %+v", v)
	}
	// podman that does not answer in time
	v = Collect(t.Context(), Deps{Root: t.TempDir(), Containers: func(ctx context.Context) ([]string, error) {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > ContainersTimeout {
			t.Errorf("deadline %v %v", dl, ok)
		}
		return nil, errors.New("podman: not found")
	}})
	if v.Containers != nil || !strings.Contains(strings.Join(v.Problems, "\n"), "podman: not found") {
		t.Errorf("no podman %+v", v)
	}
}

// What a machine sends round-trips through Parse; Received moves its
// clock to Reported.
func TestEncodeParseReceived(t *testing.T) {
	v := Collect(t.Context(), Deps{Root: fixture(t, machineFiles()), Now: func() time.Time { return now }, DiskPaths: []string{"/"}, Statfs: fakeDisk})
	b, err := v.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Taken.Equal(now) || got.Hostname != "helpera" || got.Memory.Used != v.Memory.Used || len(got.Disks) != 1 || got.Containers != nil {
		t.Errorf("round trip %+v", got)
	}
	later := now.Add(time.Hour)
	r := got.Received(later)
	if !r.Taken.Equal(later) || !r.Reported.Equal(now) {
		t.Errorf("received %v %v", r.Taken, r.Reported)
	}
	if b, _ := got.Encode(); strings.Contains(string(b), "reported") {
		t.Errorf("a zero Reported is written: %s", b)
	}
	// an empty list of containers is not "not asked"
	if got, err := Parse([]byte(`{"containers": []}`)); err != nil || got.Containers == nil {
		t.Errorf("empty containers %v %v", got.Containers, err)
	}
	// strings are trimmed and cut at a rune
	long := strings.Repeat("é", 150)
	got, err = Parse(fmt.Appendf(nil, `{"hostname": "  helpera  ", "problems": [%q]}`, long))
	if err != nil || got.Hostname != "helpera" || len(got.Problems[0]) != 200 || !strings.HasPrefix(long, got.Problems[0]) {
		t.Errorf("clean %q %d %v", got.Hostname, len(got.Problems), err)
	}
	// image versions, cleaned like every string; vitals with none (an
	// older helper's) have none
	got, err = Parse([]byte(`{"images": {" pi ": {"version": "0.9.1 ", "held": "0.9.2"}}}`))
	if err != nil || len(got.Images) != 1 || got.Images["pi"] != (Image{Version: "0.9.1", Held: "0.9.2"}) {
		t.Errorf("images %+v %v", got.Images, err)
	}
	if got, err := Parse([]byte(`{"hostname": "helpera"}`)); err != nil || got.Images != nil || got.Workers != nil {
		t.Errorf("no images or workers %+v %+v %v", got.Images, got.Workers, err)
	}
	// a pull host's workers, its note cleaned like every string
	got, err = Parse([]byte(`{"workers": {"up": true, "running": 3, "free": 1, "note": " fw2 stopped "}}`))
	if err != nil || got.Workers == nil || *got.Workers != (Workers{Up: true, Running: 3, Free: 1, Note: "fw2 stopped"}) {
		t.Errorf("workers %+v %v", got.Workers, err)
	}
}

// manyImages is n more images.json entries, each with a comma before it.
func manyImages(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `, "kind%d": {"version": "1"}`, i)
	}
	return b.String()
}

// What a worker sends is untrusted: anything out of bounds refuses it.
func TestParseRefuses(t *testing.T) {
	many := func(key, item string, n int) string {
		return fmt.Sprintf(`{%q: [%s]}`, key, strings.TrimSuffix(strings.Repeat(item+",", n), ","))
	}
	for name, in := range map[string]string{
		"not json":         `{"hostname": `,
		"not an object":    `[1, 2]`,
		"wrong type":       `{"cpus": "four"}`,
		"too big":          `{"hostname": "` + strings.Repeat("x", MaxBytes) + `"}`,
		"four loads":       `{"load": [1, 2, 3, 4]}`,
		"negative load":    `{"load": [-1, 0, 0]}`,
		"huge load":        `{"load": [1e9, 0, 0]}`,
		"cpus":             `{"cpus": -2}`,
		"uptime":           `{"uptime_seconds": -5}`,
		"memory":           `{"memory": {"total": 10, "used": 11}}`,
		"swap":             `{"memory": {"total": 10, "swap_total": 1, "swap_used": 2}}`,
		"disk":             `{"disks": [{"path": "/", "total": 1, "free": 2}]}`,
		"disks":            many("disks", `{"path": "/"}`, 17),
		"containers":       many("containers", `"c"`, 65),
		"problems":         many("problems", `"p"`, 33),
		"temp":             `{"temp": {"zone": "x", "celsius": 900}}`,
		"control hostname": `{"hostname": "helpera\u001b[31m"}`,
		"newline problem":  `{"problems": ["a\nb"]}`,
		"control disk":     `{"disks": [{"path": "/\u0000"}]}`,
		"control version":  `{"version": "x\ty"}`,
		"control image":    `{"images": {"pi": {"version": "0.9\u001b"}}}`,
		"control kind":     `{"images": {"p\ni": {"version": "1"}}}`,
		"images":           `{"images": {"k": {}` + manyImages(16) + `}}`,
		"workers free":     `{"workers": {"up": true, "running": 1, "free": 2}}`,
		"workers negative": `{"workers": {"running": -1}}`,
		"workers note":     `{"workers": {"note": "a\nb"}}`,
	} {
		if v, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: taken as %+v", name, v)
		}
	}
	if _, err := Parse([]byte(many("containers", `"c"`, 64))); err != nil {
		t.Errorf("64 containers: %v", err)
	}
}
