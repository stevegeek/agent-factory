package ticket

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// mutation is one random typed change, applied only to a ticket that has
// what it changes.
type mutation struct {
	name  string
	needs func(tk *Ticket) bool
	do    func(r *rand.Rand, tk *Ticket)
}

func has[T any](o Opt[T]) bool { return o.Set && !o.Null }

func treeHas(tk *Ticket, key string, kind jsonx.Kind) bool { return tk.tree.Get(key).Kind() == kind }

func always(*Ticket) bool { return true }

func mutations(t *testing.T) []mutation {
	now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	return []mutation{
		{"move", always, func(r *rand.Rand, tk *Ticket) {
			Move(tk, []string{"ready", "needs_human", "pr_open", "done"}[r.Intn(4)], model.ReasonByHuman, "by human", "", "maintainer", now)
		}},
		{"note", always, func(r *rand.Rand, tk *Ticket) { AddNote(tk, "note", "maintainer", nil, now) }},
		{"merge requested", func(tk *Ticket) bool { return has(tk.PR) }, func(r *rand.Rand, tk *Ticket) { tk.PR.V.MergeRequested = Some(clock.Stamp(now)) }},
		{"merge request cleared", func(tk *Ticket) bool { return has(tk.PR) }, func(r *rand.Rand, tk *Ticket) { tk.PR.V.MergeRequested = Opt[string]{} }},
		{"review seen", func(tk *Ticket) bool { return has(tk.PR) }, func(r *rand.Rand, tk *Ticket) {
			tk.PR.V.ReviewsSeen = append(tk.PR.V.ReviewsSeen, int64(r.Intn(100)))
		}},
		{"draft", func(tk *Ticket) bool { return has(tk.PR) }, func(r *rand.Rand, tk *Ticket) { tk.PR.V.Draft = Some(r.Intn(2) == 0) }},
		{"changed lines", func(tk *Ticket) bool { return has(tk.Gate) }, func(r *rand.Rand, tk *Ticket) { tk.Gate.V.ChangedLines = r.Intn(50) }},
		{"blocker", func(tk *Ticket) bool { return has(tk.Gate) }, func(r *rand.Rand, tk *Ticket) { tk.Gate.V.Blockers = append(tk.Gate.V.Blockers, "b") }},
		{"memory settled", func(tk *Ticket) bool { return has(tk.Agent) && has(tk.Agent.V.Memory) }, func(r *rand.Rand, tk *Ticket) {
			tk.Agent.V.Memory.V.Settled = r.Intn(2) == 0
		}},
		{"agent host", func(tk *Ticket) bool { return has(tk.Agent) }, func(r *rand.Rand, tk *Ticket) { tk.Agent.V.Host = "helper" }},
		{"title", always, func(r *rand.Rand, tk *Ticket) { tk.Title = "title " + string(rune('a'+r.Intn(26))) }},
		{"label", func(tk *Ticket) bool { return treeHas(tk, "labels", jsonx.KindArray) }, func(r *rand.Rand, tk *Ticket) {
			tk.Labels = append(tk.Labels, "l")
		}},
		{"security tier", always, func(r *rand.Rand, tk *Ticket) { tk.SecurityTier = Some(r.Intn(5)) }},
		{"history detail", func(tk *Ticket) bool { return len(tk.History) > 2 }, func(r *rand.Rand, tk *Ticket) {
			tk.History[2].Detail = "edited detail"
		}},
		{"start", always, func(r *rand.Rand, tk *Ticket) {
			RecordStart(tk, StartIn{Stage: model.StageWork, Agent: "t0001-w3", Host: "local", Branch: "bot/x"}, now)
		}},
		{"report", func(tk *Ticket) bool { return treeHas(tk, "reports", jsonx.KindObject) }, func(r *rand.Rand, tk *Ticket) {
			tk.Reports.Set("work", parse(t, `{"summary": "s", "n": 4.0}`))
		}},
		{"report removed", func(tk *Ticket) bool { return treeHas(tk, "reports", jsonx.KindObject) }, func(r *rand.Rand, tk *Ticket) {
			tk.Reports.Delete("security")
		}},
		{"source", always, func(r *rand.Rand, tk *Ticket) { tk.Source = Some(Source{Issue: 12, URL: "u"}) }},
		{"question", always, func(r *rand.Rand, tk *Ticket) {
			tk.Question = Some(Question{Text: "q", Stage: "work", Agent: "a", At: "s"})
		}},
	}
}

// withUnknownKeys adds an unknown key at the top of a ticket and inside
// its pr, agent, gate and first history entry when they are objects, so
// every fixture carries unknown keys at several depths. The nested
// fixture has its own already.
func withUnknownKeys(t *testing.T, b []byte) []byte {
	t.Helper()
	tree := parse(t, string(b))
	top, ok := tree.Object()
	if !ok {
		t.Fatal("a ticket that is not an object")
	}
	for _, key := range []string{"pr", "agent", "gate"} {
		if o, ok := top.Get(key).Object(); ok {
			o = o.Clone()
			o.Set("x_unknown_in_"+key, jsonx.String("kept"))
			top.Set(key, o.Value())
		}
	}
	if h, ok := top.Get("history").Array(); ok && len(h) > 0 {
		if e, ok := h[0].Object(); ok {
			h = append([]jsonx.Value{}, h...)
			e = e.Clone()
			e.Set("x_unknown_in_entry", jsonx.ArrayOf(jsonx.Int(1), jsonx.String("two")))
			h[0] = e.Value()
			top.Set("history", jsonx.ArrayOf(h...))
		}
	}
	top.Set("x_unknown_top", jsonx.ObjectOf(jsonx.Member{Key: "n", Value: parse(t, "4.0")}))
	return jsonx.EncodePython(top.Value(), 2)
}

// mutationProperty is the mutation property on one ticket file: random
// typed mutations; Decode(Encode(t)) gives the same views, a second save
// changes nothing, and every unknown key is still there with its value.
func mutationProperty(t *testing.T, name string, base []byte, seeds int64) {
	t.Helper()
	unknown := map[string]string{}
	unknownPaths(parse(t, string(base)), "", unknown)
	if len(unknown) == 0 {
		t.Fatalf("%s: no unknown keys to follow", name)
	}
	all := mutations(t)
	for seed := int64(1); seed <= seeds; seed++ {
		r := rand.New(rand.NewSource(seed))
		tk := mustDecode(t, base)
		var applied []string
		for range 1 + r.Intn(8) {
			m := all[r.Intn(len(all))]
			if !m.needs(tk) {
				continue // the fixture lacks what it changes
			}
			m.do(r, tk)
			applied = append(applied, m.name)
		}
		out := Encode(tk)
		again := mustDecode(t, out)
		if !reflect.DeepEqual(views(again), views(tk)) {
			t.Fatalf("%s seed %d %v: views changed through a save:\n got %+v\nwant %+v", name, seed, applied, views(again), views(tk))
		}
		if !bytes.Equal(Encode(again), out) {
			t.Fatalf("%s seed %d %v: a second save changed the file", name, seed, applied)
		}
		have := map[string]string{}
		unknownPaths(again.Tree(), "", have)
		for p, v := range unknown {
			if have[p] != v {
				t.Fatalf("%s seed %d %v: unknown key %s is %q, was %q", name, seed, applied, p, have[p], v)
			}
		}
		if again.tree.Get("security_tier").Kind() == jsonx.KindString && tk.SecurityTier.Set {
			t.Fatalf("%s seed %d: a typed tier left the wrong-typed one", name, seed)
		}
	}
}

// The mutation property, on every fixture: the nested-order fixture with
// its own unknown keys at every depth (200 seeds), and every legacy,
// contract, history-order and new ticket with unknown keys added (20
// seeds each).
func TestRandomMutationsKeepViewsAndUnknownKeys(t *testing.T) {
	nested := mustRead(t, "testdata/nested/T0001.json")
	unknown := map[string]string{}
	unknownPaths(parse(t, string(nested)), "", unknown)
	if len(unknown) != 6 {
		t.Fatalf("the nested fixture has %d unknown keys: %v", len(unknown), unknown)
	}
	mutationProperty(t, "nested/T0001.json", nested, 200)
	n := 0
	for _, f := range allFixtures(t) {
		if filepath.Base(filepath.Dir(f.name)) == "nested" {
			continue
		}
		mutationProperty(t, f.name, withUnknownKeys(t, f.data), 20)
		n++
	}
	if n < 20 {
		t.Errorf("the property ran on %d fixtures besides the nested one", n)
	}
}

// The same on the live board, when a private snapshot is at hand
// (FACTORY_LIVE_SNAPSHOT).
func TestRandomMutationsOnTheLiveBoard(t *testing.T) {
	dir := testkit.LiveSnapshot(t)
	paths, _ := filepath.Glob(filepath.Join(dir, "board", "tickets", "T*.json"))
	if len(paths) == 0 {
		t.Skip("no tickets in the live snapshot")
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		mutationProperty(t, filepath.Base(p), withUnknownKeys(t, b), 5)
	}
}
