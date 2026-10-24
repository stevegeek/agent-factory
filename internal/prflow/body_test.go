package prflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The PR body is public text that the off-box approve job reads: it must be
// Python's, byte for byte. The goldens under testdata/py/pr_body were written
// by tools/goldens.py from Python's pr_body.
func TestPRBodyMatchesPythonGoldens(t *testing.T) {
	cases, err := filepath.Glob(filepath.Join("testdata", "py", "pr_body", "*.json"))
	if err != nil || len(cases) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	for _, path := range cases {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			in, err := jsonx.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			tk, _, err := ticket.Decode(jsonx.EncodePython(in.Get("ticket"), 2))
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{}
			if footer, ok := in.Get("config").Get("identity").Get("pr_footer").Str(); ok {
				cfg.Identity.PRFooter = footer
			}
			want, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".md")
			if err != nil {
				t.Fatal(err)
			}
			if got := PRBody(cfg, tk); got != string(want) {
				t.Errorf("PR body differs (-python +go):\n%s", testkit.Diff(string(want), got))
			}
		})
	}
}

// port: TF GithubOutTest.test_the_pr_body_says_how_it_was_tested_its_risk_and_how_to_revert
func TestPRBodySaysHowItWasTestedItsRiskAndHowToRevert(t *testing.T) {
	tk := decode(t, `{"id": "T0042", "title": "Tidy README", "column": "ready_for_pr", "branch": "bot/x", "history": [],
		"source": {"issue": 7},
		"reports": {"work": {"summary": "Clarify the install steps.", "tests": "pass", "lint": "pass"},
		            "review": {"verdict": "pass", "runtime_behaviour": "no"},
		            "security": {"answers": [{"concern": "none"}]}},
		"security_tier": 0,
		"gate": {"auto_eligible": true, "blockers": [], "head": "abc", "changed_lines": 4, "files": ["README.md"]}}`)
	body := PRBody(parseConfig(t, testConfig), tk)
	for _, want := range []string{"**Testing**", "ran the tests (pass) and lint (pass)", "**Risk**",
		"Text only (security tier 0)", "4 changed lines in 1 file(s)", "**How to revert**", "git revert -m 1",
		"Small, docs/tests/config-only change: eligible for automatic approval.", "Fixes #7", "Opened by a bot."} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "T0042") || hasTicketID(body) {
		t.Errorf("body names a ticket:\n%s", body)
	}
}

// Values of unexpected types read as Python's str() of them, and the tier
// name is looked up the way Python's dict does (1.0 and True are 1).
func TestPRBodyReadsOddValuesAsPythonDoes(t *testing.T) {
	tk, _, err := ticket.Decode([]byte(`{"id": "T0042", "title": "x", "column": "ready_for_pr", "history": [],
		"reports": {"work": {"summary": 42, "tests": true, "lint": null}, "review": {}, "security": {"answers": [1]}},
		"security_tier": 1.0,
		"gate": {"auto_eligible": 0, "blockers": ["a", "b"], "changed_lines": 2.5, "files": "ab"}}`))
	if err != nil {
		t.Fatal(err)
	}
	body := PRBody(&config.Config{}, tk)
	for _, want := range []string{
		"42\n\n**Testing**",
		"- The author ran the tests (True) and lint (None).",
		"- Independent review: ?; changes runtime behaviour for users: ?.",
		"- Security review (tier 1.0): 1 question answered, none above low concern.",
		"- Needs a maintainer's review: a; b.",
		"- Plain code (security tier 1.0); 2.5 changed lines in 2 file(s); changes behaviour for users: ?.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
}

// hasTicketID is the Python test's re.search(r"T\d{4}", body), by hand.
func hasTicketID(s string) bool {
	for i := 0; i+5 <= len(s); i++ {
		if s[i] != 'T' {
			continue
		}
		digits := true
		for _, c := range s[i+1 : i+5] {
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		if digits {
			return true
		}
	}
	return false
}

// Gate 1's hygiene scans gate.PublicTexts as the report text the PR body
// prints: every report field the body prints is there, and only those. A
// body that starts printing another field fails here.
func TestPublicTextsMatchPRBody(t *testing.T) {
	fields := map[string][]string{
		"work":     {"status", "summary", "tests", "lint", "category", "notes", "files", "question"},
		"review":   {"verdict", "runtime_behaviour", "summary", "notes", "concerns"},
		"security": {"summary", "verdict", "notes"},
	}
	var reports []string
	for report, keys := range fields {
		var kv []string
		for _, k := range keys {
			kv = append(kv, `"`+k+`": "mark-`+report+`-`+k+`"`)
		}
		reports = append(reports, `"`+report+`": {`+strings.Join(kv, ", ")+`}`)
	}
	tk := decode(t, `{"id": "T0042", "title": "t", "column": "ready_for_pr", "branch": "bot/x", "history": [],
		"reports": {`+strings.Join(reports, ", ")+`}}`)
	body := PRBody(&config.Config{}, tk)
	texts := gate.PublicTexts(tk)
	for report, keys := range fields {
		for _, k := range keys {
			mark := "mark-" + report + "-" + k
			printed := strings.Contains(body, mark)
			scanned := false
			for _, v := range texts {
				scanned = scanned || strings.Contains(v, mark)
			}
			if printed != scanned {
				t.Errorf("%s.%s: printed in the body %v, in PublicTexts %v", report, k, printed, scanned)
			}
		}
	}
}

// A change to the walls says so under Risk, naming the files; one that
// touches none has no such line.
func TestPRBodyListsWalls(t *testing.T) {
	tk := readyTicket(t, false, sha('a'))
	if body := PRBody(&config.Config{}, tk); strings.Contains(body, "walls") {
		t.Errorf("a line with no walls:\n%s", body)
	}
	g := tk.Gate.V
	g.Walls = []string{"internal/gate/gate.go", "deploy.sh"}
	tk.Gate = ticket.Some(g)
	body := PRBody(&config.Config{}, tk)
	risk := body[strings.Index(body, "**Risk**"):strings.Index(body, "**How to revert**")]
	if !strings.Contains(risk, "\n- Touches the walls: internal/gate/gate.go, deploy.sh.\n") {
		t.Errorf("risk:\n%s", risk)
	}
}
