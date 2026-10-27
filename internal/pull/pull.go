// Package pull is factory-pull, the desktop half of a pull host
// (`transport = "pull"` in the dispatcher's [hosts]): a long-running user
// service that claims the host's jobs from the dispatcher's mailbox over
// ssh (a key forced to `factory-remote mailbox <host>`), lays each out in
// an idle Lever worker's directory, hands it over with an operator note,
// beats while the worker runs it, and posts what it left as the collect
// tar factory-remote's collect would send. Every Config.Vitals it posts the
// desktop's vitals too, for Foreman's System card (the dispatcher cannot
// ask: it cannot reach this host).
//
// It is a proxy of bytes: it never runs git or anything from a job's
// directory, and reads and writes there only through an os.Root at the
// worker's directory, every read capped. The worker does the git, inside
// its jail, with the job.sh written from factory-pull's own template; for
// a gate job (Gate 1's checks) the worker only starts gate.sh, from the
// same kind of template, which writes the report itself.
package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// Step is how often Run looks at its jobs.
const Step = 5 * time.Second

// msgGap keeps two notes to one worker apart: Lever merges the messages a
// worker gets within 2 s into one turn.
const msgGap = 3 * time.Second

// AckWindow is how long a worker has to show it took a job (Lever reports
// it busy, or it starts writing in the job's directory) before the job is
// given back for another host.
const AckWindow = 3 * time.Minute

// startGap keeps factory-pull's starts of one worker apart, so a worker
// that will not start is not asked every step.
const startGap = 2 * time.Minute

// benchFor is how long a worker that did not take its job gets no other:
// as long as the dispatcher's pause of a host that gave a job back.
const benchFor = 10 * time.Minute

// Puller is one factory-pull.
type Puller struct {
	Cfg   Config
	Exec  execx.Execer // ssh and lever
	Clock clock.Clock
	// Lever is the instance's broker; nil is Broker on Cfg.OperatorSock().
	Lever Lever
	Log   func(format string, args ...any)
	// Home is the user's home, whose disk the vitals report besides /.
	Home string
	// Vitals is this host's vitals; nil is vitals.Collect of / and Home
	// (no containers: Lever's workers are not podman's).
	Vitals func(ctx context.Context) vitals.Vitals

	jobs       map[string]*job
	lastClaim  time.Time
	lastVitals time.Time
	lastNote   map[string]time.Time // per worker
	lastStart  map[string]time.Time // per worker: factory-pull's last start of it
	benched    map[string]time.Time // per worker: when it last failed to take a job
	view       view                 // Lever as this step found it
	said       string               // the last account of Lever logged
	loaded     bool
}

// view is the instance's workers as one step found them: up is the broker
// answering (else why), workers by name.
type view struct {
	up      bool
	why     string
	workers map[string]WorkerState
}

// The phases of a job in hand.
const (
	phaseRunning    = "running"     // the worker has it
	phasePosting    = "posting"     // finished (or failed): its collect tar goes to the dispatcher
	phaseReleasing  = "releasing"   // cancelled: the claim goes back
	phaseGivingBack = "giving_back" // never started here: it goes to another host
)

// job is one job in hand, kept in <state>/<id>.json so a restart carries
// on with it.
type job struct {
	ID       string `json:"id"`
	Worker   string `json:"worker"`
	Phase    string `json:"phase"`
	Deadline int64  `json:"deadline"` // unix: past it the job is stopped
	Memory   bool   `json:"memory"`
	LastBeat int64  `json:"last_beat"`
	// Failure, when set, is posted as the job's report (status "error")
	// instead of anything the worker wrote.
	Failure string `json:"failure,omitempty"`
	// Handed is when the worker got its note (unix); Acked, that it showed
	// it took the job. A job from before these were kept has neither and
	// counts as taken.
	Handed int64  `json:"handed,omitempty"`
	Acked  bool   `json:"acked,omitempty"`
	Why    string `json:"why,omitempty"` // why it is given back
}

func (p *Puller) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

func (p *Puller) now() time.Time { return p.Clock.Now() }

// Run steps until ctx ends.
func (p *Puller) Run(ctx context.Context) error {
	t := time.NewTicker(Step)
	defer t.Stop()
	for {
		if err := p.Step(ctx); err != nil {
			p.logf("%v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Step is one pass: Lever is looked at (and its stopped workers started),
// each job in hand moves on (acknowledged, posted, beaten, stopped,
// released or given back), then, if a worker is running and free and the
// poll interval has passed, one job is claimed.
func (p *Puller) Step(ctx context.Context) error {
	if !p.loaded {
		if err := p.load(); err != nil {
			return err
		}
		p.loaded = true
	}
	p.look(ctx)
	ids := make([]string, 0, len(p.jobs))
	for id := range p.jobs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil
		}
		p.tend(ctx, p.jobs[id])
	}
	p.postVitals(ctx)
	if ctx.Err() != nil || p.now().Sub(p.lastClaim) < p.Cfg.Poll {
		return nil
	}
	worker, ok := p.idleWorker()
	if !ok {
		return nil
	}
	p.lastClaim = p.now()
	return p.claim(ctx, worker)
}

// idleWorker is the first configured worker that Lever reports running and
// not busy, with no job in hand, nothing unfinished in its jobs/, and no
// note in the last msgGap.
func (p *Puller) idleWorker() (string, bool) {
	for _, w := range p.Cfg.Workers {
		if p.free(w) {
			return w, true
		}
	}
	return "", false
}

func (p *Puller) free(w string) bool {
	return p.view.workers[w].Ready() && !p.held(w) && p.now().Sub(p.lastNote[w]) >= msgGap &&
		p.now().Sub(p.benched[w]) >= benchFor && !p.busy(w)
}

func (p *Puller) held(w string) bool {
	for _, j := range p.jobs {
		if j.Worker == w {
			return true
		}
	}
	return false
}

func (p *Puller) lever() Lever {
	if p.Lever != nil {
		return p.Lever
	}
	return Broker{Sock: p.Cfg.OperatorSock()}
}

// look reads the instance's workers, starts any configured worker that is
// stopped (at most once a startGap each), and logs Lever's state when it
// changes, not every step.
func (p *Puller) look(ctx context.Context) {
	if p.read(ctx) && p.startStopped(ctx) {
		p.read(ctx) // what the starts made of them, for this step's claim and vitals
	}
	if s := p.account(); s != p.said {
		p.said = s
		p.logf("lever: %s", s)
	}
}

// read takes the broker's worker list into the view; false when the broker
// does not answer.
func (p *Puller) read(ctx context.Context) bool {
	ws, err := p.lever().Workers(ctx)
	if err != nil {
		p.view = view{why: "the Lever broker does not answer: " + textx.Prefix(err.Error(), 200)}
		return false
	}
	p.view = view{up: true, workers: map[string]WorkerState{}}
	for _, w := range ws {
		p.view.workers[w.Name] = w
	}
	return true
}

// startStopped starts each configured worker the broker can start, at most
// once a startGap each; true when one started.
func (p *Puller) startStopped(ctx context.Context) bool {
	started := false
	for _, w := range p.Cfg.Workers {
		st, ok := p.view.workers[w]
		if ok && !st.Startable() || p.held(w) || p.now().Sub(p.lastStart[w]) < startGap || ctx.Err() != nil {
			continue
		}
		p.lastStart[w] = p.now()
		if err := p.lever().Start(ctx, w); err != nil {
			p.logf("start %s: %v", w, err)
			continue
		}
		p.logf("started %s (it was %s)", w, phaseOf(st, ok))
		started = true
	}
	return started
}

func phaseOf(st WorkerState, ok bool) string {
	if !ok {
		return "not in Lever's list"
	}
	return st.Phase
}

// account is Lever's state in one line: down and why, or each configured
// worker's phase and, when running, whether it is idle.
func (p *Puller) account() string {
	if !p.view.up {
		return "down: " + p.view.why
	}
	var parts []string
	for _, w := range p.Cfg.Workers {
		st, ok := p.view.workers[w]
		switch {
		case !ok:
			parts = append(parts, w+" missing")
		case st.Phase != "running":
			parts = append(parts, w+" "+st.Phase)
		case st.Busy():
			parts = append(parts, w+" busy ("+st.Activity+")")
		case st.Idle != nil:
			parts = append(parts, w+" idle")
		default:
			parts = append(parts, w+" running ("+st.Activity+")")
		}
	}
	return "up: " + strings.Join(parts, ", ")
}

// workers is what the vitals say of the workers: the dispatcher sends no
// more jobs than are running, and none while the broker is down.
func (p *Puller) workers() *vitals.Workers {
	w := &vitals.Workers{Up: p.view.up}
	if !p.view.up {
		w.Note = textx.Prefix(p.view.why, 200)
		return w
	}
	var off []string
	for _, name := range p.Cfg.Workers {
		st := p.view.workers[name]
		if st.Phase != "running" {
			off = append(off, name+" "+phaseOf(st, st.Name != ""))
			continue
		}
		w.Running++
		if p.free(name) {
			w.Free++
		}
	}
	if len(off) > 0 {
		w.Note = textx.Prefix(strings.Join(off, ", "), 200)
	}
	return w
}

// mail runs one mailbox verb on the dispatcher. ok is false for a refusal
// (exit 2) or, for claim, nothing queued (exit 3); err is for an ssh that
// failed, timed out or could not reach the dispatcher.
func (p *Puller) mail(ctx context.Context, verb, id string, stdin []byte, maxOut int) (out []byte, code int, err error) {
	argv, err := jobproto.MailArgv(p.Cfg.SSHBin, p.Cfg.SSH, p.Cfg.Key, verb, p.Cfg.Host, id)
	if err != nil {
		return nil, 0, err
	}
	cmd := execx.Cmd{Argv: argv, Timeout: jobproto.MailTimeout(verb), MaxOut: maxOut}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	res, err := p.Exec.Run(ctx, cmd)
	switch {
	case err != nil:
		return nil, 0, fmt.Errorf("%s %s: %w", verb, id, err)
	case res.Truncated:
		return nil, 0, fmt.Errorf("%s %s: the answer is over %d bytes", verb, id, maxOut)
	case res.Code != 0 && res.Code != 2 && res.Code != jobproto.ExitNoJob:
		return nil, res.Code, fmt.Errorf("%s %s failed (%d): %s", verb, id, res.Code, textx.Prefix(textx.Strip(string(res.Stderr)), 300))
	case res.Code == 2:
		p.logf("%s %s refused: %s", verb, id, textx.Prefix(textx.Strip(string(res.Stderr)), 300))
	}
	return res.Stdout, res.Code, nil
}

// postVitals sends this host's vitals once Cfg.Vitals has passed since the
// last try. A failure is logged and waits for the next interval.
func (p *Puller) postVitals(ctx context.Context) {
	if p.Cfg.Vitals <= 0 || ctx.Err() != nil || p.now().Sub(p.lastVitals) < p.Cfg.Vitals {
		return
	}
	p.lastVitals = p.now()
	var v vitals.Vitals
	if p.Vitals != nil {
		v = p.Vitals(ctx)
	} else {
		v = vitals.Collect(ctx, vitals.Deps{Now: p.Clock.Now, DiskPaths: []string{"/", p.Home}})
	}
	v.Workers = p.workers()
	b, err := v.Encode()
	if err == nil && len(b) > jobproto.MaxVitals {
		err = fmt.Errorf("over %d bytes", jobproto.MaxVitals)
	}
	if err != nil {
		p.logf("vitals: %v", err)
		return
	}
	if _, code, err := p.mail(ctx, jobproto.MailVitals, "", b, 1<<10); err != nil {
		p.logf("vitals: %v", err)
	} else if code != 0 {
		p.logf("vitals: refused (%d)", code)
	}
}

// claim asks the dispatcher for a job for worker, lays it out in the
// worker's jobs/<id> and hands it over.
func (p *Puller) claim(ctx context.Context, worker string) error {
	out, code, err := p.mail(ctx, jobproto.MailClaim, "", nil, jobproto.MaxInputBytes+128)
	if err != nil || code != 0 {
		return err
	}
	line, start, ok := bytes.Cut(out, []byte("\n"))
	id := string(line)
	if !ok || !jobproto.JobNameOK(id) {
		return fmt.Errorf("claim: the answer does not start with a job name")
	}
	if _, held := p.jobs[id]; held {
		return fmt.Errorf("claim: %s is already in hand", id)
	}
	j := &job{ID: id, Worker: worker, Phase: phaseRunning, LastBeat: p.now().Unix()}
	meta, memory, err := p.install(worker, id, start)
	if err != nil {
		p.logf("job %s: %v", id, err)
		j.Phase, j.Failure = phasePosting, "factory-pull could not lay out the job: "+err.Error()
		return p.keep(j)
	}
	j.Memory = memory
	j.Deadline = p.now().Add(time.Duration(meta.TimeoutMinutes) * time.Minute).Unix()
	if err := p.keep(j); err != nil {
		return err
	}
	p.logf("job %s claimed for %s (timeout %d min)", id, worker, meta.TimeoutMinutes)
	p.lastNote[worker] = p.now()
	if err := p.hand(ctx, worker, id); err != nil {
		p.logf("job %s: %v; giving it back", id, err)
		j.Phase, j.Why = phaseGivingBack, "the note to "+worker+" failed: "+textx.Prefix(err.Error(), 200)
		if err := p.keep(j); err != nil {
			return err
		}
		p.giveBack(ctx, j)
		return nil
	}
	j.Handed = p.now().Unix()
	p.save(j)
	return nil
}

// tend moves one job in hand on.
func (p *Puller) tend(ctx context.Context, j *job) {
	switch j.Phase {
	case phaseRunning:
		p.running(ctx, j)
	case phasePosting:
		p.post(ctx, j)
	case phaseReleasing:
		p.release(ctx, j)
	case phaseGivingBack:
		p.giveBack(ctx, j)
	}
}

func (p *Puller) running(ctx context.Context, j *job) {
	now := p.now()
	if j.Handed != 0 && !j.Acked {
		switch {
		case p.took(j):
			p.logf("job %s: %s took it", j.ID, j.Worker)
			j.Acked = true
			p.save(j)
		case now.Sub(time.Unix(j.Handed, 0)) >= AckWindow:
			p.logf("job %s: %s did not take it within %s; giving it back, and %s gets no job for %s", j.ID, j.Worker, AckWindow, j.Worker, benchFor)
			p.benched[j.Worker] = now
			p.stop(ctx, j, "it did not start in time")
			j.Phase, j.Why = phaseGivingBack, "worker "+j.Worker+" did not start it within "+AckWindow.String()
			p.save(j)
			p.giveBack(ctx, j)
			return
		}
	}
	switch {
	case p.reported(j.Worker, j.ID):
		p.logf("job %s: %s wrote its report", j.ID, j.Worker)
		j.Phase = phasePosting
		p.save(j)
		p.post(ctx, j)
	case now.Unix() >= j.Deadline:
		p.logf("job %s: timed out on %s", j.ID, j.Worker)
		p.stop(ctx, j, "it timed out")
		j.Phase, j.Failure = phasePosting, "timed out on "+p.Cfg.Host+" (worker "+j.Worker+")"
		p.save(j)
		p.post(ctx, j)
	case now.Sub(time.Unix(j.LastBeat, 0)) >= p.Cfg.Beat:
		out, code, err := p.mail(ctx, jobproto.MailBeat, j.ID, p.progress(j.Worker, j.ID), 1<<10)
		if err != nil {
			p.logf("job %s: beat: %v", j.ID, err)
			return
		}
		j.LastBeat = now.Unix()
		p.save(j)
		if code == 0 && strings.TrimSpace(string(out)) == jobproto.BeatCancel {
			p.logf("job %s: the factory cancelled it", j.ID)
			p.stop(ctx, j, "the factory cancelled it")
			j.Phase = phaseReleasing
			p.save(j)
			p.release(ctx, j)
		}
	}
}

// stop interrupts the worker.
func (p *Puller) stop(ctx context.Context, j *job, why string) {
	p.lastNote[j.Worker] = p.now()
	if err := p.send(ctx, j.Worker, StopMessage(j.ID, why), true); err != nil {
		p.logf("job %s: %v", j.ID, err)
	}
}

// post sends the job's collect tar; a refusal (the job is gone or already
// posted) ends it as well as an answer does.
func (p *Puller) post(ctx context.Context, j *job) {
	var report []byte
	if j.Failure != "" {
		report, _ = json.Marshal(map[string]string{"status": "error", "summary": j.Failure})
	}
	tar, err := p.pack(j.Worker, j.ID, j.Memory, report)
	if err != nil {
		p.logf("job %s: pack: %v", j.ID, err)
		return
	}
	if _, _, err := p.mail(ctx, jobproto.MailDone, j.ID, tar, 1<<10); err != nil {
		p.logf("job %s: done: %v", j.ID, err)
		return
	}
	p.logf("job %s posted", j.ID)
	p.drop(j)
}

func (p *Puller) release(ctx context.Context, j *job) {
	if _, _, err := p.mail(ctx, jobproto.MailRelease, j.ID, nil, 1<<10); err != nil {
		p.logf("job %s: release: %v", j.ID, err)
		return
	}
	p.logf("job %s released", j.ID)
	p.drop(j)
}

// giveBack hands a job no worker started back to the dispatcher, with why,
// so it goes to another host at once. A dispatcher from before giveback
// refuses the verb: the job is released instead, as it was then.
func (p *Puller) giveBack(ctx context.Context, j *job) {
	_, code, err := p.mail(ctx, jobproto.MailGiveBack, j.ID, []byte(j.Why), 1<<10)
	if err != nil {
		p.logf("job %s: giveback: %v", j.ID, err)
		return
	}
	if code != 0 {
		p.release(ctx, j)
		return
	}
	p.logf("job %s given back: %s", j.ID, j.Why)
	p.drop(j)
}

// drop forgets a job: its directory goes from the worker's jobs/, its
// state file from the state directory.
func (p *Puller) drop(j *job) {
	if err := p.remove(j.Worker, j.ID); err != nil {
		p.logf("job %s: remove from %s: %v", j.ID, j.Worker, err)
	}
	if err := os.Remove(p.statePath(j.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.logf("job %s: %v", j.ID, err)
	}
	delete(p.jobs, j.ID)
}

func (p *Puller) statePath(id string) string { return filepath.Join(p.Cfg.State, id+".json") }

// keep takes j in hand and saves it.
func (p *Puller) keep(j *job) error {
	p.jobs[j.ID] = j
	return p.saveErr(j)
}

func (p *Puller) save(j *job) {
	if err := p.saveErr(j); err != nil {
		p.logf("job %s: %v", j.ID, err)
	}
}

func (p *Puller) saveErr(j *job) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(p.statePath(j.ID), b, 0o600)
}

// load reads the jobs in hand from the state directory.
func (p *Puller) load() error {
	p.jobs, p.lastNote, p.lastStart, p.benched = map[string]*job{}, map[string]time.Time{}, map[string]time.Time{}, map[string]time.Time{}
	if err := os.MkdirAll(p.Cfg.State, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(p.Cfg.State)
	if err != nil {
		return err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !jobproto.JobNameOK(id) {
			continue
		}
		b, err := fsx.ReadNoFollow(filepath.Join(p.Cfg.State, e.Name()), 1<<16)
		var j job
		if err == nil {
			err = json.Unmarshal(b, &j)
		}
		if err != nil || j.ID != id || !slices.Contains(p.Cfg.Workers, j.Worker) {
			p.logf("state %s: unreadable or for an unknown worker; ignored", e.Name())
			continue
		}
		p.jobs[id] = &j
		p.logf("job %s on %s: carried on (%s)", id, j.Worker, j.Phase)
	}
	return nil
}
