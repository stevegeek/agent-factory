package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/builds"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/sshjobs"
)

// runBuild is `factory build COMMIT12 [--host NAME]`: build a core commit
// on the builder helper ([build] host, or --host) and store the checked
// result as a candidate in ~/factory/builds/<commit12>, which deploy.sh's
// candidate verb installs. The source is the tree deploy.sh extracted for
// that commit (~/factory/src/<commit12>). An existing candidate is kept.
// With [build] host = "self" (P6) this host's own factory-remote builds it.
func runBuild(ctx context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	commit := a.Pos("commit")
	if !jobproto.Commit12OK(commit) {
		return fmt.Errorf("bad commit %q: the first 12 hex digits of its id", commit)
	}
	name := a.Str("host")
	if name == "" {
		name = cfg.Build.Host
	}
	if name == "" {
		return errors.New("no builder: set [build] host, or --host")
	}
	var h *config.HostCfg
	if name != config.BuildSelf {
		var ok bool
		if h, ok = cfg.HostNamed(name); !ok || h.Pull() {
			return fmt.Errorf("%s is not an ssh helper in [hosts]", name)
		}
	}
	p := e.Paths()
	dir := p.Builds()
	if builds.Has(dir, commit) {
		fmt.Fprintf(e.Stdout, "%s is built already: %s\n", commit, filepath.Join(dir, commit))
		return nil
	}
	src, pin, err := builds.SourceTar(p.Src(commit))
	if err != nil {
		return fmt.Errorf("no source to build %s (deploy.sh puts it in %s): %w", commit, p.Src(commit), err)
	}
	log := e.Log()
	var answer []byte
	if h == nil {
		answer, err = buildSelf(ctx, e, commit, src)
	} else {
		answer, err = sshjobs.Client{Exec: e.Exec}.Do(ctx, hostrun.HostOf(p, *h), jobproto.VerbBuild, commit, src, 0)
	}
	if err == nil {
		err = builds.Put(dir, commit, pin, name, answer)
	}
	if err != nil {
		log.Printf("build %s on %s: failed: %v", commit, name, err)
		return fmt.Errorf("build %s on %s: %w", commit, name, err)
	}
	log.Printf("build %s on %s: stored", commit, name)
	fmt.Fprintf(e.Stdout, "built %s on %s: %s\n", commit, name, filepath.Join(dir, commit))
	return nil
}

// buildSelf runs this host's `factory-remote build COMMIT` with the source
// on stdin, as a helper runs it over ssh: the same verb, timeout and output
// cap, reading its own [remote.<hostname>]. SSH_ORIGINAL_COMMAND is left
// out of its environment, so the verb is taken from argv.
func buildSelf(ctx context.Context, e *Env, commit string, src []byte) ([]byte, error) {
	var env []string
	for _, kv := range environList(e.Environ) {
		if !strings.HasPrefix(kv, "SSH_ORIGINAL_COMMAND=") {
			env = append(env, kv)
		}
	}
	verb := jobproto.VerbBuild
	res, err := e.Exec.Run(ctx, execx.Cmd{Argv: []string{filepath.Join(e.Paths().Bin, "factory-remote"), verb, commit},
		Stdin: bytes.NewReader(src), Env: env, Timeout: jobproto.Timeout(verb), MaxOut: jobproto.OutputCap(verb)})
	switch {
	case errors.Is(err, execx.ErrTimeout):
		return nil, jobproto.TimedOut(config.BuildSelf, verb)
	case err != nil:
		return nil, fmt.Errorf("%s %s: %w", config.BuildSelf, verb, err)
	case res.Code != 0:
		return nil, jobproto.Failed(config.BuildSelf, verb, res.Code, res.Stderr)
	case res.Truncated:
		return nil, jobproto.TooMuch(config.BuildSelf, verb)
	}
	return res.Stdout, nil
}
