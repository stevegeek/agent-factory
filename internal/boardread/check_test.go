package boardread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// factory check --board: every ticket file that does not read cleanly, and
// why. A clean board has nothing to say.
func TestCheckBoard(t *testing.T) {
	b := newBoard(t)
	b.put(t, "T0001", "fine")
	if problems := b.r.CheckBoard(); len(problems) != 0 {
		t.Fatalf("a clean board: %v", problems)
	}
	b.raw(t, "T0002.json", []byte(`{"id": "T0002", "title": 7}`))
	b.raw(t, "T0003.json", []byte(`{"id": `))
	b.raw(t, "T0004.json", []byte(`{"id": "T0005", "title": "x"}`))
	b.raw(t, "T12.json", []byte(`{"id": "T12", "title": "x"}`))
	got := map[string]string{}
	for _, p := range b.r.CheckBoard() {
		got[filepath.Base(p.File)] = p.String()
	}
	want := map[string]string{"T0002.json": "title", "T0003.json": "cannot be read", "T0004.json": "T0005", "T12.json": "not a ticket id"}
	for name, part := range want {
		if !strings.Contains(got[name], part) {
			t.Errorf("%s: %q, want it to mention %q", name, got[name], part)
		}
	}
	if len(got) != len(want) {
		t.Errorf("problems %v", got)
	}
}

// factory check --asks: every factory ticket in an ask column (needs_human,
// pr_open, ready_for_pr) without a stored ask, which Foreman does not show.
func TestCheckAsks(t *testing.T) {
	b := newBoard(t)
	put := func(id ticket.ID, column string, mode string, ask string) {
		tk := ticket.New(id, ticket.NewIn{Project: "p", Title: "x", Door: "human", Column: column}, testkit.Stamp())
		data := ticket.Encode(tk)
		v, _ := jsonx.Parse(data)
		o, _ := v.Object()
		if mode != "" {
			o.Set("mode", jsonx.String(mode))
		}
		if ask == "" {
			o.Delete("ask")
		} else {
			a, _ := jsonx.Parse([]byte(ask))
			o.Set("ask", a)
		}
		b.raw(t, string(id)+".json", jsonx.EncodePython(o.Value(), 2))
	}
	put("T0001", "needs_human", "", `{"kind": "unstick", "question": "q"}`) // stored
	put("T0002", "needs_human", "", "")                                     // missing
	put("T0003", "pr_open", "", "null")                                     // null
	put("T0004", "ready_for_pr", "", `{"kind": ""}`)                        // no kind
	put("T0005", "ready", "", "")                                           // not an ask column
	put("T0006", "needs_human", "board", "")                                // a board ticket asks otherwise
	put("T0007", "pr_open", "", `"review"`)                                 // wrong type
	var got []string
	for _, m := range b.r.CheckAsks() {
		got = append(got, string(m.ID)+":"+m.Column)
	}
	if s := strings.Join(got, ","); s != "T0002:needs_human,T0003:pr_open,T0004:ready_for_pr,T0007:pr_open" {
		t.Errorf("missing asks %s", s)
	}
}

// Every ticket of a private snapshot of the live board (FACTORY_LIVE_SNAPSHOT,
// skipped when unset) reads with no problem, and its index entries agree
// with the snapshot's own index.json wherever the ticket is unchanged since.
func TestLiveSnapshot(t *testing.T) {
	src := testkit.LiveSnapshot(t)
	b := newBoard(t)
	names, _ := filepath.Glob(filepath.Join(src, "board", "tickets", "T*.json"))
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		b.raw(t, filepath.Base(n), data)
	}
	ts, problems := b.r.List()
	if len(ts) != len(names) {
		t.Errorf("%d of %d tickets read", len(ts), len(names))
	}
	for _, p := range problems {
		t.Errorf("%v", p)
	}
	if missing := b.r.CheckAsks(); len(missing) != 0 {
		t.Logf("%d tickets in an ask column have no stored ask: %v", len(missing), missing)
	}
	data, err := os.ReadFile(filepath.Join(src, "board", "index.json"))
	if err != nil {
		t.Skip("no index.json in the snapshot")
	}
	old := ParseIndex(data)
	ix, _, err := b.r.ScanIndex(Index{})
	if err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, id := range old.IDs() {
		o, _ := old.Get(id)
		n, ok := ix.Get(id)
		if !ok || o.Str("updated") != n.Str("updated") {
			continue
		}
		compared++
		if a, b := compact(withoutStat(o.Object()).Value()), compact(withoutStat(n.Object()).Value()); a != b {
			t.Errorf("%s:\n%s", id, testkit.Diff(a, b))
		}
	}
	t.Logf("%d tickets, %d index entries compared", len(ts), compared)
}
