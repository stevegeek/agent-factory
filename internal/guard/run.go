package guard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"time"
)

const (
	// DefaultSlotWait is how long a call waits for a slot before it is
	// busy.
	DefaultSlotWait = 10 * time.Second
	// DefaultTimeout is how long a call may run.
	DefaultTimeout = 30 * time.Second
)

var (
	// ErrBusy is Run's error when no slot came free in time.
	ErrBusy = errors.New("no free slot for a ticket verb")
	// ErrPanic is Run's error when the call panicked; the panic and its
	// stack are in the journal, never in the answer.
	ErrPanic = errors.New("a ticket verb failed; it is logged")
)

// Run runs fn for the token name in a slot: it waits up to SlotWait for the
// token's own slot (one at a time per token, so one token cannot hold them
// all) and for one of the shared ones, else ErrBusy. fn gets a context that
// keeps ctx's values but not its cancellation, with the guard's Timeout: a
// client that hangs up never cuts a write short. A panic in fn is
// recovered into ErrPanic and logged with its stack.
func (g *Guard) Run(ctx context.Context, name string, fn func(context.Context) error) (err error) {
	timer := time.NewTimer(g.slotWait)
	defer timer.Stop()
	mine := g.tokenSlot(name)
	select {
	case mine <- struct{}{}:
	case <-timer.C:
		return ErrBusy
	}
	defer func() { <-mine }()
	select {
	case g.slots <- struct{}{}:
	case <-timer.C:
		return ErrBusy
	}
	defer func() { <-g.slots }()
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			g.logf("ticket verb as %s panicked: %v\n%s", name, p, debug.Stack())
			err = ErrPanic
		}
	}()
	return fn(cctx)
}

func (g *Guard) tokenSlot(name string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.holding[name]
	if c == nil {
		c = make(chan struct{}, 1)
		g.holding[name] = c
	}
	return c
}

// WriteJSON answers v as JSON with status, never cached. A v that cannot be
// encoded is answered as a 500 internal error instead.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"the answer could not be written"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(data, '\n'))
}

// WriteError answers {"error": {code, message?, field?}} with status.
func WriteError(w http.ResponseWriter, status int, code, message, field string) {
	e := map[string]string{"code": code}
	if message != "" {
		e["message"] = message
	}
	if field != "" {
		e["field"] = field
	}
	WriteJSON(w, status, map[string]any{"error": e})
}

// Internal is the answer to a failure of the factory's own: no detail for
// the caller (the journal has it).
func Internal(w http.ResponseWriter) {
	WriteError(w, http.StatusInternalServerError, "internal", "the factory could not do that; it is logged", "")
}
