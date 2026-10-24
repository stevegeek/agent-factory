package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// jobScript runs inside the container as uid 1000, byte for byte
// Python's JOB_SCRIPT. /job is the job's work/ directory (the agent can
// write there); exit, run.log and meta.json stay outside it, so the agent
// cannot fake them. The prompt comes on stdin (Pi reads piped stdin as
// the message), as Claude's does: an argument would be parsed as options
// when it starts with "-" and fail exec (E2BIG) over 128 KiB. This one
// line departs from Python's script; the frozen copy has it too.
const jobScript = jobSetup + piRun + jobFinish

// claudeScript is jobScript with Claude Code in Pi's place. The image's
// managed rules (/etc/claude-code/CLAUDE.md) load by themselves, so only
// the role is appended, joined to its flag so a leading "-" stays text;
// the prompt comes on stdin, so neither its size nor a leading "-" matters. Permissions are skipped as Scion's Claude harness
// skips them: the container is the sandbox.
const claudeScript = jobSetup + claudeRun + jobFinish

const jobSetup = `
set -u
cd /job
if [ -f repo.bundle ]; then
  git -c init.defaultBranch=main clone -q --no-checkout repo.bundle repo && cd repo \
    && git checkout -q -B "$JOB_BRANCH" "origin/$JOB_START" || exit 1
else
  # a task with no project: scratch space, a repository only so the head check works
  mkdir repo && cd repo && git init -q -b "$JOB_BRANCH" \
    && git -c user.name=job -c user.email=job@localhost commit -q --allow-empty -m scratch || exit 1
fi
cp -a /job/untracked/. . 2>/dev/null || true
`

const piRun = `pi --provider anthropic --model "$JOB_MODEL" -p --no-session --no-approve \
   --append-system-prompt /etc/agent-rules.md --append-system-prompt /job/role.md \
   < /job/prompt.md > /job/agent.log 2>&1
echo "pi exited $?" >> /job/agent.log
`

const claudeRun = `claude -p --model "$JOB_MODEL" --dangerously-skip-permissions \
   --append-system-prompt="$(cat /job/role.md)" < /job/prompt.md > /job/agent.log 2>&1
echo "claude exited $?" >> /job/agent.log
`

const jobFinish = `git rev-parse "refs/heads/$JOB_BRANCH" > /job/head
if [ "$JOB_BUNDLE" = 1 ]; then git bundle create -q /job/out.bundle "refs/heads/$JOB_BRANCH"; fi
if [ -d /job/memory ]; then tar -C /job/memory -cf /job/memory.tar .; fi
`

// depsFailed is the report of a job whose dependencies did not install:
// blocked, as the dispatcher's own failed reports are, so the ticket goes
// to a person with the reason rather than as "finished without a report".
const depsFailed = `{"status": "blocked", "verdict": "fail", "summary": "The dependencies did not install on this helper (bundle or shards install failed, or the shared cache stayed locked by another install too long; the end of deps.log is in agent.log), so the agent did not run."}`

// depsStep runs deps without the job's credential, since a Gemfile and a
// gem's extconf are code from the checkout, and, when the image has flock
// and BUNDLE_PATH is a writable directory, one job at a time on the
// shared cache: a wait over 900 s is a failed install. Without either it
// runs unlocked.
const depsStep = `deps_step() (
  unset CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_API_KEY
  if [ -n "${BUNDLE_PATH:-}" ] && [ -d "$BUNDLE_PATH" ] && [ -w "$BUNDLE_PATH" ] && command -v flock > /dev/null 2>&1; then
    (
      flock -w 900 9 || { echo "waited 900 s for another job's install (lock $BUNDLE_PATH/.factory-deps.lock)" >> "$JOB/deps.log"; exit 1; }
      deps
    ) 9>> "$BUNDLE_PATH/.factory-deps.lock"
  else
    deps
  fi
)`

// withDeps is a harness's script on a host with [remote] deps: the
// checkout's dependencies installed first (jobproto.DepsFunc through
// depsStep, into the writable cache the host mounts), and the agent run
// only when they are in, with its credential; otherwise the end of
// deps.log goes to agent.log and the report is depsFailed. head, the
// bundle and memory are written either way.
func withDeps(run string) string {
	return jobSetup + "JOB=/job\n" + jobproto.DepsFunc + "\n" + depsStep + `
if deps_step; then
` + run + `else
  echo "the dependencies did not install; the agent did not run" > /job/agent.log
  tail -n 40 /job/deps.log >> /job/agent.log 2>/dev/null
  printf '%s\n' '` + depsFailed + `' > /home/scion/report.json
fi
` + jobFinish
}

// Harness is one command a job can run: the script the container's
// /bin/sh runs (Run between the checkout and the finish), and what it adds
// to the job's environment.
type Harness struct {
	Name   string
	Script string
	Run    string
	Env    []string // "K=V", after the job's own
}

// harnesses is the table of what a helper can run; its [remote] agent
// picks one. A new harness is a row here.
func harnesses() []Harness {
	return []Harness{{Name: AgentPi, Script: jobScript, Run: piRun}, {Name: AgentClaude, Script: claudeScript, Run: claudeRun}}
}

func harnessFor(name string) (Harness, bool) {
	for _, h := range harnesses() {
		if h.Name == name {
			return h, true
		}
	}
	return Harness{}, false
}

// harnessOn is the harness a job runs on this host: the host's agent. The
// dispatcher leaves meta.json's harness out (the default); one it names
// must be the host's.
func harnessOn(cfg *Config, meta jobproto.Meta) (Harness, error) {
	if meta.Harness != jobproto.DefaultHarness && meta.Harness != cfg.Remote.Agent {
		return Harness{}, refuse("no harness %q on this host", meta.Harness)
	}
	h, _ := harnessFor(cfg.Remote.Agent)
	return h, nil
}

// credentialEnv are the names a credential travels under: a job gets the
// one its auth names, from its file, and never another from [remote] env.
func credentialEnv() []string {
	return []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"}
}

// Container limits of every job.
const (
	jobPids      = "512"
	maxKeyBytes  = 64 << 10
	maxRunOutput = 4 << 20 // of each of podman's streams kept for run.log
	// cleanupTimeout bounds removing a cancelled run's container.
	cleanupTimeout = 30 * time.Second
)

// run is `run <job>` (run_job), started by the job's unit: the
// job's container, with the helper's own image, agent, memory, cpus,
// mounts and env, and its one credential read from its file into podman's
// environment (never argv). Its output goes to run.log, and its status to
// exit, which is written whatever happens (255 when the run failed before
// podman gave one): status reads "done" from it.
func (s *Server) run(ctx context.Context, job string) (err error) {
	d, err := s.jobDir(job)
	if err != nil {
		return err
	}
	rc := 255
	defer func() {
		werr := fsx.WriteAtomic(filepath.Join(d, "exit"), []byte(strconv.Itoa(rc)), 0o644)
		if werr != nil {
			err = errors.Join(err, fmt.Errorf("exit file: %w", werr))
		}
	}()
	cmd, err := s.jobCmd(d, job)
	if err != nil {
		return err
	}
	res, err := s.Exec.Run(ctx, cmd)
	if err != nil {
		// A cancel (SIGTERM from the unit's stop): execx TERMed
		// podman's client and cmd.Cleanup ran `podman rm -f --time 5`,
		// which stops the container (TERM, then KILL after 5 s) and removes
		// it. exit stays 255, since no container status came back; what
		// the run printed so far still goes to run.log.
		if out := append(res.Stdout, res.Stderr...); len(out) > 0 {
			if werr := fsx.WriteAtomic(filepath.Join(d, "run.log"), out, 0o644); werr != nil {
				err = errors.Join(err, fmt.Errorf("run.log: %w", werr))
			}
		}
		return fmt.Errorf("run %s: %w", job, err)
	}
	rc = res.Code
	out := append(res.Stdout, res.Stderr...)
	if err := fsx.WriteAtomic(filepath.Join(d, "run.log"), out, 0o644); err != nil {
		return fmt.Errorf("run.log: %w", err)
	}
	return nil
}

// jobCmd is the job's podman run.
func (s *Server) jobCmd(d, job string) (execx.Cmd, error) {
	meta, err := loadMeta(d)
	if err != nil {
		return execx.Cmd{}, fmt.Errorf("meta.json: %w", err)
	}
	cfg, err := s.config()
	if err != nil {
		return execx.Cmd{}, err
	}
	h, err := harnessOn(cfg, meta)
	if err != nil {
		return execx.Cmd{}, err
	}
	var env orderedEnv
	creds := credentialEnv()
	for _, kv := range cfg.JobEnv() {
		k, v, _ := strings.Cut(kv, "=")
		if !slices.Contains(creds, k) {
			env.set(k, v)
		}
	}
	bundle := "0"
	if meta.Bundle {
		bundle = "1"
	}
	for _, kv := range [][2]string{{"JOB_BRANCH", meta.Branch}, {"JOB_START", meta.Start}, {"JOB_MODEL", meta.Model},
		{"JOB_BUNDLE", bundle}, {"HOME", "/home/scion"}} {
		env.set(kv[0], kv[1])
	}
	for _, kv := range h.Env {
		k, v, _ := strings.Cut(kv, "=")
		env.set(k, v)
	}
	credName, credFile, credWhat := "ANTHROPIC_API_KEY", s.keyFile(), "the API key"
	if cfg.Remote.Auth == AuthSubscription {
		credName, credFile, credWhat = "CLAUDE_CODE_OAUTH_TOKEN", s.tokenFile(), "the Claude token"
	}
	cred, err := readCapped(credFile, maxKeyBytes)
	if err == nil && textx.Strip(string(cred)) == "" {
		err = fmt.Errorf("%s is empty", credFile)
	}
	if err != nil {
		return execx.Cmd{}, fmt.Errorf("%s (auth = %q): %w", credWhat, cfg.Remote.Auth, err)
	}
	name := unit(job)
	argv := []string{"podman", "run", "--rm", "--name", name, "--timeout", strconv.Itoa(meta.TimeoutMinutes * 60),
		"--memory", cfg.Remote.Memory, "--cpus", cfg.Remote.CPUs, "--pids-limit", jobPids,
		"--cap-drop=all", "--security-opt=no-new-privileges", podman.KeepID, "--user", "1000:1000",
		"-e", credName}
	for _, kv := range env.list() {
		argv = append(argv, "-e", kv)
	}
	argv = append(argv, "-v", d+"/work:/job", "-v", d+"/home:/home/scion")
	for _, m := range cfg.Remote.Mounts {
		argv = append(argv, "-v", strings.ReplaceAll(m, "{root}", s.Root))
	}
	script := h.Script
	if cfg.Remote.Deps {
		script = withDeps(h.Run)
	}
	argv = append(argv, "-w", "/job", "--entrypoint", "/bin/sh", cfg.Remote.Image, "-c", script)
	pm := podman.Client{Exec: s.Exec, Env: s.env()}
	return execx.Cmd{
		Argv:   argv,
		Dir:    "/",
		Env:    append(s.env(), credName+"="+textx.Strip(string(cred))),
		MaxOut: maxRunOutput,
		// systemd stopping the unit cancels the run: the container goes too
		Cleanup: func() {
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			pm.Remove(ctx, name)
		},
	}, nil
}

// readCapped reads a file the factory user owns (the key, as Python's
// open() does: links followed), refusing one over max bytes.
func readCapped(name string, max int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", name, max)
	}
	return b, nil
}
