package operator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The rebuild's transient unit: `factory rebuild` takes the tick lock
// itself, so ticks wait while it builds.
const (
	RebuildUnit       = "factory-rebuild-now"
	rebuildMaxRuntime = "4h"
	systemdRunTimeout = 30 * time.Second
)

// The project switch values (ops.SwitchOn, SwitchOff, SwitchFollow).
const (
	switchOn     = "on"
	switchOff    = "off"
	switchFollow = "follow"
)

// switchNote is said of every project switch the operator makes.
const switchNote = "a project switch overrides config.toml's enabled until it is back to follow"

// actions acts on a ticket's new and requested actions. offers are the
// deploy actions that join the pass's one offer; lines are what the settle
// move says.
func (s Service) actions(ctx context.Context, cfg *config.Config, t *ticket.Ticket, trusted bool) (changed bool, lines []string, offers []offered) {
	for i := range t.Actions {
		if !pending(ticket.ActionState(t.Actions[i])) {
			continue
		}
		changed = true
		line, offer := s.action(ctx, cfg, t, i, trusted)
		if line != "" {
			lines = append(lines, line)
		}
		if offer {
			offers = append(offers, offered{id: t.ID, index: i, run: t.Actions[i].Run})
		}
	}
	return changed, lines, offers
}

// action acts on one action. Its tier is model's table, never what the
// ticket says; a kind not in it, or a target the running config does not
// name, is refused. A new action runs by itself only when it is auto and
// the ticket is the maintainer's alone; anything else waits for a tap.
// offer is a deploy action, applied pending the pass's offer.
func (s Service) action(ctx context.Context, cfg *config.Config, t *ticket.Ticket, i int, trusted bool) (line string, offer bool) {
	a := &t.Actions[i]
	requested := ticket.ActionState(*a) == model.ItemRequested
	if requested && !ticket.Tapped(t, a.Run, a.Requested, cfg.Maintainer()) {
		requested, a.State, a.Requested = false, model.ItemNew, ""
		defer func() { a.Note = textx.Clip(tapNote+a.Note, 2000) }()
	}
	info, known := model.ActionOf(a.Kind)
	if !known {
		a.Tier, a.State, a.Note = string(model.TierNever), model.ItemRefused, "not an action the manager may propose"
		return a.ItemLabel() + ": refused", false
	}
	a.Tier = string(info.Tier)
	if why := s.badTarget(cfg, info, a.Target); why != "" {
		a.State, a.Note = model.ItemRefused, why
		return a.ItemLabel() + ": refused (" + why + ")", false
	}
	if info.Kind == model.ActionDeploy && !cfg.Deploy.On() {
		a.State, a.Note = model.ItemRefused, "no [deploy] in config"
		return a.ItemLabel() + ": refused (no [deploy] in config)", false
	}
	if !requested && (info.Tier != model.TierAuto || !trusted) {
		a.State, a.Note = model.ItemWaiting, ""
		if info.Tier == model.TierAuto {
			a.Note = "the ticket holds words that are not yours, so nothing applies by itself"
		}
		return "", false
	}
	note, err := s.run(ctx, cfg, t, *a, info)
	if err != nil {
		a.State, a.Note = model.ItemFailed, textx.Prefix(err.Error(), noteRunes)
		return a.ItemLabel() + ": failed", false
	}
	a.State, a.Note = model.ItemApplied, note
	return a.ItemLabel() + ": " + note, info.Kind == model.ActionDeploy
}

// badTarget is why a target does not fit the action ("" when it does): a
// host must be a [hosts] name or local, a project a factory project of the
// running config, and an action without a target takes none.
func (s Service) badTarget(cfg *config.Config, info model.ActionInfo, target string) string {
	switch info.Target {
	case "host":
		if _, ok := cfg.HostNamed(target); target == "local" || ok && target != "" {
			return ""
		}
		return fmt.Sprintf("no host %q in config", target)
	case "project":
		if p, ok := cfg.Project(target); ok && target != "" && p.Mode == config.ModeFactory {
			return ""
		}
		return fmt.Sprintf("no factory project %q in config", target)
	}
	if target != "" {
		return string(info.Kind) + " takes no target"
	}
	return ""
}

// run carries out an action and is its one-line note.
func (s Service) run(ctx context.Context, cfg *config.Config, t *ticket.Ticket, a ticket.Action, info model.ActionInfo) (string, error) {
	why := string(t.ID) + ": " + textx.Clip(orString(a.Why, info.Label), 200)
	by := "manager " + string(t.ID)
	switch info.Kind {
	case model.ActionPauseHost:
		until, err := s.Board.PauseHost(a.Target, why, time.Duration(hours(a.Hours))*time.Hour)
		if err != nil {
			return "", err
		}
		return "paused until " + until, nil
	case model.ActionResumeHost:
		ok, err := s.Board.ResumeHost(a.Target, why)
		switch {
		case err != nil:
			return "", err
		case !ok:
			return "was not paused", nil
		}
		return "resumed", nil
	case model.ActionPauseProject:
		return s.switchProject(ctx, a.Target, switchOff, by)
	case model.ActionEnableProject:
		state := switchOn
		if p, ok := cfg.Project(a.Target); ok && p.ConfigEnabled {
			state = switchFollow
		}
		return s.switchProject(ctx, a.Target, state, by)
	case model.ActionDeploy:
		return "offered with this pass's deploy of main (asks Deploy? while held)", nil
	case model.ActionRebuild:
		if err := s.rebuild(ctx); err != nil {
			return "", err
		}
		return "started; ticks wait while it builds", nil
	}
	return "", fmt.Errorf("%s: not an action the tick runs", info.Kind)
}

func (s Service) switchProject(ctx context.Context, key, state, by string) (string, error) {
	if s.Switches == nil {
		return "", errors.New("no project switches")
	}
	line, err := s.Switches.SwitchProject(ctx, key, state, by)
	if err != nil {
		return "", err
	}
	return textx.Clip(line, 200) + "; " + switchNote, nil
}

// hours is a host pause's length: 1 to 168, 24 when unset.
func hours(h int) int {
	if h == 0 {
		return 24
	}
	return min(max(h, 1), 168)
}

func orString(s, dflt string) string {
	if strings.TrimSpace(s) == "" {
		return dflt
	}
	return s
}

// rebuild starts `factory rebuild` as the transient unit RebuildUnit and
// returns. The unit gets the factory's root and FACTORY_* settings (the
// user manager's environment is not the tick's; none is a secret). A unit
// already there counts as started.
func (s Service) rebuild(ctx context.Context) error {
	if s.Exec == nil {
		return errors.New("no way to run systemd-run")
	}
	argv := []string{"systemd-run", "--user", "--no-block", "--collect", "--unit=" + RebuildUnit,
		"--property=RuntimeMaxSec=" + rebuildMaxRuntime}
	for _, kv := range s.factoryEnv() {
		argv = append(argv, "--setenv="+kv)
	}
	argv = append(argv, s.Paths.FactoryBin(), "rebuild")
	var env []string
	for _, k := range []string{"PATH", "HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := s.Env[k]; ok {
			env = append(env, k+"="+v)
		}
	}
	res, err := s.Exec.Run(ctx, execx.Cmd{Argv: argv, Env: env, Timeout: systemdRunTimeout})
	if err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	if res.Code != 0 && !alreadyThere(res.Stderr) {
		return fmt.Errorf("rebuild: %w", execx.CheckErr(res))
	}
	s.logf("rebuild started as %s by the operator", RebuildUnit)
	return nil
}

// alreadyThere recognises systemd refusing the unit name: a rebuild runs.
func alreadyThere(stderr []byte) bool {
	e := strings.ToLower(string(stderr))
	return strings.Contains(e, "already exists") || strings.Contains(e, "already loaded")
}

// factoryEnv is FACTORY_ROOT and FACTORY_API_TOKENS from Paths, plus every
// other FACTORY_* setting of the tick but its GitHub token, sorted.
func (s Service) factoryEnv() []string {
	vars := map[string]string{}
	for k, v := range s.Env {
		if strings.HasPrefix(k, "FACTORY_") && v != "" && k != "FACTORY_GH_TOKEN" {
			vars[k] = v
		}
	}
	vars["FACTORY_ROOT"] = s.Paths.Root
	if s.Paths.Tokens != "" {
		vars["FACTORY_API_TOKENS"] = s.Paths.Tokens
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
