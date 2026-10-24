package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deploy"
	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/doors"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/sshjobs"
)

func deployVerbs() []string {
	return []string{"start", "ask", "run", "hold", "unhold", "status", "restore"}
}

// runDeploy is `factory deploy VERB [REV] [--why TEXT] [--drill]
// [--allow-older] [--note TEXT] [--force]` (docs/plans/2026-10-24-p2-self-deploy.md):
//
//	start [REV]  deploy now, whatever the hold says: main, or a commit on main; --drill fails the check on purpose,
//	             --allow-older lets deploy.sh install a commit older than the running one
//	ask [REV]    file or update the Deploy? ask, as a held merge does
//	run          the factory-deploy unit's command: run the waiting requests
//	hold         merges leave a Deploy? ask instead of deploying (--note)
//	unhold       merges deploy by themselves again
//	status       the hold, the running or last deploy, and a request waiting
//	restore      put back the snapshot of the files the last deploy replaced; --force when it is not that deploy's
func runDeploy(ctx context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	verb, rev := a.Pos("verb"), a.Pos("rev")
	if rev != "" && verb != "start" && verb != "ask" {
		return fmt.Errorf("deploy %s takes no REV", verb)
	}
	if a.Bool("drill") && verb != "start" {
		return errors.New("--drill goes with deploy start")
	}
	if a.Bool("allow-older") && verb != "start" {
		return errors.New("--allow-older goes with deploy start")
	}
	if a.Str("why") != "" && verb != "start" && verb != "ask" {
		return errors.New("--why goes with deploy start or ask")
	}
	if a.Bool("force") && verb != "restore" {
		return errors.New("--force goes with deploy restore")
	}
	if a.Str("note") != "" && verb != "hold" {
		return errors.New("--note goes with deploy hold")
	}
	s := deployService(e, cfg)
	switch verb {
	case "start":
		if rev == "" {
			rev = "main"
		}
		if !revOK(rev) {
			return fmt.Errorf("bad REV %q: main, or a commit id on core main", rev)
		}
		r := deployrec.Request{Rev: rev, By: cfg.Maintainer(), Why: orText(a.Str("why"), "factory deploy start"), Drill: a.Bool("drill"),
			AllowOlder: a.Bool("allow-older")}
		if err := s.Start(ctx, cfg, r); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "deploying %s (factory deploy status shows it)\n", rev)
		return nil
	case "ask":
		if !cfg.Deploy.On() {
			return errors.New("no [deploy] in config: this host does not deploy itself")
		}
		t, err := e.doors().DeployAsk(ctx, orText(rev, "main"), a.Str("why"))
		if err != nil {
			return err
		}
		fmt.Fprintln(e.Stdout, t.ID)
		return nil
	case "run":
		return s.Run(ctx, cfg)
	case "hold", "unhold":
		o := &ops.Service{Board: e.Board(), Config: loadedConfig{cfg}, Clock: e.Clock, Wake: waker(e), Log: e.Log()}
		line, err := o.HoldDeploys(ctx, verb == "hold", a.Str("note"), "")
		if err != nil {
			return refusal(err)
		}
		fmt.Fprintln(e.Stdout, line)
		return nil
	case "status":
		return deployStatus(e)
	case "restore":
		restored, err := s.RestoreByHand(ctx, a.Bool("force"))
		if err != nil {
			return err
		}
		if len(restored) == 0 {
			fmt.Fprintln(e.Stdout, "nothing differed from the snapshot")
			return nil
		}
		fmt.Fprintf(e.Stdout, "put back: %s\n", strings.Join(restored, ", "))
		return nil
	}
	return fmt.Errorf("unknown deploy verb %q", verb)
}

// revOK is main or a commit id (hex): what the mirror resolves.
func revOK(rev string) bool {
	if rev == "main" {
		return true
	}
	if len(rev) < 7 || len(rev) > 64 {
		return false
	}
	for _, c := range rev {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// deployStatus prints the hold, the running or last deploy, and the
// request waiting, as System status shows them.
func deployStatus(e *Env) error {
	p := e.Paths()
	w := e.Stdout
	h, held, err := deployrec.ReadHold(p.DeployHold())
	switch {
	case err != nil:
		fmt.Fprintf(w, "hold: %v (held)\n", err)
	case held:
		fmt.Fprintf(w, "hold: held by %s since %s%s\n", h.By, h.At, colonNote(h.Note))
	default:
		fmt.Fprintln(w, "hold: none: merges deploy by themselves")
	}
	st, err := deployrec.ReadState(p.DeployState())
	if err != nil {
		return err
	}
	printDeployState(w, st)
	r, waiting, err := deployrec.ReadRequest(p.DeployRequest())
	if err != nil {
		fmt.Fprintf(w, "waiting: %v\n", err)
	} else if waiting {
		fmt.Fprintf(w, "waiting: %s, by %s at %s%s\n", r.Rev, orText(r.By, "the factory"), r.At, drillNote(r.Drill))
	}
	return nil
}

func colonNote(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

func drillNote(drill bool) string {
	if drill {
		return " (a drill)"
	}
	return ""
}

func printDeployState(w io.Writer, st deployrec.State) {
	switch st.Phase {
	case "":
		fmt.Fprintln(w, "deploy: no deploy yet")
		return
	case deployrec.PhaseRunning:
		fmt.Fprintf(w, "deploy: running %s (%s) since %s: %s\n", orText(st.Commit, st.Rev), st.Rev, st.Started, orText(st.Step, "starting"))
	default:
		fmt.Fprintf(w, "deploy: %s %s at %s, from %s, config %s (started %s by %s)\n", orText(st.Commit, st.Rev), st.Result,
			st.Finished, orText(st.From, "-"), orText(st.ConfigCommit, "-"), st.Started, orText(st.By, "the factory"))
	}
	names := make([]string, 0, len(st.Helpers))
	for n := range st.Helpers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  helper %s: %s\n", n, st.Helpers[n])
	}
	if st.Log != "" {
		fmt.Fprintf(w, "  log: %s\n", st.Log)
	}
	if st.Tail != "" {
		fmt.Fprintf(w, "  the end of the log:\n%s\n", indent(st.Tail))
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// deployService wires the self-deploy for one invocation. The config
// repo's token is minted for that repo alone, contents read
// (App.MintScoped): GitHub finds the name in the installation's own
// account, so a [deploy] config of another owner gets a token that does
// not reach it, and its fetch fails.
func deployService(e *Env, cfg *config.Config) deploy.Service {
	d := e.doors()
	return deploy.Service{
		Paths: e.Paths(), Exec: e.Exec, Clock: e.Clock, Log: e.Log(), Board: e.Board(),
		Notices: deployNotices{d}, SSH: sshjobs.Client{Exec: e.Exec}, Env: e.Environ,
		Token: func(ctx context.Context) (string, error) {
			_, name, _ := strings.Cut(cfg.Deploy.Config, "/")
			app, ok := opsCLI{}.githubClient(e, cfg).Auth.(*github.App)
			if !ok {
				return "", errors.New("no GitHub App")
			}
			inst, err := app.MintScoped(ctx, []string{name}, map[string]string{"contents": "read"})
			if err != nil {
				return "", err
			}
			return inst.Token, nil
		},
	}
}

// doors is the doors service the deploy files its notices and asks
// through.
func (e *Env) doors() doors.Service {
	return doors.Service{Board: e.Board(), Exec: e.Exec, Log: e.Log()}
}

// deployNotices and deployAsks fit doors.Service to deploy's interfaces
// (deployAsks is the Asker P3's merge hook hands Service.Offer).
type deployNotices struct{ d doors.Service }

func (n deployNotices) Notice(ctx context.Context, title, body string) error {
	_, err := n.d.Notice(ctx, title, body)
	return err
}

type deployAsks struct{ d doors.Service }

func (a deployAsks) DeployAsk(ctx context.Context, rev, why string) error {
	_, err := a.d.DeployAsk(ctx, rev, why)
	return err
}
