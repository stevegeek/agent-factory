// Package helper is factory-remote, the server side of the dispatcher ↔
// helper contract (ARCHITECTURE.md §2.3, inventory-pipeline §15.3,
// inventory-contracts §3). The dispatcher's SSH key is forced to this
// binary, so the nine verbs of jobproto.SSHVerbs, read from
// SSH_ORIGINAL_COMMAND, are all it can do on a helper host: the job verbs;
// `vitals`, the host's own load, memory, disks, temperature, containers
// (vitals.Collect) and images.json; `build` on a builder host; and
// `install` on a host whose maintainer opted it in. Two more verbs
// come only from the local argv, never over SSH: `run <job>` (the job's
// systemd unit) and `rebuild` (the weekly image rebuild). Invoked as
// `factory` (the helper's bin/factory symlink, which the root-installed
// rebuild unit runs), it takes only `rebuild` and `version`.
//
// On the dispatcher it has one more mode, `factory-remote mailbox <host>`:
// the forced command of a pull host's key, which serves that host's
// mailbox (claim, beat, done, release, vitals; internal/mailbox) and
// nothing else.
//
// The helper links no dispatcher, board, config or GitHub code (§2.2 rule
// 5): it decodes its own Config straight from the TOML.
package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/mailbox"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/toml"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// usage is bin/factory-remote's docstring, printed for an unknown verb.
const usage = `Runs dispatcher stages as Pi or Claude Code jobs on a helper host. The dispatcher's SSH key is
forced to this script, so these verbs are all it can do here:

  start <job>     read a tar from stdin ([repo.bundle], prompt.md, role.md, meta.json,
                  untracked/...), start the job in the background
  status <job>    {"state": "running" | "done" | "missing"}
  collect <job>   tar on stdout: report.json, agent.log, prompt.md, head, out.bundle, memory.tar
  tail <job>      the last 64 KB of a job's output while it runs (for the live view)
  remove <job>    stop the job and delete its files
  list            job names, one per line
  vitals          this host's load, memory, disks, temperature, containers and images (JSON)
  build <commit>  read a source tar from stdin, build it with the pinned Go in a container with no
                  network and no credential, answer a tar of the three binaries and BUILD
                  (only with build = true; one at a time)
  install <commit>
                  read the master's bundle from stdin (factory-remote, BUILD, pi-run, config.toml,
                  units, image contexts), check it against BUILD, run the new factory-remote, then
                  install it, keeping .prev (only with ~/factory/accept-installs; one at a time)

Each job runs with only its work/ and home/ directories mounted, plus what this host's
config.toml sets under [remote] and, over it, [remote.<this host's short hostname>]
(agent, auth, image, memory, cpus, mounts, env, max_jobs, deps); the caller only picks the
branch, model and timeout. An install (only with ~/factory/accept-installs) replaces that
config.toml with the master's. The one credential auth names (the Anthropic API key, or
for Claude Code the subscription token) is read from its 600 file into the
environment, never argv. A job can read its credential, so whoever holds the
dispatcher key can spend it, or send it away, through a job's prompt: give this host
its own API key with a spend limit, above all if its jobs take untrusted input. A
subscription token is the maintainer's own and long-lived (revoke it in claude.ai's
settings, make a new one with claude setup-token).`

// factoryUsage is what the helper's bin/factory symlink says to anything
// but its two verbs.
const factoryUsage = "factory on a helper host runs only `factory rebuild` and `factory version`"

// DrainingFile is set by the host's weekly reboot window (host/updates.sh):
// while it exists, start refuses new jobs.
const DrainingFile = "/run/agent-factory-draining"

// Server is one invocation of the helper. Every path is derived from Root
// and Home; every command goes through Exec.
type Server struct {
	Root string // FACTORY_ROOT, or ~/factory
	Home string // the factory user's home: the API key lives under it
	Self string // this binary's absolute path: what a job's unit runs as `<Self> run <job>`
	// Host is this helper's name in config: its [remote.<Host>] goes over
	// [remote]. Main sets HostName(); "" applies none.
	Host     string
	Draining string // DrainingFile; tests point it elsewhere
	Exec     execx.Execer
	Clock    clock.Clock
	// Environ is this process's environment. Only SSH_ORIGINAL_COMMAND and
	// the names podman.PassEnv keeps are read from it.
	Environ []string
	// FreeDisk is the space free to an unprivileged user on dir's
	// filesystem (shutil.disk_usage().free); nil asks statfs(2).
	FreeDisk func(dir string) (uint64, error)
	// ProcRoot is where vitals finds /proc and /sys ("" is "/"), and
	// Statfs its disks (nil is vitals.Statfs); tests point them elsewhere.
	ProcRoot string
	Statfs   func(path string) (vitals.Disk, error)
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	// Latest is an npm package's latest release, for the rebuild's
	// automatic updates (§15.1); nil asks the registry
	// (FACTORY_NPM_REGISTRY, else registry.npmjs.org).
	Latest func(ctx context.Context, pkg string) (string, error)
}

// Main is factory-remote: it serves one call and returns the exit status.
// argv is the whole os.Args; environ is os.Environ().
func Main(ctx context.Context, argv, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	s := &Server{
		Draining: DrainingFile,
		Exec:     execx.OS{},
		Clock:    clock.System{},
		Environ:  environ,
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
	}
	s.Host = HostName()
	s.Home = lookup(environ, "HOME")
	if s.Home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			s.Home = h
		}
	}
	s.Root = lookup(environ, "FACTORY_ROOT")
	if s.Root == "" {
		s.Root = filepath.Join(s.Home, "factory")
	}
	if self, err := os.Executable(); err == nil {
		// a binary replaced while it runs reads as "<path> (deleted)"
		s.Self = strings.TrimSuffix(self, " (deleted)")
	}
	return s.Serve(ctx, argv)
}

// refusal is a call the helper turns down: its message on stderr, exit 2
// (Python's die()).
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// Serve runs the call argv names (argv[0] is the name it was invoked by)
// and returns its exit status: 0, 2 for a refusal, 1 for a failure.
func (s *Server) Serve(ctx context.Context, argv []string) int {
	if len(argv) > 1 && argv[1] == jobproto.MailMode && s.name(argv) == "factory-remote" {
		return s.mailbox(argv[2:])
	}
	err := s.serve(ctx, argv)
	var r *refusal
	switch {
	case err == nil:
		return 0
	case errors.As(err, &r):
		fmt.Fprintln(s.Stderr, r.msg)
		return 2
	}
	fmt.Fprintf(s.Stderr, "%s: %v\n", s.name(argv), err)
	return 1
}

func (s *Server) name(argv []string) string {
	if len(argv) > 0 && filepath.Base(argv[0]) == "factory" {
		return "factory"
	}
	return "factory-remote"
}

func (s *Server) serve(ctx context.Context, argv []string) error {
	var args []string
	if len(argv) > 1 {
		args = argv[1:]
	}
	sshCommand, overSSH := lookupOK(s.Environ, "SSH_ORIGINAL_COMMAND")
	if s.name(argv) == "factory" {
		switch {
		case overSSH || len(args) != 1:
			return refuse("%s", factoryUsage)
		case args[0] == "version":
			_, err := fmt.Fprintln(s.Stdout, buildinfo.String())
			return err
		case args[0] == "rebuild":
			return s.rebuild(ctx)
		}
		return refuse("%s", factoryUsage)
	}
	if err := os.MkdirAll(s.jobs(), 0o777); err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	if overSSH {
		// the verb comes from the forced command's line alone: whatever
		// argv holds, run and rebuild never arrive this way
		if len(args) != 0 {
			return refuse("%s", usage)
		}
		var err error
		if args, err = shlexSplit(sshCommand); err != nil {
			return refuse("%s", usage)
		}
	} else if len(args) > 0 {
		switch {
		case args[0] == jobproto.VerbRun && len(args) == 2:
			return s.run(ctx, args[1])
		case args[0] == "rebuild" && len(args) == 1:
			return s.rebuild(ctx)
		case args[0] == "version" && len(args) == 1:
			_, err := fmt.Fprintln(s.Stdout, buildinfo.String())
			return err
		}
	}
	if len(args) == 0 || !jobproto.IsSSHVerb(args[0]) {
		return refuse("%s", usage)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case jobproto.VerbList:
		return s.list()
	case jobproto.VerbVitals:
		if len(rest) != 0 {
			return refuse("usage: %s", verb)
		}
		return s.vitals(ctx)
	}
	if len(rest) != 1 {
		return refuse("usage: %s <job>", verb)
	}
	job := rest[0]
	switch verb {
	case jobproto.VerbStart:
		return s.start(ctx, job)
	case jobproto.VerbStatus:
		return s.status(ctx, job)
	case jobproto.VerbCollect:
		return s.collect(job)
	case jobproto.VerbTail:
		return s.tail(job)
	case jobproto.VerbBuild:
		return s.build(ctx, job)
	case jobproto.VerbInstall:
		return s.install(ctx, job)
	case jobproto.VerbRemove:
		return s.remove(ctx, job)
	}
	return refuse("%s", usage)
}

// mailbox is mailbox mode, `factory-remote mailbox <host>`: the forced
// command of a pull host's key on the dispatcher (internal/mailbox). Its
// argv comes from authorized_keys, so the host it names is the only one
// the key acts for; the verb comes from SSH_ORIGINAL_COMMAND. Nothing here
// runs a command, reads the config or touches jobs/.
func (s *Server) mailbox(args []string) int {
	command, overSSH := lookupOK(s.Environ, "SSH_ORIGINAL_COMMAND")
	if !overSSH || len(args) != 1 {
		fmt.Fprintln(s.Stderr, "factory-remote mailbox HOST runs only as the forced command of a pull host's ssh key")
		return 2
	}
	box := mailbox.Box{Dir: filepath.Join(s.Root, "mailbox"), Clock: s.Clock}
	return box.Serve(args[0], command, s.Stdin, s.Stdout, s.Stderr)
}

func (s *Server) jobs() string       { return filepath.Join(s.Root, "jobs") }
func (s *Server) configPath() string { return filepath.Join(s.Root, "config.toml") }
func (s *Server) config() (*Config, error) {
	return LoadConfig(s.configPath(), s.Host)
}
func (s *Server) keyFile() string {
	return filepath.Join(s.Home, ".config", "factory", "anthropic-api-key")
}
func (s *Server) tokenFile() string {
	return filepath.Join(s.Home, ".config", "factory", "claude-oauth-token")
}

// HostName is the helper's name for [remote.<name>]: the kernel's
// hostname up to its first dot, lowercased. It is set by root on the
// helper, so neither the dispatcher's key nor the SSH session's
// environment can choose which settings a job gets.
func HostName() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

// jobDir is job_dir: the job's directory, once its name passes JOB_NAME.
func (s *Server) jobDir(job string) (string, error) {
	if !jobproto.JobNameOK(job) {
		return "", refuse("bad job name: %s", toml.PyRepr(job))
	}
	return filepath.Join(s.jobs(), job), nil
}

// unit is a job's systemd unit, which is also its container's name.
func unit(job string) string { return "factory-job-" + job }

// env is the minimal environment of every command the helper runs
// (§5.3): podman, systemd-run and systemctl need only the user's runtime
// dir and bus, never what the SSH session or the unit carried.
func (s *Server) env() []string { return podman.PassEnv(s.Environ) }

func (s *Server) freeDisk(dir string) (uint64, error) {
	if s.FreeDisk != nil {
		return s.FreeDisk(dir)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return st.Bavail * uint64(st.Frsize), nil
}

func lookup(environ []string, key string) string {
	v, _ := lookupOK(environ, key)
	return v
}

// lookupOK finds key in environ; the last entry wins, as in Python's
// os.environ.
func lookupOK(environ []string, key string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(environ[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}
