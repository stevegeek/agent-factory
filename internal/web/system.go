package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
)

// The System card on the Settings page, the maintainer's only. Foreman's
// sandbox hides the host's /proc and cannot reach a worker host, so the
// tick writes what it sees, and each worker host's vitals, to sysinfo.json
// at the end of every pass and this reads it; disks and Foreman's own
// memory it reads live.

// StaleAfter is the age past which the snapshot says the tick is not
// running (it runs every 5 minutes).
const StaleAfter = 15 * time.Minute

// SystemView is the System card.
type SystemView struct {
	Missing    bool   // the tick has not written a snapshot yet
	Error      string // it could not be read
	Age        string // "3 min ago"
	Taken      string
	Stale      bool
	Host       string // "hub · Linux 6.12.1 · up 3 days"
	Load       string
	LoadLvl    string
	Temp       string
	TempLvl    string
	Bars       []Bar
	Machines   []Machine // the summary at the top: every configured host
	Hosts      []HostRow
	Workers    []WorkerView // the worker hosts' vitals, one block each
	Containers []string
	Podman     bool // podman answered (no containers is then none running)
	Images     []string
	Rebuilds   []RebuildRow
	Problems   []string
	Versions   string
	Live       []Bar // read by Foreman now
	Deploys    DeploysView
}

// DeploysView is the Deploys card: the hold, and the running or last
// self-deploy (deploys/state.json, which the runner writes).
type DeploysView struct {
	Held     bool
	Hold     string // "Held by alice since 9 Oct 10:00: a demo", or that merges deploy by themselves
	CanHold  bool   // the hold form: this server can switch it
	Error    string // the state did not read
	Running  bool
	Deploy   *DeployRow // nil: no deploy yet
	Waiting  string     // a request waiting for the runner
	WaitedAt string
}

// DeployRow is one deploy on the card.
type DeployRow struct {
	Rev, Commit, ConfigCommit, From, Step, Result string
	Started, Finished                             string // "9 Oct 11:50"
	Failed                                        bool
	Helpers                                       []HelperRow
	Tail                                          string
}

// HelperRow is one helper's install outcome.
type HelperRow struct {
	Name, Outcome string
	OK            bool
}

// deploysView reads the hold and the deploy state.
func (s *Server) deploysView() DeploysView {
	v := DeploysView{CanHold: s.svc.Deploys != nil, Hold: "Merges deploy by themselves."}
	h, held, err := deployrec.ReadHold(s.paths.DeployHold())
	switch {
	case err != nil:
		v.Held, v.Hold = true, "Held: the hold does not read ("+err.Error()+")."
	case held:
		v.Held, v.Hold = true, "Held by "+orDash(h.By)+" since "+stampLine(h.At)+colonNote(h.Note)
	}
	st, err := deployrec.ReadState(s.paths.DeployState())
	if err != nil {
		v.Error = err.Error()
	} else if st.Phase != "" {
		v.Running = st.Phase == deployrec.PhaseRunning
		row := &DeployRow{Rev: st.Rev, Commit: orDash(st.Commit), ConfigCommit: orDash(st.ConfigCommit), From: orDash(st.From),
			Step: st.Step, Result: st.Result, Started: stampLine(st.Started), Finished: stampLine(st.Finished),
			Failed: st.Phase == deployrec.PhaseDone && st.Result != deployrec.ResultOK, Tail: st.Tail}
		names := make([]string, 0, len(st.Helpers))
		for n := range st.Helpers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			row.Helpers = append(row.Helpers, HelperRow{Name: n, Outcome: st.Helpers[n], OK: st.Helpers[n] == "ok"})
		}
		if !row.Failed {
			row.Tail = ""
		}
		v.Deploy = row
	}
	if r, ok, err := deployrec.ReadRequest(s.paths.DeployRequest()); err == nil && ok {
		v.Waiting, v.WaitedAt = r.Rev, stampLine(r.At)
	}
	return v
}

// stampLine is a stamp as the card shows it, "9 Oct 11:50", or the stamp
// itself when it does not parse.
func stampLine(stamp string) string {
	if t := present.ParseTime(stamp); !t.IsZero() {
		return t.Local().Format("2 Jan 15:04")
	}
	return orDash(stamp)
}

func colonNote(note string) string {
	if note == "" {
		return "."
	}
	return ": " + note
}

// holdDeploys is the Deploys card's form: held is "yes" or "no".
func (s *Server) holdDeploys(w http.ResponseWriter, r *http.Request) {
	if s.svc.Deploys == nil {
		s.flashRedirect(w, r, "/system", "error", "Holding deploys is off on this server.")
		return
	}
	var held bool
	switch r.PostFormValue("held") {
	case "yes":
		held = true
	case "no":
	default:
		http.Error(w, "held is yes or no", http.StatusBadRequest)
		return
	}
	note := r.PostFormValue("note")
	var line string
	err := s.call(r, fileTimeout, func(ctx context.Context) (err error) {
		line, err = s.svc.Deploys.HoldDeploys(ctx, held, note, s.actor(r))
		return err
	})
	if err != nil {
		s.logf("deploy hold %v by %s: %v", held, s.who(r), err)
		s.flashRedirect(w, r, "/system", "error", sentence(err.Error()))
		return
	}
	s.flashRedirect(w, r, "/system", "ok", sentence(line))
}

// Bar is one meter: Pct of the whole, coloured past Low and High.
type Bar struct {
	Label, Detail string
	Pct           int
	Low, High     int
	Lvl           string // "", "warn" or "bad"
}

// HostRow is one host's agents and slots.
type HostRow struct {
	Name, Agents, Paused string
	Busy                 bool
}

// Machine is one host in the summary: whether it reports and whether it
// runs agents.
type Machine struct {
	Name   string
	State  string // "online", "offline" or "unknown" (no report yet)
	Work   string // "working", "paused" or "idle"
	Agents string // "1 of 2 agents"
}

// WorkerView is one worker host's vitals on the System card.
type WorkerView struct {
	Name     string
	Reported bool   // it has vitals (this pass's, or its last)
	Age      string // "3 min ago"
	Taken    string
	Stale    bool
	Host     string // "helpera · Linux 6.12.1 · up 3 days"
	Version  string // the factory-remote (or factory-pull) that took the vitals: "<commit> <go>"
	Load     string
	LoadLvl  string
	Temp     string
	TempLvl  string
	Bars     []Bar
	// Containers are its running containers; Podman is false when it was
	// not asked (a pull host) or did not answer.
	Containers []string
	Podman     bool
	Images     []string // its images.json, as Images on the card
	Problems   []string
	Error      string // why there are no vitals this pass
}

// RebuildRow is the last rebuild of one image.
type RebuildRow struct {
	Kind, When, Verdict, Summary string
	Failed                       bool
}

func bar(label string, used, total uint64, low, high int, detail string) Bar {
	pct := 0
	if total > 0 {
		pct = int((used*100 + total/2) / total)
	}
	pct = min(pct, 100)
	if detail == "" {
		detail = fmt.Sprintf("%s of %s", bytesOf(used), bytesOf(total))
	}
	return Bar{Label: label, Detail: fmt.Sprintf("%s · %d%%", detail, pct), Pct: pct, Low: low, High: high, Lvl: level(pct, low, high)}
}

// level matches a <meter>'s regions with optimum below low: warn from
// low, bad above high.
func level(v, warn, bad int) string {
	switch {
	case v > bad:
		return "bad"
	case v >= warn:
		return "warn"
	}
	return ""
}

// bytesOf is a size in binary units: "812 MiB", "6.1 GiB".
func bytesOf(n uint64) string {
	const k = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v, i := float64(n), 0
	for v >= k && i < len(units)-1 {
		v /= k
		i++
	}
	if i == 0 || v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func diskBars(ds []sysinfo.Disk, root string) []Bar {
	var out []Bar
	for _, d := range ds {
		label := "Disk " + d.Path
		if d.Path == root {
			label = "Factory disk " + d.Path
		}
		out = append(out, bar(label, d.Used, d.Used+d.Free, 80, 90, fmt.Sprintf("%s free of %s", bytesOf(d.Free), bytesOf(d.Total))))
	}
	return out
}

// systemView reads the snapshot and what Foreman sees live.
func (s *Server) systemView() *SystemView {
	now := s.now()
	v := &SystemView{Deploys: s.deploysView()}
	snap, err := sysinfo.Read(sysinfo.File(s.paths))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		v.Missing = true
	case err != nil:
		v.Error = err.Error()
	default:
		fillSnapshot(v, snap, now, s.paths.Root)
	}
	live := sysinfo.LiveOf("", s.paths, now, nil)
	v.Live = diskBars(live.Disks, s.paths.Root)
	p := live.Process
	if cg := p.Cgroup; cg != nil && cg.Max > 0 {
		v.Live = append(v.Live, bar("Foreman's memory", cg.Current, cg.Max, 75, 90, fmt.Sprintf("%s of its %s limit", bytesOf(cg.Current), bytesOf(cg.Max))))
	} else {
		detail := "Go runtime " + bytesOf(p.GoSys)
		if p.RSS > 0 {
			detail = bytesOf(p.RSS) + " resident · " + detail
		}
		v.Live = append(v.Live, Bar{Label: "Foreman's memory", Detail: detail})
	}
	return v
}

func fillSnapshot(v *SystemView, s sysinfo.Snapshot, now time.Time, root string) {
	age := now.Sub(s.Taken)
	v.Taken = s.Taken.Local().Format("2 Jan 15:04")
	v.Age = present.Age(max(age, 0))
	if v.Age != "just now" {
		v.Age += " ago"
	}
	v.Stale = age > StaleAfter
	v.Host = hostLine(s.Hostname, s.Kernel, s.Uptime)
	v.Load, v.LoadLvl = loadLine(s.Load, s.CPUs)
	v.Temp, v.TempLvl = tempLine(s.Temp)
	v.Bars = memoryBars(s.Memory)
	if cg := s.Slice; cg != nil {
		limit, what := cg.Max, "limit"
		if cg.High > 0 && (limit == 0 || cg.High < limit) {
			limit, what = cg.High, "soft limit"
		}
		if limit > 0 {
			detail := fmt.Sprintf("%s of its %s %s", bytesOf(cg.Current), bytesOf(limit), what)
			if cg.SwapCurrent > 0 {
				detail += ", " + bytesOf(cg.SwapCurrent) + " swapped"
			}
			v.Bars = append(v.Bars, bar("The factory's slice", cg.Current, limit, 75, 90, detail))
		} else {
			v.Bars = append(v.Bars, Bar{Label: "The factory's slice", Detail: bytesOf(cg.Current) + ", no limit"})
		}
	}
	v.Bars = append(v.Bars, diskBars(s.Disks, root)...)
	v.Machines = machines(s, !v.Stale, now)
	v.Hosts = hostRows(s, now)
	v.Workers = workerViews(s.Hosts, now)
	v.Containers, v.Podman = s.Containers, s.Containers != nil
	v.Images = imageLines(s.Images)
	for _, r := range s.Rebuilds {
		row := RebuildRow{Kind: r.Kind, Verdict: r.Verdict, Summary: r.Summary, Failed: r.Verdict == "FAILED"}
		if !r.When.IsZero() {
			row.When = present.Age(now.Sub(r.When)) + " ago"
		}
		v.Rebuilds = append(v.Rebuilds, row)
	}
	v.Problems = s.Problems
	v.Versions = "factory " + orDash(s.Version) + " · Foreman " + buildinfo.String()
}

// imageLines are a host's images.json entries, by kind: "claude 2.1.3
// (since 3 Oct; previous 2.1.0)".
func imageLines(images map[string]sysinfo.Image) []string {
	kinds := make([]string, 0, len(images))
	for k := range images {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var out []string
	for _, k := range kinds {
		im := images[k]
		line := k + " " + orDash(im.Version)
		var more []string
		if t := present.ParseTime(im.Since); !t.IsZero() {
			more = append(more, "since "+t.Local().Format("2 Jan"))
		}
		if im.Previous != "" {
			more = append(more, "previous "+im.Previous)
		}
		if im.Held != "" {
			more = append(more, "holding back "+im.Held)
		}
		if len(more) > 0 {
			line += " (" + strings.Join(more, "; ") + ")"
		}
		out = append(out, line)
	}
	return out
}

// hostLine is "hub · Linux 6.12.1 · up 3 days", leaving out what is not
// known.
func hostLine(hostname, kernel string, uptime int64) string {
	var host []string
	for _, p := range []string{hostname, prefixed("Linux ", kernel)} {
		if p != "" {
			host = append(host, p)
		}
	}
	if uptime > 0 {
		host = append(host, "up "+present.Age(time.Duration(uptime)*time.Second))
	}
	return strings.Join(host, " · ")
}

// loadLine is the three load averages and, with the CPUs known, how hard
// the last minute's is on them.
func loadLine(load []float64, cpus int) (string, string) {
	if len(load) != 3 {
		return "", ""
	}
	line := fmt.Sprintf("%.2f · %.2f · %.2f", load[0], load[1], load[2])
	if cpus <= 0 {
		return line, ""
	}
	return line + fmt.Sprintf(" on %d CPUs", cpus), level(int(load[0]*100/float64(cpus)), 100, 200)
}

func tempLine(t *sysinfo.Temp) (string, string) {
	if t == nil {
		return "", ""
	}
	return fmt.Sprintf("%.0f °C", t.Celsius), level(int(t.Celsius), 70, 85)
}

func memoryBars(m *sysinfo.Memory) []Bar {
	if m == nil {
		return nil
	}
	out := []Bar{bar("Memory", m.Used, m.Total, 75, 90, "")}
	if m.SwapTotal > 0 {
		out = append(out, bar("Swap", m.SwapUsed, m.SwapTotal, 50, 80, ""))
	}
	return out
}

// workerViews are the worker hosts' blocks: every configured host but the
// dispatcher, in config order.
func workerViews(hosts []sysinfo.Host, now time.Time) []WorkerView {
	var out []WorkerView
	for _, h := range hosts {
		if h.Name == model.LocalHost {
			continue
		}
		w := WorkerView{Name: h.Name, Error: h.VitalsError}
		if v := h.Vitals; v != nil {
			age := now.Sub(v.Taken)
			w.Reported, w.Stale = true, age > StaleAfter
			w.Taken = v.Taken.Local().Format("2 Jan 15:04")
			w.Age = present.Age(max(age, 0))
			if w.Age != "just now" {
				w.Age += " ago"
			}
			w.Host = hostLine(v.Hostname, v.Kernel, v.Uptime)
			w.Version = v.Version
			w.Load, w.LoadLvl = loadLine(v.Load, v.CPUs)
			w.Temp, w.TempLvl = tempLine(v.Temp)
			w.Bars = append(memoryBars(v.Memory), diskBars(v.Disks, "")...)
			w.Containers, w.Podman = v.Containers, v.Containers != nil
			w.Images = imageLines(v.Images)
			w.Problems = v.Problems
		}
		out = append(out, w)
	}
	return out
}

// machines are the configured hosts in config order. The dispatcher is
// online while its snapshot is fresh; a worker host while its vitals are
// fresh and it answered this pass.
func machines(s sysinfo.Snapshot, fresh bool, now time.Time) []Machine {
	var out []Machine
	for _, h := range s.Hosts {
		m := Machine{Name: h.Name, State: "offline", Work: "idle", Agents: fmt.Sprintf("%d of %d agents", h.Agents, h.Slots)}
		switch {
		case h.Name == model.LocalHost:
			if fresh {
				m.State = "online"
			}
		case h.Vitals != nil:
			if h.VitalsError == "" && now.Sub(h.Vitals.Taken) <= StaleAfter {
				m.State = "online"
			}
		case h.VitalsError == "":
			m.State = "unknown"
		}
		switch {
		case h.Agents > 0:
			m.Work = "working"
		case h.PausedUntil != nil && h.PausedUntil.After(now):
			m.Work = "paused"
		}
		out = append(out, m)
	}
	return out
}

// hostRows are the configured hosts, then any other host the board has an
// agent on.
func hostRows(s sysinfo.Snapshot, now time.Time) []HostRow {
	var out []HostRow
	seen := map[string]bool{}
	for _, h := range s.Hosts {
		seen[h.Name] = true
		row := HostRow{Name: h.Name, Agents: fmt.Sprintf("%d of %d", h.Agents, h.Slots), Busy: h.Agents > 0}
		if h.PausedUntil != nil && h.PausedUntil.After(now) {
			row.Paused = "paused until " + h.PausedUntil.Local().Format("2 Jan 15:04")
		}
		out = append(out, row)
	}
	var extra []string
	for name := range s.Agents {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		out = append(out, HostRow{Name: name, Agents: fmt.Sprintf("%d", s.Agents[name]), Busy: true})
	}
	for _, p := range s.Pauses {
		if !seen[p.Host] && p.Until.After(now) {
			out = append(out, HostRow{Name: p.Host, Agents: "0", Paused: "paused until " + p.Until.Local().Format("2 Jan 15:04")})
		}
	}
	return out
}

func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// systemPage is System status: the tick's snapshot of every host and what
// Foreman sees itself. The maintainer's only.
func (s *Server) systemPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "system", &Page{Title: "System status", Tab: "system", System: s.systemView()}, s.visible(r))
}

// systemJSON is /system.json (and /settings/system.json): the snapshot as the tick wrote it,
// its age, and what Foreman reads live. The maintainer's only.
func (s *Server) systemJSON(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	out := map[string]any{"live": sysinfo.LiveOf("", s.paths, now, nil), "stale_after_seconds": int(StaleAfter.Seconds())}
	snap, err := sysinfo.Read(sysinfo.File(s.paths))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		out["snapshot"], out["stale"] = nil, true
	case err != nil:
		out["snapshot"], out["stale"], out["error"] = nil, true, err.Error()
	default:
		age := now.Sub(snap.Taken)
		out["snapshot"], out["age_seconds"], out["stale"] = snap, int(age.Seconds()), age > StaleAfter
	}
	jsonReply(w, http.StatusOK, out)
}
