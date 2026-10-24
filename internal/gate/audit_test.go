package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func decodeFile(t *testing.T, path string) *ticket.Ticket {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tk, _, err := ticket.Decode(b)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return tk
}

func decodeText(t *testing.T, text string) *ticket.Ticket {
	t.Helper()
	tk, _, err := ticket.Decode([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// The audits tools/goldens.py had audit_text write for each ticket shape.
func TestAuditGoldens(t *testing.T) {
	dir := filepath.Join("testdata", "py", "audit")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no audit goldens: %v", err)
	}
	for _, path := range files {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			got, err := AuditText(decodeFile(t, path))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(dir, name+".md"))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("audit differs from Python's (-want +got):\n%s", testkit.Diff(string(want), got))
			}
		})
	}
}

// The audit the Python tick wrote after the last gate run of the
// change-through-a-pr life (tools/goldens.py g_life), from the ticket as
// that step saved it.
func TestAuditOfARealLife(t *testing.T) {
	tk := decodeFile(t, filepath.Join("..", "ticket", "testdata", "py", "history-order", "change-through-a-pr",
		"10-security-gate-pr-updated", "T0001.json"))
	got, err := AuditText(tk)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "py", "audit", "flow-T0001.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("audit differs from Python's (-want +got):\n%s", testkit.Diff(string(want), got))
	}
}

// Report values are whatever the agent wrote: the audit prints them as
// Python's f-strings do.
func TestAuditPrintsValuesAsPython(t *testing.T) {
	tk := decodeText(t, `{"id": "T0002", "title": "Odd", "door": "human", "project": null, "branch": "",
		"original": {}, "parent": "", "security_tier": null,
		"reports": {"triage": {"reason": 3},
			"work": {"status": true, "category": null, "tests": 1.5, "lint": 1e16, "summary": ["a", {"b": "it's"}]},
			"review": {"verdict": "changes", "runtime_behaviour": {"x": [1, 2.0]}, "findings": "ab"},
			"security": {"verdict": "pass", "answers": [{"concern": 0.0001, "question": "q", "answer": 0.00001}]}},
		"history": [{"at": "t", "event": "note", "detail": "  trailing  "}]}`)
	got, err := AuditText(tk)
	if err != nil {
		t.Fatal(err)
	}
	want := "# T0002: Odd\n\n" +
		"- Door: human; project: None\n" +
		"- Worker: True; category None; tests 1.5, lint 1e+16. ['a', {'b': \"it's\"}]\n" +
		"- Review: changes; runtime behaviour changes: {'x': [1, 2.0]}.\n" +
		"  - finding: a\n  - finding: b\n" +
		"- Security (tier None): pass.\n" +
		"  - [0.0001] q: 1e-05\n" +
		"\n## History\n\n- t note   trailing\n"
	if got != want {
		t.Errorf("audit (-want +got):\n%s", testkit.Diff(want, got))
	}
}

// Where Python's audit_text raises (a missing title, a report that is not
// an object), AuditText is an error, not a made-up line.
func TestAuditRefusesWhatPythonCannotRead(t *testing.T) {
	for name, text := range map[string]string{
		"no title":               `{"id": "T1", "door": "human", "reports": {}, "history": []}`,
		"a work report string":   `{"id": "T1", "title": "x", "door": "human", "reports": {"work": "done"}, "history": []}`,
		"findings a number":      `{"id": "T1", "title": "x", "door": "human", "reports": {"review": {"findings": 3}}, "history": []}`,
		"a history entry no at":  `{"id": "T1", "title": "x", "door": "human", "reports": {}, "history": [{"event": "e", "detail": ""}]}`,
		"a gate with no files":   `{"id": "T1", "title": "x", "door": "human", "reports": {}, "gate": {"checks_pass": true, "changed_lines": 1, "auto_eligible": true}, "history": []}`,
		"no reports":             `{"id": "T1", "title": "x", "door": "human", "history": []}`,
		"a blocker not a string": `{"id": "T1", "title": "x", "door": "human", "reports": {}, "gate": {"checks_pass": true, "changed_lines": 1, "files": [], "auto_eligible": false, "blockers": [1]}, "history": []}`,
	} {
		if _, err := AuditText(decodeText(t, text)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// write_audit: logs/<id>/audit.md, the directory made if need be.
func TestWriteAudit(t *testing.T) {
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	tk := decodeFile(t, filepath.Join("testdata", "py", "audit", "00-full.json"))
	if err := WriteAudit(p, tk); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(p.TicketLogs("T0007"), "audit.md"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join("testdata", "py", "audit", "00-full.md"))
	if string(got) != string(want) {
		t.Errorf("audit.md (-want +got):\n%s", testkit.Diff(string(want), string(got)))
	}
}
