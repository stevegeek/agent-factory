package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/paths"
)

// What a run reads from the mirrors, at most.
const (
	maxDeploySh  = 1 << 20
	maxConfigTar = 64 << 20
	// tailBytes is how much of a failed run's log the state and the
	// notice carry.
	tailBytes = 2 << 10
)

// rootPaths are the files whose change needs a root step on the master
// (decision 18): git pathspecs for Mirror.Changed.
func rootPaths() []string {
	return []string{"host/", "systemd/factory@.service", "systemd/*.timer", "systemd/factory-rebuild.service", "systemd/install-units.sh"}
}

// Run is `factory deploy run`, the factory-deploy unit's command. It holds
// the deploy lock while it deploys each request it takes, until none
// waits (a second runner returns, as does deploy.sh prepare from the
// desktop). The lock is waited for a moment (RunLockWait): the tick's
// look for an interrupted deploy holds it briefly. Once it lets the lock
// go it looks for a request once more, so a start that came as it
// finished, and found the unit still there, is not left waiting.
func (s Service) Run(ctx context.Context, cfg *config.Config) error {
	if !cfg.Deploy.On() {
		return errors.New("no [deploy] in config: this host does not deploy itself")
	}
	if want := filepath.Join(s.Paths.Home, "factory"); filepath.Clean(s.Paths.Root) != want {
		return fmt.Errorf("deploy.sh installs into %s, not the factory root %s", want, s.Paths.Root)
	}
	if err := os.MkdirAll(s.Paths.Deploys(), 0o700); err != nil {
		return err
	}
	for {
		ran, err := s.runLocked(ctx, cfg)
		if !ran || err != nil {
			return err
		}
		if s.afterLock != nil {
			s.afterLock()
		}
		if _, waiting, _ := deployrec.ReadRequest(s.Paths.DeployRequest()); !waiting {
			return nil
		}
	}
}

// runLocked takes the deploy lock and runs the requests; ran is false
// when another runner holds the lock. A take that fails twice running
// (a request that cannot be renamed or removed) ends the run, which
// would otherwise spin on it.
func (s Service) runLocked(ctx context.Context, cfg *config.Config) (ran bool, err error) {
	wait := s.RunLockWait
	if wait == 0 {
		wait = RunLockWait
	}
	lock, err := fsx.Lock(ctx, s.Paths.DeployLock(), fsx.Wait(wait))
	if errors.Is(err, fsx.ErrBusy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer lock.Unlock()
	// a deploy still marked running under a free lock was cut off
	if err := s.interrupted(ctx); err != nil {
		s.logf("deploy: %v", err)
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		r, ok, err := deployrec.TakeRequest(s.Paths.DeployRequest())
		if err != nil {
			s.logf("deploy: a request did not read and is dropped: %v", err)
			if lastErr != nil {
				return true, err
			}
			lastErr = err
			continue
		}
		lastErr = nil
		if !ok {
			return true, nil
		}
		// the deploy before may have installed another config
		cfg = s.installedConfig(cfg)
		s.runOne(ctx, cfg, r)
	}
}

// run is one deploy's state as it goes, written at each step.
type run struct {
	s   Service
	st  deployrec.State
	log string // the deploy.sh log
}

func (r *run) step(step string) {
	r.st.Step = step
	if err := deployrec.WriteState(r.s.Paths.DeployState(), r.st); err != nil {
		r.s.logf("deploy: state: %v", err)
	}
}

// runOne deploys one request; what happened is in the state, the log and
// any notice.
func (s Service) runOne(ctx context.Context, cfg *config.Config, req deployrec.Request) {
	r := &run{s: s, st: deployrec.State{
		Phase: deployrec.PhaseRunning, Rev: req.Rev, By: req.By, Started: s.stamp(), From: buildCommit(s.Paths),
	}}
	r.step("setting up")
	result, msg := s.deploy(ctx, cfg, req, r)
	r.st.Phase, r.st.Step, r.st.Result, r.st.Finished = deployrec.PhaseDone, "", result, s.stamp()
	if result != deployrec.ResultOK {
		r.st.Tail = tail(r.log, msg)
	}
	if err := deployrec.WriteState(s.Paths.DeployState(), r.st); err != nil {
		s.logf("deploy: state: %v", err)
	}
	what := orString(r.st.Commit, req.Rev)
	s.logf("deploy %s (from %s, by %s): %s%s", what, orString(r.st.From, "-"), orFactory(req.By), result, suffix(msg))
	s.pruneLogs()
}

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return ": " + msg
}

// deploy runs the steps and returns the result, and for a failure the
// reason; it files the notices.
func (s Service) deploy(ctx context.Context, cfg *config.Config, req deployrec.Request, r *run) (string, string) {
	p := s.Paths
	fail := func(what, msg string) (string, string) {
		s.notice(ctx, "Deploy of "+what+" failed: nothing installed", msg+tailNote(r.log))
		return deployrec.ResultNothingInstalled, msg
	}
	work := p.DeployWork()
	if err := fsx.RemoveTree(work); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(req.Rev, err.Error())
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return fail(req.Rev, err.Error())
	}
	if err := s.desktopDeploy(); err != nil {
		return fail(req.Rev, err.Error())
	}

	r.step("fetching")
	core := gitx.Mirror{Dir: p.CoreMirror(), Base: s.MirrorBase, Exec: s.exec(), Env: s.environ()}
	conf := gitx.Mirror{Dir: p.ConfigMirror(), Base: s.MirrorBase, Exec: s.exec(), Env: s.environ()}
	if err := core.Fetch(ctx, cfg.Deploy.Core, ""); err != nil {
		return fail(req.Rev, err.Error())
	}
	token := ""
	if s.Token != nil {
		t, err := s.Token(ctx)
		if err != nil {
			return fail(req.Rev, "the config repo's read token: "+err.Error())
		}
		token = t
	}
	if err := conf.Fetch(ctx, cfg.Deploy.Config, token); err != nil {
		return fail(req.Rev, err.Error())
	}
	sha, err := core.Resolve(ctx, req.Rev)
	onMain := false
	if err == nil {
		onMain, err = core.OnMain(ctx, sha)
	}
	if err != nil || !onMain {
		return fail(req.Rev, req.Rev+" is not on core main")
	}
	c := sha[:12]
	r.st.Commit = c
	if req.Drill && c == r.st.From {
		return fail(c, "a drill needs a commit other than the running one")
	}
	confSha, err := conf.Resolve(ctx, "main")
	if err != nil {
		return fail(c, "config main: "+err.Error())
	}
	r.st.ConfigCommit = confSha[:12]

	r.step("preparing " + c)
	script, err := core.Show(ctx, sha, "deploy.sh", maxDeploySh)
	if err != nil {
		return fail(c, err.Error())
	}
	if err := os.WriteFile(filepath.Join(work, "deploy.sh"), script, 0o700); err != nil {
		return fail(c, err.Error())
	}
	tree, err := conf.ArchiveTree(ctx, confSha, configDir(cfg), maxConfigTar)
	if err == nil {
		err = extractTo(filepath.Join(work, "config"), tree)
	}
	if err != nil {
		return fail(c, "config "+r.st.ConfigCommit+": "+err.Error())
	}
	if err := sameDeploy(cfg, filepath.Join(work, "config", "config.toml")); err != nil {
		return fail(c, "config "+r.st.ConfigCommit+": "+err.Error())
	}

	r.step("snapshot")
	if err := Snapshot(p, c, r.st.From); err != nil {
		return fail(c, "snapshot: "+err.Error())
	}
	r.st.Snapshotted = true

	r.step("deploy.sh")
	if err := os.MkdirAll(p.DeployLogs(), 0o700); err != nil {
		return fail(c, err.Error())
	}
	r.log = filepath.Join(p.DeployLogs(), s.now().UTC().Format("20060102T150405Z")+"-"+c+".log")
	r.st.Log = r.log
	// a deploy of the running commit cannot tell the new Foreman by its
	// version: the check waits for one started after deploy.sh began
	// (deploy.sh always renames bin/foreman into place, which restarts it)
	var since time.Time
	if c == r.st.From {
		since = s.now()
	}
	// a drill restores whatever it installed, so it may take an older commit
	deployErr := s.deploySh(ctx, work, c, r.log, req.AllowOlder || req.Drill)

	var checkErr error
	switch {
	case deployErr != nil:
	case req.Drill:
		checkErr = errors.New("a drill: the check fails on purpose")
	default:
		r.step("checking")
		checkErr = s.Healthy(ctx, c, since)
	}
	if deployErr != nil || checkErr != nil {
		why := errors.Join(deployErr, checkErr).Error()
		if deployErr != nil && len(changedSinceSnapshot(p)) == 0 {
			return fail(c, why)
		}
		r.step("restoring " + orString(r.st.From, "the snapshot"))
		restoring := s.now()
		restored, rerr := s.Restore(ctx)
		if rerr == nil {
			// a foreman.env put back restarts Foreman: wait for that one
			var since time.Time
			if slices.Contains(restored, "foreman.env") {
				since = restoring
			}
			rerr = s.Healthy(ctx, r.st.From, since)
		}
		if rerr != nil {
			s.notice(ctx, "Deploy of "+c+" failed and the restored build is not healthy: deploy from the desktop",
				why+"\n\nThe restore: "+rerr.Error()+tailNote(r.log))
			return deployrec.ResultRollbackFailed, why + "; the restore: " + rerr.Error()
		}
		s.notice(ctx, "Deploy of "+c+" failed its check: "+orString(r.st.From, "the snapshot")+" is back",
			why+"\n\nPut back: "+orString(strings.Join(restored, ", "), "nothing differed")+tailNote(r.log))
		return deployrec.ResultRolledBack, why
	}

	if full := strings.TrimSpace(confSha); full != "" {
		if err := fsx.WriteAtomic(p.DeployConfigCommit(), []byte(full+"\n"), 0o644); err != nil {
			s.logf("deploy %s: config-commit: %v", c, err)
		}
	}
	r.step("helpers")
	// the helpers of the config just installed
	r.st.Helpers = s.updateHelpers(ctx, s.installedConfig(cfg), c)
	s.rootSteps(ctx, core, r.st.From, sha)
	return deployrec.ResultOK, ""
}

// installedConfig is the config.toml installed now, or was when it does
// not read.
func (s Service) installedConfig(was *config.Config) *config.Config {
	cfg, err := config.Load(s.Paths.Config)
	if err != nil {
		s.logf("deploy: %v: going on with the config read before", err)
		return was
	}
	return cfg
}

// deploySh runs the new commit's own deploy.sh against this host, staged
// in work/stage, its output appended to log.
func (s Service) deploySh(ctx context.Context, work, c, log string, allowOlder bool) error {
	p := s.Paths
	argv := []string{"bash", "-c", `exec bash "$@" >>"$0" 2>&1`, log, filepath.Join(work, "deploy.sh"),
		"--core", p.CoreMirror(), "--commit", c, "--config", filepath.Join(work, "config"),
		"--host", "self", "--build", "helper"}
	if allowOlder {
		argv = append(argv, "--allow-older")
	}
	env := s.environ("FACTORY_DEPLOY_SELF=1", "FACTORY_DEPLOY_DIR="+filepath.Join(work, "stage"),
		"HOME="+p.Home, "FACTORY_ROOT="+p.Root)
	res, err := s.exec().Run(ctx, execx.Cmd{Argv: argv, Env: env, Dir: work, Timeout: DeployShTimeout})
	if err == nil {
		err = execx.CheckErr(res)
	}
	if err != nil {
		return fmt.Errorf("deploy.sh: %w", err)
	}
	return nil
}

// environ is this process's environment as KEY=value lines, without any
// token, with extra set over it.
func (s Service) environ(extra ...string) []string {
	vars := map[string]string{}
	for k, v := range s.Env {
		if k != "FACTORY_GH_TOKEN" && !strings.HasPrefix(k, "FACTORY_DEPLOY_") {
			vars[k] = v
		}
	}
	for _, kv := range extra {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// sameDeploy refuses a new config.toml whose [deploy] names other repos
// or another config directory than the running one: config main decides
// what the next deploy fetches and runs, so moving it takes a desktop
// deploy.
func sameDeploy(cfg *config.Config, path string) error {
	next, err := config.Load(path)
	if err != nil {
		return err
	}
	for _, f := range []struct{ name, have, want string }{
		{"core", cfg.Deploy.Core, next.Deploy.Core},
		{"config", cfg.Deploy.Config, next.Deploy.Config},
		{"config_dir", configDir(cfg), configDir(next)},
	} {
		if f.have != f.want {
			return fmt.Errorf("[deploy] %s changes from %q to %q: changing [deploy] takes a desktop deploy", f.name, f.have, f.want)
		}
	}
	return nil
}

func configDir(cfg *config.Config) string {
	return orString(cfg.Deploy.ConfigDir, "config")
}

// extractTo unpacks a tar into dir, made fresh, through fsx.Extract.
func extractTo(dir string, tarball []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fsx.Extract(root, bytes.NewReader(tarball))
}

// buildCommit is the commit bin/BUILD names ("commit <c> go <v>"), or "".
func buildCommit(p paths.Paths) string {
	b, err := fsx.ReadNoFollow(filepath.Join(p.Bin, "BUILD"), 64<<10)
	if err != nil {
		return ""
	}
	return commitOf(b)
}

func commitOf(build []byte) string {
	for _, line := range strings.Split(string(build), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "commit" {
			return f[1]
		}
	}
	return ""
}

// tail is the end of the log, or msg when there is none.
func tail(log, msg string) string {
	if log != "" {
		if t := logTail(log); t != "" {
			return t
		}
	}
	return msg
}

func logTail(log string) string {
	b, err := os.ReadFile(log)
	if err != nil {
		return ""
	}
	if len(b) > tailBytes {
		b = b[len(b)-tailBytes:]
	}
	return strings.ToValidUTF8(string(b), "")
}

func tailNote(log string) string {
	if log == "" {
		return ""
	}
	t := logTail(log)
	if t == "" {
		return ""
	}
	return "\n\nThe end of " + log + ":\n\n```\n" + t + "\n```"
}

// pruneLogs keeps the newest KeepLogs logs (their names start with the
// time).
func (s Service) pruneLogs() {
	dir := s.Paths.DeployLogs()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".log") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > KeepLogs {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			s.logf("deploy: %v", err)
		}
		names = names[1:]
	}
}
