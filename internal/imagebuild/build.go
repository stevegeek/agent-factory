// Package imagebuild rebuilds the agent images: build a candidate at the
// pinned version with fresh OS packages, smoke-test it without network,
// run the projects' checks in it for the gate image, and only then tag the
// old image :previous and the candidate as the image. The candidate is
// always removed and dangling images pruned.
//
// It holds no board, GitHub or config code: upkeep (the dispatcher) and
// helper (a helper host) build its Specs and handle the outcome (the
// failure notice is upkeep's).
package imagebuild

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/logx"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Kind is one row of Python's IMAGES table: how an image is built and
// checked, and where its releases are announced.
type Kind struct {
	Kind      string // "claude" or "pi": the [images] pin's key
	Name      string // "Claude Code"
	Dir       string // the build context under ROOT/images
	Arg       string // the build arg the pin is passed as
	Check     string // the version command the smoke test ends with
	NPM       string // the npm package `versions` compares the pin with
	Changelog string
}

// Kinds is the IMAGES table, in Python's order: a fresh copy.
func Kinds() []Kind {
	return []Kind{
		{Kind: "claude", Name: "Claude Code", Dir: "scion-agent", Arg: "CLAUDE_VERSION", Check: "claude --version",
			NPM: "@anthropic-ai/claude-code", Changelog: "https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md"},
		{Kind: "pi", Name: "Pi", Dir: "pi", Arg: "PI_VERSION", Check: "pi --version",
			NPM: "@earendil-works/pi-coding-agent", Changelog: "https://www.npmjs.com/package/@earendil-works/pi-coding-agent?activeTab=versions"},
	}
}

// DefaultPiImage is [remote] image when config leaves it out.
const DefaultPiImage = "localhost/factory-pi:dev"

// TagFor is image_tag: the claude image is the top-level `image`,
// the pi image is [remote] image (default localhost/factory-pi:dev).
func TagFor(kind, image, remoteImage string) string {
	switch kind {
	case "claude":
		return image
	case "pi":
		if remoteImage == "" {
			return DefaultPiImage
		}
		return remoteImage
	}
	return ""
}

// toolArgs are the toolchain pins passed to every build, in Python's order.
func toolArgs() [][2]string {
	return [][2]string{{"crystal", "CRYSTAL_VERSION"}, {"ameba", "AMEBA_VERSION"}}
}

// toolNames name toolArgs' tools in a smoke test's failure.
func toolNames() map[string]string {
	return map[string]string{"CRYSTAL_VERSION": "Crystal", "AMEBA_VERSION": "ameba"}
}

// ProjectCheck is one project's gate checks on its base, run in an image
// (upkeep passes gate.Checks on a fresh checkout of the base's head).
type ProjectCheck struct {
	Key, Base string
	Run       func(ctx context.Context, image string) (ok bool, output string, err error)
}

// Spec is one image to rebuild.
type Spec struct {
	Kind       Kind
	Tag        string   // the image's tag; "" when this host has none configured
	ContextDir string   // ROOT/images/<Kind.Dir>
	Pinned     string   // [images] <kind>; "" builds the Containerfile's default
	BuildArgs  []string // "NAME=value", the pin first, then the toolchain pins
	// Projects run in the candidate before the swap: the gate image's
	// project checks (only for the top-level image, and only projects with
	// a checkout on this host). nil for the others. A project that fails
	// in the candidate is run again in the live image, and blocks the swap
	// only if it passes there: only a regression blocks.
	Projects []ProjectCheck
	// Update: Pinned is a newer release than From, the version the live
	// image has; the caller records it once promoted.
	Update bool
	From   string
	// Trial: a latest-toolchain trial (the BuildArgs carry newer
	// toolchain pins). The candidate is <repo>:trial, every project runs
	// (a regression does not stop it) and is sorted into the Result's
	// Passed, Broken and AlreadyFailing, and nothing is ever tagged: the
	// live image never changes, and the trial image is always removed.
	Trial bool
}

// SpecFor builds a Spec from the [images] pins.
func SpecFor(k Kind, imagesDir, tag string, pins map[string]string) Spec {
	s := Spec{Kind: k, Tag: tag, ContextDir: filepath.Join(imagesDir, k.Dir), Pinned: pins[k.Kind]}
	if s.Pinned != "" {
		s.BuildArgs = append(s.BuildArgs, k.Arg+"="+s.Pinned)
	}
	for _, t := range toolArgs() {
		if v := pins[t[0]]; v != "" {
			s.BuildArgs = append(s.BuildArgs, t[1]+"="+v)
		}
	}
	return s
}

// Repo is the tag without its version: the part before the last ":" when
// that ":" follows the last "/" (a registry may carry a port).
func Repo(tag string) string {
	last := tag[strings.LastIndex(tag, "/")+1:]
	if i := strings.LastIndex(last, ":"); i >= 0 {
		return tag[:len(tag)-len(last)+i]
	}
	return tag
}

// Result is the outcome of one image's rebuild.
type Result struct {
	Kind    string
	Tag     string
	OK      bool
	Summary string   // the factory.log line's reason: "build failed", the new version, ...
	Log     []string // the parts of logs/rebuild-<kind>.log
	Err     error    // set on the exception path (a timeout, a failed swap, a cancel)
	// Version is the version built (the Spec's Pinned), Update and From
	// the Spec's; AlreadyFailing are the projects that failed in the
	// candidate and in the live image alike, which did not block.
	Version        string
	Update         bool
	From           string
	AlreadyFailing []string
	// A trial's outcome (Spec.Trial): OK means it ran to the end (built,
	// smoke-tested, every project checked). Passed pass in the trial
	// image, Broken fail there but pass in the live image; Tails holds
	// each project's output in the trial image.
	Passed []string
	Broken []string
	Tails  map[string]string
}

// TrialCounts is a trial's outcome in a few words: "6 pass, 1 breaks,
// 1 already failing".
func TrialCounts(r Result) string {
	verb := func(n int, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	return verb(len(r.Passed), "passes", "pass") + ", " + verb(len(r.Broken), "breaks", "break") +
		fmt.Sprintf(", %d already failing", len(r.AlreadyFailing))
}

// Runner runs rebuilds on this host.
type Runner struct {
	Podman podman.Client
	Clock  clock.Clock
	Log    *logx.Log // factory.log; nil logs nothing
	LogDir string    // where rebuild-<kind>.log goes (paths.Logs)
	// Lock is a flock held for the whole of All (paths.TickLock): no agent
	// starts while images change, and a tick meanwhile waits or leaves
	// .tick.again. "" takes none.
	Lock string
}

// cleanupTimeout bounds the candidate's removal and the prune, which run
// even when the rebuild's context has ended.
const cleanupTimeout = 5 * time.Minute

// smokeScript is the smoke test: every toolchain and the harness answer.
func smokeScript(check string) string {
	return "ruby -v && bundle -v && crystal --version && ameba --version && git --version && " + check
}

// Rebuild rebuilds one image (rebuild_image). It never returns an
// error: what Python raised is the Err of a Result whose Summary is
// "error: <first 200 characters>" and whose Log is the error alone.
func (r Runner) Rebuild(ctx context.Context, s Spec) Result {
	res := Result{Kind: s.Kind.Kind, Tag: s.Tag, Version: s.Pinned, Update: s.Update, From: s.From}
	repo := Repo(s.Tag)
	candidate, previous := repo+":candidate", repo+":previous"
	clash := s.Tag == candidate || s.Tag == previous
	if s.Trial {
		candidate = repo + ":trial"
		clash = clash || s.Tag == candidate
	}
	if clash {
		res.Summary = fmt.Sprintf("image tag %s clashes with the rebuild's own tags", s.Tag)
		return res
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		r.Podman.Rmi(cctx, candidate)
		r.Podman.Prune(cctx)
	}()
	ok, summary, out, failing, err := r.rebuild(ctx, s, candidate, previous, &res)
	res.AlreadyFailing = failing
	if err != nil {
		res.Err = err
		res.Summary = "error: " + textx.Prefix(err.Error(), 200)
		res.Log = []string{err.Error()}
		return res
	}
	res.OK, res.Summary, res.Log = ok, summary, out
	return res
}

func (r Runner) rebuild(ctx context.Context, s Spec, candidate, previous string, res *Result) (bool, string, []string, []string, error) {
	out := []string{fmt.Sprintf("== %s rebuild %s -> %s", clock.Stamp(r.Clock.Now()), s.Kind.Kind, s.Tag)}
	if s.Trial {
		out[0] = fmt.Sprintf("== %s trial %s -> %s", clock.Stamp(r.Clock.Now()), s.Kind.Kind, candidate)
	}
	if s.Update {
		out = append(out, fmt.Sprintf("update %s %s -> %s", s.Kind.Name, orNone(s.From), s.Pinned))
	}
	b, err := r.Podman.Build(ctx, podman.Build{Tag: candidate, Context: s.ContextDir, Args: s.BuildArgs})
	if err != nil {
		return false, "", nil, nil, err
	}
	out = append(out, podman.LastRunes(string(b.Stdout)+string(b.Stderr), 6000))
	if b.Code != 0 {
		return false, "build failed", out, nil, nil
	}
	// keep-id here too: it pays the one-time image copy now instead of on the next agent start
	sm, err := r.Podman.RunSmoke(ctx, podman.Smoke{Name: "factory-smoke-" + s.Kind.Kind, Image: candidate, Script: smokeScript(s.Kind.Check)})
	if err != nil {
		return false, "", nil, nil, err
	}
	stdout := string(sm.Stdout)
	out = append(out, stdout+string(sm.Stderr))
	if sm.Code != 0 || !hasVersion(stdout, s.Pinned) {
		why := "smoke test failed"
		if s.Pinned != "" {
			why += fmt.Sprintf(" (expected %s %s)", s.Kind.Name, s.Pinned)
		}
		return false, why, out, nil, nil
	}
	if s.Trial {
		return r.trial(ctx, s, candidate, stdout, out, res)
	}
	var failing []string
	for _, p := range s.Projects {
		ok, output, err := p.Run(ctx, candidate)
		if err != nil {
			return false, "", nil, failing, fmt.Errorf("%s checks: %w", p.Key, err)
		}
		verdict := "pass"
		if !ok {
			verdict = "FAIL"
		}
		out = append(out, fmt.Sprintf("-- %s on %s: %s\n%s", p.Key, p.Base, verdict, output))
		if ok {
			continue
		}
		// only a regression blocks: the same checks in the live image
		liveOK, liveOut, err := p.Run(ctx, s.Tag)
		if err != nil {
			return false, "", nil, failing, fmt.Errorf("%s checks in the live image: %w", p.Key, err)
		}
		if liveOK {
			out = append(out, fmt.Sprintf("-- %s on %s in the live image: pass, so the new image broke it", p.Key, p.Base))
			return false, p.Key + " tests fail in the new image", out, failing, nil
		}
		out = append(out, fmt.Sprintf("-- %s on %s in the live image: FAIL too (already failing; not blocking)\n%s", p.Key, p.Base, liveOut))
		failing = append(failing, p.Key)
	}
	if _, err := r.Podman.Tag(ctx, s.Tag, previous); err != nil { // one week back, for a rollback
		return false, "", nil, failing, err
	}
	t, err := r.Podman.Tag(ctx, candidate, s.Tag)
	if err == nil {
		err = execx.CheckErr(t)
	}
	if err != nil {
		return false, "", nil, failing, err
	}
	summary := "ok"
	if lines := textx.SplitLines(textx.Strip(stdout)); len(lines) > 0 {
		summary = lines[len(lines)-1]
	}
	if len(failing) > 0 {
		summary += " (already failing on main: " + strings.Join(failing, ", ") + ")"
	}
	return true, summary, out, failing, nil
}

// trial is a trial's checks after its smoke test: the image must have the
// toolchain versions it was built with, then every project runs in it, and
// one that fails runs again in the live image to tell a break from a
// failure already there. Nothing is tagged.
func (r Runner) trial(ctx context.Context, s Spec, image, smoke string, out []string, res *Result) (bool, string, []string, []string, error) {
	names := toolNames()
	for _, a := range s.BuildArgs {
		name, v, _ := strings.Cut(a, "=")
		if tool, ok := names[name]; ok && !hasVersion(smoke, v) {
			return false, fmt.Sprintf("smoke test failed (expected %s %s)", tool, v), out, nil, nil
		}
	}
	res.Tails = map[string]string{}
	var failing []string
	for _, p := range s.Projects {
		ok, output, err := p.Run(ctx, image)
		if err != nil {
			return false, "", nil, failing, fmt.Errorf("%s checks: %w", p.Key, err)
		}
		res.Tails[p.Key] = output
		verdict := "pass"
		if !ok {
			verdict = "FAIL"
		}
		out = append(out, fmt.Sprintf("-- %s on %s: %s\n%s", p.Key, p.Base, verdict, output))
		if ok {
			res.Passed = append(res.Passed, p.Key)
			continue
		}
		liveOK, liveOut, err := p.Run(ctx, s.Tag)
		if err != nil {
			return false, "", nil, failing, fmt.Errorf("%s checks in the live image: %w", p.Key, err)
		}
		if liveOK {
			out = append(out, fmt.Sprintf("-- %s on %s in the live image: pass, so the new toolchain broke it", p.Key, p.Base))
			res.Broken = append(res.Broken, p.Key)
			continue
		}
		out = append(out, fmt.Sprintf("-- %s on %s in the live image: FAIL too (already failing)\n%s", p.Key, p.Base, liveOut))
		failing = append(failing, p.Key)
	}
	res.AlreadyFailing = failing
	return true, "trial: " + TrialCounts(*res), out, failing, nil
}

func orNone(v string) string {
	if v == "" {
		return "(the Containerfile's default)"
	}
	return v
}

// All rebuilds each image this host has (rebuild), under Lock:
// a Spec with no tag, or whose tag podman does not have, is skipped. Each
// rebuilt image gets logs/rebuild-<kind>.log (overwritten) and one
// factory.log line. The error is for a host where podman does not run, a
// lock not taken, or a context that ended; the Results so far come with it.
func (r Runner) All(ctx context.Context, specs []Spec) ([]Result, error) {
	if r.Lock != "" {
		if err := os.MkdirAll(filepath.Dir(r.Lock), 0o755); err != nil {
			return nil, fmt.Errorf("rebuild: %w", err)
		}
		l, err := fsx.Lock(ctx, r.Lock, fsx.Forever)
		if err != nil {
			return nil, fmt.Errorf("rebuild: %w", err)
		}
		defer l.Unlock()
	}
	var results []Result
	for _, s := range specs {
		if err := ctx.Err(); err != nil {
			return results, fmt.Errorf("rebuild: %w", err)
		}
		if s.Tag == "" {
			continue
		}
		have, err := r.Podman.ImageExists(ctx, s.Tag)
		if err != nil {
			return results, fmt.Errorf("rebuild: %w", err)
		}
		if !have {
			continue
		}
		res := r.Rebuild(ctx, s)
		if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
			return results, fmt.Errorf("rebuild: %w", err)
		}
		what := "rebuild"
		if s.Trial {
			what = "trial" // the weekly rebuild's log stays
		}
		name := filepath.Join(r.LogDir, what+"-"+s.Kind.Kind+".log")
		if err := fsx.WriteAtomic(name, []byte(strings.Join(res.Log, "\n")), 0o644); err != nil {
			return results, fmt.Errorf("rebuild: %w", err)
		}
		if r.Log != nil {
			verdict := "FAILED"
			if res.OK {
				verdict = "ok"
			}
			r.Log.Printf("%s %s: %s: %s", what, s.Kind.Kind, verdict, res.Summary)
		}
		results = append(results, res)
	}
	return results, nil
}
