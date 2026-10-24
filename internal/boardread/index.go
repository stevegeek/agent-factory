package boardread

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// IndexVersion is index.json's version (INDEX_VERSION).
const IndexVersion = 2

// MaxIndexBytes caps the read of index.json; a larger one is rebuilt.
const MaxIndexBytes = 256 << 20

// Stat is the key an index entry (and the snapshot's cache) is reused by:
// a file whose inode, mtime in nanoseconds and size are unchanged is not
// read again.
type Stat struct {
	Ino     uint64
	MtimeNS int64
	Size    int64
}

// StatOf is the key of an lstat result.
func StatOf(fi fs.FileInfo) Stat {
	st := Stat{Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.Ino = sys.Ino
		st.MtimeNS = sys.Mtim.Sec*1e9 + sys.Mtim.Nsec
	}
	return st
}

// Index is board/index.json's {id: entry}, a cache rebuilt from the ticket
// files (ticket_index). The entries are in the order of the directory scan
// that made them, as Python writes them. An Index is read-only once made.
type Index struct {
	entries jsonx.Object
	pos     map[string]int
}

// Entry is one ticket's index entry: index_entry's dict, in its key order,
// with the values as the ticket holds them. It is read-only.
type Entry struct{ obj jsonx.Object }

func newIndex(entries jsonx.Object) Index {
	ix := Index{entries: entries, pos: make(map[string]int, len(entries))}
	for i, m := range entries {
		ix.pos[m.Key] = i
	}
	return ix
}

// Len is the number of entries.
func (ix Index) Len() int { return len(ix.entries) }

// IDs are the entries' ids, in the index's order.
func (ix Index) IDs() []string { return ix.entries.Keys() }

// Get is the entry of a ticket, if there is one and it is an object.
func (ix Index) Get(id string) (Entry, bool) {
	i, ok := ix.pos[id]
	if !ok {
		return Entry{}, false
	}
	o, ok := ix.entries[i].Value.Object()
	return Entry{o}, ok
}

// Tree is index.json as Python writes it: {"version": 2, "tickets": ...}.
func (ix Index) Tree() jsonx.Value {
	return jsonx.ObjectOf(
		jsonx.Member{Key: "version", Value: jsonx.Int(IndexVersion)},
		jsonx.Member{Key: "tickets", Value: ix.entries.Clone().Value()},
	)
}

// Encode is json.dump(index) with no indent.
func (ix Index) Encode() []byte { return jsonx.EncodePython(ix.Tree(), -1) }

// ParseIndex reads index.json as ticket_index does: bad JSON, a version
// other than 2, or tickets that are not an object give an empty index.
func ParseIndex(data []byte) Index {
	if !utf8.Valid(data) {
		return Index{}
	}
	v, err := jsonx.Parse(data)
	if err != nil {
		return Index{}
	}
	o, ok := v.Object()
	if !ok {
		return Index{}
	}
	// data.get("version") != 2: a number equal to 2 (2.0 too) is version 2
	if v := o.Get("version"); v.Kind() != jsonx.KindNumber {
		return Index{}
	} else if f, _ := v.Float64(); f != IndexVersion {
		return Index{}
	}
	tickets, ok := o.Get("tickets").Object()
	if !ok {
		return Index{}
	}
	return newIndex(tickets)
}

// ReadIndex reads board/index.json, empty when it is missing or bad. It
// takes no lock: a reader sees the old file or the new one.
func (r *Reader) ReadIndex() Index {
	data, err := readFile(r.Paths.Index, MaxIndexBytes)
	if err != nil {
		return Index{}
	}
	return ParseIndex(data)
}

// Index is the index as it is now, from index.json and a scan of the
// ticket files, without writing anything (store.Index.Refresh writes it).
func (r *Reader) Index() (Index, error) {
	ix, _, err := r.ScanIndex(r.ReadIndex())
	return ix, err
}

// ScanIndex is ticket_index's refresh of old against the files: it takes
// the names matching T\d{4,6}.json that are regular files (never a link),
// reuses an entry whose (ino, mtime_ns, size) match, re-reads the others,
// and leaves out a file that will not parse, until it does. changed is
// Python's test for rewriting index.json: an entry was re-read, or ids
// appeared or vanished. The scan is in directory order, as os.scandir.
func (r *Reader) ScanIndex(old Index) (Index, bool, error) {
	d, err := os.Open(r.Paths.Tickets)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return newIndex(nil), old.Len() != 0, nil
		}
		return Index{}, false, fmt.Errorf("scan the board: %w", err)
	}
	entries, err := d.ReadDir(-1) // unsorted: directory order
	d.Close()
	if err != nil {
		return Index{}, false, fmt.Errorf("scan the board: %w", err)
	}
	var out jsonx.Object
	changed := false
	for _, e := range entries {
		id, ok := TicketFileName(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // gone since the listing
		}
		st := StatOf(fi)
		if cur, ok := old.Get(string(id)); ok {
			if have, ok := cur.Stat(); ok && have == st {
				out = append(out, jsonx.Member{Key: string(id), Value: cur.obj.Value()})
				continue
			}
		}
		data, err := readFile(filepath.Join(r.Paths.Tickets, e.Name()), MaxTicketBytes)
		if err != nil || !utf8.Valid(data) {
			continue // half-written or broken: left out until it reads
		}
		v, err := jsonx.Parse(data)
		if err != nil {
			continue
		}
		if _, ok := v.Object(); !ok {
			continue
		}
		out = append(out, jsonx.Member{Key: string(id), Value: entryOf(v, &st).Value()})
		changed = true
	}
	ix := newIndex(out)
	return ix, changed || !sameIDs(ix, old), nil
}

func sameIDs(a, b Index) bool {
	if a.Len() != b.Len() {
		return false
	}
	for id := range a.pos {
		if _, ok := b.pos[id]; !ok {
			return false
		}
	}
	return true
}

// EntryOf is index_entry(t) of a ticket's tree, without the stat triple:
// what summary_of reads.
func EntryOf(tree jsonx.Value) Entry { return Entry{entryOf(tree, nil)} }

// entryOf is index_entry, reading the dict as Python does.
func entryOf(tree jsonx.Value, st *Stat) jsonx.Object {
	t, _ := tree.Object()
	get := t.Get
	mode, ok := t.Lookup("mode")
	if !ok {
		mode = jsonx.String("factory")
	}
	board := false
	if s, ok := mode.Str(); ok && s == "board" {
		board = true
	}
	qs := objects(get("questions"))
	var open []jsonx.Object
	for _, q := range qs {
		if isOpen(q) {
			open = append(open, q)
		}
	}
	idem, _ := get("idempotency").Object()

	var wrote jsonx.Object
	add := func(name jsonx.Value, n int) {
		s, ok := name.Str()
		if !ok || n == 0 {
			return
		}
		if i := wrote.Index(s); i >= 0 {
			c, _ := wrote[i].Value.Int64()
			wrote[i].Value = jsonx.Int(c + int64(n))
			return
		}
		wrote = append(wrote, jsonx.Member{Key: s, Value: jsonx.Int(int64(n))})
	}
	text := func(v jsonx.Value) int { return pyLen(jsonx.PyStr(or(v, jsonx.String("")))) }
	add(get("by"), text(get("title"))+text(get("body")))
	for _, c := range objects(get("comments")) {
		add(c.Get("by"), text(c.Get("text"))+64)
	}
	for _, q := range qs {
		add(q.Get("by"), text(q.Get("text"))+128)
		add(q.Get("answered_by"), text(q.Get("answer")))
	}
	for _, h := range objects(get("history")) {
		add(h.Get("by"), text(h.Get("detail"))+128)
	}

	kind := "change"
	if board {
		kind = "item"
	}
	idemV := jsonx.Null()
	if len(idem) > 0 {
		idemV = jsonx.ArrayOf(idem.Get("by"), idem.Get("key"), idem.Get("digest"))
	}
	var openFor, answersFor []string
	for _, q := range open {
		openFor = append(openFor, jsonx.PyStr(q.Get("recipient")))
	}
	for _, q := range qs {
		if !q.Get("answer").IsNull() {
			answersFor = append(answersFor, jsonx.PyStr(q.Get("by")))
		}
	}
	waiting := jsonx.Null()
	if w, ok := get("waiting").Object(); ok {
		waiting = w.Get("reason")
	}
	if wrote == nil {
		wrote = jsonx.Object{}
	}
	e := jsonx.Object{
		{Key: "project", Value: get("project")},
		{Key: "mode", Value: mode},
		{Key: "kind", Value: or(get("kind"), jsonx.String(kind))},
		{Key: "column", Value: get("column")},
		{Key: "assignee", Value: get("assignee")},
		{Key: "labels", Value: or(get("labels"), jsonx.ArrayOf())},
		{Key: "parent", Value: get("parent")},
		{Key: "title", Value: or(get("title"), jsonx.String(""))},
		{Key: "created", Value: get("created")},
		{Key: "updated", Value: get("updated")},
		{Key: "by", Value: get("by")},
		{Key: "idem", Value: idemV},
		{Key: "open_for", Value: sortedSet(openFor)},
		{Key: "open_questions", Value: jsonx.Int(int64(len(open)))},
		{Key: "answers_for", Value: sortedSet(answersFor)},
		{Key: "wrote", Value: wrote.Value()},
		{Key: "waiting_reason", Value: waiting},
	}
	for i := range e {
		e[i].Value = e[i].Value.Clone()
	}
	if st != nil {
		e = append(e,
			jsonx.Member{Key: "ino", Value: jsonx.Int(int64(st.Ino))},
			jsonx.Member{Key: "mtime_ns", Value: jsonx.Int(st.MtimeNS)},
			jsonx.Member{Key: "size", Value: jsonx.Int(st.Size)})
	}
	return e
}

// objects are the dicts among `v or []`: only a list has any.
func objects(v jsonx.Value) []jsonx.Object {
	a, _ := v.Array()
	var out []jsonx.Object
	for _, x := range a {
		if o, ok := x.Object(); ok {
			out = append(out, o)
		}
	}
	return out
}

// isOpen is is_open(q): no answer (absent or null) and not withdrawn.
func isOpen(q jsonx.Object) bool {
	return q.Get("answer").IsNull() && !jsonx.Truthy(q.Get("withdrawn"))
}

// sortedSet is sorted({...}) of strings: by code point, which is byte
// order for UTF-8 and for jsonx's WTF-8 surrogates alike.
func sortedSet(ss []string) jsonx.Value {
	sort.Strings(ss)
	out := []jsonx.Value{}
	for i, s := range ss {
		if i > 0 && ss[i-1] == s {
			continue
		}
		out = append(out, jsonx.String(s))
	}
	return jsonx.ArrayOf(out...)
}

// Object is the entry as stored (shared: do not change it).
func (e Entry) Object() jsonx.Object { return e.obj }

// Get is one value of the entry (null when absent).
func (e Entry) Get(key string) jsonx.Value { return e.obj.Get(key) }

// Str is a string value of the entry, "" when absent or not a string.
func (e Entry) Str(key string) string {
	s, _ := e.obj.Get(key).Str()
	return s
}

// Stat is the entry's (ino, mtime_ns, size), when it holds all three as
// integers.
func (e Entry) Stat() (Stat, bool) {
	ino, ok1 := intOf(e.obj, "ino")
	mt, ok2 := intOf(e.obj, "mtime_ns")
	size, ok3 := intOf(e.obj, "size")
	if !ok1 || !ok2 || !ok3 || ino < 0 {
		return Stat{}, false
	}
	return Stat{Ino: uint64(ino), MtimeNS: mt, Size: size}, true
}

func intOf(o jsonx.Object, key string) (int64, bool) {
	v, ok := o.Lookup(key)
	if !ok || !v.IsInt() {
		return 0, false
	}
	return v.Int64()
}

// Idem is the [by, key, digest] a token-filed create was made with, when
// all three are strings.
func (e Entry) Idem() (by, key, digest string, ok bool) {
	a, isArr := e.obj.Get("idem").Array()
	if !isArr || len(a) != 3 {
		return "", "", "", false
	}
	var okBy, okKey, okDigest bool
	by, okBy = a[0].Str()
	key, okKey = a[1].Str()
	digest, okDigest = a[2].Str()
	return by, key, digest, okBy && okKey && okDigest
}

// Wrote is roughly how many bytes name has put in the ticket: the basis of
// a token's quota.
func (e Entry) Wrote(name string) int64 {
	w, _ := e.obj.Get("wrote").Object()
	n, _ := w.Get(name).Int64()
	return n
}
