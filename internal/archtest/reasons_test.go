package archtest

import (
	"context"
	"strings"
	"testing"
)

// Every model.Reason is written by name somewhere in production code (a
// ticket.Move call site, or a writer that hands it on to one), and no
// reason is spelled as a string literal.
func TestEveryReasonIsWrittenByName(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the whole module with go list -export")
	}
	g, err := Load(context.Background(), moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	use, vs := CheckReasons(g)
	for _, v := range vs {
		t.Error(v)
	}
	if len(use.Reasons) < 51 {
		t.Errorf("found %d model.Reason constants, want 51: %v", len(use.Reasons), use.Reasons)
	}
	// The board moves of ticketapi are among the writers.
	for _, r := range []string{"claimed", "parked", "waiting", "finished", "dropped", "reopened", "question_asked"} {
		found := false
		for _, at := range use.Written[r] {
			if strings.Contains(at, "ticketapi") {
				found = true
			}
		}
		if !found {
			t.Errorf("reason %s: no writer in ticketapi; written at %v", r, use.Written[r])
		}
	}
}

func TestReasonsCheckOnAFixture(t *testing.T) {
	g, _ := loadFixture(t, map[string]string{
		"internal/model/model.go": "package model\n\n// Reason is why.\ntype Reason string\n\n" +
			"const (\n\tReasonA Reason = \"a\"\n\tReasonB Reason = \"b\"\n\tReasonC Reason = \"c\"\n)\n",
		"internal/ticket/ticket.go": "package ticket\n\nimport \"" + fixMod + "/internal/model\"\n\n" +
			"// Move moves.\nfunc Move(r model.Reason) string { return string(r) }\n\n" +
			"// Moves writes A, only compares with B and spells c as a literal.\n" +
			"func Moves(r model.Reason) string {\n\tif r == model.ReasonB {\n\t\treturn \"\"\n\t}\n" +
			"\treturn Move(model.ReasonA) + Move(\"c\")\n}\n",
	})
	use, vs := CheckReasons(g)
	if strings.Join(use.Reasons, ",") != "a,b,c" {
		t.Errorf("reasons = %v", use.Reasons)
	}
	got := join(vs)
	for _, want := range []string{
		`the string literal "c" is used as a model.Reason`,
		"model.Reason b is never written",
		"model.Reason c is never written",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no violation %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "model.Reason a is never written") {
		t.Errorf("a is written:\n%s", got)
	}
}
