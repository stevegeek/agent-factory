package config

import (
	"strings"
	"testing"
)

// A task schedule or recipe with findings = true asks its agent for
// structured findings; [findings] auto_file says what happens to the ones
// a verifier confirms.
func TestFindingsKeys(t *testing.T) {
	c := mustParse(t, `
[findings]
auto_file = "confirmed-safe"

[[schedules]]
name = "bug-hunt"
kind = "task"
findings = true
title = "Bug hunt"
body = "Look."
every_days = 30

[[schedules]]
name = "rails"
title = "Rails"
body = "Check."
every_days = 30

[[recipes]]
name = "dependency-review"
kind = "task"
findings = true
title = "Review the dependencies"
body = "List them."
`)
	if c.Findings.AutoFile != AutoFileConfirmedSafe {
		t.Errorf("auto_file = %q", c.Findings.AutoFile)
	}
	if !c.Schedules[0].Findings || c.Schedules[1].Findings {
		t.Errorf("schedules' findings: %v %v", c.Schedules[0].Findings, c.Schedules[1].Findings)
	}
	if r, _ := c.Recipe("dependency-review"); !r.Findings {
		t.Error("the recipe asks for findings")
	}
	if r, _ := c.Recipe("bug-hunt"); !r.Findings {
		t.Error("a schedule is a recipe too, findings and all")
	}
	if len(c.Unknown) != 0 {
		t.Errorf("unknown keys %v", c.Unknown)
	}
	problems, warnings := c.Problems(nil)
	if len(problems) != 0 || len(warnings) != 0 {
		t.Errorf("problems %v warnings %v", problems, warnings)
	}

	if d := mustParse(t, "").Findings.AutoFile; d != AutoFileNone {
		t.Errorf("auto_file defaults to %q", d)
	}
}

func TestFindingsChecks(t *testing.T) {
	c := mustParse(t, `
[findings]
auto_file = "all"

[[schedules]]
name = "port-parity"
findings = true
title = "Port"
body = "Port."
every_days = 30
`)
	problems, warnings := c.Problems(nil)
	if !strings.Contains(strings.Join(problems, "\n"), `[findings] auto_file = "all"`) {
		t.Errorf("problems %v", problems)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "[[schedules]] port-parity: findings = true is for a task") {
		t.Errorf("warnings %v", warnings)
	}
	if c.Schedules[0].Findings {
		t.Error("findings on a change is ignored")
	}
}
