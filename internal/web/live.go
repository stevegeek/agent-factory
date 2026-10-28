package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The Live tab: what the agents are doing right now, and the factory's own
// log. Read-only: nothing here can type into an agent. Each screen polls
// its own small text endpoint while the page is open.

const (
	screenLines   = 150
	lookTimeout   = 25 * time.Second
	screenTTL     = 3 * time.Second
	screenSlots   = 2
	screenEntries = 64
	watchEvery    = time.Second
)

// livePage is In flight, the agents' screens and the log.
func (s *Server) livePage(w http.ResponseWriter, r *http.Request) {
	ts, now := s.visible(r), s.now()
	plan, stalled := s.flightPlan(now)
	v := &LiveView{InFlight: present.BuildInFlight(ts, plan, nil, s.deskViewer(r), now), Stalled: stalled,
		Screens: true, Agents: liveAgents(ts, now)}
	s.render(w, r, "live", &Page{Title: "Live", Tab: "live", Live: v}, ts)
}

// flightPlan is what In flight reads beyond the board: config.toml with
// the project switches, and from the tick's last sysinfo.json the hosts
// paused after a usage limit and when each pull host last reported.
// stalled says the tick has not written a snapshot for StaleAfter: then
// nothing starts until it runs again.
func (s *Server) flightPlan(now time.Time) (p present.Plan, stalled string) {
	if s.svc.Config == nil {
		return p, ""
	}
	cfg, err := s.svc.Config.Current()
	if err != nil || cfg == nil {
		return p, ""
	}
	var snap sysinfo.Snapshot
	if s.paths.Root != "" {
		snap, err = sysinfo.Read(sysinfo.File(s.paths))
		if err != nil {
			snap = sysinfo.Snapshot{}
		} else if age := now.Sub(snap.Taken); age > StaleAfter {
			stalled = "The tick last ran " + present.Age(age) + " ago: nothing starts until it runs again."
		}
	}
	paused := map[string]string{}
	for _, ps := range snap.Pauses {
		if ps.Until.After(now) {
			paused[ps.Host] = "is paused until " + ps.Until.Local().Format("15:04") + " (a usage limit)"
		}
	}
	p = present.Plan{Known: true, MaxAgents: cfg.MaxAgents, Projects: map[string]present.PlanProject{}, LocalDown: paused[model.LocalHost]}
	for _, pr := range cfg.Projects {
		pp := present.PlanProject{Board: pr.Mode == string(model.ModeBoard), HeavyGate: cfg.HeavyGate(pr),
			GateMemory: cfg.GateMemory(pr), HeavyMemory: cfg.Gate.HeavyMemory, Hygiene: pr.Hygiene}
		switch {
		case pr.Enabled:
		case pr.Switch != nil:
			pp.Paused = "switched off in Foreman" + switchBy(pr.Switch)
		default:
			pp.Paused = "paused in config.toml"
		}
		p.Projects[pr.Key] = pp
	}
	for _, h := range cfg.Hosts {
		ph := present.PlanHost{Name: h.Name, Slots: h.Slots, Stages: h.Stages, Prefer: h.Prefer, Pull: h.Pull(), Down: paused[h.Name]}
		for _, pr := range cfg.Projects {
			if !h.TakesKind(cfg.KindName(pr)) {
				ph.Refuses = append(ph.Refuses, pr.Key)
			}
		}
		if ph.Down == "" && h.Pull() {
			ph.Down = pullAway(snap, h.Name, time.Duration(h.BeatMinutes)*time.Minute, now)
		}
		p.Hosts = append(p.Hosts, ph)
	}
	return p, stalled
}

// switchBy is " by robin at 20 Oct 14:02", or as much of it as the switch
// records.
func switchBy(sw *config.Switch) string {
	s := ""
	if sw.By != "" {
		s += " by " + sw.By
	}
	if at, err := clock.Parse(sw.At); err == nil {
		s += " at " + at.Local().Format("2 Jan 15:04")
	}
	return s
}

// pullAway tells a pull host that may be away: the tick sends no jobs to
// one that has not called in for its beat window, which only its mailbox
// knows (Foreman cannot read it). Its vitals, posted every minute when it
// runs, are stamped with the dispatcher's clock when they arrive, so ones
// older than the window say it has likely gone quiet. No vitals: nothing
// can be told.
func pullAway(snap sysinfo.Snapshot, name string, window time.Duration, now time.Time) string {
	for _, h := range snap.Hosts {
		if h.Name != name || h.Vitals == nil || h.Vitals.Taken.IsZero() || window <= 0 {
			continue
		}
		if now.Sub(h.Vitals.Taken) > window {
			return "may be away: its last report is from " + h.Vitals.Taken.Local().Format("2 Jan 15:04")
		}
	}
	return ""
}

func plain(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, text)
}

// liveScreen is the running agent's screen now, cached for a few seconds
// so two open pages do not double the work on a small box.
func (s *Server) liveScreen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	t := findTicket(s.visible(r), id)
	if t == nil || !t.Agent.Present() || s.svc.Looker == nil {
		plain(w, "(this agent has finished; its log is on the ticket)")
		return
	}
	if ag, _ := t.Agent.Get(); ag.Host == "" || ag.Host == model.LocalHost {
		s.watching()
	}
	plain(w, s.screens.get(id, func() string {
		var out string
		err := s.call(r, lookTimeout, func(ctx context.Context) (err error) {
			out, err = s.svc.Looker.Look(ctx, ticket.ID(id), screenLines)
			return err
		})
		if err != nil {
			return "(could not read the screen: " + err.Error() + ")"
		}
		return out
	}))
}

// watchMark is when Foreman last touched foreman-state/watching.
type watchMark struct {
	mu sync.Mutex
	at time.Time
}

// watching marks that someone has Live open: it touches
// foreman-state/watching, at most once a second, never through a link.
// Its mtime is all the screen relay (factory-screens.path, `factory
// screens`) reads: while it is fresh, the relay writes the local agents'
// screens to screens/, which Foreman's sandbox cannot make itself.
func (s *Server) watching() {
	if s.paths.Root == "" {
		return
	}
	now := s.clock.Now()
	s.watch.mu.Lock()
	if !s.watch.at.IsZero() && now.Sub(s.watch.at) < watchEvery {
		s.watch.mu.Unlock()
		return
	}
	s.watch.at = now
	s.watch.mu.Unlock()
	if err := fsx.Touch(s.paths.Watching()); err != nil && s.log != nil {
		s.log.Printf("live: %v", err)
	}
}

// liveLog is the end of the factory's log and of Foreman's own
// (foreman-state/foreman.log: logs/ is read-only to Foreman), merged by
// time, newest last.
// logsPage shows the factory's log; its text polls /live/log.
func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "logs", &Page{Title: "Logs", Tab: "logs"}, s.visible(r))
}

func (s *Server) liveLog(w http.ResponseWriter, r *http.Request) {
	const max, lines = 48 << 10, 200
	foreman := ""
	if s.paths.Root != "" {
		foreman = tailFile(s.paths.ForemanLog(), max, lines)
	}
	plain(w, mergeLogs(lines, tailFile(s.paths.FactoryLog, max, lines), foreman))
}

// mergeLogs merges log texts by their lines' stamps, earlier texts first
// at the same second, and keeps the last n lines. A line that does not
// start with a stamp (the rest of a message with a newline in it) stays
// with the line before it.
func mergeLogs(n int, texts ...string) string {
	type entry struct {
		at   time.Time
		text string
	}
	var all []entry
	for _, text := range texts {
		if text == "" {
			continue
		}
		var last time.Time
		start := len(all)
		for _, line := range strings.Split(text, "\n") {
			stamp, _, _ := strings.Cut(line, " ")
			if at, err := clock.Parse(stamp); err == nil {
				last = at
			} else if len(all) > start {
				all[len(all)-1].text += "\n" + line
				continue
			}
			all = append(all, entry{last, line})
		}
	}
	slices.SortStableFunc(all, func(a, b entry) int { return a.at.Compare(b.at) })
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = e.text
	}
	merged := strings.Split(strings.Join(out, "\n"), "\n")
	if len(merged) > n {
		merged = merged[len(merged)-n:]
	}
	return strings.Join(merged, "\n")
}

// agentLog is an archived agent's log, from the ticket page: the name must
// be one of the ticket's own runs; no path from the request reaches the
// file system unchecked.
func (s *Server) agentLog(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	if !validID(id) || !present.ValidAgentName(name) || !strings.HasPrefix(name, strings.ToLower(id)+"-") || s.paths.Logs == "" {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.paths.TicketLogs(id), name, "agent.log")
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	plain(w, tailFile(path, 1<<20, 0))
}

// tailFile is the last max bytes of a regular file, cut to whole lines and
// with invalid UTF-8 replaced; at most n lines when n > 0. It never follows
// a link and never blocks on a fifo.
func tailFile(path string, max int64, n int) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	off := st.Size() - max
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, max))
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	text := strings.TrimRight(strings.ToValidUTF8(string(b), "�"), "\n")
	if n > 0 {
		lines := strings.Split(text, "\n")
		if len(lines) > n {
			text = strings.Join(lines[len(lines)-n:], "\n")
		}
	}
	return text
}

// screens caches agent screens for a few seconds per ticket, runs at most
// two looks at once and collapses concurrent misses for one ticket into one
// look (bug 3). It is a field of the server, not a global.
type screens struct {
	clock    clock.Clock
	slots    chan struct{}
	mu       sync.Mutex
	vals     map[string]screen
	inflight map[string]*pending
}

type screen struct {
	at   time.Time
	text string
}

type pending struct {
	done chan struct{}
	text string
}

func newScreens(c clock.Clock) *screens {
	return &screens{clock: c, slots: make(chan struct{}, screenSlots), vals: map[string]screen{}, inflight: map[string]*pending{}}
}

func (c *screens) get(key string, fill func() string) string {
	c.mu.Lock()
	if v, ok := c.vals[key]; ok && c.clock.Now().Sub(v.at) < screenTTL {
		c.mu.Unlock()
		return v.text
	}
	if p, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-p.done
		return p.text
	}
	p := &pending{done: make(chan struct{})}
	c.inflight[key] = p
	c.mu.Unlock()

	c.slots <- struct{}{}
	p.text = fill()
	<-c.slots

	c.mu.Lock()
	if len(c.vals) >= screenEntries {
		c.vals = map[string]screen{}
	}
	c.vals[key] = screen{c.clock.Now(), p.text}
	delete(c.inflight, key)
	c.mu.Unlock()
	close(p.done)
	return p.text
}
