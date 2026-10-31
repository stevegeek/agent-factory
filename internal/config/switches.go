package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
)

// Switch is one project's entry in foreman-state/projects.json: the
// maintainer switched a factory project on or off from Foreman (or with
// `factory project`), by whom and when. It overrides the project's
// `enabled` in config.toml until it is removed ("Follow config").
type Switch struct {
	Enabled bool   `json:"enabled"`
	By      string `json:"by"`
	At      string `json:"at"` // a clock.Stamp
}

// Switches are projects.json: the switched projects by key. A project it
// does not name follows config.toml.
type Switches map[string]Switch

// FactorySwitch is the key in projects.json that switches the whole
// factory off: every factory project is paused, and so is work that names
// no configured project (the clock's and the manager's), until it is
// removed. Only "off" is stored: switching the factory on removes it.
const FactorySwitch = "*"

// FactoryOff is the factory's switch in the projects.json at path, when it
// switches the factory off. A file that does not read is no switch, as
// Switched follows config.toml then.
func FactoryOff(path string) (Switch, bool) {
	sw, err := ReadSwitches(path)
	off, ok := sw[FactorySwitch]
	return off, err == nil && ok && !off.Enabled
}

// maxSwitchesBytes caps what ReadSwitches reads (a line per project).
const maxSwitchesBytes = 256 << 10

// ReadSwitches reads projects.json. A missing file is no switches and no
// error; a file that is not one JSON object of {"enabled": bool, ...}
// entries is an error, and its caller follows config.toml alone. It never
// follows a link: the file is in foreman-state/, which Foreman writes.
func ReadSwitches(path string) (Switches, error) {
	b, err := fsx.ReadNoFollow(path, maxSwitchesBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("project switches: %w", err)
	}
	var raw map[string]struct {
		Enabled *bool  `json:"enabled"`
		By      string `json:"by"`
		At      string `json:"at"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("project switches %s: %w", path, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("project switches %s: not a JSON object", path)
	}
	out := make(Switches, len(raw))
	for k, v := range raw {
		if v.Enabled == nil {
			return nil, fmt.Errorf("project switches %s: %q has no \"enabled\"", path, k)
		}
		out[k] = Switch{Enabled: *v.Enabled, By: v.By, At: v.At}
	}
	return out, nil
}

// UpdateSwitches changes projects.json under its lock (projects.json.lock
// beside it) and writes it atomically (with no switch left it is "{}").
// A file that does not read is replaced: it was not being
// followed anyway (Switched logs it), so starting from no switches is what
// the factory already did.
func UpdateSwitches(ctx context.Context, path string, change func(Switches) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("project switches: %w", err)
	}
	lock, err := fsx.Lock(ctx, path+".lock", fsx.Wait(10*time.Second))
	if err != nil {
		return fmt.Errorf("project switches: %w", err)
	}
	defer lock.Unlock()
	sw, err := ReadSwitches(path)
	if err != nil || sw == nil {
		sw = Switches{}
	}
	if err := change(sw); err != nil {
		return err
	}
	b, err := json.MarshalIndent(sw, "", "  ") // keys sorted
	if err != nil {
		return fmt.Errorf("project switches: %w", err)
	}
	return fsx.WriteAtomicSync(path, append(b, '\n'), 0o600)
}

// WithSwitches is c with each factory project that sw names switched: its
// Enabled is the switch's, and Switch says who switched it and when. A
// board project or a name config does not have is ignored. c is not
// changed (a *Config is shared); with nothing to switch c itself comes
// back.
func (c *Config) WithSwitches(sw Switches) *Config {
	if c == nil {
		return nil
	}
	off, isOff := sw[FactorySwitch]
	isOff = isOff && !off.Enabled
	hit := isOff
	for _, p := range c.Projects {
		if _, ok := sw[p.Key]; ok && p.Mode == ModeFactory {
			hit = true
		}
	}
	if !hit {
		return c
	}
	n := *c
	n.Projects = slices.Clone(c.Projects)
	if isOff {
		n.Off = &off
	}
	for i := range n.Projects {
		p := &n.Projects[i]
		switch s, ok := sw[p.Key]; {
		case p.Mode != ModeFactory:
		case isOff:
			p.Enabled, p.Switch = false, &off
		case ok:
			p.Enabled, p.Switch = s.Enabled, &s
		}
	}
	return &n
}

// Switched is c with the switches at path applied (WithSwitches). A file
// that does not read is logged and c comes back as it is: a broken file
// never stops the factory, it only stops overriding config.toml.
func (c *Config) Switched(path string, log Logger) *Config {
	sw, err := ReadSwitches(path)
	if err != nil {
		if log != nil {
			log.Printf("%v; following config.toml's enabled", err)
		}
		return c
	}
	return c.WithSwitches(sw)
}

// SwitchNote is how a switched project reads in `factory check` and
// Foreman: "paused here by alice at <stamp>"; "" when p follows config.
func (p Project) SwitchNote() string {
	if p.Switch == nil {
		return ""
	}
	state := "paused"
	if p.Switch.Enabled {
		state = "switched on"
	}
	var b strings.Builder
	b.WriteString(state + " here")
	if p.Switch.By != "" {
		b.WriteString(" by " + p.Switch.By)
	}
	if p.Switch.At != "" {
		b.WriteString(" at " + p.Switch.At)
	}
	return b.String()
}

// ConfigState is the project's state as config.toml has it: "enabled" or
// "paused".
func (p Project) ConfigState() string {
	if p.ConfigEnabled {
		return "enabled"
	}
	return "paused"
}
