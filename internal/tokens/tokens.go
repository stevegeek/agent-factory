// Package tokens is the API token store (ARCHITECTURE.md §2.3, inventory
// features §15.10): api-tokens.json, {"version": 1, "tokens": [...]}, where
// each agent outside the factory has a record with its name, the sha256 of
// its token, the projects it may see and what it may do there. The plain
// token is shown once, when it is made, and never kept.
//
// The file is read and written through its ordered JSON tree, so a hand
// edit's keys and records survive a change, and the bytes are Python's
// json.dump(indent=2). Changes are made under <store>.lock (flock), the
// directory is 0700 and the file 0600.
//
// Reads follow the file: a Store re-reads it whenever its (inode, mtime,
// size) changes, so a revoked token stops working on the next call, in the
// CLI and in a long-running Foreman alike.
package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/paths"
)

// MaxBytes caps one read of the store. A real store is a few KiB.
const MaxBytes = 16 << 20

// Capabilities are what a token may be granted (CAPABILITIES), in their
// canonical order; read is always granted. tokens may not import model, so
// this is a copy of model.Capabilities, and cli's TestTokensShareTheModel
// fails if the two drift (as ReservedNames and model.ReservedNames).
func Capabilities() []string { return []string{"read", "file", "talk", "work", "plan"} }

// ReservedNames are RESERVED_NAMES: no token (and no person but the default
// "maintainer") may be called these.
func ReservedNames() []string { return []string{"factory", "maintainer", "human", "system", "me"} }

// ValidName is TOKEN_NAME.fullmatch: ^[a-z][a-z0-9-]{0,31}$, a name a token
// or a person may have.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Reserved reports a name in ReservedNames.
func Reserved(name string) bool {
	for _, r := range ReservedNames() {
		if r == name {
			return true
		}
	}
	return false
}

// Logger is where a Store reports a store it refuses (*logx.Log, or
// Foreman's journal).
type Logger interface {
	Printf(format string, args ...any)
}

// Store is the token store at Path. Clock is when tokens are added and
// expire (nil is the system clock); Log may be nil. Use it by pointer: it
// caches the file between reads and is safe for concurrent use.
type Store struct {
	Path  string
	Clock clock.Clock
	Log   Logger

	mu     sync.Mutex
	key    fileKey
	cached *File
	warned string
}

// New is the store at p.Tokens ($FACTORY_API_TOKENS or
// ~/.config/factory/api-tokens.json).
func New(p paths.Paths, c clock.Clock, log Logger) *Store {
	return &Store{Path: p.Tokens, Clock: c, Log: log}
}

func (s *Store) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now().UTC()
}

// UnknownStoreError is a file that is JSON but not a token store: not an
// object, a version other than 1, or tokens that are not a list (Python's
// RuntimeError, word for word).
type UnknownStoreError struct{ Path string }

func (e *UnknownStoreError) Error() string {
	return e.Path + " is not a token store this factory knows"
}

// Refused is a token change the store will not make, with the message
// `factory tokens` prints before it exits 1.
type Refused struct{ Msg string }

func (e *Refused) Error() string { return e.Msg }

func refuse(format string, args ...any) error { return &Refused{Msg: fmt.Sprintf(format, args...)} }

// File is the store as read: the ordered tree, with its records.
type File struct {
	top  jsonx.Object
	recs []jsonx.Value
}

// Records are the store's records, in the file's order. A record that is
// not an object is kept (and written back) but matches no name.
func (f *File) Records() []Record {
	out := make([]Record, len(f.recs))
	for i, v := range f.recs {
		out[i] = Record{v: v}
	}
	return out
}

// Record is the first record named name, if there is one.
func (f *File) Record(name string) (Record, bool) {
	for _, v := range f.recs {
		r := Record{v: v}
		if n, ok := r.Name(); ok && n == name {
			return r, true
		}
	}
	return Record{}, false
}

// Set changes one key of the first record named name: for hand-made
// settings such as expires or tickets, which `tokens add` stores as null.
func (f *File) Set(name, key string, v jsonx.Value) error {
	for i, rv := range f.recs {
		r := Record{v: rv}
		if n, ok := r.Name(); ok && n == name {
			o, _ := rv.Object()
			o = o.Clone()
			o.Set(key, v)
			f.recs[i] = o.Value()
			return nil
		}
	}
	return refuse("no token %s", name)
}

// Encode is json.dump(data, f, indent=2) of the store.
func (f *File) Encode() []byte {
	top := f.top.Clone()
	top.Set("tokens", jsonx.ArrayOf(f.recs...))
	return jsonx.EncodePython(top.Value(), 2)
}

func emptyFile() *File {
	return &File{top: jsonx.Object{{Key: "version", Value: jsonx.Int(1)}, {Key: "tokens", Value: jsonx.ArrayOf()}}}
}

// parse is load_tokens on the file's bytes.
func parse(path string, data []byte) (*File, error) {
	v, err := jsonx.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	top, ok := v.Object()
	if !ok || !pyEqualsOne(top.Get("version")) {
		return nil, &UnknownStoreError{Path: path}
	}
	recs, ok := top.Get("tokens").Array()
	if !ok {
		return nil, &UnknownStoreError{Path: path}
	}
	return &File{top: top, recs: append([]jsonx.Value(nil), recs...)}, nil
}

// pyEqualsOne is Python's v == 1 for a JSON value: 1, 1.0 and true.
func pyEqualsOne(v jsonx.Value) bool {
	if b, ok := v.Bool(); ok {
		return b
	}
	f, ok := v.Float64()
	return ok && f == 1
}

// fileKey is one state of the file; the zero key is "missing".
type fileKey struct {
	dev, ino uint64
	mtime    int64
	size     int64
	present  bool
}

func statKey(path string) (fileKey, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileKey{}, nil
	}
	if err != nil {
		return fileKey{}, fmt.Errorf("token store: %w", err)
	}
	k := fileKey{present: true, mtime: fi.ModTime().UnixNano(), size: fi.Size()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.dev, k.ino = uint64(st.Dev), st.Ino
	}
	return k, nil
}

// Load is load_tokens(): a missing store is empty; a store that is not one
// is an *UnknownStoreError. The file is re-read only when it changed. The
// File returned is shared: read it, never change it (Change gives a fresh
// one).
func (s *Store) Load() (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*File, error) {
	k, err := statKey(s.Path)
	if err != nil {
		return nil, err
	}
	if s.cached != nil && k == s.key {
		return s.cached, nil
	}
	f, err := s.read(k)
	if err != nil {
		s.cached = nil
		return nil, err
	}
	s.key, s.cached = k, f
	return f, nil
}

func (s *Store) read(k fileKey) (*File, error) {
	if !k.present {
		return emptyFile(), nil
	}
	// the path is followed, as Python's open() follows it: FACTORY_API_TOKENS
	// may name a link to the store
	f, err := os.Open(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return emptyFile(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("token store: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("token store: %w", err)
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("token store %s: %w", s.Path, fsx.ErrTooLarge)
	}
	return parse(s.Path, data)
}

// Live is token_record(name): the record for a token name, false once it is
// revoked or past its expiry. A store that will not read is an error.
func (s *Store) Live(name string) (Record, bool, error) {
	f, err := s.Load()
	if err != nil {
		return Record{}, false, err
	}
	r, ok := f.Record(name)
	if !ok || r.Expired(s.now()) {
		return Record{}, false, nil
	}
	return r, true, nil
}

// Change is tokens_changing(): under <store>.lock, the store read afresh is
// passed to fn and, if fn returns nil and ctx is live, written back through
// "<store>.<pid>.tmp" (0600, fsync'd) and renamed into place. The directory
// is made 0700 if it is missing.
func (s *Store) Change(ctx context.Context, fn func(*File) error) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("token store: %w", err)
	}
	l, err := fsx.Lock(ctx, s.Path+".lock", fsx.Forever)
	if err != nil {
		return fmt.Errorf("token store lock: %w", err)
	}
	defer l.Unlock()
	k, err := statKey(s.Path)
	if err != nil {
		return err
	}
	f, err := s.read(k)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("token store not changed: %w", err)
	}
	if err := fsx.WriteAtomicSync(s.Path, f.Encode(), 0o600); err != nil {
		return fmt.Errorf("token store: %w", err)
	}
	return nil
}

// AddIn is `factory tokens add NAME --project P... --can C`.
type AddIn struct {
	Name     string
	Projects []string // --project, in order (duplicates are dropped)
	Can      string   // --can, comma-separated
	People   []string // [board] people: no token takes a person's name
	Known    []string // the projects config names, in its order
	Rand     io.Reader
}

// Added is a new token: the secret, shown once, and the line `tokens add`
// prints on stderr.
type Added struct {
	Token string
	Note  string
}

// Add makes a token (cmd_tokens add), refusing with Python's messages a bad
// or reserved name, a person's name, no or unknown projects, unknown
// capabilities and a name already in the store.
func (s *Store) Add(ctx context.Context, in AddIn) (Added, error) {
	name := in.Name
	if name == "" {
		return Added{}, refuse("factory tokens add NAME")
	}
	if !ValidName(name) {
		return Added{}, refuse("a token name is lower-case letters, digits and dashes, starting with a letter (at most 32)")
	}
	person := contains(in.People, name)
	if Reserved(name) || person {
		what := "reserved"
		if person {
			what = "a person in [board] people"
		}
		return Added{}, refuse("%s is %s; pick another name", name, what)
	}
	var projects []string
	for _, p := range in.Projects {
		if !contains(projects, p) {
			projects = append(projects, p)
		}
	}
	if len(projects) == 0 {
		return Added{}, refuse("name at least one --project")
	}
	var unknown []string
	for _, p := range projects {
		if !contains(in.Known, p) {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		return Added{}, refuse("unknown project %s; known: %s", strings.Join(unknown, ", "), strings.Join(in.Known, ", "))
	}
	var asked []string
	for _, c := range strings.Split(in.Can, ",") {
		if c = pyStrip(c); c != "" {
			asked = append(asked, c)
		}
	}
	all := Capabilities()
	bad := len(asked) == 0
	for _, c := range asked {
		bad = bad || !contains(all, c)
	}
	if bad {
		return Added{}, refuse("--can takes some of: %s", strings.Join(all, ","))
	}
	var can []string
	for _, c := range all {
		if c == "read" || contains(asked, c) {
			can = append(can, c)
		}
	}
	src := in.Rand
	if src == nil {
		src = rand.Reader
	}
	secret := make([]byte, 32)
	if _, err := io.ReadFull(src, secret); err != nil {
		return Added{}, fmt.Errorf("token: %w", err)
	}
	token := "fct_" + base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(token))
	rec := jsonx.ObjectOf(
		jsonx.Member{Key: "name", Value: jsonx.String(name)},
		jsonx.Member{Key: "sha256", Value: jsonx.String(hex.EncodeToString(sum[:]))},
		jsonx.Member{Key: "projects", Value: strList(projects)},
		jsonx.Member{Key: "can", Value: strList(can)},
		jsonx.Member{Key: "added", Value: jsonx.String(clock.Stamp(s.now()))},
		jsonx.Member{Key: "expires", Value: jsonx.Null()},
		jsonx.Member{Key: "tickets", Value: jsonx.Null()},
	)
	err := s.Change(ctx, func(f *File) error {
		for _, r := range f.Records() {
			if n, ok := r.Name(); ok && n == name {
				return refuse("there is already a token %s; revoke it first", name)
			}
		}
		f.recs = append(f.recs, rec)
		return nil
	})
	if err != nil {
		return Added{}, err
	}
	return Added{Token: token, Note: fmt.Sprintf("%s: %s on %s. The token is shown this once; it is not kept.",
		name, strings.Join(can, ","), strings.Join(projects, ", "))}, nil
}

// Revoke removes every record named name (cmd_tokens revoke); a name with
// none is "no token NAME", and nothing is written.
func (s *Store) Revoke(ctx context.Context, name string) error {
	if name == "" {
		return refuse("factory tokens revoke NAME")
	}
	return s.Change(ctx, func(f *File) error {
		var keep []jsonx.Value
		for _, v := range f.recs {
			if n, ok := (Record{v: v}).Name(); ok && n == name {
				continue
			}
			keep = append(keep, v)
		}
		if len(keep) == len(f.recs) {
			return refuse("no token %s", name)
		}
		f.recs = keep
		return nil
	})
}

// listKeys are the keys `tokens list` shows: never the hash.
func listKeys() []string { return []string{"name", "projects", "can", "added", "expires", "tickets"} }

// Rows is what `tokens list --json` prints: each record's listed keys
// (null when absent), never its hash.
func (s *Store) Rows() (jsonx.Value, error) {
	f, err := s.Load()
	if err != nil {
		return jsonx.Value{}, err
	}
	var rows []jsonx.Value
	for _, r := range f.Records() {
		o, _ := r.v.Object()
		row := jsonx.Object{}
		for _, k := range listKeys() {
			row = append(row, jsonx.Member{Key: k, Value: o.Get(k)})
		}
		rows = append(rows, row.Value())
	}
	return jsonx.ArrayOf(rows...), nil
}

// Lines is what `tokens list` prints, one line per record:
// name(24) caps(24) projects(30) added X[, expires Y].
func (s *Store) Lines() ([]string, error) {
	f, err := s.Load()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range f.Records() {
		o, _ := r.v.Object()
		line := ljust(jsonx.PyStr(o.Get("name")), 24) + " " + ljust(joinStrs(o.Get("can")), 24) + " " +
			ljust(joinStrs(o.Get("projects")), 30) + " added " + jsonx.PyStr(o.Get("added"))
		if jsonx.Truthy(o.Get("expires")) {
			line += ", expires " + jsonx.PyStr(o.Get("expires"))
		}
		out = append(out, line)
	}
	return out, nil
}

// CheckPerms is Foreman's storePerms: a store others can read, or one in a
// directory others can write, holds no tokens. Foreman never writes the
// store, so it refuses it rather than tightening it. A missing store is
// fine (it holds none).
func (s *Store) CheckPerms() error {
	fi, err := os.Stat(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("token store: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by others (%v); no API token works until it is 0600 and its tokens are re-made",
			s.Path, fi.Mode().Perm())
	}
	dir := filepath.Dir(s.Path)
	di, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("token store: %w", err)
	}
	if di.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by others; no API token works while the store is kept there", dir)
	}
	return nil
}

// Verify is the bearer check Foreman makes: the name of the live record
// whose sha256 matches secret (compared in constant time), and whether there
// is one. A loose or unreadable store matches nothing, and is logged once
// per refusal.
func (s *Store) Verify(secret string) (name string, ok bool) {
	s.match(secret, func(r Record, expired bool) {
		if n, isStr := r.Name(); isStr && !expired {
			name, ok = n, true
		}
	})
	return name, ok
}

// Stale reports a token this store issued whose record has expired: Foreman
// tells its caller so rather than "unauthorized".
func (s *Store) Stale(secret string) bool {
	stale := false
	s.match(secret, func(_ Record, expired bool) { stale = stale || expired })
	return stale
}

func (s *Store) match(secret string, each func(Record, bool)) {
	if secret == "" {
		return
	}
	s.mu.Lock()
	f, err := s.checkedLoad()
	s.mu.Unlock()
	if err != nil {
		return
	}
	sum := sha256.Sum256([]byte(secret))
	now := s.now()
	for _, r := range f.Records() {
		want, err := hex.DecodeString(r.str("sha256"))
		if err == nil && len(want) == len(sum) && subtle.ConstantTimeCompare(sum[:], want) == 1 {
			each(r, r.Expired(now))
		}
	}
}

// checkedLoad is Load with the permission check (on every call: a chmod
// changes neither the mtime nor the size), logging a refusal once.
func (s *Store) checkedLoad() (*File, error) {
	err := s.CheckPerms()
	var f *File
	if err == nil {
		f, err = s.loadLocked()
	}
	if err != nil {
		if msg := err.Error(); msg != s.warned {
			s.warned = msg
			if s.Log != nil {
				s.Log.Printf("%s", msg)
			}
		}
		return nil, err
	}
	s.warned = ""
	return f, nil
}

// Record is one token record, as stored.
type Record struct{ v jsonx.Value }

// Value is the record as stored (shared: do not change it).
func (r Record) Value() jsonx.Value { return r.v }

func (r Record) get(key string) jsonx.Value {
	o, _ := r.v.Object()
	return o.Get(key)
}

func (r Record) str(key string) string {
	s, _ := r.get(key).Str()
	return s
}

// Name is the record's name, when it is an object whose name is a string.
func (r Record) Name() (string, bool) {
	return r.get("name").Str()
}

// NameString is str(r.get("name")): what `factory check` prints.
func (r Record) NameString() string { return jsonx.PyStr(r.get("name")) }

// Projects are rec.get("projects") or [] as Python iterates them: a list's
// strings, a string's characters, an object's keys. Elements that are not
// strings match no project and are left out.
func (r Record) Projects() []string { return pyIter(r.get("projects")) }

// Can are rec.get("can") or [], iterated the same way.
func (r Record) Can() []string { return pyIter(r.get("can")) }

// Tickets is rec.get("tickets"): null when the token is not scoped to some
// tickets (the default), else what `tid in tickets` is asked of.
func (r Record) Tickets() jsonx.Value { return r.get("tickets") }

// Expires is rec.get("expires"), as stored.
func (r Record) Expires() jsonx.Value { return r.get("expires") }

// Expired is expired(rec): an expiry that is set (truthy) and at or before
// now. One that does not parse, has no offset (Python cannot compare it with
// an aware time) or is not a string has expired.
func (r Record) Expired(now time.Time) bool {
	v := r.get("expires")
	if !jsonx.Truthy(v) {
		return false
	}
	s, ok := v.Str()
	if !ok {
		return true
	}
	at, aware, err := clock.ParseISO(s)
	if err != nil || !aware {
		return true
	}
	return !at.After(now)
}

// pyIter is `for x in (v or [])` keeping the strings.
func pyIter(v jsonx.Value) []string {
	var out []string
	switch v.Kind() {
	case jsonx.KindArray:
		a, _ := v.Array()
		for _, e := range a {
			if s, ok := e.Str(); ok {
				out = append(out, s)
			}
		}
	case jsonx.KindString:
		s, _ := v.Str()
		for _, r := range s {
			out = append(out, string(r))
		}
	case jsonx.KindObject:
		o, _ := v.Object()
		out = o.Keys()
	}
	return out
}

func joinStrs(v jsonx.Value) string {
	if !jsonx.Truthy(v) {
		return ""
	}
	return strings.Join(pyIter(v), ",")
}

func strList(ss []string) jsonx.Value {
	vs := make([]jsonx.Value, len(ss))
	for i, s := range ss {
		vs[i] = jsonx.String(s)
	}
	return jsonx.ArrayOf(vs...)
}

// ljust is format(s, "<n"): padded with spaces to n code points.
func ljust(s string, n int) string {
	if l := len([]rune(s)); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
}

// pyStrip is str.strip() for the ASCII whitespace a --can value holds, and
// the Unicode whitespace Python strips too.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
