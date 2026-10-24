// Package testkit holds the test-only fakes and fixture builders shared by
// the module's tests (ARCHITECTURE.md §10.6). Production code never imports
// it (archtest enforces this).
//
// This is the S0 skeleton: a hermetic factory root, clock fakes, a real-git
// repository builder, golden files, a small diff for failure messages and
// the opt-in live-board snapshot. Later slices add their fakes beside these.
package testkit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Env is a hermetic factory root for one test: the board, the config, the
// token store and HOME all live inside t.TempDir(). Nothing reads the real
// environment's HOME or FACTORY_ROOT; Vars is what a paths.FromEnv call or
// a child process should see instead.
type Env struct {
	Root string // FACTORY_ROOT
	Home string // HOME
	// Vars are the variables a test passes on: FACTORY_ROOT, HOME,
	// FACTORY_API_TOKENS and the git isolation variables.
	Vars map[string]string
}

// Root builds a fresh factory root and home under t.TempDir(), with the
// directories the factory expects to exist (board/tickets, logs,
// ~/.config/factory at 0700, ~/.local/bin).
func Root(t testing.TB) *Env {
	t.Helper()
	base := t.TempDir()
	e := &Env{Root: filepath.Join(base, "factory"), Home: filepath.Join(base, "home")}
	for _, d := range []struct {
		path string
		perm os.FileMode
	}{
		{filepath.Join(e.Root, "board", "tickets"), 0o755},
		{filepath.Join(e.Root, "logs"), 0o755},
		{filepath.Join(e.Home, ".config", "factory"), 0o700},
		{filepath.Join(e.Home, ".local", "bin"), 0o755},
	} {
		if err := os.MkdirAll(d.path, d.perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.path, d.perm); err != nil {
			t.Fatal(err)
		}
	}
	gitconfig := filepath.Join(e.Home, ".gitconfig")
	if err := os.WriteFile(gitconfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.Vars = map[string]string{
		"FACTORY_ROOT":        e.Root,
		"HOME":                e.Home,
		"FACTORY_API_TOKENS":  filepath.Join(e.Home, ".config", "factory", "api-tokens.json"),
		"GIT_CONFIG_GLOBAL":   gitconfig,
		"GIT_CONFIG_NOSYSTEM": "1",
		"XDG_CONFIG_HOME":     filepath.Join(e.Home, ".config"),
	}
	return e
}

// Path joins elements onto the root.
func (e *Env) Path(elem ...string) string {
	return filepath.Join(append([]string{e.Root}, elem...)...)
}

// WriteFile writes data at a root-relative path, creating directories.
func (e *Env) WriteFile(t testing.TB, rel string, data string) string {
	t.Helper()
	path := e.Path(filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// WriteConfig writes ROOT/config.toml.
func (e *Env) WriteConfig(t testing.TB, toml string) string {
	t.Helper()
	return e.WriteFile(t, "config.toml", toml)
}

// Environ is the environment for a child process: the parent's, without
// any variable Vars sets, then Vars, sorted.
func (e *Env) Environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := e.Vars[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range e.Vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
