package present

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// In flight, at the top of Live: the factory tickets waiting for the tick
// to start an agent or a gate, the ones with an agent on them, and the
// moves of the last day, so that a person who answered an ask can follow
// the ticket from the queue to a worker and on. It is told from the board
// and a Plan (what config.toml and the tick's last snapshot say), never
// from the hosts themselves: why a ticket waits is the tick's own start
// loop played over the board, and where that cannot be told it says so.

const (
	// MovedWindow is how far back Just moved looks.
	MovedWindow = 24 * time.Hour
	// MovedMax caps Just moved.
	MovedMax = 30
)

// Plan is what In flight knows beyond the board, as the web layer reads
// it from config.toml (with Foreman's project switches) and the tick's
// last sysinfo.json. With Known false (config could not be read) no
// reason is given beyond the board's.
type Plan struct {
	Known     bool
	MaxAgents int                    // this host's slots (max_agents)
	Projects  map[string]PlanProject // the configured projects by key
	Hosts     []PlanHost             // [hosts], in config order
	// LocalDown is why this host gets no new agents now, said after its
	// name ("is paused until 15:40"); "" while it does.
	LocalDown string
}

// PlanProject is one configured project as the start loop reads it.
type PlanProject struct {
	Board bool // a board project: the factory never starts anything on it
	// Paused is why the factory starts nothing on it, said after "the
	// project is" ("paused in config.toml", "switched off in Foreman by
	// robin at 20 Oct 14:02"); "" when it does.
	Paused string
	// HeavyGate: its gate needs this host to itself (config.HeavyGate),
	// with GateMemory and HeavyMemory for the words.
	HeavyGate               bool
	GateMemory, HeavyMemory string
}

// PlanHost is one [hosts] host as routing reads it (hosts.Pick).
type PlanHost struct {
	Name           string
	Slots          int
	Stages, Prefer []string
	Pull           bool
	// Down is why it gets no new agents now, said after its name ("is
	// paused until 15:40", "may be away: its last report is from 14:02");
	// "" while it does.
	Down string
}

// InFlight is the In flight section.
type InFlight struct {
	Waiting []Queued
	Running []Running
	Moved   []JustMoved
}

// Queued is a ticket waiting for the tick to start its next step.
type Queued struct {
	T      *ticket.Ticket
	For    string    // what it waits for: "Reviewer", "the gate"
	Since  time.Time // when it came into its column (zero when history does not say)
	Waited string    // "12 min"
	Why    string    // why it has not started, or what happens next
	Held   bool      // something other than the next tick holds it (Why says what)
}

// JustMoved is one move of the last day.
type JustMoved struct {
	T        *ticket.Ticket
	At       time.Time
	Time     string // "15:04"
	Ago      string // "12 min"
	From, To string // column labels
	Who      string // "you", a person's name, "the factory"
	Why      string // the move's words, or its reason
	Mine     bool   // the viewer moved it (their answer)
}

// BuildInFlight is In flight at now over the whole board ts (in board
// order, as the tick takes it: the slots a ticket takes are counted
// whoever may see it), showing v only the tickets of the projects sees
// lets through (nil: all). Board tickets and board projects are left out.
// It reads each ticket once.
func BuildInFlight(ts []*ticket.Ticket, p Plan, sees func(project string) bool, v Viewer, now time.Time) InFlight {
	r := new(reader)
	var f InFlight
	busy := map[string]int{}
	var queue []*ticket.Ticket
	queued := map[*ticket.Ticket]int{} // index in f.Waiting
	cutoff := now.Add(-MovedWindow)
	for _, t := range ts {
		if t.Board() || p.Projects[t.Project].Board {
			continue
		}
		shown := sees == nil || sees(t.Project)
		a, running := agentOf(t)
		what, waits := "", false
		if running {
			busy[hostOrLocal(a.Host)]++
			if shown {
				f.Running = append(f.Running, runningOf(t, a, now))
			}
		} else {
			what, waits = waitsFor(t)
		}
		if waits {
			queue = append(queue, t)
		}
		if !shown {
			continue
		}
		since := r.scan(t, cutoff, v, now, waits, &f.Moved)
		if waits {
			q := Queued{T: t, For: what, Since: since}
			if !since.IsZero() {
				q.Waited = Age(now.Sub(since))
			}
			queued[t] = len(f.Waiting)
			f.Waiting = append(f.Waiting, q)
		}
	}
	if p.Known {
		p.reasons(queue, busy, func(t *ticket.Ticket, why string, held bool) {
			if i, ok := queued[t]; ok {
				f.Waiting[i].Why, f.Waiting[i].Held = why, held
			}
		})
	}
	sort.SliceStable(f.Waiting, func(i, j int) bool {
		a, b := f.Waiting[i].Since, f.Waiting[j].Since
		if a.IsZero() != b.IsZero() {
			return b.IsZero()
		}
		return a.Before(b)
	})
	sort.SliceStable(f.Running, func(i, j int) bool { return f.Running[i].started.Before(f.Running[j].started) })
	sort.SliceStable(f.Moved, func(i, j int) bool { return f.Moved[i].At.After(f.Moved[j].At) })
	if len(f.Moved) > MovedMax {
		f.Moved = f.Moved[:MovedMax]
	}
	return f
}

// waitsFor is what a ticket with no agent waits for the tick to start, as
// the start loop's is_waiting reads it: its next stage (model.NextStage),
// or the gate. A closed ticket waits for nothing.
func waitsFor(t *ticket.Ticket) (string, bool) {
	if t.Column == string(model.ColGate) {
		return "the gate", true
	}
	if s, ok := model.NextStage(t.KindName(), t.Column); ok {
		return RoleLabel(string(s)), true
	}
	return "", false
}

func runningOf(t *ticket.Ticket, a ticket.Agent, now time.Time) Running {
	role := roleOf(a.Stage)
	if a.Stage == string(model.ColGate) {
		role = "Gate"
	}
	run := Running{T: t, Role: role, Host: localAsEmpty(a.Host), started: ParseTime(a.Started)}
	if a.Starting.Or(false) {
		run.Elapsed = "starting"
	} else if !run.started.IsZero() {
		run.Elapsed = Age(now.Sub(run.started))
	}
	return run
}

func hostOrLocal(h string) string {
	if h == "" {
		return model.LocalHost
	}
	return h
}

// scan reads t's history once, newest first: the moves since cutoff go to
// moved, and, for a waiting ticket, the result is when it came into its
// column (its last move there, or its filing there).
func (r *reader) scan(t *ticket.Ticket, cutoff time.Time, v Viewer, now time.Time, waits bool, moved *[]JustMoved) time.Time {
	var since time.Time
	for i := len(t.History) - 1; i >= 0; i-- {
		e := t.History[i]
		at := ParseTime(e.At)
		recent := !at.Before(cutoff)
		if !recent && (!waits || !since.IsZero()) {
			break
		}
		switch e.Event {
		case "move":
			m := r.moveOf(e)
			if waits && since.IsZero() && m.To == t.Column {
				since = at
			}
			if recent && !at.IsZero() {
				*moved = append(*moved, justMoved(t, e, m, at, v, now))
			}
		case "created":
			if waits && since.IsZero() && r.createdIn(e) == t.Column {
				since = at
			}
		}
	}
	return since
}

func justMoved(t *ticket.Ticket, e ticket.Entry, m MoveFacts, at time.Time, v Viewer, now time.Time) JustMoved {
	by := byOf(e)
	person := m.Human
	if _, typed := e.Data.(ticket.Moved); typed {
		person = by != "" && by != "factory"
	}
	j := JustMoved{T: t, At: at, Time: at.Local().Format("15:04"), Ago: Age(now.Sub(at)),
		From: ColumnLabel(m.From), To: ColumnLabel(m.To), Who: "the factory", Mine: person && v.made(by)}
	switch {
	case j.Mine:
		j.Who = "you"
	case person && by != "":
		j.Who = by
	case person:
		j.Who = "a person"
	}
	why := strings.TrimSpace(m.Text)
	if rest, ok := strings.CutPrefix(why, "by human"); ok {
		why = strings.TrimLeft(rest, ":;, ") // older history's mark of a hand move, which Who says
	}
	if line, _, _ := strings.Cut(why, "\n"); line != "" {
		why = Clip(line, 120)
	} else if m.Reason != "" && m.Reason != string(model.ReasonByHuman) {
		why = strings.ReplaceAll(m.Reason, "_", " ")
	}
	j.Why = why
	return j
}

// reasons plays the tick's start loop (pipeline pass, hosts.Pick,
// gateHost, gate.Fits) over the waiting tickets in board order, with the
// slots the running agents take, and tells each ticket why it waits or
// where it would start at the next tick; a ticket that would start takes
// its slot from the ones after it, as in the tick. Only what config and the
// snapshot say is used: a host's own state (podman's count here, a
// helper refusing a job, the tick's time left for a gate) is not known.
func (p Plan) reasons(queue []*ticket.Ticket, busy map[string]int, tell func(t *ticket.Ticket, why string, held bool)) {
	var heldFor ticket.ID // a heavy gate that holds this host this pass
	for _, t := range queue {
		proj, known := p.Projects[t.Project]
		if known && proj.Paused != "" {
			tell(t, "the project is "+proj.Paused, true)
			continue
		}
		if t.Column == string(model.ColGate) {
			why, held, hold := p.gate(t, proj, busy, heldFor)
			if hold {
				heldFor = t.ID
			}
			tell(t, why, held)
			continue
		}
		s, _ := model.NextStage(t.KindName(), t.Column)
		localOK := known || t.KindName() == string(model.KindManage)
		why, held := p.pick(t, string(s), localOK, busy, heldFor)
		tell(t, why, held)
	}
}

// gate is one gate in the start loop: as a gate job on a pull host when
// one takes it (gateHost), else here, in a free slot and, for a heavy
// one, with no agent beside it. hold is a heavy gate that holds this host:
// no new agent starts here after it.
func (p Plan) gate(t *ticket.Ticket, proj PlanProject, busy map[string]int, heldFor ticket.ID) (why string, held, hold bool) {
	full := busy[model.LocalHost] >= p.MaxAgents
	if gateJobs(t) < maxGateJobs {
		for _, prefer := range []bool{true, false} {
			for _, h := range p.Hosts {
				list := h.Stages
				if prefer {
					list = h.Prefer
				} else if !proj.HeavyGate && !full {
					list = nil
				}
				if h.Pull && slices.Contains(list, string(model.ColGate)) && h.Down == "" && busy[h.Name] < h.Slots {
					busy[h.Name]++
					return "starts at the next tick, as a gate job on " + h.Name, false, false
				}
			}
		}
	}
	switch {
	case full:
		return "waiting for a free slot here: " + p.localSlots(busy, heldFor), true, false
	case proj.HeavyGate && busy[model.LocalHost] > 0:
		why = fmt.Sprintf("a heavy gate (gate memory %s, above heavy_memory %s): it waits for this host to be idle, and %s run here",
			proj.GateMemory, proj.HeavyMemory, Plural(busy[model.LocalHost], "agent"))
		busy[model.LocalHost] = p.MaxAgents
		return why, true, true
	}
	return "runs at the next tick, here", false, false
}

// maxGateJobs is pipeline's: how many gate jobs a ticket gets in the gate
// column before its gate runs here.
const maxGateJobs = 2

// gateJobs counts the gate jobs started since the ticket last came into
// the gate column (pipeline's gateJobs).
func gateJobs(t *ticket.Ticket) int {
	n := 0
	for i := len(t.History) - 1; i >= 0; i-- {
		e := t.History[i]
		if m, ok := e.Data.(ticket.Moved); ok && m.To == string(model.ColGate) {
			break
		}
		if s, ok := e.Data.(ticket.Started); ok && s.Stage == string(model.ColGate) {
			n++
		}
	}
	return n
}

// pick is hosts.Pick for one waiting ticket: where it starts at the next
// tick (taking the slot), or why it cannot.
func (p Plan) pick(t *ticket.Ticket, stage string, localOK bool, busy map[string]int, heldFor ticket.ID) (string, bool) {
	slots := func(name string) (int, bool) {
		if name == model.LocalHost {
			return p.MaxAgents, true
		}
		for _, h := range p.Hosts {
			if h.Name == name {
				return h.Slots, true
			}
		}
		return 0, false
	}
	down := func(name string) string {
		if name == model.LocalHost {
			return p.LocalDown
		}
		for _, h := range p.Hosts {
			if h.Name == name {
				return h.Down
			}
		}
		return ""
	}
	start := func(name string) (string, bool) {
		busy[name]++
		if name == model.LocalHost {
			return "starts at the next tick, here", false
		}
		return "starts at the next tick, on " + name, false
	}
	if pinned := t.Host.Or(""); pinned != "" {
		n, known := slots(pinned)
		switch {
		case !known || pinned == model.LocalHost && !localOK:
			return "pinned to " + pinned + ", which cannot run it", true
		case down(pinned) != "":
			return "pinned to " + pinned + ", which " + down(pinned), true
		case busy[pinned] >= n:
			if pinned == model.LocalHost {
				return "pinned here, waiting for a free slot: " + p.localSlots(busy, heldFor), true
			}
			return fmt.Sprintf("pinned to %s, waiting for a free slot: %d of %d busy", pinned, busy[pinned], n), true
		}
		return start(pinned)
	}
	// the hosts that may run the stage, in Pick's order
	var cands []string
	for _, h := range p.Hosts {
		if slices.Contains(h.Prefer, stage) {
			cands = append(cands, h.Name)
		}
	}
	if localOK {
		cands = append(cands, model.LocalHost)
	}
	for _, h := range p.Hosts {
		if slices.Contains(h.Stages, stage) && !slices.Contains(cands, h.Name) {
			cands = append(cands, h.Name)
		}
	}
	if len(cands) == 0 {
		return "no host runs it: this project has no checkout here, and no [hosts] host lists " + stage, true
	}
	var full, downs []string
	for _, name := range cands {
		n, _ := slots(name)
		switch {
		case down(name) != "":
			downs = append(downs, hostName(name)+" "+down(name))
		case busy[name] < n:
			return start(name)
		case name == model.LocalHost:
			full = append(full, "here "+p.localSlots(busy, heldFor))
		default:
			full = append(full, fmt.Sprintf("%s %d of %d", name, busy[name], n))
		}
	}
	why := "waiting for a free slot: " + strings.Join(full, ", ")
	if len(full) == 0 {
		why = "every host that runs it is unavailable"
	}
	if len(downs) > 0 {
		why += "; " + strings.Join(downs, "; ")
	}
	return why, true
}

func hostName(name string) string {
	if name == model.LocalHost {
		return "this host"
	}
	return name
}

// localSlots says how this host's slots are taken: "2 of 2 busy", or held
// for a heavy gate.
func (p Plan) localSlots(busy map[string]int, heldFor ticket.ID) string {
	if heldFor != "" {
		return "held for " + string(heldFor) + "'s heavy gate"
	}
	return fmt.Sprintf("%d of %d busy", busy[model.LocalHost], p.MaxAgents)
}
