// Package deployrec is the self-deploy's records on disk: the request
// waiting for the runner (deploys/request.json), the running or last
// deploy (deploys/state.json) and the maintainer's hold
// (foreman-state/deploy-hold.json). The runner (package deploy) writes the
// first two and Foreman only reads them; the hold is written by Foreman
// and `factory deploy hold`. One package keeps the JSON in one place.
package deployrec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/stevegeek/agent-factory/internal/fsx"
)

// The phases of a deploy: the runner holds deploys/.lock while it is
// running.
const (
	PhaseRunning = "running"
	PhaseDone    = "done"
)

// The results of a finished deploy.
const (
	ResultOK               = "ok"
	ResultNothingInstalled = "nothing_installed"
	ResultRolledBack       = "rolled_back"
	ResultRollbackFailed   = "rollback_failed"
	ResultInterrupted      = "interrupted"
)

// maxBytes caps what a read takes: each record is a few hundred bytes,
// a state's tail at most a few KB.
const maxBytes = 256 << 10

// Request is a deploy waiting for the runner. Rev is "main" unless the
// CLI named a commit; Ticket is the Deploy? ticket it came from, if any.
// AllowOlder lets deploy.sh install a core that lacks the running one.
type Request struct {
	Rev        string `json:"rev"`
	By         string `json:"by"`
	Why        string `json:"why,omitempty"`
	Ticket     string `json:"ticket,omitempty"`
	At         string `json:"at"` // a clock.Stamp
	Drill      bool   `json:"drill,omitempty"`
	AllowOlder bool   `json:"allow_older,omitempty"`
}

// State is the running or last deploy. Commit, From and ConfigCommit are
// 12-digit commits (the new core, the core it replaced, the config);
// Helpers is each ssh helper's install outcome, "ok" or the error.
// Snapshotted is set once the run has replaced deploys/before with its
// own snapshot: before that, the snapshot there is an older deploy's.
type State struct {
	Phase        string            `json:"phase"`
	Step         string            `json:"step,omitempty"` // free text, for display
	Rev          string            `json:"rev,omitempty"`
	Commit       string            `json:"commit,omitempty"`
	From         string            `json:"from,omitempty"`
	ConfigCommit string            `json:"config_commit,omitempty"`
	By           string            `json:"by,omitempty"`
	Started      string            `json:"started,omitempty"`
	Finished     string            `json:"finished,omitempty"`
	Result       string            `json:"result,omitempty"`
	Tail         string            `json:"tail,omitempty"` // the log's end when it failed
	Log          string            `json:"log,omitempty"`
	Helpers      map[string]string `json:"helpers,omitempty"`
	Snapshotted  bool              `json:"snapshotted,omitempty"`
}

// Hold is the maintainer's hold: while it is set, a merge's deploy
// becomes a Deploy? ask.
type Hold struct {
	By   string `json:"by"`
	At   string `json:"at"` // a clock.Stamp
	Note string `json:"note,omitempty"`
}

// WriteRequest writes the request atomically, replacing any request still
// waiting: the newer one deploys main as well.
func WriteRequest(path string, r Request) error { return write(path, r) }

// ReadRequest reads the waiting request without taking it (for display);
// ok is false when none waits.
func ReadRequest(path string) (r Request, ok bool, err error) {
	found, err := read(path, &r)
	return r, found && err == nil, err
}

// TakeRequest reads the waiting request and removes it; ok is false when
// none waits. A request that does not read is removed too, and reported.
// It renames the request to path.taken before it reads, so a request
// written meanwhile waits for the next take instead of being removed
// unread; a .taken left by a take that died is taken first.
func TakeRequest(path string) (r Request, ok bool, err error) { return takeRequest(path, nil) }

// takeRequest is TakeRequest; between (for tests) runs after the rename.
func takeRequest(path string, between func()) (r Request, ok bool, err error) {
	taken := path + ".taken"
	if err := os.Rename(path, taken); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Request{}, false, fmt.Errorf("deploy request: %w", err)
	}
	if between != nil {
		between()
	}
	found, err := read(taken, &r)
	if !found {
		return Request{}, false, err
	}
	if rmErr := os.Remove(taken); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return Request{}, false, fmt.Errorf("deploy request: %w", rmErr)
	}
	if err != nil {
		return Request{}, false, err
	}
	return r, true, nil
}

// ReadState reads the deploy state; a missing file is the zero State (no
// deploy yet).
func ReadState(path string) (State, error) {
	var s State
	_, err := read(path, &s)
	return s, err
}

// WriteState writes the deploy state atomically.
func WriteState(path string, s State) error { return write(path, s) }

// ReadHold reads the hold. A missing file is not held. A file that does
// not read holds anyway, with the error: holding is the safe side.
func ReadHold(path string) (h Hold, held bool, err error) {
	found, err := read(path, &h)
	return h, found, err
}

// SetHold holds deploys (h) or releases them (nil removes the file).
func SetHold(path string, h *Hold) error {
	if h != nil {
		return write(path, *h)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deploy hold: %w", err)
	}
	return nil
}

// read decodes the JSON object at path into v, never following a link
// (the hold is in foreman-state/, which Foreman writes). found is false
// only for a missing file.
func read(path string, v any) (found bool, err error) {
	b, err := fsx.ReadNoFollow(path, maxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("deploy record: %w", err)
	}
	if len(b) == 0 || b[0] != '{' {
		return true, fmt.Errorf("deploy record %s: not a JSON object", path)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return true, fmt.Errorf("deploy record %s: %w", path, err)
	}
	return true, nil
}

func write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("deploy record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("deploy record: %w", err)
	}
	return fsx.WriteAtomic(path, append(b, '\n'), 0o600)
}
