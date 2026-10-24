package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"syscall"
)

// Logger is where Source says what it re-read or refused (logx.Log
// satisfies it).
type Logger interface {
	Printf(format string, args ...any)
}

// Source is a config that follows its file (§3.4). Each Current call stats
// the file and re-parses it when its (inode, mtime_ns, size) changed, so a
// project removed from config, a new person, a paused project or a changed
// mode takes effect on the next call, as it did when every action was a
// fresh process. A file that does not parse (or has gone) keeps the last
// good config, logged once per file state; with no good config yet,
// Current returns the error.
//
// With Switches set (foreman-state/projects.json), the config Current
// returns has the maintainer's project switches applied (WithSwitches),
// followed the same way: re-read when the file changes, and a file that
// does not read is logged once per state and switches nothing.
//
// Use a *Source; it is safe for concurrent use. ticketapi, ops and the tick
// take a *Source and call Current once per call or pass.
type Source struct {
	Path     string
	Switches string // "" means config.toml alone
	Log      Logger // may be nil

	mu      sync.Mutex
	good    *Config
	goodKey fileKey
	badKey  fileKey
	badErr  error

	// the switched config: switched is good with the switches at swKey
	// applied, for swBase (the good it was made from)
	swBase   *Config
	swKey    fileKey
	switched *Config
}

// fileKey identifies one state of the file. missing is a state too.
type fileKey struct {
	ino     uint64
	mtimeNS int64
	size    int64
	missing bool
	valid   bool
}

func keyOf(fi fs.FileInfo) fileKey {
	k := fileKey{mtimeNS: fi.ModTime().UnixNano(), size: fi.Size(), valid: true}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.ino = st.Ino
	}
	return k
}

// Current returns the config for the file as it is now, switched.
func (s *Source) Current() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.current()
	if err != nil || s.Switches == "" {
		return cfg, err
	}
	return s.withSwitches(cfg), nil
}

// withSwitches is base with the switches file applied, kept while neither
// changes.
func (s *Source) withSwitches(base *Config) *Config {
	key := fileKey{missing: true, valid: true}
	if fi, err := os.Lstat(s.Switches); err == nil {
		key = keyOf(fi)
	} else if !errors.Is(err, fs.ErrNotExist) {
		key = fileKey{} // unknown: read it every time
	}
	if key.valid && key == s.swKey && base == s.swBase {
		return s.switched
	}
	sw, err := ReadSwitches(s.Switches)
	if err != nil && key != s.swKey {
		s.logf("%v; following config.toml's enabled", err)
	}
	s.swBase, s.swKey, s.switched = base, key, base.WithSwitches(sw)
	return s.switched
}

func (s *Source) current() (*Config, error) {
	key, cfg, err := s.read()
	switch {
	case err == nil && s.good != nil && key == s.goodKey:
		return s.good, nil
	case err == nil:
		if s.good != nil {
			s.logf("config: %s changed; re-read", s.Path)
		}
		s.good, s.goodKey, s.badKey, s.badErr = cfg, key, fileKey{}, nil
		return cfg, nil
	}
	if key != s.badKey || s.badErr == nil {
		s.badKey, s.badErr = key, err
		if s.good != nil {
			s.logf("config: %v; keeping the last good config", err)
		}
	}
	if s.good != nil {
		return s.good, nil
	}
	return nil, s.badErr
}

// read stats and, when the state is new, parses the file. A known good
// state comes back with a nil config and no error. The stat is of the open
// file, so the key and the bytes belong together.
func (s *Source) read() (fileKey, *Config, error) {
	f, err := os.Open(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileKey{missing: true, valid: true}, nil, fmt.Errorf("config %s: %w", s.Path, err)
	}
	if err != nil {
		return fileKey{}, nil, fmt.Errorf("config %s: %w", s.Path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fileKey{}, nil, fmt.Errorf("config %s: %w", s.Path, err)
	}
	key := keyOf(fi)
	if s.good != nil && key == s.goodKey {
		return key, nil, nil
	}
	if key == s.badKey && s.badErr != nil {
		return key, nil, s.badErr
	}
	b, err := readAll(f, s.Path)
	if err != nil {
		return key, nil, err
	}
	cfg, err := Parse(b)
	if err != nil {
		return key, nil, fmt.Errorf("config %s: %w", s.Path, err)
	}
	return key, cfg, nil
}

func (s *Source) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}
