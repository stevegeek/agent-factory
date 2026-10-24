package jobproto

import (
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

func mustParse(t *testing.T, s string) jsonx.Value {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return v
}

// port: TF FactoryRemoteTest.test_meta_is_validated_and_clamped
func TestMetaIsValidatedAndClamped(t *testing.T) {
	ok, err := CheckMeta(mustParse(t, `{"branch": "bot/x", "start": "main", "timeout_minutes": 9999}`))
	if err != nil {
		t.Fatal(err)
	}
	if ok.Model != "claude-sonnet-5-5" || ok.TimeoutMinutes != 120 || ok.Bundle {
		t.Errorf("got %+v", ok)
	}
	for _, bad := range []string{
		`{"branch": "../x", "start": "main"}`, `{"branch": "-x", "start": "main"}`,
		`{"branch": "a b", "start": "main"}`, `{"branch": "x", "start": "main", "model": "--help"}`,
		`{"branch": "x", "start": "main", "model": "a;b"}`, `["not", "a", "dict"]`,
	} {
		if _, err := CheckMeta(mustParse(t, bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

// The golden is check_meta run by Python over a corpus of metas
// (tools/goldens.py). Each row's out is compared field by field; branch and
// start as str() of what Python returned.
func TestMetaGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/py/meta.json")
	if err != nil {
		t.Fatal(err)
	}
	doc := mustParse(t, string(b))
	rows, _ := doc.Get("rows").Array()
	if len(rows) < 20 {
		t.Fatalf("only %d rows", len(rows))
	}
	for _, row := range rows {
		in := row.Get("in")
		name := string(jsonx.EncodePython(in, -1))
		got, err := CheckMeta(in)
		// Deliberate difference: a trailing newline is refused (Python's $).
		if br, _ := in.Get("branch").Str(); strings.HasSuffix(br, "\n") {
			if err == nil {
				t.Errorf("%s: a trailing newline was accepted", name)
			}
			continue
		}
		if e, ok := row.Get("error").Str(); ok {
			if err == nil {
				t.Errorf("%s: accepted, Python said %s", name, e)
				continue
			}
			if want := strings.TrimPrefix(e, "ValueError: "); err.Error() != want {
				t.Errorf("%s: error %q, Python %q", name, err, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		out := row.Get("out")
		model, _ := out.Get("model").Str()
		bundle, _ := out.Get("bundle").Bool()
		minutes, _ := out.Get("timeout_minutes").Int64()
		want := Meta{Branch: jsonx.PyStr(out.Get("branch")), Start: jsonx.PyStr(out.Get("start")), Model: model,
			Bundle: bundle, TimeoutMinutes: int(minutes), Harness: DefaultHarness}
		if got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
}

func TestMetaEdges(t *testing.T) {
	for _, c := range []struct {
		in      string
		minutes int
		bundle  bool
		model   string
	}{
		{`{"branch": "x", "start": "y", "timeout_minutes": " \u0663_0 "}`, 30, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": "+5"}`, 5, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": "1__0"}`, 45, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": "1_"}`, 45, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": "0x10"}`, 45, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": "\ud835\udfd7"}`, 9, false, DefaultModel}, // MATHEMATICAL BOLD DIGIT NINE
		{`{"branch": "x", "start": "y", "timeout_minutes": -3.5}`, 1, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": 1e30}`, 120, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": 123456789012345678901234567890}`, 120, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": NaN}`, 45, false, DefaultModel},
		{`{"branch": "x", "start": "y", "timeout_minutes": [1]}`, 45, false, DefaultModel},
		{`{"branch": "x", "start": "y", "bundle": 0.0}`, 45, false, DefaultModel},
		{`{"branch": "x", "start": "y", "bundle": [0]}`, 45, true, DefaultModel},
		{`{"branch": "x", "start": "y", "model": 0}`, 45, false, DefaultModel},
		{`{"branch": "x", "start": "y", "model": 5}`, 45, false, "5"},
		{`{"branch": "x", "start": "y", "model": 2.5}`, 45, false, "2.5"},
		{`{"branch": null, "start": "y"}`, 45, false, DefaultModel}, // str(None) is "None", a valid name
	} {
		m, err := CheckMeta(mustParse(t, c.in))
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if m.TimeoutMinutes != c.minutes || m.Bundle != c.bundle || m.Model != c.model {
			t.Errorf("%s: got %+v", c.in, m)
		}
	}
	if _, err := CheckMeta(mustParse(t, `{"branch": "x", "start": "y", "timeout_minutes": Infinity}`)); err == nil {
		t.Error("an infinite timeout was accepted (Python crashes on it)")
	}
	for _, in := range []string{`{"branch": "x\n", "start": "y"}`, `{"branch": "x", "start": "y", "model": "m\n"}`,
		`{"branch": true, "start": "y"}`, `{"branch": "x", "start": "y", "harness": "Pi"}`, `{"branch": "x", "start": "y", "harness": "-x"}`} {
		m, err := CheckMeta(mustParse(t, in))
		if strings.Contains(in, "true") {
			if err != nil || m.Branch != "True" {
				t.Errorf("%s: %+v %v", in, m, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted", in)
		}
	}
	m, err := CheckMeta(mustParse(t, `{"branch": "x", "start": "y", "harness": "claude-code"}`))
	if err != nil || m.Harness != "claude-code" {
		t.Errorf("harness: %+v %v", m, err)
	}
}

// meta.json as start_remote writes it: json.dumps of the five keys.
func TestMetaEncodeIsPythons(t *testing.T) {
	for _, c := range []struct {
		m    Meta
		want string
	}{
		{Meta{Branch: "triage", Start: "main", TimeoutMinutes: 30},
			`{"branch": "triage", "start": "main", "model": "", "bundle": false, "timeout_minutes": 30}`},
		{Meta{Branch: "bot/é", Start: "main", Model: "m", Bundle: true, TimeoutMinutes: 60, Harness: DefaultHarness},
			`{"branch": "bot/\u00e9", "start": "main", "model": "m", "bundle": true, "timeout_minutes": 60}`},
		{Meta{Branch: "task", Start: "none", TimeoutMinutes: 45, Harness: "claude-code"},
			`{"branch": "task", "start": "none", "model": "", "bundle": false, "timeout_minutes": 45, "harness": "claude-code"}`},
	} {
		if got := string(c.m.Encode()); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
	back, err := ParseMeta(Meta{Branch: "bot/x", Start: "bot/x", Model: "m", Bundle: true, TimeoutMinutes: 7}.Encode())
	if err != nil || back != (Meta{Branch: "bot/x", Start: "bot/x", Model: "m", Bundle: true, TimeoutMinutes: 7, Harness: DefaultHarness}) {
		t.Errorf("round trip: %+v %v", back, err)
	}
}

// A gate job names its kind last and starts from a full commit id; an
// agent job never writes "kind", and reads "agent" as one.
func TestGateMeta(t *testing.T) {
	head := strings.Repeat("0123456789abcdef", 3)[:40]
	gate := Meta{Branch: "gate", Start: head, TimeoutMinutes: 30, Kind: KindGate}
	want := `{"branch": "gate", "start": "` + head + `", "model": "", "bundle": false, "timeout_minutes": 30, "kind": "gate"}`
	if got := string(gate.Encode()); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	back, err := ParseMeta(gate.Encode())
	if err != nil || back.Kind != KindGate || back.Start != head || back.TimeoutMinutes != 30 {
		t.Errorf("round trip: %+v %v", back, err)
	}
	if got := string((Meta{Branch: "x", Start: "main", Kind: KindAgent}).Encode()); strings.Contains(got, "kind") {
		t.Errorf("an agent job wrote its kind: %s", got)
	}
	if m, err := CheckMeta(mustParse(t, `{"branch": "x", "start": "main", "kind": "agent"}`)); err != nil || m.Kind != "" {
		t.Errorf("kind agent: %+v %v", m, err)
	}
	for _, bad := range []string{
		`{"branch": "gate", "start": "main", "kind": "gate"}`,
		`{"branch": "gate", "start": "` + strings.ToUpper(head) + `", "kind": "gate"}`,
		`{"branch": "gate", "start": "` + head[:39] + `", "kind": "gate"}`,
		`{"branch": "x", "start": "main", "kind": "build"}`,
		`{"branch": "x", "start": "main", "kind": 1}`,
	} {
		if _, err := CheckMeta(mustParse(t, bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}
