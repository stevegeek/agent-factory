package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/config"
)

// The states a project can be switched to: on, off (paused), or back to
// following config.toml's `enabled`.
const (
	SwitchOn     = "on"
	SwitchOff    = "off"
	SwitchFollow = "follow"
)

// SwitchProject switches factory project key on or off over config.toml's
// `enabled`, or back to following it, in foreman-state/projects.json
// (config.UpdateSwitches), and wakes the tick so it takes effect now. It
// is the maintainer's: Foreman offers it to the maintainer alone, and
// `factory project` is run as the factory user. by is who, for the record;
// "" is the maintainer. Each change is a line in the log (factory.log, or
// foreman.log from Foreman); no ticket records it. It returns a line for
// the person.
func (s *Service) SwitchProject(ctx context.Context, key, state, by string) (string, error) {
	cfg, err := s.config()
	if err != nil {
		return "", err
	}
	p, ok := cfg.Project(key)
	switch {
	case !ok:
		return "", refuse(BadRequest, "no project %s in config.toml", key)
	case p.Mode != config.ModeFactory:
		return "", refuse(BadRequest, "%s is a board project: only a factory project is switched on or off", key)
	case state != SwitchOn && state != SwitchOff && state != SwitchFollow:
		return "", refuse(BadRequest, "switch %s on, off or back to config, not %q", key, state)
	case s.Board == nil:
		return "", errors.New("ops: no board")
	}
	if by == "" {
		by = cfg.Maintainer()
	}
	changed := false
	err = config.UpdateSwitches(ctx, s.Board.Paths.ProjectSwitches(), func(sw config.Switches) error {
		old, had := sw[key]
		switch state {
		case SwitchFollow:
			delete(sw, key)
			changed = had
		default:
			on := state == SwitchOn
			sw[key] = config.Switch{Enabled: on, By: by, At: s.stamp()}
			changed = !had || old.Enabled != on
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("switch %s: %w", key, err)
	}
	if !changed && state == SwitchFollow {
		return fmt.Sprintf("%s already follows config.toml (%s)", key, p.ConfigState()), nil
	}
	var what, line string
	switch state {
	case SwitchOn:
		what, line = "switched on", key+" switched on: the factory works on it"
	case SwitchOff:
		what, line = "paused", key+" paused: no new agents, gates, schedules or sweeps; running agents finish and open PRs are still followed"
	default:
		what = "follows config.toml (" + p.ConfigState() + ")"
		line = key + " " + what + " again"
	}
	s.logf("project %s %s by %s", key, what, by)
	s.wake(ctx)
	return line, nil
}
