package web

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

func snapshotAt(taken time.Time) sysinfo.Snapshot {
	until := testNow.Add(2 * time.Hour)
	return sysinfo.Snapshot{
		Taken: taken, Version: "abc123 go1.26", Hostname: "hub", Kernel: "6.12.1", Uptime: 3 * 86400,
		Load: []float64{0.42, 0.30, 0.25}, CPUs: 4,
		Memory:     &sysinfo.Memory{Total: 16 << 30, Available: 2 << 30, Used: 14 << 30, SwapTotal: 8 << 30, SwapUsed: 1 << 30},
		Slice:      &sysinfo.Cgroup{Path: "/user.slice/user-1001.slice", Current: 500 << 20, Max: 1200 << 20},
		Disks:      []sysinfo.Disk{{Path: "/", Total: 100 << 30, Free: 5 << 30, Used: 95 << 30}},
		Temp:       &sysinfo.Temp{Zone: "x86_pkg_temp", Celsius: 51},
		Containers: []string{"t0007-w1"},
		Agents:     map[string]int{"local": 1},
		Hosts:      []sysinfo.Host{{Name: "local", Slots: 2, Agents: 1}, {Name: "helpera", Slots: 1, PausedUntil: &until}},
		Images:     map[string]sysinfo.Image{"claude": {Version: "2.1.3", Previous: "2.1.0"}},
		Rebuilds:   []sysinfo.Rebuild{{Kind: "pi", When: testNow.Add(-50 * time.Hour), Verdict: "FAILED", Summary: "smoke test failed"}},
		Problems:   []string{"podman ps: exit 125"},
	}
}

func (h *harness) writeSnapshot(s sysinfo.Snapshot) {
	h.t.Helper()
	if err := sysinfo.Write(sysinfo.File(h.paths), s); err != nil {
		h.t.Fatal(err)
	}
}

func TestSystemCardFresh(t *testing.T) {
	h := newHarness(t, false)
	h.writeSnapshot(snapshotAt(testNow.Add(-3 * time.Minute)))
	page := h.do("GET", "/system", nil, h.login()).Body.String()
	for _, want := range []string{
		`<h2 class="page-h">System status</h2>`, `As of <time title="9 Oct 11:57">3 min ago</time>.`,
		"hub · Linux 6.12.1 · up 3 days", "0.42 · 0.30 · 0.25 on 4 CPUs", "51 °C",
		"factory abc123 go1.26 · Foreman ",
		"Memory", "14.0 GiB of 16.0 GiB · 88%", `<meter class="gauge" min="0" max="100" low="75" high="90" optimum="0" value="88">`,
		"Swap", "The factory&#39;s slice", "500 MiB of its 1.2 GiB limit · 42%",
		"Disk /", `<span class="lvl-bad">5.0 GiB free of 100 GiB · 95%`,
		"<b>local</b> <span class=\"busy\">1 of 2 agents</span>", "<b>helpera</b>", "paused until 9 Oct 14:00",
		"t0007-w1", "claude 2.1.3 (previous 2.1.0)",
		`<b>pi</b> 2 days ago <span class="out failed">FAILED</span> <span class="muted">smoke test failed</span>`,
		"1 thing the tick could not read", "podman ps: exit 125",
		"Seen by Foreman now", "Foreman&#39;s memory", `href="/system.json"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the System card lacks %q", want)
		}
	}
	if strings.Contains(page, "the tick is not running") {
		t.Error("a fresh snapshot called stale")
	}
}

func TestSystemCardStale(t *testing.T) {
	h := newHarness(t, false)
	h.writeSnapshot(snapshotAt(testNow.Add(-40 * time.Minute)))
	page := h.do("GET", "/system", nil, h.login()).Body.String()
	if !strings.Contains(page, `<p class="sys-age stale">As of <time title="9 Oct 11:20">40 min ago</time>: older than 15 minutes, so the tick is not running.</p>`) {
		t.Errorf("stale snapshot not flagged:\n%s", page)
	}
}

func TestSystemCardMissingOrBroken(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	page := h.do("GET", "/system", nil, c).Body.String()
	if !strings.Contains(page, "No snapshot yet") || !strings.Contains(page, "Seen by Foreman now") {
		t.Errorf("missing snapshot:\n%s", page)
	}
	os.WriteFile(sysinfo.File(h.paths), []byte("{not json"), 0o644)
	page = h.do("GET", "/system", nil, c).Body.String()
	if !strings.Contains(page, "The tick's snapshot could not be read") {
		t.Errorf("broken snapshot:\n%s", page)
	}
	// podman that did not answer is not "none running"
	s := snapshotAt(testNow)
	s.Containers = nil
	h.writeSnapshot(s)
	page = h.do("GET", "/system", nil, c).Body.String()
	if !strings.Contains(page, "podman did not answer.") || !strings.Contains(page, "As of <time title=\"9 Oct 12:00\">just now</time>.") {
		t.Errorf("no podman:\n%s", page)
	}
	s.Containers = []string{}
	h.writeSnapshot(s)
	if page = h.do("GET", "/system", nil, c).Body.String(); !strings.Contains(page, "None running.") {
		t.Errorf("no containers")
	}
}

// Only the maintainer sees the host: other people get neither the card
// nor the JSON.
func TestSystemIsTheMaintainers(t *testing.T) {
	h := newHarness(t, false)
	h.writeSnapshot(snapshotAt(testNow.Add(-time.Minute)))
	m := h.login()
	secret, _ := h.users.Add("robin", []string{"factory"})
	robin := h.loginWith(secret, "/")
	page := h.do("GET", "/settings", nil, robin).Body.String()
	if !strings.Contains(page, "Notifications") || strings.Contains(page, "System") || strings.Contains(page, "hub") || strings.Contains(page, `href="/logs"`) {
		t.Errorf("robin's settings or menu show the system or the logs")
	}
	for _, p := range []string{"/system", "/system.json", "/logs"} {
		if w := h.do("GET", p, nil, robin); w.Code != 404 || strings.Contains(w.Body.String(), "hub") {
			t.Errorf("robin's %s: %d", p, w.Code)
		}
	}
	if mine := h.do("GET", "/settings", nil, m).Body.String(); strings.Contains(mine, `id="system"`) || !strings.Contains(mine, `<a href="/system" >System status</a>`) || !strings.Contains(mine, `<a href="/logs" >Logs</a>`) {
		t.Errorf("the maintainer's settings still hold the system card, or the menu lacks System status and Logs")
	}
	if w := h.do("GET", "/settings/system.json", nil, robin); w.Code != 404 || strings.Contains(w.Body.String(), "hub") {
		t.Errorf("robin's system.json: %d %s", w.Code, w.Body.String())
	}
	if w := h.do("GET", "/settings/system.json", nil); w.Code == 200 {
		t.Errorf("system.json without a login: %d", w.Code)
	}
	w := h.do("GET", "/settings/system.json", nil, m)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("maintainer's system.json: %d", w.Code)
	}
	var got struct {
		Snapshot *sysinfo.Snapshot `json:"snapshot"`
		Age      int               `json:"age_seconds"`
		Stale    bool              `json:"stale"`
		Live     sysinfo.Live      `json:"live"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Snapshot == nil || got.Snapshot.Hostname != "hub" || got.Age != 60 || got.Stale || got.Live.Process.GoSys == 0 {
		t.Errorf("system.json %s", w.Body.String())
	}
	os.Remove(sysinfo.File(h.paths))
	w = h.do("GET", "/settings/system.json", nil, m)
	if !strings.Contains(w.Body.String(), `"snapshot":null`) || !strings.Contains(w.Body.String(), `"stale":true`) {
		t.Errorf("no snapshot: %s", w.Body.String())
	}
}

// Each worker host has a block: its vitals and their age, its last ones
// with the error when it gave none this pass, or why there are none.
func TestSystemCardWorkerHosts(t *testing.T) {
	h := newHarness(t, false)
	s := snapshotAt(testNow.Add(-time.Minute))
	s.Hosts[1].Vitals = &vitals.Vitals{
		Taken: testNow.Add(-4 * time.Minute), Hostname: "helpera-box", Kernel: "6.6.0", Uptime: 2 * 86400, Version: "a5b5d0003dd6 go1.27.1",
		Load: []float64{3.5, 2, 1}, CPUs: 2, Temp: &sysinfo.Temp{Celsius: 88},
		Memory:     &sysinfo.Memory{Total: 8 << 30, Used: 2 << 30, Available: 6 << 30},
		Disks:      []sysinfo.Disk{{Path: "/srv/factory", Total: 50 << 30, Free: 25 << 30, Used: 25 << 30}},
		Containers: []string{"factory-job-t0007-w1"},
		Images:     map[string]sysinfo.Image{"pi": {Version: "0.9.1", Held: "0.9.2"}, "claude": {Version: "2.1.3"}},
		Problems:   []string{"loadavg: denied"},
	}
	s.Hosts = append(s.Hosts,
		sysinfo.Host{Name: "desktop", Slots: 3, Pull: true, VitalsError: "factory@desktop vitals timed out",
			Vitals: &vitals.Vitals{Taken: testNow.Add(-time.Hour), Hostname: "desk", Disks: []sysinfo.Disk{{Path: "/", Total: 10, Free: 5, Used: 5}}}},
		sysinfo.Host{Name: "laptop", Slots: 1, Pull: true, VitalsError: "no report yet"},
		sysinfo.Host{Name: "spare", Slots: 1})
	h.writeSnapshot(s)
	page := h.do("GET", "/system", nil, h.login()).Body.String()
	for _, want := range []string{
		`<h4>helpera</h4>`, `<dt>Version</dt><dd class="mono">a5b5d0003dd6 go1.27.1</dd>`, `Reported <time title="9 Oct 11:56">4 min ago</time>.`, "helpera-box · Linux 6.6.0 · up 2 days",
		`<dd class="lvl-warn">3.50 · 2.00 · 1.00 on 2 CPUs</dd>`, `<dd class="lvl-bad">88 °C</dd>`,
		"2.0 GiB of 8.0 GiB · 25%", "Disk /srv/factory", "25.0 GiB free of 50.0 GiB",
		"<dd>1 running</dd>", "factory-job-t0007-w1", "<dt>Images</dt><dd>claude 2.1.3 · pi 0.9.1 (holding back 0.9.2)</dd>", "1 thing it could not read", "loadavg: denied",
		`<h4>desktop</h4>`, `<p class="sys-age stale">Reported <time title="9 Oct 11:00">1 h ago</time>: older than 15 minutes.</p>`,
		"No report this pass: factory@desktop vitals timed out; this is its last.",
		`<h4>laptop</h4>`, "No report: no report yet",
		`<h4>spare</h4>`, "No report yet.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the worker blocks lack %q", want)
		}
	}
	if strings.Contains(page, "<h4>local</h4>") || strings.Contains(page, "Factory disk /srv/factory") {
		t.Error("the dispatcher has a worker block, or a worker's disk is called the factory's")
	}
	// the JSON has them as the tick wrote them
	w := h.do("GET", "/settings/system.json", nil, h.login())
	if !strings.Contains(w.Body.String(), `"hostname":"helpera-box"`) || !strings.Contains(w.Body.String(), `"vitals_error":"no report yet"`) {
		t.Errorf("system.json lacks the hosts' vitals: %s", w.Body.String())
	}
}

func TestSystemFormatting(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 512: "512 B", 1536: "1.5 KiB", 800 << 20: "800 MiB", 15 << 30: "15.0 GiB", 200 << 30: "200 GiB"} {
		if got := bytesOf(n); got != want {
			t.Errorf("bytesOf(%d) = %q, want %q", n, got, want)
		}
	}
	for _, c := range []struct {
		v    int
		want string
	}{{10, ""}, {75, "warn"}, {90, "warn"}, {91, "bad"}} {
		if got := level(c.v, 75, 90); got != c.want {
			t.Errorf("level(%d) = %q", c.v, got)
		}
	}
	if b := bar("x", 1, 0, 75, 90, ""); b.Pct != 0 || b.Lvl != "" {
		t.Errorf("a bar of nothing %+v", b)
	}
}

// The summary above the details has every configured host: whether it
// reports and whether it runs agents.
func TestSystemSummaryOfMachines(t *testing.T) {
	h := newHarness(t, false)
	s := snapshotAt(testNow.Add(-time.Minute))
	s.Hosts[1].Vitals = &vitals.Vitals{Taken: testNow.Add(-4 * time.Minute)}
	s.Hosts = append(s.Hosts,
		sysinfo.Host{Name: "desktop", Slots: 3, Agents: 2, VitalsError: "vitals timed out", Vitals: &vitals.Vitals{Taken: testNow.Add(-time.Hour)}},
		sysinfo.Host{Name: "laptop", Slots: 1, VitalsError: "no report yet"},
		sysinfo.Host{Name: "spare", Slots: 1})
	h.writeSnapshot(s)
	page := h.do("GET", "/system", nil, h.login()).Body.String()
	for _, want := range []string{
		`<li class="machine online"><b>local</b> <span class="out merged">online</span> <span class="out merged">working</span> <span class="muted">1 of 2 agents</span></li>`,
		`<li class="machine online"><b>helpera</b> <span class="out merged">online</span> <span class="out waiting">paused</span> <span class="muted">0 of 1 agents</span></li>`,
		`<li class="machine offline"><b>desktop</b> <span class="out failed">offline</span> <span class="out merged">working</span> <span class="muted">2 of 3 agents</span></li>`,
		`<li class="machine offline"><b>laptop</b> <span class="out failed">offline</span> <span class="out info">idle</span>`,
		`<li class="machine unknown"><b>spare</b> <span class="out info">unknown</span> <span class="out info">idle</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the summary lacks %q", want)
		}
	}
	if sum, details := strings.Index(page, `class="machines"`), strings.Index(page, `class="sys-age`); sum < 0 || sum > details {
		t.Error("the summary is not above the details")
	}

	s.Taken = testNow.Add(-40 * time.Minute)
	if got := machines(s, false, testNow); got[0].State != "offline" {
		t.Errorf("a stale snapshot leaves the dispatcher %q", got[0].State)
	}
	s.Hosts[1].Vitals.Taken = testNow.Add(-20 * time.Minute)
	if got := machines(s, true, testNow); got[1].State != "offline" {
		t.Errorf("stale vitals leave a worker host %q", got[1].State)
	}
}
