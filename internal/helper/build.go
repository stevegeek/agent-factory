package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/podman"
)

// buildTimeout bounds the build container, inside the verb's 20 minutes.
const buildTimeout = 18 * time.Minute

// buildLogTail is how much of a failed build's output its error carries.
const buildLogTail = 2 << 10

// build is `build <commit12>` on a host that opts in (build = true): the
// master's source tree of that commit arrives as a tar on stdin, its
// build.sh runs in a container with no network and no credential, the
// pinned toolchain mounted read-only, and the answer on stdout is a tar
// of the three binaries and BUILD. The master checks them (internal/builds);
// nothing here trusts the source beyond running it in the container.
func (s *Server) build(ctx context.Context, commit string) (err error) {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	if !cfg.Remote.Build {
		return refuse("no builds on this host")
	}
	if !jobproto.Commit12OK(commit) {
		return refuse("bad commit: %q (the first 12 hex digits of its id)", commit)
	}
	base := filepath.Join(s.Root, "builds-work")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	lock, err := fsx.TryLock(filepath.Join(base, ".lock"))
	if errors.Is(err, fsx.ErrBusy) {
		return refuse("a build is already running here")
	}
	if err != nil {
		return err
	}
	defer lock.Unlock()

	// A helper killed mid-build (the master's ssh gone, then SIGPIPE) runs
	// no defers: under the lock, nothing of another build is running, so
	// what is left of one goes, its container too.
	name := "factory-build-" + commit
	pm := podman.Client{Exec: s.Exec, Env: s.env()}
	if err := sweep(base); err != nil {
		return err
	}
	pm.Remove(ctx, name)
	work := filepath.Join(base, commit+"-"+strconv.Itoa(os.Getpid()))
	defer os.RemoveAll(work)
	src, out, cache := filepath.Join(work, "src"), filepath.Join(work, "out"), filepath.Join(work, "cache")
	for _, d := range []string{work, src, out, cache} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
	}
	in := &countingReader{r: io.LimitReader(s.Stdin, jobproto.MaxBuildInput+1)}
	data, rerr := io.ReadAll(in)
	if in.n > jobproto.MaxBuildInput {
		return refuse("build input too large")
	}
	if rerr != nil {
		return rerr
	}
	if err := extract(bytes.NewReader(data), src); err != nil {
		return refuse("bad build input: %v", err)
	}
	gomod, err := fsx.ReadNoFollow(filepath.Join(src, "go.mod"), 64<<10)
	if err != nil {
		return refuse("bad build input: go.mod: %v", errors.Unwrap(err))
	}
	pin, err := jobproto.ToolchainPin(gomod)
	if err != nil {
		return refuse("bad build input: %v", err)
	}
	goroot := filepath.Join(s.Home, ".local", "share", "factory", "go", pin)
	if fi, err := os.Stat(filepath.Join(goroot, "bin", "go")); err != nil || !fi.Mode().IsRegular() {
		return refuse("no Go toolchain %s at %s: install it (README, Builds)", pin, goroot)
	}

	argv := []string{"podman", "run", "--rm", "--name", name, "--network=none",
		"--timeout", strconv.Itoa(int(buildTimeout.Seconds())), // bounds the container even if this process dies
		"--memory", cfg.Remote.BuildMemory, "--cpus", cfg.Remote.CPUs, "--pids-limit", jobPids,
		"--cap-drop=all", "--security-opt=no-new-privileges", podman.KeepID, "--user", "1000:1000",
		"-e", "HOME=/tmp", "-e", "FACTORY_GOROOT=/go", "-e", "FACTORY_GO_CACHE=/cache",
		"-v", src + ":/src:ro", "-v", goroot + ":/go:ro", "-v", cache + ":/cache", "-v", out + ":/out",
		"-w", "/src", "--entrypoint", "/bin/bash", cfg.Remote.Image,
		"/src/build.sh", "--commit", commit, "--out", "/out", "--no-box", "--no-scope"}
	res, err := s.Exec.Run(ctx, execx.Cmd{Argv: argv, Dir: "/", Env: s.env(), Timeout: buildTimeout, MaxOut: buildLogTail,
		Cleanup: func() {
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			pm.Remove(ctx, name)
		}})
	if err != nil {
		return fmt.Errorf("build %s: %w: %s", commit, err, tail(res))
	}
	if res.Code != 0 {
		return fmt.Errorf("build %s exited %d: %s", commit, res.Code, tail(res))
	}
	var files []jobproto.File
	for _, n := range jobproto.BuildFiles() {
		b, err := fsx.ReadNoFollow(filepath.Join(out, n), jobproto.MaxBuildInput*2)
		if err != nil {
			return fmt.Errorf("build %s wrote no %s (build.sh writes factory, foreman, factory-remote and BUILD): %v", commit, n, errors.Unwrap(err))
		}
		files = append(files, jobproto.File{Name: n, Data: b})
	}
	answer, err := jobproto.TarFlat(files, s.Clock.Now())
	if err != nil {
		return err
	}
	_, err = s.Stdout.Write(answer)
	return err
}

// sweep removes everything in the builds' work dir but its lock.
func sweep(base string) error {
	es, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Name() != ".lock" {
			if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// tail is the end of a run's output, stdout then stderr, for an error.
func tail(r execx.Result) string {
	t := strings.TrimSpace(string(r.Stdout) + string(r.Stderr))
	if len(t) > buildLogTail {
		t = t[len(t)-buildLogTail:]
	}
	return t
}
