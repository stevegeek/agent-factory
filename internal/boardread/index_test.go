package boardread

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

func parse(t *testing.T, s string) jsonx.Value {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return v
}

func compact(v jsonx.Value) string { return string(jsonx.EncodePython(v, -1)) }

// withoutStat drops the stat triple, as the golden does.
func withoutStat(o jsonx.Object) jsonx.Object {
	o = o.Clone()
	for _, k := range []string{"ino", "mtime_ns", "size"} {
		o.Delete(k)
	}
	return o
}

// index_entry for every ticket of the golden board equals Python's, key
// order included; the stat triple is the file's own.
func TestIndexEntriesMatchPython(t *testing.T) {
	b := newBoard(t)
	src := filepath.Join("..", "ticketapi", "testdata", "py", "board", "tickets")
	names, err := filepath.Glob(filepath.Join(src, "T*.json"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no golden board: %v", err)
	}
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		b.raw(t, filepath.Base(n), data)
	}
	golden, err := os.ReadFile(filepath.Join("..", "store", "testdata", "py", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := parse(t, string(golden)).Get("tickets").Object()

	ix, changed, err := b.r.ScanIndex(Index{})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || ix.Len() != len(want) {
		t.Fatalf("changed %v, %d entries, want %d", changed, ix.Len(), len(want))
	}
	for _, m := range want {
		e, ok := ix.Get(m.Key)
		if !ok {
			t.Errorf("%s missing", m.Key)
			continue
		}
		w, _ := m.Value.Object()
		if got := compact(withoutStat(e.Object()).Value()); got != compact(w.Value()) {
			t.Errorf("%s:\n%s", m.Key, testkit.Diff(compact(w.Value()), got))
		}
		st, ok := e.Stat()
		var fi os.FileInfo
		if fi, err = os.Lstat(filepath.Join(b.p.Tickets, m.Key+".json")); err != nil {
			t.Fatal(err)
		}
		if !ok || st != StatOf(fi) || st.Size != fi.Size() || st.Ino == 0 {
			t.Errorf("%s: stat %+v", m.Key, st)
		}
	}
}

// index_entry's Python reading of odd values: `x or default`, str() of
// non-strings, an empty idempotency dict, mode null, lists that are not.
func TestIndexEntryOddValues(t *testing.T) {
	for _, c := range []struct{ name, ticket, want string }{
		{"empty", `{}`,
			`{"project": null, "mode": "factory", "kind": "change", "column": null, "assignee": null, "labels": [], "parent": null, "title": "", "created": null, "updated": null, "by": null, "idem": null, "open_for": [], "open_questions": 0, "answers_for": [], "wrote": {}, "waiting_reason": null}`},
		{"mode null, kind 0, labels {}, title 0", `{"mode": null, "kind": 0, "labels": {}, "title": 0, "by": "a", "body": 12.0}`,
			`{"project": null, "mode": null, "kind": "change", "column": null, "assignee": null, "labels": [], "parent": null, "title": "", "created": null, "updated": null, "by": "a", "idem": null, "open_for": [], "open_questions": 0, "answers_for": [], "wrote": {"a": 4}, "waiting_reason": null}`},
		{"board, truthy odd values", `{"mode": "board", "kind": [1], "labels": "x", "title": true, "by": "a", "body": [1, "é"]}`,
			`{"project": null, "mode": "board", "kind": [1], "column": null, "assignee": null, "labels": "x", "parent": null, "title": true, "created": null, "updated": null, "by": "a", "idem": null, "open_for": [], "open_questions": 0, "answers_for": [], "wrote": {"a": 12}, "waiting_reason": null}`},
		{"empty idempotency", `{"idempotency": {}, "waiting": {}}`,
			`{"project": null, "mode": "factory", "kind": "change", "column": null, "assignee": null, "labels": [], "parent": null, "title": "", "created": null, "updated": null, "by": null, "idem": null, "open_for": [], "open_questions": 0, "answers_for": [], "wrote": {}, "waiting_reason": null}`},
		{"idempotency, waiting", `{"idempotency": {"key": "k"}, "waiting": {"reason": "question"}}`,
			`{"project": null, "mode": "factory", "kind": "change", "column": null, "assignee": null, "labels": [], "parent": null, "title": "", "created": null, "updated": null, "by": null, "idem": [null, "k", null], "open_for": [], "open_questions": 0, "answers_for": [], "wrote": {}, "waiting_reason": "question"}`},
		{"questions, comments, history",
			`{"by": "a", "title": "\udc80x", "comments": [{"by": "b", "text": "hi"}, "junk", {"by": 5, "text": "x"}, {"by": "a"}],
			  "questions": [{"by": "q", "recipient": "z", "text": "?"}, {"by": "q", "recipient": "y", "answer": "", "answered_by": "y"},
			                {"recipient": null}, {"by": "q", "recipient": "z", "withdrawn": true}, {"recipient": 3, "withdrawn": 0}, 7],
			  "history": [{"by": "b", "detail": "d"}, {"detail": "no by"}, {"by": "c"}, "junk"]}`,
			`{"project": null, "mode": "factory", "kind": "change", "column": null, "assignee": null, "labels": [], "parent": null, "title": "\udc80x", "created": null, "updated": null, "by": "a", "idem": null, "open_for": ["3", "None", "z"], "open_questions": 3, "answers_for": ["q"], "wrote": {"a": 66, "b": 195, "q": 385, "c": 128}, "waiting_reason": null}`},
		{"questions not a list", `{"questions": {"by": "x"}, "comments": "abc", "history": 0}`,
			`{"project": null, "mode": "factory", "kind": "change", "column": null, "assignee": null, "labels": [], "parent": null, "title": "", "created": null, "updated": null, "by": null, "idem": null, "open_for": [], "open_questions": 0, "answers_for": [], "wrote": {}, "waiting_reason": null}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := EntryOf(parse(t, c.ticket))
			if got := compact(e.Object().Value()); got != compact(parse(t, c.want)) {
				t.Errorf("\n got %s\nwant %s", got, compact(parse(t, c.want)))
			}
		})
	}
}

// A scan reuses an entry whose (ino, mtime_ns, size) match, without reading
// the file, and re-reads the rest; a file that will not parse is left out.
func TestScanReusesEntriesByStat(t *testing.T) {
	b := newBoard(t)
	b.put(t, "T0001", "one")
	b.put(t, "T0002", "two")
	first, changed, err := b.r.ScanIndex(Index{})
	if err != nil || !changed || first.Len() != 2 {
		t.Fatalf("%v %v %d", err, changed, first.Len())
	}
	again, changed, err := b.r.ScanIndex(first)
	if err != nil || changed {
		t.Fatalf("an unchanged board changed the index: %v", err)
	}
	if compact(again.Tree()) != compact(first.Tree()) {
		t.Error("an unchanged board gave another index")
	}
	// an entry with a matching stat is trusted, not re-read: a planted title
	// shows, which proves the file was not read
	e, _ := first.Get("T0001")
	o := e.Object().Clone()
	o.Set("title", jsonx.String("from the old index"))
	tickets, _ := first.Tree().Get("tickets").Object()
	tickets = tickets.Clone()
	tickets.Set("T0001", o.Value())
	planted := ParseIndex(jsonx.EncodePython(jsonx.ObjectOf(
		jsonx.Member{Key: "version", Value: jsonx.Int(2)}, jsonx.Member{Key: "tickets", Value: tickets.Value()}), -1))
	reused, changed, _ := b.r.ScanIndex(planted)
	if e, _ := reused.Get("T0001"); changed || e.Str("title") != "from the old index" {
		t.Errorf("a matching entry was re-read (changed %v)", changed)
	}
	// a broken file is left out until it reads again
	b.raw(t, "T0002.json", []byte(`{"id": "T0002", "trunc`))
	broken, changed, _ := b.r.ScanIndex(first)
	if _, ok := broken.Get("T0002"); ok || !changed || broken.Len() != 1 {
		t.Errorf("a broken file stayed in: %v", broken.IDs())
	}
	// a link named like a ticket is never followed
	if err := os.Symlink(filepath.Join(b.p.Tickets, "T0001.json"), filepath.Join(b.p.Tickets, "T0003.json")); err != nil {
		t.Fatal(err)
	}
	linked, _, _ := b.r.ScanIndex(Index{})
	if _, ok := linked.Get("T0003"); ok {
		t.Error("a link was indexed")
	}
}

// json.load of index.json: bad JSON, the wrong version or tickets that are
// not an object all start empty.
func TestParseIndex(t *testing.T) {
	for _, junk := range []string{"{not json", `{"version": 99, "tickets": {"T9999": {}}}`, `{"version": 2, "tickets": []}`,
		`[]`, ``, `{"version": "2", "tickets": {"T9999": {}}}`, `{"version": true, "tickets": {"T9999": {}}}`} {
		if ix := ParseIndex([]byte(junk)); ix.Len() != 0 {
			t.Errorf("%q: %d entries", junk, ix.Len())
		}
	}
	ix := ParseIndex([]byte(`{"version": 2, "tickets": {"T0002": {"title": "b"}, "T0001": 5}}`))
	if ix.Len() != 2 || ix.IDs()[0] != "T0002" {
		t.Errorf("%v", ix.IDs())
	}
	if _, ok := ix.Get("T0001"); ok {
		t.Error("an entry that is not an object is no entry")
	}
	// 2.0 == 2 in Python
	if ix := ParseIndex([]byte(`{"version": 2.0, "tickets": {"T9999": {}}}`)); ix.Len() != 1 {
		t.Error("version 2.0 is version 2")
	}
}

// The index is read-only here: Reader.Index never writes index.json.
func TestReaderIndexWritesNothing(t *testing.T) {
	b := newBoard(t)
	b.put(t, "T0001", "one")
	ix, err := b.r.Index()
	if err != nil || ix.Len() != 1 {
		t.Fatalf("%v %d", err, ix.Len())
	}
	if _, err := os.Stat(b.p.Index); !os.IsNotExist(err) {
		t.Errorf("index.json written: %v", err)
	}
}

func TestEntryAccessors(t *testing.T) {
	e := EntryOf(parse(t, `{"project": "p", "idempotency": {"by": "a", "key": "k", "digest": "d"}, "by": "a", "title": "xy",
		"mode": "board", "column": "inbox"}`))
	if e.Str("project") != "p" || e.Str("column") != "inbox" || e.Str("kind") != "item" || e.Str("nope") != "" {
		t.Errorf("Str: %s", compact(e.Object().Value()))
	}
	if by, key, digest, ok := e.Idem(); !ok || by != "a" || key != "k" || digest != "d" {
		t.Errorf("Idem %q %q %q %v", by, key, digest, ok)
	}
	if e.Wrote("a") != 2 || e.Wrote("b") != 0 {
		t.Errorf("Wrote %d", e.Wrote("a"))
	}
	if _, ok := e.Stat(); ok {
		t.Error("an entry without stat has none")
	}
	if _, _, _, ok := EntryOf(parse(t, `{}`)).Idem(); ok {
		t.Error("no idem")
	}
}
