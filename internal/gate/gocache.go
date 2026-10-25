package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/gocache"
	"github.com/stevegeek/agent-factory/internal/podman"
)

// goCache is the build cache generation a go project's checks are lent
// (gocache.Lend; Gen "" for none), with what happened logged: this host's cache warmed from
// in.Base, the base head the gate took, when there is none for it yet. The
// warm compiles a fresh clone of that commit, read-only at /work where the
// checks run (go keys a package's entries by its directory), in its own
// container with no network and no credential, the gate's image, memory
// and the Go settings of [gate] env. A check with no base (the merge
// queue's) is lent the cache as it is, and so is one whose context leaves
// too little time to warm before the checks' own GateTimeout (warmWindow).
// A check in a candidate image (the rebuild's, the trial's) runs cold:
// entries the live image compiled could hide what the candidate breaks.
func (r Runner) goCache(ctx context.Context, in CheckIn, image string) gocache.Lend {
	p := in.Project
	if in.Config.Toolchain(p) != "go" {
		return gocache.Lend{}
	}
	if in.Image != "" {
		r.logf("%s: build cache: not lent to a check in %s; running cold", in.ID, in.Image)
		return gocache.Lend{}
	}
	dir, err := gocache.For(r.Paths.Root, p.Key)
	if err != nil {
		r.logf("%s: build cache: %v; running cold", in.ID, err)
		return gocache.Lend{}
	}
	window, ok := warmWindow(ctx)
	if !ok {
		lend := dir.AsItIs("too little time is left to warm the build cache before the checks")
		r.logf("%s: %s", in.ID, lend.Note)
		return lend
	}
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	var env []string
	for _, e := range in.Config.Gate.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	lend := dir.Ensure(ctx, in.Base, func(ctx context.Context, scratch string) error {
		wt := filepath.Join(r.Paths.Gate, "warm-"+p.Key)
		if err := fsx.RemoveTree(wt); err != nil {
			return err
		}
		if err := os.MkdirAll(r.Paths.Gate, 0o755); err != nil {
			return err
		}
		defer fsx.RemoveTree(wt)
		clone, err := gitx.Writer{Repo: r.Repo(p)}.CloneNoCheckout(ctx, wt)
		if err != nil {
			return err
		}
		if err := clone.CheckoutDetach(ctx, in.Base); err != nil {
			return err
		}
		ok, tail, err := r.Podman.RunWarm(ctx, podman.Warm{Name: containerName("factory-warm-", p.Key), Image: image,
			Memory: in.Config.GateMemory(p), Env: gocache.WarmEnv(env), Cache: scratch, Target: gocache.Target,
			Source: wt, SourceAt: "/work", Script: gocache.Compile})
		if err == nil && !ok {
			err = fmt.Errorf("the compile failed: %s", podman.LastRunes(tail, 500))
		}
		return err
	})
	if lend.Note != "" {
		r.logf("%s: %s", in.ID, lend.Note)
	}
	return lend
}

// warmMargin is what a gate's warm leaves free before its context's
// deadline besides the checks' podman.GateTimeout; minWarm is the least
// time worth starting a warm with.
const (
	warmMargin = 2 * time.Minute
	minWarm    = time.Minute
)

// warmWindow is how long a warm may take under ctx: podman.WarmTimeout,
// or, under a deadline (the tick's), what is left of it once the checks'
// whole GateTimeout and warmMargin are kept back; false when that is
// under minWarm.
func warmWindow(ctx context.Context) (time.Duration, bool) {
	dl, ok := ctx.Deadline()
	if !ok {
		return podman.WarmTimeout, true
	}
	w := min(time.Until(dl)-podman.GateTimeout-warmMargin, podman.WarmTimeout)
	return w, w >= minWarm
}

// errCacheMount is a [gate] mount that reaches the trusted build caches.
var errCacheMount = errors.New("reaches the trusted build caches, which only a warm may mount")

// errEarlyMount is a [gate] mount that reaches the early checks' kept
// results, which a branch's tests could forge through it.
var errEarlyMount = errors.New("reaches the gate's kept early checks, which no container may mount")
