package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// The small state files. Each is kept as its ordered JSON tree, so a
// key's place and any key the code does not know survive a write, which is
// json.dump with Python's formatting through fsx.WriteAtomic.

// maxStateBytes caps a state file read.
const maxStateBytes = 16 << 20

func readObject(path string) (jsonx.Object, bool, error) {
	data, err := fsx.ReadNoFollow(path, maxStateBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	v, err := jsonx.Parse(data)
	if err != nil {
		return nil, true, err
	}
	o, ok := v.Object()
	if !ok {
		return nil, true, fmt.Errorf("%s is not a JSON object", path)
	}
	return o, true, nil
}

// State is state.json: {"clock": {"memory-report", "versions", or a
// schedule name from before runs were kept per project: stamp},
// "schedules": {schedule name: {project: stamp}}}.
type State struct{ tree jsonx.Object }

// LoadState reads state.json (load_state): missing is empty; a file that
// will not parse is an error, as in Python.
func (b *Board) LoadState() (*State, error) {
	o, _, err := readObject(b.Paths.State)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	return &State{tree: o}, nil
}

// Clock is when a schedule (or the memory report, or the version check)
// last ran, if it has; "" values count as never, as Python's `not prev`.
func (s *State) Clock(name string) (string, bool) {
	c, _ := s.tree.Get("clock").Object()
	v, _ := c.Get(name).Str()
	return v, v != ""
}

// SetClock records a run (last[name] = now()), keeping the name's place.
func (s *State) SetClock(name, stamp string) {
	c, ok := s.tree.Get("clock").Object()
	if !ok {
		c = jsonx.Object{}
	}
	c.Set(name, jsonx.String(stamp))
	s.tree.Set("clock", c.Value())
}

// ScheduleRun is when a schedule last filed its ticket on a project, if it
// has since runs were kept per project; "" counts as never.
func (s *State) ScheduleRun(name, project string) (string, bool) {
	all, _ := s.tree.Get("schedules").Object()
	runs, _ := all.Get(name).Object()
	v, _ := runs.Get(project).Str()
	return v, v != ""
}

// SetScheduleRun records a schedule's run on a project, keeping the
// names' places.
func (s *State) SetScheduleRun(name, project, stamp string) {
	all, ok := s.tree.Get("schedules").Object()
	if !ok {
		all = jsonx.Object{}
	}
	runs, ok := all.Get(name).Object()
	if !ok {
		runs = jsonx.Object{}
	}
	runs.Set(project, jsonx.String(stamp))
	all.Set(name, runs.Value())
	s.tree.Set("schedules", all.Value())
}

// StateLock is ROOT/.state.lock, held by whoever reads state.json,
// changes it and writes it back (the daily clock, the latest-toolchain
// trial), so neither write drops the other's change.
func (b *Board) StateLock(ctx context.Context, wait fsx.Wait) (*fsx.Locked, error) {
	if err := os.MkdirAll(b.Paths.Root, 0o777); err != nil {
		return nil, fmt.Errorf("state lock: %w", err)
	}
	return fsx.Lock(ctx, b.Paths.StateLock, wait)
}

// SaveState writes state.json, json.dump(indent=2).
func (b *Board) SaveState(s *State) error {
	if s.tree == nil {
		s.tree = jsonx.Object{}
	}
	if err := fsx.WriteAtomic(b.Paths.State, jsonx.EncodePython(s.tree.Value(), 2), 0o666); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	return nil
}

// Watches is watches.json: {watch name: {"at": stamp, "hash": sha256}}.
type Watches struct{ tree jsonx.Object }

// LoadWatches reads watches.json: missing or broken is empty, as in Python.
func (b *Board) LoadWatches() *Watches {
	o, _, err := readObject(b.Paths.Watches)
	if err != nil {
		o = nil
	}
	return &Watches{tree: o}
}

// Names are the watches with a state, in the file's order.
func (w *Watches) Names() []string { return w.tree.Keys() }

// Get is a watch's last run and output hash; hasHash is Python's
// "hash" in last (the first run only learns what is there).
func (w *Watches) Get(name string) (at, hash string, hasHash bool) {
	o, _ := w.tree.Get(name).Object()
	at, _ = o.Get("at").Str()
	h, has := o.Lookup("hash")
	hash, _ = h.Str()
	return at, hash, has
}

func (w *Watches) entry(name string) jsonx.Object {
	o, ok := w.tree.Get(name).Object()
	if !ok {
		o = jsonx.Object{}
	}
	return o
}

// SetAt records when the watch last ran (state.setdefault(name, {})["at"]).
func (w *Watches) SetAt(name, stamp string) {
	o := w.entry(name)
	o.Set("at", jsonx.String(stamp))
	w.tree.Set(name, o.Value())
}

// SetHash records the hash of the watch's output.
func (w *Watches) SetHash(name, hash string) {
	o := w.entry(name)
	o.Set("hash", jsonx.String(hash))
	w.tree.Set(name, o.Value())
}

// SaveWatches writes watches.json, json.dump(indent=2).
func (b *Board) SaveWatches(w *Watches) error {
	if w.tree == nil {
		w.tree = jsonx.Object{}
	}
	if err := fsx.WriteAtomic(b.Paths.Watches, jsonx.EncodePython(w.tree.Value(), 2), 0o666); err != nil {
		return fmt.Errorf("watches: %w", err)
	}
	return nil
}

// Pause is one host that gets no new agents until a time (a usage limit).
type Pause struct {
	Host  string
	Until time.Time
	stamp jsonx.Value // as stored
}

// LoadPauses is load_pauses(): the hosts paused now, in the file's order.
// Expired entries are ignored here and dropped by the next PauseHost;
// missing or broken is none. An entry whose time does not read
// is ignored too (Python would raise).
func (b *Board) LoadPauses() []Pause {
	o, _, err := readObject(b.Paths.Pauses)
	if err != nil {
		return nil
	}
	now := b.now()
	var out []Pause
	for _, m := range o {
		s, ok := m.Value.Str()
		if !ok {
			continue
		}
		until, aware, err := clock.ParseISO(s)
		if err != nil || !aware || !until.After(now) {
			continue
		}
		out = append(out, Pause{Host: m.Key, Until: until, stamp: m.Value})
	}
	return out
}

// Paused reports a host paused now.
func (b *Board) Paused(host string) bool {
	for _, p := range b.LoadPauses() {
		if p.Host == host {
			return true
		}
	}
	return false
}

// PauseHost is pause_host(): host gets no new agents for d. The file is
// rewritten from the pauses still running plus this one (a host paused
// already keeps its place), json.dump with no indent, and the pause is
// logged. A host paused until later already keeps that time: a short
// usage-limit pause never cuts the maintainer's or the operator's longer
// one. It returns the until stamp stored.
func (b *Board) PauseHost(host, why string, d time.Duration) (string, error) {
	var o jsonx.Object
	at := b.now().Add(d)
	until := clock.Stamp(at)
	for _, p := range b.LoadPauses() {
		o = append(o, jsonx.Member{Key: p.Host, Value: p.stamp})
		if p.Host == host && p.Until.After(at) {
			s, _ := p.stamp.Str()
			until = s
		}
	}
	o.Set(host, jsonx.String(until))
	if err := b.writePauses(o); err != nil {
		return "", err
	}
	b.logf("%s paused until %s: %s", host, until, why)
	return until, nil
}

// ResumeHost lifts host's pause: the file is rewritten from the pauses
// still running without it, and the resume is logged. It reports false,
// and writes nothing, when the host was not paused.
func (b *Board) ResumeHost(host, why string) (bool, error) {
	o := jsonx.Object{}
	found := false
	for _, p := range b.LoadPauses() {
		if p.Host == host {
			found = true
			continue
		}
		o = append(o, jsonx.Member{Key: p.Host, Value: p.stamp})
	}
	if !found {
		return false, nil
	}
	if err := b.writePauses(o); err != nil {
		return false, err
	}
	b.logf("%s resumed: %s", host, why)
	return true, nil
}

func (b *Board) writePauses(o jsonx.Object) error {
	if o == nil {
		o = jsonx.Object{}
	}
	if err := fsx.WriteAtomic(b.Paths.Pauses, jsonx.EncodePython(o.Value(), -1), 0o666); err != nil {
		return fmt.Errorf("pauses: %w", err)
	}
	return nil
}
