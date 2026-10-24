// Package gate is Gate 1, the deterministic gate with no model: the
// project's install, tests and lint on a fresh clone of the branch in a
// container with no network, the size and path rules that decide whether a
// change may merge without a person, the merge queue's re-check against a
// moved base, the audit trail written after every gate run, and the off-box
// gate's GitHub workflow.
//
// The gate never runs anything from the agent-writable clone itself: it
// clones it (objects and refs only; no hooks, no config), checks the ref out
// detached and runs the checks in podman with --network none as the keep-id
// user. Every git on the project goes through the Runner's guard first.
package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/security"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// MergeConflictTail is how much of git's output a conflicting merge check
// keeps, in code points.
const MergeConflictTail = 1500

// Containers is what the gate needs of podman (podman.Client).
type Containers interface {
	RunGate(ctx context.Context, g podman.Gate) (ok bool, tail string, err error)
}

// Logger is the factory log (logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Runner runs Gate 1 on this host.
type Runner struct {
	Paths paths.Paths
	Exec  execx.Execer // git
	// Env is the base environment of every git (gitread.SafeEnv is
	// applied to it); nil means this process's.
	Env []string
	// Guard checks a project's .git before git runs there: the process's
	// one *gitx.Guarder in the dispatcher. nil means gitread.Verifier,
	// which refuses rather than cleans.
	Guard  gitread.Guard
	Podman Containers
	Log    Logger      // nil logs nothing
	Clock  clock.Clock // the time of the merge check's gate event
	// Agents counts the agents running on this host, which the merge
	// check must fit beside (Fits); nil counts none.
	Agents func(ctx context.Context, cfg *config.Config) (int, error)
}

// Repo is the project's clone, as the Runner's git sees it.
func (r Runner) Repo(p config.Project) *gitread.Repo {
	return &gitread.Repo{Path: p.Path, Base: p.Base, GitHub: p.GitHub, Exec: r.Exec, Env: r.Env, Guard: r.Guard}
}

func (r Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

// CheckIn is one run of the checks (checks_on_fresh_checkout).
type CheckIn struct {
	Config  *config.Config
	Project config.Project
	// ID names the checkout (gate/<ID>) and the container: a ticket's id,
	// or "rebuild-<project>" for the image rebuild's check.
	ID     string
	Branch string // checked out when Ref is ""
	Ref    string // a commit to check out instead of the branch head
	Image  string // "" is the config's image
	// Merge is a commit merged into the checkout first (the merge queue's
	// check: the base as it is now).
	Merge string
}

// Checks is checks_on_fresh_checkout: the project's install, tests and lint
// on a fresh clone of the ref, in a container with no network. ok is a zero
// exit, tail the last 4000 code points of the output. A merge that
// conflicts is not ok, and nothing runs then. The error is for a gate that
// could not run or did not finish (a missing command, a refused repository,
// git failing, a timeout); the checkout is removed whatever happens.
func (r Runner) Checks(ctx context.Context, in CheckIn) (ok bool, tail string, err error) {
	p := in.Project
	script, err := gateScript(p)
	if err != nil {
		return false, "", err
	}
	image := in.Image
	if image == "" {
		image = in.Config.Image
	}
	if image == "" {
		return false, "", errors.New("gate: no image to run the checks in: set image in config.toml")
	}
	pinned, err := pinnedPaths(p)
	if err != nil {
		return false, "", err
	}
	if in.ID == "" {
		return false, "", errors.New("gate: no checkout name")
	}
	wt := filepath.Join(r.Paths.Gate, in.ID)
	if err := fsx.RemoveTree(wt); err != nil {
		return false, "", fmt.Errorf("gate %s: %w", in.ID, err)
	}
	if err := os.MkdirAll(r.Paths.Gate, 0o755); err != nil {
		return false, "", fmt.Errorf("gate %s: %w", in.ID, err)
	}
	defer fsx.RemoveTree(wt)

	repo := r.Repo(p)
	clone, err := gitx.Writer{Repo: repo}.CloneNoCheckout(ctx, wt)
	if err != nil {
		return false, "", fmt.Errorf("gate %s: %w", in.ID, err)
	}
	ref := in.Ref
	if ref == "" {
		if ref, err = repo.Head(ctx, in.Branch); err != nil {
			return false, "", fmt.Errorf("gate %s: %w", in.ID, err)
		}
	}
	if err := clone.CheckoutDetach(ctx, ref); err != nil {
		return false, "", fmt.Errorf("gate %s: %w", in.ID, err)
	}
	prefix := "factory-gate-"
	if in.Merge != "" {
		prefix = "factory-merge-"
		merged, out, err := clone.Merge(ctx, in.Merge)
		if err != nil {
			return false, "", fmt.Errorf("gate %s: %w", in.ID, err)
		}
		if !merged {
			return false, fmt.Sprintf("merging %s conflicts:\n%s", textx.Prefix(in.Merge, 12), podman.LastRunes(out, MergeConflictTail)), nil
		}
	}
	if err := copyPinned(p.Path, wt, pinned); err != nil {
		return false, "", fmt.Errorf("gate %s: %w", in.ID, err)
	}
	g := podman.Gate{Name: containerName(prefix, in.ID), Image: image, Memory: in.Config.GateMemory(p),
		Workdir: wt, Script: script}
	for _, e := range in.Config.Gate.Env {
		g.Env = append(g.Env, e.Name+"="+e.Value)
	}
	for _, m := range in.Config.Gate.Mounts {
		g.Mounts = append(g.Mounts, strings.ReplaceAll(m, "{root}", r.Paths.Root))
	}
	ok, tail, err = r.Podman.RunGate(ctx, g)
	if err != nil {
		return false, "", fmt.Errorf("gate %s: %w", in.ID, err)
	}
	return ok, tail, nil
}

// Script is the checks' shell command, "set -e; <install>; <test>; <lint>":
// what the gate's container runs, and a gate job's checks.sh.
func Script(p config.Project) (string, error) { return gateScript(p) }

// gateScript is the container's shell command. A project without install,
// test or lint is refused, naming the key (Python ran the string
// "None"); an empty command is kept as Python had it.
func gateScript(p config.Project) (string, error) {
	cmds := []string{p.Install, p.Test, p.Lint}
	for _, key := range []string{"install", "test", "lint"} {
		if !p.Has(key) {
			return "", fmt.Errorf("gate: [projects.%s] has no %s command", p.Key, key)
		}
	}
	return "set -e; " + strings.Join(cmds, "; "), nil
}

// containerName is prefix+id with anything podman would refuse in a name
// replaced by "-".
func containerName(prefix, id string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, c := range id {
		if c < utf8.RuneSelf && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Run is gate(cfg, project, t): what changed on the ticket's branch, the
// checks on a fresh checkout, and the decision. The checks' output is
// written to logs/<id>/gate-output.txt. The ticket is not changed: Record
// puts the result on it.
//
// The branch's and the base's heads are taken first, and everything is
// read from those two commits: the files, the size and the checks (Python
// read the head after the checks, so a commit landing during
// them could be approved untested). The gate records that head; a branch
// that moved meanwhile is logged, and its new commits are not gated (the
// merge path merges only the gated commit).
func (r Runner) Run(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket) (ticket.Gate, error) {
	branch := t.Branch.Or("")
	repo := r.Repo(p)
	head, err := repo.Head(ctx, branch)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	base, err := repo.Head(ctx, p.Base)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	at := *repo
	at.Base = base // the diffs are base...head, both as taken
	files, err := at.ChangedFiles(ctx, head)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	size, err := at.DiffSize(ctx, head)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	walls, err := wallsTouched(ctx, p, &at, head)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	hits, err := hygieneHits(ctx, cfg, p, t, &at, head)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	ok, output, err := r.Checks(ctx, CheckIn{Config: cfg, Project: p, ID: string(t.ID), Branch: branch, Ref: head})
	if err != nil {
		return ticket.Gate{}, err
	}
	if now, err := repo.Head(ctx, branch); err == nil && now != head {
		r.logf("%s: %s moved during the gate (now %s); the gate's result is for %s only", t.ID, branch, textx.Prefix(now, 7), textx.Prefix(head, 7))
	}
	g, err := Evaluate(GateIn{Config: cfg, Ticket: t, Files: files, Size: size, ChecksPass: ok, Head: head, Base: base,
		Walls: walls, Hygiene: hits})
	if err != nil {
		return ticket.Gate{}, err
	}
	return g, r.writeOutput(t, hygieneOutput(hits, output))
}

// wallsTouched are the project's wall files the branch touches from at's
// base to head, a rename at both ends, so moving a wall file away counts
// (none, and no git, on a project without walls).
func wallsTouched(ctx context.Context, p config.Project, at *gitread.Repo, head string) ([]string, error) {
	if len(p.Walls) == 0 {
		return nil, nil
	}
	files, err := at.TouchedFiles(ctx, head)
	if err != nil {
		return nil, err
	}
	return Walls(p, files), nil
}

// hygieneHits is Hygiene over what the branch publishes from at's base to
// head, on a hygiene project (none on another): the kept paths, the added
// lines (both of every commit, read as text), the commit messages, the
// branch name and the report texts.
func hygieneHits(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, at *gitread.Repo, head string) ([]string, error) {
	if !p.Hygiene {
		return nil, nil
	}
	files, err := at.KeptFiles(ctx, head)
	if err != nil {
		return nil, err
	}
	added, err := at.PublishedLines(ctx, head)
	if err != nil {
		return nil, err
	}
	msgs, err := at.Messages(ctx, head)
	if err != nil {
		return nil, err
	}
	return Hygiene(cfg.Hygiene.Deny, HygieneIn{Files: files, Added: added, Messages: msgs, Branch: t.Branch.Or(""),
		Texts: PublicTexts(t)}), nil
}

// writeOutput keeps the checks' output in logs/<id>/gate-output.txt.
func (r Runner) writeOutput(t *ticket.Ticket, output string) error {
	dir := r.Paths.TicketLogs(string(t.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("gate %s: %w", t.ID, err)
	}
	if err := fsx.WriteAtomic(filepath.Join(dir, "gate-output.txt"), []byte(output), 0o644); err != nil {
		return fmt.Errorf("gate %s: %w", t.ID, err)
	}
	return nil
}

// Remote is Gate 1 whose checks ran on another host (a gate job): the
// heads the job was started on, the checks' outcome, their output (shown,
// never interpreted: it comes from another machine) and the host.
type Remote struct {
	Head, Base string
	ChecksPass bool
	Output     string
	Host       string
}

// Decide is the rest of Run for checks that ran elsewhere: what changed
// from in.Base to in.Head (the commits the job was started on, so the
// decision is about the commit that was checked), the decision, naming the
// host, and the output in logs/<id>/gate-output.txt. The merge queue's
// re-check (StillHoldsOnMain) and the image rebuild's checks always run
// here.
func (r Runner) Decide(ctx context.Context, cfg *config.Config, p config.Project, t *ticket.Ticket, in Remote) (ticket.Gate, error) {
	at := *r.Repo(p)
	at.Base = in.Base
	files, err := at.ChangedFiles(ctx, in.Head)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	size, err := at.DiffSize(ctx, in.Head)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	walls, err := wallsTouched(ctx, p, &at, in.Head)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	hits, err := hygieneHits(ctx, cfg, p, t, &at, in.Head)
	if err != nil {
		return ticket.Gate{}, fmt.Errorf("gate %s: %w", t.ID, err)
	}
	g, err := Evaluate(GateIn{Config: cfg, Ticket: t, Files: files, Size: size, ChecksPass: in.ChecksPass, Head: in.Head, Base: in.Base,
		Walls: walls, Hygiene: hits})
	if err != nil {
		return ticket.Gate{}, err
	}
	g.Host = in.Host
	return g, r.writeOutput(t, hygieneOutput(hits, in.Output))
}

// GateIn is what Evaluate decides from.
type GateIn struct {
	Config     *config.Config
	Ticket     *ticket.Ticket // its work and review reports
	Files      []string       // diff --name-only base...branch
	Size       int            // lines added and deleted (binary files 0)
	ChecksPass bool
	Head, Base string   // the branch's and the base's heads
	Walls      []string // the wall files the branch touches, a rename at both ends (Walls)
	Hygiene    []string // the public-repo hygiene hits (Hygiene); private
}

// Evaluate is the decision half of gate(): the blockers that keep a change
// from merging without a person. Tests or lint failing, a file outside
// [gate] allowed_paths (none allowed by default), more lines than
// max_changed_lines, a reviewer who did not say the runtime behaviour is
// unchanged ("no"), and a worker who did not state its category and
// reasoning each block it, as do a file in the project's walls and a
// public-repo hygiene hit (counted: the blocker names no term); with none
// it is auto-eligible.
//
// The reports are read as Python reads them: a work or review report that
// is not an object is an error (Python's gate raised).
func Evaluate(in GateIn) (ticket.Gate, error) {
	tree, err := pyTicket(in.Ticket)
	if err != nil {
		return ticket.Gate{}, err
	}
	r, err := tree.index("reports")
	if err != nil {
		return ticket.Gate{}, err
	}
	review, err := r.dictGet("review", objectNode())
	if err != nil {
		return ticket.Gate{}, err
	}
	behaviour, err := review.dictGet("runtime_behaviour", stringNode("unknown"))
	if err != nil {
		return ticket.Gate{}, err
	}
	work, err := r.dictGet("work", objectNode())
	if err != nil {
		return ticket.Gate{}, err
	}
	category, err := work.dictGet("category", stringNode(""))
	if err != nil {
		return ticket.Gate{}, err
	}
	summary, err := work.dictGet("summary", stringNode(""))
	if err != nil {
		return ticket.Gate{}, err
	}
	reasoning := textx.Strip(category.str()) != "" && textx.Strip(summary.str()) != ""

	limit := in.Config.MaxChangedLines()
	outside := []string{}
	for _, f := range in.Files {
		allowed := false
		for _, p := range in.Config.Gate.AllowedPaths {
			if textx.Fnmatch(f, p) {
				allowed = true
				break
			}
		}
		if !allowed {
			outside = append(outside, f)
		}
	}
	g := ticket.Gate{
		ChecksPass:      in.ChecksPass,
		Files:           append([]string{}, in.Files...),
		ChangedLines:    in.Size,
		WithinSize:      in.Size <= limit,
		OutsideAllowed:  outside,
		ReasoningStated: reasoning,
		Head:            in.Head,
		Base:            in.Base,
		Blockers:        []string{},
		Walls:           append([]string(nil), in.Walls...),
		HygieneFindings: len(in.Hygiene),
	}
	if err := setBehaviour(&g, in.Ticket, behaviour); err != nil {
		return ticket.Gate{}, err
	}
	saysNo := behaviour.isStr("no")
	for _, b := range []struct {
		why    string
		failed bool
	}{
		{"tests or lint failed", !in.ChecksPass},
		{"files outside the allowed paths: " + strings.Join(outside, ", "), len(outside) > 0},
		{fmt.Sprintf("%d changed lines (limit %d)", in.Size, limit), !g.WithinSize},
		{"reviewer says runtime behaviour changes: " + behaviour.str(), !saysNo},
		{"worker did not state its category and reasoning", !reasoning},
		{"touches the walls", len(in.Walls) > 0},
		{fmt.Sprintf("public-repo hygiene: %d finding(s), see the gate output", len(in.Hygiene)), len(in.Hygiene) > 0},
	} {
		if b.failed {
			g.Blockers = append(g.Blockers, b.why)
		}
	}
	g.AutoEligible = len(g.Blockers) == 0
	return g, nil
}

// setBehaviour records the reviewer's runtime_behaviour on g as the
// ticket holds it, or the string "unknown" when the review report does not
// say.
//
// The gate may not import jsonx, so the value is taken from the
// ticket's own reports, and "unknown" is made by ticket.Decode.
func setBehaviour(g *ticket.Gate, t *ticket.Ticket, behaviour node) error {
	if review, ok := t.Reports.Lookup("review"); ok {
		if o, ok := review.Object(); ok {
			if v, ok := o.Lookup("runtime_behaviour"); ok {
				g.RuntimeBehaviour = v
				return nil
			}
		}
	}
	if !behaviour.isStr("unknown") {
		return errors.New("gate: the review report changed while it was read")
	}
	u, _, err := ticket.Decode([]byte(`{"gate": {"runtime_behaviour": "unknown"}}`))
	if err != nil {
		return err
	}
	g.RuntimeBehaviour = u.Gate.V.RuntimeBehaviour
	return nil
}

// Rule is gate_rule: the rule sentence of the move after Gate 1.
func Rule(cfg *config.Config, g ticket.Gate) string {
	if len(g.Blockers) > 0 {
		return "gate: needs a maintainer: " + strings.Join(g.Blockers, "; ")
	}
	return fmt.Sprintf("gate: auto-merge eligible: tests and lint pass, only allowed paths, %d changed lines "+
		"(limit %d), reviewer says no runtime change", g.ChangedLines, cfg.MaxChangedLines())
}

// Record puts Gate 1's result on the ticket and moves it as the tick does:
// failed checks, or public-repo hygiene hits, need a person
// (gate_failed), anything else goes to
// ready_for_pr (gate_passed), saying what keeps it from auto-merging. The
// caller writes the audit (WriteAudit) and updates an open PR.
//
// Checks that ran on another host say so: "(checks on <host>)" ends the
// move's detail.
func Record(t *ticket.Ticket, cfg *config.Config, g ticket.Gate, now time.Time) {
	t.Gate = ticket.Some(g)
	rule := Rule(cfg, g)
	where := ""
	if g.Host != "" {
		where = " (checks on " + g.Host + ")"
	}
	if !g.ChecksPass {
		detail := "gate: tests or lint failed on a fresh checkout"
		if g.HygieneFindings > 0 {
			detail += fmt.Sprintf("; public-repo hygiene: %d finding(s)", g.HygieneFindings)
		}
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonGateFailed, detail+where, rule, "", now)
		return
	}
	if g.HygieneFindings > 0 {
		ticket.Move(t, string(model.ColNeedsHuman), model.ReasonGateFailed,
			fmt.Sprintf("gate: public-repo hygiene: %d finding(s)", g.HygieneFindings)+where, rule, "", now)
		return
	}
	detail := "auto-merge eligible"
	if !g.AutoEligible {
		detail = "needs human review: " + textx.Prefix(strings.Join(g.Blockers, "; "), 200)
	}
	ticket.Move(t, string(model.ColReadyForPR), model.ReasonGatePassed, detail+where, rule, "", now)
}

// DiffReader is what ClassifyBranch reads (gitread.Repo).
type DiffReader interface {
	ChangedFiles(ctx context.Context, branch string) ([]string, error)
	AddedLines(ctx context.Context, branch string) ([]string, error)
}

// ClassifyBranch is classify(project, branch): the security tier of what
// the branch changes against the project's base, with the project's own
// tier patterns.
func ClassifyBranch(ctx context.Context, repo DiffReader, branch string, p config.Project) (int, error) {
	files, err := repo.ChangedFiles(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("classify %s: %w", branch, err)
	}
	added, err := repo.AddedLines(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("classify %s: %w", branch, err)
	}
	return security.Classify(files, added, security.ProjectPatterns(p)), nil
}
