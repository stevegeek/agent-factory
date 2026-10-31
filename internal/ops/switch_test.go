package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
)

// The factory is switched off in projects.json and back on by removing it.
func TestSwitchTheWholeFactory(t *testing.T) {
	w := newWorld(t, twoProjects)
	ctx := context.Background()
	read := func() (config.Switch, bool) {
		sw, err := config.ReadSwitches(w.paths.ProjectSwitches())
		if err != nil {
			t.Fatal(err)
		}
		s, ok := sw[config.FactorySwitch]
		return s, ok
	}
	line, err := w.svc.SwitchProject(ctx, config.FactorySwitch, "off", "alice")
	if err != nil || !strings.HasPrefix(line, "The factory is off") {
		t.Fatalf("off: %q %v", line, err)
	}
	if s, ok := read(); !ok || s.Enabled || s.By != "alice" || s.At == "" {
		t.Errorf("switch %+v %v", s, ok)
	}
	if line, _ := w.svc.SwitchProject(ctx, config.FactorySwitch, "off", "alice"); line != "The factory is already off" {
		t.Errorf("again: %q", line)
	}
	if line, err := w.svc.SwitchProject(ctx, config.FactorySwitch, "on", "alice"); err != nil || !strings.HasPrefix(line, "The factory is on") {
		t.Fatalf("on: %q %v", line, err)
	}
	if _, ok := read(); ok {
		t.Error("the factory's switch stays after switching it on")
	}
	if _, err := w.svc.SwitchProject(ctx, config.FactorySwitch, "maybe", ""); err == nil {
		t.Error("a state that is neither on nor off")
	}
}
