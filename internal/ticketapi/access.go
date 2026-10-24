package ticketapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

// caller is caller_for's dict: who asks, what they may see and do.
type caller struct {
	name     string
	person   bool
	projects []string
	can      []string
	tickets  jsonx.Value // null: every ticket of its projects
	expires  jsonx.Value
}

// resolve is caller_for. Without a token the caller is a person
// with every project and capability. A token's live record gives its
// projects (those still in config) and capabilities (those that exist); a
// token that is missing, expired, reserved or now a person's name is
// unauthorized, so a token never answers as a person.
func (cx *call) resolve(c Caller) (*caller, error) {
	var known []string
	for _, p := range cx.cfg.Projects {
		known = append(known, p.Key)
	}
	if c.person {
		name := c.name
		if name == "" {
			name = cx.cfg.Maintainer()
		} else if tokens.Reserved(name) {
			return nil, refuse(Unauthorized, "", "%s is a reserved name, not a person's", name)
		}
		var caps []string
		for _, x := range model.Capabilities() {
			caps = append(caps, string(x))
		}
		return &caller{name: name, person: true, projects: known, can: caps, tickets: jsonx.Null(), expires: jsonx.Null()}, nil
	}
	rec, ok, err := cx.s.Tokens.Live(c.name)
	if err != nil {
		return nil, fmt.Errorf("token %s: %w", c.name, err)
	}
	if !ok || tokens.Reserved(c.name) || contains(cx.cfg.People(), c.name) {
		return nil, refuse(Unauthorized, "", "no such token, or it has expired")
	}
	out := &caller{name: c.name, tickets: rec.Tickets(), expires: rec.Expires()}
	for _, p := range rec.Projects() {
		if contains(known, p) {
			out.projects = append(out.projects, p)
		}
	}
	for _, x := range rec.Can() {
		for _, cp := range model.Capabilities() {
			if x == string(cp) {
				out.can = append(out.can, x)
			}
		}
	}
	return out, nil
}

// sees is sees(): the project is the caller's, and the caller is not
// scoped to some tickets, or this is one of them, or it filed it. tid ""
// asks about the project only.
func (c *caller) sees(project jsonx.Value, tid string, by jsonx.Value) bool {
	p, ok := project.Str()
	if !ok || !contains(c.projects, p) {
		return false
	}
	return c.tickets.IsNull() || tid == "" || pyIn(tid, c.tickets) || isStr(by, c.name)
}

func (c *caller) seesTicket(t *ticket.Ticket) bool {
	tr := t.Tree()
	return c.sees(tr.Get("project"), string(t.ID), tr.Get("by"))
}

// need is need(): forbidden without the capability.
func (c *caller) need(cp string) error {
	if !contains(c.can, cp) {
		return refuse(Forbidden, "", "this token may not %s here", cp)
	}
	return nil
}

// isTicketID is TICKET_ID.fullmatch: ^T\d{4,6}$ (an upper-case T).
func isTicketID(s string) bool {
	id, ok := ticket.ParseID(s)
	return ok && string(id) == s
}

// tryLoad is try_load(): the ticket an id names, or nil when the id is not
// one (or not a string), or the file is missing or will not read.
func (cx *call) tryLoad(v jsonx.Value) *ticket.Ticket {
	s, ok := v.Str()
	if !ok || !isTicketID(s) {
		return nil
	}
	t, err := cx.s.Board.Reader.Load(ticket.ID(s))
	if err != nil {
		return nil
	}
	return t
}

// visible is visible_ticket(): a ticket the caller cannot see is not
// found, in the same words as one that is not there.
func (cx *call) visible(id jsonx.Value) (*ticket.Ticket, error) {
	t := cx.tryLoad(id)
	if t == nil || !cx.c.seesTicket(t) {
		return nil, refuse(NotFound, "id", "no ticket %s", jsonx.PyStr(id))
	}
	return t, nil
}

// boardOnly is board_only(): only a board project's tickets move by the
// API; the factory's pipeline moves its own.
func boardOnly(t *ticket.Ticket, code string) error {
	if !isStr(t.Tree().Get("mode"), string(model.ModeBoard)) {
		return refuse(code, "", "%s is a factory ticket: only the factory's pipeline moves it", t.ID)
	}
	return nil
}

// spec is one key a request may hold, and its type: str, int, bool, strs,
// list or dict, with a trailing "?" allowing null.
type spec struct{ key, typ string }

// take is take(): the request is one object, with no key the spec does not
// name, every required key not null, and every value of its type.
func take(req jsonx.Value, specs []spec, required ...string) (jsonx.Object, error) {
	o, ok := req.Object()
	if !ok {
		return nil, bad("", "the request is one JSON object")
	}
	types := map[string]string{}
	for _, s := range specs {
		types[s.key] = s.typ
	}
	for _, m := range o {
		if _, ok := types[m.Key]; !ok {
			return nil, bad(m.Key, "unknown key %s", repr(m.Key))
		}
	}
	for _, k := range required {
		if o.Get(k).IsNull() {
			return nil, bad(k, "%s is needed", k)
		}
	}
	words := map[string]string{"str": "a string", "int": "an integer", "bool": "true or false", "strs": "a list of strings",
		"list": "a list", "dict": "an object"}
	for _, m := range o {
		typ := types[m.Key]
		base, nullable := strings.CutSuffix(typ, "?")
		if m.Value.IsNull() && nullable {
			continue
		}
		var good bool
		switch base {
		case "str":
			good = m.Value.Kind() == jsonx.KindString
		case "int":
			good = m.Value.Kind() == jsonx.KindNumber && m.Value.IsInt()
		case "bool":
			good = m.Value.Kind() == jsonx.KindBool
		case "strs":
			good = m.Value.Kind() == jsonx.KindArray
			for _, e := range list(m.Value) {
				good = good && e.Kind() == jsonx.KindString
			}
		case "list":
			good = m.Value.Kind() == jsonx.KindArray
		case "dict":
			good = m.Value.Kind() == jsonx.KindObject
		}
		if !good {
			return nil, bad(m.Key, "%s must be %s", m.Key, words[base])
		}
	}
	return o, nil
}

// textIn is text_in(): a text of least to most UTF-8 bytes.
func textIn(value, field string, most, least int) (string, error) {
	n := len(value)
	if n < least || n > most {
		if least > 0 {
			return "", bad(field, "%s must be %d to %d bytes", field, least, most)
		}
		return "", bad(field, "%s is at most %d bytes", field, most)
	}
	return value, nil
}

// strsOf are the strings of a list a take() checked.
func strsOf(v jsonx.Value) []string {
	var out []string
	for _, e := range list(v) {
		out = append(out, sval(e))
	}
	return out
}

// intOf is an integer a take() checked; ok is false past int64.
func intOf(v jsonx.Value) (int64, bool) { return v.Int64() }

// lockWait is d as the store's lock wait: negative waits as long as the
// context allows. (The type is fsx.Wait, named through the store's
// constants: this package may not import fsx.)
func lockWait[W ~int64](_ W, d time.Duration) W {
	if d < 0 {
		return W(-1)
	}
	return W(d)
}

// lockErr turns the store's refusals into the API's: a ticket another
// writer holds is busy, one that is not there not found.
func lockErr(err error) error {
	var be *store.BusyError
	var nf *store.NotFoundError
	switch {
	case errors.As(err, &be):
		return refuse(Busy, "", "%s is busy: the dispatcher is working on it; try again in a few minutes", be.ID)
	case errors.As(err, &nf):
		return refuse(NotFound, "", "no ticket %s", nf.ID)
	}
	return err
}

// changing is changing(): the ticket locked, read afresh, changed by fn and
// saved when fn returns nil. The saved ticket is returned.
func (cx *call) changing(id string, fn func(*ticket.Ticket) error) (*ticket.Ticket, error) {
	var saved *ticket.Ticket
	err := cx.s.Board.Changing(cx.ctx, id, lockWait(store.NoWait, cx.wait), func(t *ticket.Ticket) error {
		if err := fn(t); err != nil {
			return err
		}
		saved = t
		return nil
	})
	if err != nil {
		return nil, lockErr(err)
	}
	return saved, nil
}

// held is held(): several tickets under their locks, lowest id first.
func (cx *call) held(ids []string) (func(), error) {
	var tids []ticket.ID
	for _, id := range ids {
		tids = append(tids, ticket.ID(id))
	}
	unlock, err := cx.s.Board.LockMany(cx.ctx, tids, lockWait(store.NoWait, cx.wait))
	if err != nil {
		return nil, lockErr(err)
	}
	return unlock, nil
}

// createLock is create_lock(): idempotency lookups and the creates they
// guard, one at a time. Python waits for it without end; here a caller
// waits as long as for a ticket, then is told the board is busy.
func (cx *call) createLock() (func(), error) {
	l, err := cx.s.Board.CreateLock(cx.ctx, lockWait(store.NoWait, cx.wait))
	if err != nil {
		if errors.Is(err, (&store.BusyError{}).Unwrap()) {
			return nil, refuse(Busy, "", "the board is busy: tickets are being filed; try again in a few minutes")
		}
		return nil, err
	}
	return func() { l.Unlock() }, nil
}

// index is ticket_index(): the index refreshed from the ticket files.
func (cx *call) index() (boardread.Index, error) {
	ix, err := cx.s.Board.RefreshIndex(cx.ctx)
	if err != nil {
		return boardread.Index{}, fmt.Errorf("ticket index: %w", err)
	}
	return ix, nil
}

// writable is writable(): the index, once the store and the token's share
// of it have room. A person writes past a full store: the cap is there to
// stop tokens.
func (cx *call) writable() (boardread.Index, error) {
	ix, err := cx.index()
	if err != nil || cx.c.person {
		return ix, err
	}
	var size, wrote int64
	for _, id := range ix.IDs() {
		e, _ := ix.Get(id)
		if n, ok := e.Get("size").Int64(); ok {
			size += n
		}
		wrote += e.Wrote(cx.c.name)
	}
	if size > cx.lim.Store {
		return ix, refuse(StorageFull, "", "the ticket store is full; nothing more can be written until it is tidied")
	}
	if wrote > cx.lim.TokenStore {
		return ix, refuse(TooMany, "", "this token has written %d MiB of tickets, the most one token writes; the maintainer can tidy them",
			cx.lim.TokenStore>>20)
	}
	return ix, nil
}

// roomy is roomy(): a ticket's history only grows; past its cap only a
// person adds to it.
func (cx *call) roomy(t *ticket.Ticket) error {
	if !cx.c.person && len(list(t.Tree().Get("history"))) >= cx.lim.TicketHistory {
		return refuse(TooMany, "", "%s has %d events, the most a token adds to; file a new ticket to go on", t.ID, cx.lim.TicketHistory)
	}
	return nil
}

// checkCreates is check_creates(): a board project holds so many tickets,
// and a token files so many a day (UTC).
func (cx *call) checkCreates(project string, n int, ix boardread.Index) error {
	if mode, _ := cx.cfg.ProjectMode(project); mode == string(model.ModeBoard) {
		count := 0
		for _, id := range ix.IDs() {
			if e, _ := ix.Get(id); isStr(e.Get("project"), project) {
				count++
			}
		}
		if count+n > cx.lim.ProjectTickets {
			return refuse(TooMany, "", "%s has %d tickets, the most a board project holds", project, cx.lim.ProjectTickets)
		}
	}
	if !cx.c.person {
		today := cx.now().Format("2006-01-02")
		made := 0
		for _, id := range ix.IDs() {
			e, _ := ix.Get(id)
			if isStr(e.Get("by"), cx.c.name) && textx.Prefix(pyStrOr(e.Get("created")), 10) == today {
				made++
			}
		}
		if made+n > cx.lim.CreatesPerDay {
			return refuse(TooMany, "", "a token files at most %d tickets a day (UTC)", cx.lim.CreatesPerDay)
		}
	}
	return nil
}

// pyStrOr is str(v or "").
func pyStrOr(v jsonx.Value) string {
	if !jsonx.Truthy(v) {
		return ""
	}
	return jsonx.PyStr(v)
}

// digest is request_digest(): the sha256 of the request, less its
// idempotency key, as canonical JSON.
func digest(req jsonx.Object) string {
	body := req.Clone()
	body.Delete("idempotency_key")
	sum := sha256.Sum256(jsonx.Canonical(body.Value()))
	return hex.EncodeToString(sum[:])
}

// findIdem is find_idem(): the ticket a caller's key filed, and the digest
// of the request that filed it.
func findIdem(ix boardread.Index, name, key string) (id, dig string, ok bool) {
	for _, tid := range ix.IDs() {
		e, _ := ix.Get(tid)
		by, k, d, has := e.Idem()
		if has && by == name && k == key {
			return tid, d, true
		}
	}
	return "", "", false
}

// validIdemKey is IDEM_KEY.fullmatch: ^[A-Za-z0-9._:-]{1,128}$.
func validIdemKey(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:-", c) >= 0) {
			return false
		}
	}
	return true
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
