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

// Puller is one factory-pull.
type Puller struct {
	Cfg   Config
	Exec  execx.Execer // ssh and lever
	Clock clock.Clock
	// Wake resumes a worker; nil is OperatorWake on the instance's socket.
	Wake func(ctx context.Context, worker string) error
	Log  func(format string, args ...any)
	// Home is the user's home, whose disk the vitals report besides /.
	Home string
	// Vitals is this host's vitals; nil is vitals.Collect of / and Home
	// (no containers: Lever's workers are not podman's).
	Vitals func(ctx context.Context) vitals.Vitals

	jobs       map[string]*job
	lastClaim  time.Time
	lastVitals time.Time
	lastNote   map[string]time.Time // per worker
	loaded     bool
}

// The phases of a job in hand.
const (
	phaseRunning   = "running"   // the worker has it
	phasePosting   = "posting"   // finished (or failed): its collect tar goes to the dispatcher
	phaseReleasing = "releasing" // cancelled: the claim goes back
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

// Step is one pass: each job in hand moves on (posted, beaten, stopped or
// released), then, if a worker is idle and the poll interval has passed,
// one job is claimed.
func (p *Puller) Step(ctx context.Context) error {
	if !p.loaded {
		if err := p.load(); err != nil {
			return err
		}
		p.loaded = true
	}
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

// idleWorker is the first configured worker with no job in hand, nothing
// unfinished in its jobs/, and no note in the last msgGap.
func (p *Puller) idleWorker() (string, bool) {
	for _, w := range p.Cfg.Workers {
		held := false
		for _, j := range p.jobs {
			held = held || j.Worker == w
		}
		if held || p.now().Sub(p.lastNote[w]) < msgGap || p.busy(w) {
			continue
		}
		return w, true
	}
	return "", false
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
		j.Phase = phaseReleasing
		if err := p.keep(j); err != nil {
			return err
		}
		p.release(ctx, j)
	}
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
	}
}

func (p *Puller) running(ctx context.Context, j *job) {
	now := p.now()
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
	p.jobs, p.lastNote = map[string]*job{}, map[string]time.Time{}
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
