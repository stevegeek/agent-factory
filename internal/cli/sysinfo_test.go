package cli

import (
	"context"
	"os"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// The tick fills System status's switches from projects.json and its hold
// from the hold file.
func TestSysinfoStateReadsSwitchesAndHold(t *testing.T) {
	p := paths.FromEnv(testkit.Root(t).Vars)
	switches, hold := sysinfoState(p)
	if sw, err := switches(); err != nil || len(sw) != 0 {
		t.Errorf("no file: %v %v", sw, err)
	}
	if h, err := hold(); err != nil || h != nil {
		t.Errorf("no hold: %v %v", h, err)
	}
	if err := os.MkdirAll(p.ForemanState(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateSwitches(context.Background(), p.ProjectSwitches(), func(s config.Switches) error {
		s["alpha"] = config.Switch{Enabled: false, By: "maintainer", At: "2026-10-18T09:00:00+00:00"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := deployrec.SetHold(p.DeployHold(), &deployrec.Hold{By: "maintainer", At: "2026-10-18T08:00:00+00:00"}); err != nil {
		t.Fatal(err)
	}
	if sw, err := switches(); err != nil || sw["alpha"].By != "maintainer" || sw["alpha"].Enabled {
		t.Errorf("switches %v %v", sw, err)
	}
	if h, err := hold(); err != nil || h == nil || h.By != "maintainer" {
		t.Errorf("hold %v %v", h, err)
	}
	os.WriteFile(p.DeployHold(), []byte("nope"), 0o600)
	if _, err := hold(); err == nil {
		t.Error("a hold that does not read is an error")
	}
}
