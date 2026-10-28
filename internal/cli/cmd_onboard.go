package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/onboard"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/store"
)

// The project kinds' commands: kinds lists the kinds and what each factory
// project takes from its kind; onboard checks whether a project is ready
// to switch on. Both only read, except that onboard --checks runs the
// gate's checks on the base.

func runKinds(_ context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	kinds := cfg.Kinds()
	projects := cfg.FactoryProjects()
	if a.Bool("json") {
		ks := make([]jsonx.Value, len(kinds))
		for i, k := range kinds {
			ks[i] = jsonx.ObjectOf(
				jsonx.Member{Key: "name", Value: jsonx.String(k.Name)},
				jsonx.Member{Key: "source", Value: jsonx.String(k.Source)},
				jsonx.Member{Key: "toolchain", Value: jsonx.String(k.Toolchain)},
				jsonx.Member{Key: "from", Value: jsonx.String(k.From)})
		}
		ps := make([]jsonx.Value, len(projects))
		for i, p := range projects {
			k, inferred := cfg.KindOf(p)
			pinned := make([]jsonx.Value, len(p.PinnedUntracked))
			for j, f := range p.PinnedUntracked {
				pinned[j] = jsonx.String(f)
			}
			ps[i] = jsonx.ObjectOf(
				jsonx.Member{Key: "project", Value: jsonx.String(p.Key)},
				jsonx.Member{Key: "kind", Value: jsonx.String(kindName(p, k))},
				jsonx.Member{Key: "inferred", Value: jsonx.Bool(inferred)},
				jsonx.Member{Key: "toolchain", Value: jsonx.String(k.Toolchain)},
				jsonx.Member{Key: "language", Value: jsonx.String(p.LanguageLabel())},
				jsonx.Member{Key: "install", Value: jsonx.String(p.Install)},
				jsonx.Member{Key: "test", Value: jsonx.String(p.Test)},
				jsonx.Member{Key: "lint", Value: jsonx.String(p.Lint)},
				jsonx.Member{Key: "gate_memory", Value: jsonx.String(cfg.GateMemory(p))},
				jsonx.Member{Key: "pinned_untracked", Value: jsonx.ArrayOf(pinned...)})
		}
		e.Stdout.Write(jsonx.EncodePython(jsonx.ObjectOf(
			jsonx.Member{Key: "kinds", Value: jsonx.ArrayOf(ks...)},
			jsonx.Member{Key: "projects", Value: jsonx.ArrayOf(ps...)}), -1))
		fmt.Fprintln(e.Stdout)
		return nil
	}
	fmt.Fprintln(e.Stdout, "kinds:")
	for _, k := range kinds {
		line := "  " + ljust(k.Name, 14) + " " + ljust(k.Source, 7) + " " + k.Toolchain
		if k.From != "" {
			line += "  from " + k.From
		}
		fmt.Fprintln(e.Stdout, line)
	}
	fmt.Fprintln(e.Stdout, "projects:")
	for _, p := range projects {
		k, inferred := cfg.KindOf(p)
		name := kindName(p, k)
		if inferred {
			name += " (inferred)"
		}
		tc := k.Toolchain
		if tc == "" {
			tc = "(no such kind)"
		}
		fmt.Fprintf(e.Stdout, "  %s  %s  %s\n", p.Key, name, tc)
		for _, kv := range [][2]string{
			{"language", p.LanguageLabel()}, {"install", p.Install}, {"test", p.Test}, {"lint", p.Lint},
			{"gate_memory", cfg.GateMemory(p)}, {"pinned", strings.Join(p.PinnedUntracked, " ")},
		} {
			fmt.Fprintln(e.Stdout, strings.TrimRight("    "+ljust(kv[0], 12)+" "+kv[1], " "))
		}
	}
	if notes := cfg.KindNotes(); len(notes) > 0 {
		fmt.Fprintln(e.Stdout, "hosts:")
		for _, n := range notes {
			fmt.Fprintln(e.Stdout, "  "+n)
		}
	}
	return nil
}

// kindName is the project's kind as KindOf names it, or its `kind` as
// written when no kind has that name.
func kindName(p config.Project, k config.ProjectKind) string {
	if k.Name == "" {
		return p.Kind
	}
	return k.Name
}

// onboardCLI wires factory onboard. Deps builds what the check reads (nil
// is onboardDeps); tests pass a fake, as opsCLI takes GitHub's transport.
type onboardCLI struct {
	Deps func(e *Env, cfg *config.Config, checks bool) onboard.Deps
}

func runOnboard(ctx context.Context, e *Env, a Args) error { return onboardCLI{}.onboard(ctx, e, a) }

// onboard is `factory onboard PROJECT [--checks] [--json]`: one line per
// check ("ok    kind        crystal-shard"), or the report as JSON; exit 1
// when a check failed. With --checks the gate's checks run on the base's
// head while the tick's lock is held, so no tick starts agents beside them
// (it waits for a running tick).
func (o onboardCLI) onboard(ctx context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	mk := o.Deps
	if mk == nil {
		if err := e.Git(ctx); err != nil {
			return err
		}
		mk = onboardDeps
	}
	checks := a.Bool("checks")
	d := mk(e, cfg, checks)
	if !checks {
		d.Checks = nil
	} else if run := d.Checks; run != nil {
		board := e.Board()
		d.Checks = func(ctx context.Context, p config.Project, ref string) (bool, string, error) {
			l, err := board.TickLock(ctx, store.Forever)
			if err != nil {
				return false, "", err
			}
			defer l.Unlock()
			return run(ctx, p, ref)
		}
	}
	r, err := onboard.Check(ctx, d, a.Pos("project"))
	if err != nil {
		return refusal(err)
	}
	if a.Bool("json") {
		e.Stdout.Write(r.JSON())
		fmt.Fprintln(e.Stdout)
	} else {
		for _, it := range r.Items {
			detail := strings.ReplaceAll(strings.TrimRight(it.Detail, "\n"), "\n", "\n"+strings.Repeat(" ", 18))
			fmt.Fprintln(e.Stdout, ljust(it.Status, 5)+" "+ljust(it.Check, 11)+" "+detail)
		}
	}
	if !r.OK() {
		return Exit(1)
	}
	return nil
}

// onboardDeps reads the checkout through the gate's git, which verifies
// the project's .git and cleans nothing; asks the App when [github] is
// set; and with checks runs the gate's checks on a fresh clone of the head
// in the gate image, under a gate ID no ticket can have.
func onboardDeps(e *Env, cfg *config.Config, checks bool) onboard.Deps {
	p := e.Paths()
	env := doorsEnviron(e.Environ)
	runner := gate.Runner{Paths: p, Exec: e.Exec, Env: env, Log: e.Log(), Clock: e.Clock,
		Podman: podman.Client{Exec: e.Exec, Dir: p.Home, Env: podman.PassEnv(env)}}
	d := onboard.Deps{Config: cfg, Paths: p,
		Head: func(ctx context.Context, pr config.Project) (string, error) {
			h, err := runner.Repo(pr).Head(ctx, pr.Base)
			return h, gitread.ByHand(err, pr.GitHub)
		}}
	if cfg.HasGitHub() {
		d.GitHub = opsCLI{}.githubClient(e, cfg)
	}
	if checks {
		d.Checks = func(ctx context.Context, pr config.Project, ref string) (bool, string, error) {
			return runner.Checks(ctx, gate.CheckIn{Config: cfg, Project: pr, ID: "onboard-" + pr.Key, Ref: ref})
		}
	}
	return d
}
