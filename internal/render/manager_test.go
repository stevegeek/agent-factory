package render

import (
	"io/fs"
	"strings"
	"testing"

	agentfactory "github.com/stevegeek/agent-factory"
	"github.com/stevegeek/agent-factory/internal/model"
)

func managerFile(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(agentfactory.Assets, "templates/ruby-gem/manager/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The manager (the operator) only reads: every volume it gets is
// read-only, among them System status, the live context and the deploy
// records. The tick applies; the manager proposes.
func TestManagerTemplateMountsReadOnly(t *testing.T) {
	y := managerFile(t, "scion-agent.yaml")
	_, vols, ok := strings.Cut(y, "\nvolumes:\n")
	if !ok {
		t.Fatal("no volumes")
	}
	blocks := strings.Split(vols, "  - type:")[1:]
	if len(blocks) < 7 {
		t.Fatalf("%d volumes", len(blocks))
	}
	targets := map[string]bool{}
	for _, b := range blocks {
		if !strings.Contains(b, "\n    read_only: true") {
			t.Errorf("a volume that is not read-only:%s", b)
		}
		for _, line := range strings.Split(b, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "target: "); ok {
				targets[v] = true
			}
		}
	}
	for _, want := range []string{"/factory/sysinfo.json", "/factory/context", "/factory/deploys", "/factory/board", "/factory/logs", "/factory/config.toml"} {
		if !targets[want] {
			t.Errorf("no mount at %s", want)
		}
	}
	if !strings.Contains(y, "drafts and proposes; the tick applies under the rights tiers") {
		t.Error("the description does not say who applies")
	}
}

// The manager's role names each action it may propose, and the report's
// actions key.
func TestManagerRoleNamesActions(t *testing.T) {
	md := managerFile(t, "agents.md")
	for _, a := range model.OperatorActions() {
		if !strings.Contains(md, "`"+string(a.Kind)+"`") {
			t.Errorf("agents.md does not name %s", a.Kind)
		}
	}
	for _, want := range []string{`"actions"`, "/factory/sysinfo.json", "/factory/context", "/factory/deploys", "config `main`"} {
		if !strings.Contains(md, want) {
			t.Errorf("agents.md lacks %q", want)
		}
	}
	if strings.Contains(md, "Nothing on this box can change") {
		t.Error("agents.md still says nothing can change")
	}
}
