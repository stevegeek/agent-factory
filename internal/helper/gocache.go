package helper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gocache"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/podman"
)

// toolchainGo is the toolchain whose jobs are lent a build cache.
const toolchainGo = "go"

// warmShare: a job's warm may take at most 1/warmShare of the job's time
// (and podman.WarmTimeout); the agent's container gets what the warm left
// (agentTimeout). The dispatcher times a job from its start, so the warm
// and the agent together must keep to the one timeout.
const warmShare = 4

// minAgentTime is the least an agent's container is given.
const minAgentTime = time.Minute

// warmBudget is how long the warm of a job with this timeout may take.
func warmBudget(minutes int) time.Duration {
	return min(podman.WarmTimeout, time.Duration(minutes)*time.Minute/warmShare)
}

// agentTimeout is the agent's container's --timeout, in seconds: the
// job's time less what its warm used, and at least minAgentTime.
func agentTimeout(minutes int, warmed time.Duration) int {
	return int(max(time.Duration(minutes)*time.Minute-warmed, minAgentTime) / time.Second)
}

// jobCheckout is where jobSetup clones a job's repository in its
// container; a warm compiles there too, since go keys a package's entries
// by its directory.
const jobCheckout = "/job/repo"

// lendsGoCache says a job may be lent its project's Go build cache: its
// meta names a project, the go toolchain and the base head. What an older
// dispatcher sends names none, and runs cold.
func lendsGoCache(m jobproto.Meta) bool {
	return m.Toolchain == toolchainGo && m.Project != "" && m.Base != ""
}

// goCache is the build cache a job's agent is lent (gocache.Lend; Gen ""
// for none): for a go project, the generation warmed from the base head,
// warmed first when this host has none for it, within warmBudget. The
// warm is its own container, before the agent's and with none of its
// credential: the
// job's input read-only, which holds the dispatcher's bundle of the base,
// and only a scratch cache and a scratch /job (its clone) writable. Its output goes to warm.log, in
// the job dir and out of the agent's reach.
func (s *Server) goCache(ctx context.Context, d, job string) (gocache.Lend, error) {
	meta, err := loadMeta(d)
	if err != nil {
		return gocache.Lend{}, fmt.Errorf("meta.json: %w", err)
	}
	if !lendsGoCache(meta) {
		return gocache.Lend{}, nil
	}
	if fi, err := os.Lstat(filepath.Join(d, "work", jobproto.MemberBundle)); err != nil || !fi.Mode().IsRegular() {
		return gocache.Lend{Note: "no bundle to warm the build cache from; running cold"}, nil
	}
	cfg, err := s.config()
	if err != nil {
		return gocache.Lend{}, err
	}
	dir, err := gocache.For(s.Root, meta.Project)
	if err != nil {
		return gocache.Lend{Note: err.Error() + "; running cold"}, nil
	}
	script, err := gocache.BundleScript(meta.Base, jobCheckout)
	if err != nil {
		return gocache.Lend{Note: err.Error() + "; running cold"}, nil
	}
	pm := podman.Client{Exec: s.Exec, Env: s.env()}
	ctx, cancel := context.WithTimeout(ctx, warmBudget(meta.TimeoutMinutes))
	defer cancel()
	lend := dir.Ensure(ctx, meta.Base, func(ctx context.Context, scratch string) error {
		// what a killed earlier attempt of this job left goes first
		work := filepath.Join(d, "warm")
		if err := fsx.RemoveTree(work); err != nil {
			return err
		}
		if err := os.Mkdir(work, 0o755); err != nil {
			return err
		}
		defer fsx.RemoveTree(work)
		ok, tail, err := pm.RunWarm(ctx, podman.Warm{Name: "factory-warm-" + job, Image: cfg.Remote.Image,
			Memory: cfg.Remote.Memory, CPUs: cfg.Remote.CPUs, Env: gocache.WarmEnv(cfg.JobEnv()),
			Cache: scratch, Target: gocache.Target, Source: filepath.Join(d, "work"), SourceAt: "/src",
			Work: work, WorkAt: filepath.Dir(jobCheckout), Script: script})
		if werr := fsx.WriteAtomic(filepath.Join(d, "warm.log"), []byte(tail), 0o644); werr != nil && err == nil {
			err = werr
		}
		if err == nil && !ok {
			err = fmt.Errorf("the compile failed (warm.log)")
		}
		return err
	})
	if lend.Gen != "" {
		// the mount point, made here as the factory user: one podman makes
		// in the bind-mounted home is owned by a subordinate id, which the
		// helper could not remove with the job
		if err := os.MkdirAll(filepath.Join(d, "home", gocache.InHome), 0o755); err != nil {
			lend.Release()
			return gocache.Lend{Note: fmt.Sprintf("build cache: %v; running cold", err)}, nil
		}
	}
	return lend, nil
}
