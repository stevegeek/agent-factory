package ticket

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustDecode(t *testing.T, b []byte) *Ticket {
	t.Helper()
	tk, _, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func parse(t *testing.T, s string) jsonx.Value {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// §3.1 property 2: typed changes to a ticket whose nested keys are in an
// order no struct has, with unknown keys at every depth, give exactly the
// bytes Python's dict operations give (testdata/nested/gen.py).
func TestTypedChangesKeepNestedOrderAndUnknownKeys(t *testing.T) {
	tk := mustDecode(t, mustRead(t, "testdata/nested/T0001.json"))
	s2 := "2026-10-18T10:00:00+00:00"
	tk.Column = "ready"
	pr := tk.PR.V
	pr.MergeRequested = Opt[string]{}
	pr.MergeWait = Some("main moved")
	pr.HoldRequested = Some(s2)
	pr.Draft = Some(true)
	tk.PR = Some(pr)
	tk.History = append(tk.History, Entry{At: s2, Event: "move", Detail: "pr_open -> ready: by human", Type: model.EventMoved,
		Data: Moved{From: "pr_open", To: "ready", Reason: model.ReasonByHuman, By: "maintainer"}})
	tk.SetAsk(nil)
	tk.Notes = append(tk.Notes, Note{At: s2, Text: "n2"})
	tk.Agent.V.Memory.V.Settled = true
	tk.Reports.Set("work", parse(t, `{"status": "done", "summary": "again"}`))
	tk.Reports.Set("triage", parse(t, `{"decision": "accept"}`))
	tk.Gate.V.ChangedLines = 4
	tk.Labels = []string{"x"}
	tk.Updated = s2
	want := mustRead(t, "testdata/nested/T0001.changed.json")
	if got := Encode(tk); !bytes.Equal(got, want) {
		t.Errorf("typed changes differ from Python's:\n%s", testkit.Diff(string(want), string(got)))
	}
}

// A ticket nobody changed is saved as the bytes it was read from, even when
// a person wrote it by hand (a trailing newline, other spacing).
func TestUnchangedTicketKeepsItsBytes(t *testing.T) {
	in := []byte("{\"id\":\"T0001\",  \"title\": \"caf\u00e9\", \"x\": 1.50}\n")
	tk := mustDecode(t, in)
	if got := Encode(tk); !bytes.Equal(got, in) {
		t.Errorf("got %q", got)
	}
	tk.Title = "tea"
	if got := string(Encode(tk)); got != "{\n  \"id\": \"T0001\",\n  \"title\": \"tea\",\n  \"x\": 1.5\n}" {
		t.Errorf("changed: got %q", got)
	}
}

// Opt tells absent, null and a value apart on the way back.
func TestOptKeepsAbsentNullAndValueApart(t *testing.T) {
	tk := mustDecode(t, []byte(`{"id": "T0001", "branch": null, "parent": "T0002", "agent": null}`))
	if !tk.Branch.Set || !tk.Branch.Null || tk.Kind.Set || tk.Parent.Or("") != "T0002" {
		t.Fatalf("decoded %+v %+v %+v", tk.Branch, tk.Kind, tk.Parent)
	}
	tk.Branch = Some("bot/x")
	tk.Parent = Opt[string]{}
	tk.Kind = Nil[string]()
	got := string(Encode(tk))
	want := "{\n  \"id\": \"T0001\",\n  \"branch\": \"bot/x\",\n  \"agent\": null,\n  \"kind\": null\n}"
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

// A new nested object is written whole, in Python's order, zero fields
// included where Python writes them.
func TestNewObjectsAreWrittenInPythonOrder(t *testing.T) {
	tk := mustDecode(t, []byte(`{"id": "T0001", "pr": {"open_requested": "s"}}`))
	pr := tk.PR.V
	pr.Number, pr.URL, pr.Draft = 7, "u", Some(false)
	pr.OpenRequested = Opt[string]{}
	tk.PR = Some(pr)
	tk.Gate = Some(Gate{ChecksPass: false, Files: []string{"a"}, Blockers: nil})
	got := string(Encode(tk))
	for _, want := range []string{`"pr": {
    "number": 7,
    "url": "u",
    "draft": false
  }`, `"gate": {
    "checks_pass": false,
    "files": [
      "a"
    ],
    "changed_lines": 0,
    "within_size": false,
    "outside_allowed_paths": [],
    "runtime_behaviour": null,
    "reasoning_stated": false,
    "head": "",
    "base": "",
    "auto_eligible": false,
    "blockers": []
  }`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in\n%s", want, got)
		}
	}
	// a fresh Foreman intent on a ticket with no PR is the intent alone
	tk = mustDecode(t, []byte(`{"id": "T0001"}`))
	tk.PR = Some(PR{OpenRequested: Some("s")})
	if got := string(Encode(tk)); !strings.Contains(got, "\"pr\": {\n    \"open_requested\": \"s\"\n  }") {
		t.Errorf("intent: %s", got)
	}
}

// A typed value over a wrong-typed one replaces it; a change beside a
// wrong-typed nested value keeps it.
func TestWrongTypedValuesAreReplacedOnlyWhenSet(t *testing.T) {
	tk := mustDecode(t, []byte(`{"id": "T0001", "security_tier": "high", "pr": {"number": "7", "x": 1}}`))
	tk.SecurityTier = Some(0)
	tk.PR.V.MergeRequested = Some("s")
	want := "{\n  \"id\": \"T0001\",\n  \"security_tier\": 0,\n  \"pr\": {\n    \"number\": \"7\",\n    \"x\": 1,\n    \"merge_requested\": \"s\"\n  }\n}"
	if got := string(Encode(tk)); got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

// Waiting writes its keys in the order of the path that made it.
func TestWaitingKeyOrderFollowsItsReason(t *testing.T) {
	tk := mustDecode(t, []byte(`{"id": "T0001", "waiting": null}`))
	tk.Waiting = Some(Waiting{Reason: "other", Text: "why", From: "doing", Since: "s"})
	if got := string(Encode(tk)); !strings.Contains(got, `"reason": "other",
    "text": "why",
    "from": "doing",
    "since": "s"
  }`) {
		t.Errorf("other: %s", got)
	}
	tk.Waiting = Nil[Waiting]()
	Encode(tk)
	tk.Waiting = Some(Waiting{Reason: "question", Question: 2, From: "doing", Since: "s", Text: "q?"})
	if got := string(Encode(tk)); !strings.Contains(got, `"reason": "question",
    "question": 2,
    "from": "doing",
    "since": "s",
    "text": "q?"
  }`) {
		t.Errorf("question: %s", got)
	}
}

// views is a ticket's typed views alone, for comparing two decodes.
func views(tk *Ticket) Ticket {
	v := *tk
	v.tree, v.raw, v.logs = jsonx.Value{}, nil, nil
	return v
}

// unknownPaths lists every path of the nested fixture's x_unknown keys.
func unknownPaths(v jsonx.Value, path string, out map[string]string) {
	if o, ok := v.Object(); ok {
		for _, m := range o {
			p := path + "." + m.Key
			if strings.HasPrefix(m.Key, "x_unknown") {
				out[p] = string(jsonx.EncodePython(m.Value, -1))
			}
			unknownPaths(m.Value, p, out)
		}
	}
	if a, ok := v.Array(); ok {
		for i, e := range a {
			unknownPaths(e, path+"["+string(rune('0'+i%10))+"]", out)
		}
	}
}

func TestCloneIsDeep(t *testing.T) {
	tk := mustDecode(t, mustRead(t, "testdata/nested/T0001.json"))
	before := Encode(tk)
	c := Clone(tk)
	c.Column = "done"
	c.PR.V.ReviewsSeen[0] = 99
	c.Notes[0].Text = "changed"
	c.Reports.Set("work", jsonx.String("x"))
	Move(c, "ready", model.ReasonByHuman, "", "", "", time.Unix(0, 0))
	if got := Encode(tk); !bytes.Equal(got, before) {
		t.Errorf("changing a clone changed the original:\n%s", testkit.Diff(string(before), string(got)))
	}
	if len(tk.Logs()) != 0 || len(c.Logs()) != 1 {
		t.Errorf("logs: original %v, clone %v", tk.Logs(), c.Logs())
	}
	if !reflect.DeepEqual(views(Clone(tk)), views(tk)) {
		t.Error("a clone's views differ from the original's")
	}
}

func TestDiffNamesEachChangedPath(t *testing.T) {
	before := mustDecode(t, mustRead(t, "testdata/nested/T0001.json"))
	after := Clone(before)
	after.PR.V.MergeRequested = Opt[string]{}
	after.PR.V.HoldRequested = Some("s")
	after.Gate.V.ChangedLines = 9
	AddNote(after, "n", "", nil, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
	var got []string
	for _, c := range Diff(before, after) {
		got = append(got, c.String())
	}
	want := []string{
		`history[5]: (absent) -> {"at": "2026-10-18T00:00:00+00:00", "event": "note", "detail": "n", "type": "noted", "by": "factory", "note": 1}`,
		`gate.changed_lines: 3 -> 9`,
		`pr.hold_requested: (absent) -> "s"`,
		`pr.merge_requested: "2026-10-17T09:00:00+00:00" -> (absent)`,
		`notes[1]: (absent) -> {"at": "2026-10-18T00:00:00+00:00", "text": "n"}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff:\n got %q\nwant %q", got, want)
	}
	if d := Diff(before, Clone(before)); len(d) != 0 {
		t.Errorf("a clone differs: %v", d)
	}
}
