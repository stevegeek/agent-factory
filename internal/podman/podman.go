// Package podman runs containers and image commands on this host, with
// Python's argv: the gate's checks, the image rebuild's build, smoke test,
// tags and prune.
//
// Every container it starts is named (factory-gate-<id>,
// factory-merge-<id>, factory-smoke-<kind>), and the run's Cleanup removes
// it by name after a cancelled or timed-out run, because a container
// outlives its podman client. The name is the one difference from Python's
// argv: `--name <name>` right after `--rm`.
package podman

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/execx"
)

// Python's timeouts.
const (
	GateTimeout    = 1800 * time.Second
	BuildTimeout   = 6000 * time.Second
	SmokeTimeout   = 2400 * time.Second
	cleanupTimeout = 30 * time.Second
	// GateTail is how much of the gate's output is kept, in code points.
	GateTail = 4000
)

// KeepID is the user mapping of every factory container: the image's
// uid 1000 is the host user.
const KeepID = "--userns=keep-id:uid=1000,gid=1000"

// Client runs podman.
type Client struct {
	Exec execx.Execer
	Bin  string // "" means "podman" from PATH, as Python
	// Dir is the working directory of builds and smoke runs (Python runs
	// them in HOME); other commands run where the process is.
	Dir string
	// Env is every podman's environment; nil inherits the process's, as
	// Python does. PassEnv builds the minimal one.
	Env []string
}

// PassEnv keeps only what rootless podman needs from an environment: PATH,
// HOME, USER, LOGNAME, TMPDIR, XDG_RUNTIME_DIR, XDG_CONFIG_HOME,
// XDG_DATA_HOME and DBUS_SESSION_BUS_ADDRESS. No token or key can
// reach a container through it.
func PassEnv(environ []string) []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true,
		"XDG_RUNTIME_DIR": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "DBUS_SESSION_BUS_ADDRESS": true}
	var out []string
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); keep[k] {
			out = append(out, kv)
		}
	}
	return out
}

func (p Client) bin() string {
	if p.Bin == "" {
		return "podman"
	}
	return p.Bin
}

// run runs podman with args; the error is only for a command that did not
// run to an exit (a non-zero exit is in the Result, as Python check=False).
func (p Client) run(ctx context.Context, c execx.Cmd, args ...string) (execx.Result, error) {
	c.Argv = append([]string{p.bin()}, args...)
	c.Env = p.Env
	res, err := p.Exec.Run(ctx, c)
	if err != nil {
		return res, fmt.Errorf("podman %s: %w", args[0], err)
	}
	return res, nil
}

// remover is the Cleanup of a named container: `podman rm -f --time 5`,
// on its own short deadline (the run's context is already done).
func (p Client) remover(name string) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		p.run(ctx, execx.Cmd{Timeout: cleanupTimeout}, "rm", "-f", "--time", "5", name)
	}
}

// Remove is `podman rm -f --time 5 <name>`.
func (p Client) Remove(ctx context.Context, name string) (execx.Result, error) {
	if err := checkName(name); err != nil {
		return execx.Result{}, err
	}
	return p.run(ctx, execx.Cmd{Timeout: cleanupTimeout}, "rm", "-f", "--time", "5", name)
}

// checkName allows podman's container names, [a-zA-Z0-9][a-zA-Z0-9_.-]*.
func checkName(name string) error {
	ok := name != ""
	for i := 0; ok && i < len(name); i++ {
		c := name[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '.' || c == '-')
	}
	if !ok {
		return fmt.Errorf("bad container name %q", name)
	}
	return nil
}

// checkWord refuses an empty value or one podman would read as an option.
func checkWord(what, v string) error {
	if v == "" || strings.HasPrefix(v, "-") {
		return fmt.Errorf("podman: bad %s %q", what, v)
	}
	return nil
}

// ImageExists is `podman image exists <tag>`: false for a non-zero exit.
func (p Client) ImageExists(ctx context.Context, tag string) (bool, error) {
	if err := checkWord("image", tag); err != nil {
		return false, err
	}
	res, err := p.run(ctx, execx.Cmd{}, "image", "exists", tag)
	if err != nil {
		return false, err
	}
	return res.Code == 0, nil
}

// Build is one image build.
type Build struct {
	Tag     string   // -t: the candidate tag
	Context string   // the build context directory
	Args    []string // --build-arg values, "NAME=value", in order
}

// Build is `podman build --pull=newer --no-cache [--build-arg A]... -t <tag> <context>`
// in Dir, 6000 s.
func (p Client) Build(ctx context.Context, b Build) (execx.Result, error) {
	if err := checkWord("tag", b.Tag); err != nil {
		return execx.Result{}, err
	}
	if err := checkWord("build context", b.Context); err != nil {
		return execx.Result{}, err
	}
	args := []string{"build", "--pull=newer", "--no-cache"}
	for _, a := range b.Args {
		args = append(args, "--build-arg", a)
	}
	args = append(args, "-t", b.Tag, b.Context)
	return p.run(ctx, execx.Cmd{Dir: p.Dir, Timeout: BuildTimeout}, args...)
}

// Tag is `podman tag <src> <dst>`, unchecked.
func (p Client) Tag(ctx context.Context, src, dst string) (execx.Result, error) {
	if err := checkWord("image", src); err != nil {
		return execx.Result{}, err
	}
	if err := checkWord("tag", dst); err != nil {
		return execx.Result{}, err
	}
	return p.run(ctx, execx.Cmd{}, "tag", src, dst)
}

// Rmi is `podman rmi <tag>`, unchecked.
func (p Client) Rmi(ctx context.Context, tag string) (execx.Result, error) {
	if err := checkWord("image", tag); err != nil {
		return execx.Result{}, err
	}
	return p.run(ctx, execx.Cmd{}, "rmi", tag)
}

// Prune is `podman image prune -f`, unchecked.
func (p Client) Prune(ctx context.Context) (execx.Result, error) {
	return p.run(ctx, execx.Cmd{}, "image", "prune", "-f")
}

// Smoke is the rebuild's smoke test of a candidate image.
type Smoke struct {
	Name   string // factory-smoke-<kind>
	Image  string
	Script string // run by /bin/sh -c
}

// RunSmoke is `podman run --rm --name N --network none <keep-id> --user 1000:1000
// --entrypoint /bin/sh <image> -c <script>` in Dir, 2400 s. The
// result is unchecked: the caller reads its code and stdout.
func (p Client) RunSmoke(ctx context.Context, s Smoke) (execx.Result, error) {
	if err := checkName(s.Name); err != nil {
		return execx.Result{}, err
	}
	if err := checkWord("image", s.Image); err != nil {
		return execx.Result{}, err
	}
	return p.run(ctx, execx.Cmd{Dir: p.Dir, Timeout: SmokeTimeout, Cleanup: p.remover(s.Name)},
		"run", "--rm", "--name", s.Name, "--network", "none", KeepID, "--user", "1000:1000",
		"--entrypoint", "/bin/sh", s.Image, "-c", s.Script)
}

// Gate is one run of a project's install, test and lint on a fresh clone.
type Gate struct {
	Name    string   // factory-gate-<id> or factory-merge-<id>
	Image   string   // the gate image, or the rebuild's candidate
	Memory  string   // [projects.X] gate_memory, else [gate] memory, else "900m" (the caller's choice)
	Env     []string // [gate] env as "K=V", in order: -e each
	Mounts  []string // [gate] mounts, {root} already replaced: -v each
	Workdir string   // the clone on the host, mounted at /work
	Script  string   // "set -e; <install>; <test>; <lint>", run by sh -c
}

// RunGate runs the gate's container (checks_on_fresh_checkout):
// no network, the keep-id user, the memory limit, 1800 s. ok is a zero
// exit; tail is the last 4000 code points of stdout then stderr. The error
// is for a run that did not finish (it timed out, was cancelled, or podman
// would not start); its container is removed then.
func (p Client) RunGate(ctx context.Context, g Gate) (ok bool, tail string, err error) {
	if err := checkName(g.Name); err != nil {
		return false, "", err
	}
	if err := checkWord("image", g.Image); err != nil {
		return false, "", err
	}
	if err := checkWord("memory", g.Memory); err != nil {
		return false, "", err
	}
	if g.Workdir == "" || strings.Contains(g.Workdir, ":") {
		return false, "", fmt.Errorf("podman: bad gate directory %q", g.Workdir)
	}
	args := []string{"run", "--rm", "--name", g.Name, "--entrypoint", "", "--network", "none",
		KeepID, "--user", "1000:1000", "--memory", g.Memory}
	for _, e := range g.Env {
		args = append(args, "-e", e)
	}
	for _, m := range g.Mounts {
		args = append(args, "-v", m)
	}
	args = append(args, "-v", g.Workdir+":/work", "-w", "/work", g.Image, "sh", "-c", g.Script)
	res, err := p.run(ctx, execx.Cmd{Timeout: GateTimeout, Cleanup: p.remover(g.Name)}, args...)
	if err != nil {
		return false, "", err
	}
	return res.Code == 0, LastRunes(string(res.Stdout)+string(res.Stderr), GateTail), nil
}

// Running is the names of the containers podman runs (`podman ps --format
// {{.Names}}`) that start with prefix ("" for all).
func (p Client) Running(ctx context.Context, prefix string) ([]string, error) {
	res, err := p.run(ctx, execx.Cmd{Timeout: time.Minute}, "ps", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	if err := execx.CheckErr(res); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range strings.Split(res.Out(), "\n") {
		if n = strings.TrimSpace(n); n != "" && strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out, nil
}

// LastRunes is Python s[-n:]: the last n code points. Invalid UTF-8 counts
// one byte per code point, as a replacement character would.
func LastRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := len(s)
	for range n {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}
