package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// AuditText is audit_text(t): what happened to a ticket and which gate let
// it through, as Markdown for the PR and for a person. Report values are
// printed as Python's f-strings print them. Where Python's audit_text
// raises (a missing title, a report that is not an object), AuditText is
// an error.
//
// The audit is code rather than a text/template (§8.4 planned one): every
// line is conditional on loosely typed report values, and matching
// Python's f-strings and rstrip() is easier to review as code.
func AuditText(t *ticket.Ticket) (string, error) {
	a, err := pyTicket(t)
	if err != nil {
		return "", err
	}
	return audit(a)
}

func audit(t node) (_ string, err error) {
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	// must turns the first Python error into the result.
	must := func(n node, e error) node {
		if e != nil && err == nil {
			err = e
		}
		return n
	}
	get := func(n node, key, def string) string { return must(n.dictGet(key, stringNode(def))).str() }

	r := must(t.index("reports"))
	id, title, door := must(t.index("id")), must(t.index("title")), must(t.index("door"))
	head := "- Door: " + door.str() + "; project: " + get(t, "project", "?")
	if b, _ := t.get("branch"); b.truthy() {
		head += "; branch: `" + b.str() + "`"
	}
	add("# " + id.str() + ": " + title.str())
	add("")
	add(head)
	if o, _ := t.get("original"); o.truthy() {
		triage := must(r.dictGet("triage", objectNode()))
		add(textx.RStrip("- Triage: accepted. " + get(triage, "reason", "")))
	}
	if p, _ := t.get("parent"); p.truthy() {
		add("- From the approved design in " + p.str())
	}
	if w, ok := r.get("work"); ok {
		add(textx.RStrip(fmt.Sprintf("- Worker: %s; category %s; tests %s, lint %s. %s",
			get(w, "status", "?"), get(w, "category", "?"), get(w, "tests", "?"), get(w, "lint", "?"), get(w, "summary", ""))))
	}
	if v, ok := r.get("review"); ok {
		add(textx.RStrip(fmt.Sprintf("- Review: %s; runtime behaviour changes: %s. %s",
			get(v, "verdict", "?"), get(v, "runtime_behaviour", "?"), get(v, "summary", ""))))
		findings, e := must(v.dictGet("findings", listNode())).iter()
		must(node{}, e)
		for _, f := range findings {
			add("  - finding: " + f.str())
		}
	}
	if s, ok := r.get("security"); ok {
		add(textx.RStrip(fmt.Sprintf("- Security (tier %s): %s. %s",
			get(t, "security_tier", "?"), get(s, "verdict", "?"), get(s, "summary", ""))))
		// answers that are not a list are skipped, and so is any answer that is not an object
		if list := must(s.dictGet("answers", node{})); list.isList() {
			for _, a := range list.items() {
				if a.isDict() {
					add(fmt.Sprintf("  - [%s] %s: %s", get(a, "concern", "?"), get(a, "question", ""), get(a, "answer", "")))
				}
			}
		}
	}
	if g, _ := t.get("gate"); g.truthy() {
		line, e := gateLine(g)
		must(node{}, e)
		add(line)
	}
	add("")
	add("## History")
	add("")
	history, e := must(t.index("history")).iter()
	must(node{}, e)
	for _, h := range history {
		at, event, detail := must(h.index("at")), must(h.index("event")), must(h.index("detail"))
		add(textx.RStrip("- " + at.str() + " " + event.str() + " " + detail.str()))
	}
	if err != nil {
		return "", fmt.Errorf("audit: %w", err)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// gateLine is the audit's "- Gate 1: ..." line.
func gateLine(g node) (string, error) {
	checks, err := g.index("checks_pass")
	if err != nil {
		return "", err
	}
	lines, err := g.index("changed_lines")
	if err != nil {
		return "", err
	}
	files, err := g.index("files")
	if err != nil {
		return "", err
	}
	n, err := files.length()
	if err != nil {
		return "", err
	}
	eligible, err := g.index("auto_eligible")
	if err != nil {
		return "", err
	}
	verdict := "FAIL"
	if checks.truthy() {
		verdict = "pass"
	}
	out := fmt.Sprintf("- Gate 1: checks %s; %s lines in %d files; ", verdict, lines.str(), n)
	if eligible.truthy() {
		return out + "auto-merge eligible", nil
	}
	blockers, err := g.dictGet("blockers", listNode())
	if err != nil {
		return "", err
	}
	items, err := blockers.iter()
	if err != nil {
		return "", err
	}
	parts := make([]string, len(items))
	for i, b := range items {
		s, ok := b.text()
		if !ok {
			return "", fmt.Errorf("TypeError: sequence item %d: expected str instance, %s found", i, b.typeName())
		}
		parts[i] = s
	}
	return out + "needs human review: " + strings.Join(parts, "; "), nil
}

// WriteAudit is write_audit: the audit to logs/<id>/audit.md, written after
// every gate run.
func WriteAudit(p paths.Paths, t *ticket.Ticket) error {
	text, err := AuditText(t)
	if err != nil {
		return err
	}
	dir := p.TicketLogs(string(t.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("audit %s: %w", t.ID, err)
	}
	if err := fsx.WriteAtomic(filepath.Join(dir, "audit.md"), []byte(text), 0o644); err != nil {
		return fmt.Errorf("audit %s: %w", t.ID, err)
	}
	return nil
}
