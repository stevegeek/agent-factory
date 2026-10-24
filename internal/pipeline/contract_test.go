package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/memory"
	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// port: TT ContractFixture.test_fixture_is_current
//
// The pipeline's part of the contract fixture.
//
// The change of the contract fixture, re-driven by the Go tick: filed into
// ready, through work (a model, a memory over its limit), review and
// security, to the gate. The file the ticks write must equal the one
// Python's real code paths wrote, after the fixture's normalisation (shas to
// zeros, the memory commit to 1234567) and with every time stamp blanked
// (the Python fixture's clock moves a minute per now() call, which counts
// calls, not decisions). The Ask and its answer that follow in the fixture
// are ops' and ticketapi's, so those two entries are left out.
func TestThePipelineWritesTheContractChange(t *testing.T) {
	h := newHarness(t)
	h.config(`[projects.p]
path = "{repo}"
base = "main"

[projects.sandbox]
mode = "board"

[models]
work = "claude-opus-5-5"

[memory]
roles = ["worker"]
max_kb = 1

[board]
people = ["alice"]

[hosts.helper]
ssh = "factory@helper"
slots = 1
model = "claude-sonnet-5-5"
`)
	d := h.deps()
	d.Memory = &memory.Service{Store: &memstore.Store{Paths: h.paths, Exec: execx.OS{}, Env: h.env.Environ()}, Paths: h.paths, Clock: h.clock}
	tick := func() {
		t.Helper()
		if err := Tick(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	tk, err := h.board.FileTicket(context.Background(), ticket.NewIn{Project: "p", Title: "Fix the parser",
		Body: "It drops the last line.", Door: "human", Column: "ready", By: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	id := tk.ID
	h.local.alive = true
	h.local.onStart = func(j hostrun.Job) {
		if j.Stage != model.StageWork {
			return
		}
		notes := filepath.Join(h.paths.Run, "memory", j.Name, "tickets")
		if err := os.MkdirAll(notes, 0o755); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(notes, string(id)+".md"), []byte("stopped at the parser\n"+strings.Repeat("detail\n", 300)), 0o644); err != nil {
			t.Error(err)
		}
		h.repo.BranchWith(j.Ticket.Branch.V, map[string]string{"lib/thing.rb": "def thing; 2; end\n"})
	}
	tick() // work starts
	h.local.reports["t0001-w1"] = []byte(`{"status": "done", "summary": "Kept the last line.", "category": "fix"}`)
	tick() // work finishes; review starts
	h.local.reports["t0001-r1"] = []byte(`{"verdict": "pass", "runtime_behaviour": "yes", "summary": "Fine."}`)
	tick() // review finishes; security starts
	var answers []string
	for _, q := range h.load(id).SecurityQuestions {
		answers = append(answers, `{"question": `+string(jsonx.EncodePython(jsonx.String(q), 0))+`, "answer": "no", "concern": "none"}`)
	}
	h.local.reports["t0001-s1"] = []byte(`{"verdict": "pass", "answers": [` + strings.Join(answers, ", ") + `], "summary": "Nothing."}`)
	// the gate waits: this host is full
	h.local.running["someone"], h.local.running["else"] = true, true
	tick() // security finishes; the change waits at the gate

	want, err := os.ReadFile(filepath.Join("..", "ticket", "testdata", "legacy", "contract", "T0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	wv, err := jsonx.Parse(want)
	if err != nil {
		t.Fatal(err)
	}
	gv, err := jsonx.Parse([]byte(h.raw(id)))
	if err != nil {
		t.Fatal(err)
	}
	wantText := string(jsonx.EncodePython(normalised("", dropAskAndAnswer(wv)), 2))
	gotText := string(jsonx.EncodePython(normalised("", gv), 2))
	if gotText != wantText {
		t.Fatalf("the pipeline's file differs from Python's:\n%s", testkit.Diff(wantText, gotText))
	}
}

// dropAskAndAnswer leaves out the fixture's asked and answered entries.
func dropAskAndAnswer(v jsonx.Value) jsonx.Value {
	obj, _ := v.Object()
	out := make(jsonx.Object, 0, len(obj))
	for _, m := range obj {
		if m.Key == "history" {
			arr, _ := m.Value.Array()
			var keep []jsonx.Value
			for _, e := range arr {
				if ev, _ := e.Get("event").Str(); ev != "asked" && ev != "answered" {
					keep = append(keep, e)
				}
			}
			m.Value = jsonx.ArrayOf(keep...)
		}
		out = append(out, m)
	}
	return jsonx.ObjectOf(out...)
}

// normalised is the fixture's normalised(), plus blank time stamps.
func normalised(key string, v jsonx.Value) jsonx.Value {
	if obj, ok := v.Object(); ok {
		out := make(jsonx.Object, len(obj))
		for i, m := range obj {
			out[i] = jsonx.Member{Key: m.Key, Value: normalised(m.Key, m.Value)}
		}
		return jsonx.ObjectOf(out...)
	}
	if arr, ok := v.Array(); ok {
		out := make([]jsonx.Value, len(arr))
		for i, e := range arr {
			out[i] = normalised("", e)
		}
		return jsonx.ArrayOf(out...)
	}
	s, ok := v.Str()
	if !ok {
		return v
	}
	if key == "commit" {
		return jsonx.String("1234567")
	}
	if _, err := clock.Parse(s); err == nil && len(s) == len("2026-10-17T08:00:00+00:00") {
		return jsonx.String("<stamp>")
	}
	return jsonx.String(memoryShort(zeroShas(s)))
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

func isWordByte(c byte) bool {
	return isHex(c) || c >= 'g' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// zeroShas is re.sub(r"\b[0-9a-f]{40}\b", "0"*40, s).
func zeroShas(s string) string {
	b := []byte(s)
	for i := 0; i+40 <= len(b); i++ {
		if i > 0 && isWordByte(b[i-1]) {
			continue
		}
		j := i
		for j < len(b) && j-i < 40 && isHex(b[j]) {
			j++
		}
		if j-i == 40 && (j == len(b) || !isWordByte(b[j])) {
			copy(b[i:j], strings.Repeat("0", 40))
			i = j - 1
		}
	}
	return string(b)
}

// memoryShort is re.sub(r"(memory )[0-9a-f]{7,12}(:)", r"\g<1>1234567\2", s).
func memoryShort(s string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, "memory ")
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		j := i + len("memory ")
		k := j
		for k < len(s) && isHex(s[k]) {
			k++
		}
		if n := k - j; n >= 7 && n <= 12 && k < len(s) && s[k] == ':' {
			out.WriteString(s[:j] + "1234567")
			s = s[k:]
			continue
		}
		out.WriteString(s[:j])
		s = s[j:]
	}
}
